#!/bin/sh
set -eu
set -f

umask 077

die() { printf '错误：%s\n' "$*" >&2; exit 1; }
note() { printf '%s\n' "$*"; }

[ -n "${HOME:-}" ] || die 'HOME 未设置'
case "$HOME" in
  /*) ;;
  *) die 'HOME 必须是绝对路径' ;;
esac
[ "$HOME" != / ] || die '拒绝使用根目录作为 HOME'

case "$0" in
  */*) bootstrap_path=$0 ;;
  *) die '请通过明确路径运行 bootstrap.sh' ;;
esac
[ ! -L "$bootstrap_path" ] || die 'bootstrap.sh 不能是软链接'
script_parent=$(dirname -- "$bootstrap_path")
script_dir_logical=$(CDPATH= cd -- "$script_parent" 2>/dev/null && pwd -L) || \
  die '无法访问安装包目录'
script_dir=$(CDPATH= cd -- "$script_parent" 2>/dev/null && pwd -P) || \
  die '无法解析安装包目录'
[ "$script_dir_logical" = "$script_dir" ] || \
  die '安装包目录路径不能经过软链接'

uname_cmd=${CC_UNAME:-uname}
curl_cmd=${CC_CURL:-curl}
shasum_cmd=${CC_SHASUM:-shasum}
tar_cmd=${CC_TAR:-tar}
codesign_cmd=${CC_CODESIGN:-codesign}
install_cmd=${CC_TEST_INSTALL_SH:-$script_dir/install.sh}

activate=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --activate) [ "$activate" -eq 0 ] || die '重复的 --activate'; activate=1 ;;
    *) die "未知参数：$1" ;;
  esac
  shift
done

work_dir=
target_root=$HOME/cc-connect
target_source=$target_root/source
source_next_owned=0
source_next=$target_root/source.next-bootstrap
installer_dir=$target_root/installer
installer_source=$installer_dir/source
installer_source_next=$installer_dir/source.next-bootstrap
installer_source_backup=$installer_dir/source.previous-bootstrap
installer_source_next_owned=0
installer_source_backup_owned=0
installer_source_swap_started=0
installer_source_had_old=0
installer_source_preserve_backup=0
cleanup() {
  status=$?
  trap - EXIT HUP INT TERM
  if [ "$status" -ne 0 ] && [ "$installer_source_swap_started" -eq 1 ]; then
    if [ "$installer_source_had_old" -eq 1 ] && \
       [ "$installer_source_backup_owned" -eq 1 ] && \
       { [ -e "$installer_source_backup" ] || [ -L "$installer_source_backup" ]; }; then
      if [ -e "$installer_source" ] || [ -L "$installer_source" ]; then
        if ! rm -rf "$installer_source"; then
          status=1
          installer_source_preserve_backup=1
        fi
      fi
      if [ "$installer_source_preserve_backup" -eq 0 ] && \
         mv "$installer_source_backup" "$installer_source"; then
        installer_source_backup_owned=0
      else
        status=1
        installer_source_preserve_backup=1
      fi
    elif [ "$installer_source_had_old" -eq 0 ] && \
         { [ -e "$installer_source" ] || [ -L "$installer_source" ]; }; then
      rm -rf "$installer_source" || status=1
    fi
  fi
  if [ "$installer_source_next_owned" -eq 1 ]; then
    rm -rf "$installer_source_next" || status=1
  fi
  if [ "$installer_source_backup_owned" -eq 1 ] && \
     [ "$installer_source_preserve_backup" -eq 0 ] && \
     { [ -e "$installer_source_backup" ] || [ -L "$installer_source_backup" ]; }; then
    rm -rf "$installer_source_backup" || status=1
  fi
  if [ "$source_next_owned" -eq 1 ]; then rm -rf "$source_next" || status=1; fi
  if [ -n "$work_dir" ]; then rm -rf "$work_dir" || status=1; fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

for required_input in \
  "$script_dir/bootstrap.sh" \
  "$script_dir/checksums.txt" \
  "$script_dir/VERSION" \
  "$script_dir/go-toolchains.txt" \
  "$script_dir/setup.sh" \
  "$script_dir/install.sh" \
  "$script_dir/source/go.mod"
do
  [ -f "$required_input" ] && [ ! -L "$required_input" ] || \
    die "安装包输入必须是非软链接普通文件：$required_input"
done
[ -x "$script_dir/bootstrap.sh" ] || die 'bootstrap.sh 不可执行'
[ -x "$script_dir/setup.sh" ] || die 'setup.sh 不可执行'
[ -x "$script_dir/install.sh" ] || die 'install.sh 不可执行'
[ -x "$install_cmd" ] && [ -f "$install_cmd" ] && [ ! -L "$install_cmd" ] || \
  die "安装委托程序必须是非软链接普通可执行文件：$install_cmd"

[ ! -L "$target_root" ] || die '安装根目录不能是软链接'
[ ! -L "$target_source" ] || die '统一 source 目录不能是软链接'
if [ -e "$target_source" ] && [ ! -d "$target_source" ]; then
  die '统一 source 路径已存在且不是目录'
fi
if [ -e "$source_next" ] || [ -L "$source_next" ]; then
  die "本轮 source 候选路径必须不存在：$source_next"
fi
[ ! -L "$installer_dir" ] || die '安装材料目录不能是软链接'
if [ -e "$installer_dir" ] && [ ! -d "$installer_dir" ]; then
  die '安装材料路径已存在且不是目录'
fi
[ ! -L "$installer_source" ] || die '发行版 source 快照不能是软链接'
if [ -e "$installer_source" ] && [ ! -d "$installer_source" ]; then
  die '发行版 source 快照已存在且不是目录'
fi
for snapshot_staging_path in "$installer_source_next" "$installer_source_backup"; do
  if [ -e "$snapshot_staging_path" ] || [ -L "$snapshot_staging_path" ]; then
    die "发行版 source 快照候选路径必须不存在：$snapshot_staging_path"
  fi
done

os_name=$("$uname_cmd" -s) || die '无法识别操作系统'
[ "$os_name" = Darwin ] || die '仅支持 macOS (Darwin)'
machine=$("$uname_cmd" -m) || die '无法识别处理器架构'
case "$machine" in
  arm64) arch=arm64 ;;
  x86_64) arch=amd64 ;;
  *) die "不支持的 macOS 架构：$machine" ;;
esac

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/cc-connect-bootstrap.XXXXXX") || \
  die '无法创建临时目录'
work_dir=$(CDPATH= cd -- "$work_dir" && pwd -P) || die '无法解析临时目录'
checksum_members=$work_dir/checksum-members
expected_members=$work_dir/expected-members
: >"$checksum_members"

require_safe_bundle_member() {
  member_path=$1
  case "$member_path" in
    ./*) relative_path=${member_path#./} ;;
    *) die "校验和成员必须是以 ./ 开头的相对路径：$member_path" ;;
  esac
  [ -n "$relative_path" ] || die '校验和成员不能为空'
  case "/$relative_path/" in
    *//*|*/./*|*/../*) die "校验和成员路径不安全：$member_path" ;;
  esac
  [ "$relative_path" != checksums.txt ] || die '校验和文件不能校验自身'

  current_path=$script_dir
  old_ifs=$IFS
  IFS=/
  set -- $relative_path
  IFS=$old_ifs
  for component in "$@"; do
    [ -n "$component" ] || die "校验和成员含空路径段：$member_path"
    [ "$component" != . ] && [ "$component" != .. ] || \
      die "校验和成员含路径穿越：$member_path"
    current_path=$current_path/$component
    [ ! -L "$current_path" ] || die "校验和成员不能经过软链接：$member_path"
  done
  [ -f "$current_path" ] || die "校验和成员不是普通文件：$member_path"
}

checksum_count=0
while IFS= read -r checksum_line; do
  digest=${checksum_line%% *}
  case "$digest" in
    ''|*[!0123456789abcdefABCDEF]*) die "无效校验和：$checksum_line" ;;
  esac
  [ "${#digest}" -eq 64 ] || die "无效校验和长度：$checksum_line"
  prefix="$digest  "
  case "$checksum_line" in
    "$prefix"*) member=${checksum_line#"$prefix"} ;;
    *) die "无效校验和记录：$checksum_line" ;;
  esac
  require_safe_bundle_member "$member"
  printf '%s\n' "$member" >>"$checksum_members"
  checksum_count=$((checksum_count + 1))
done <"$script_dir/checksums.txt"
[ "$checksum_count" -gt 0 ] || die '校验和清单为空'

bundle_symlink=$(find "$script_dir" -type l -print -quit) || \
  die '无法检查安装包软链接'
[ -z "$bundle_symlink" ] || die "安装包不得包含软链接：$bundle_symlink"

(
  cd "$script_dir"
  find . -type f ! -path './checksums.txt' -print | LC_ALL=C sort
) >"$expected_members"
LC_ALL=C sort "$checksum_members" -o "$checksum_members"
cmp -s "$expected_members" "$checksum_members" || \
  die '校验和清单未精确覆盖安装包普通文件'
(
  cd "$script_dir"
  "$shasum_cmd" -a 256 -c checksums.txt >/dev/null
) || die '安装包校验和验证失败'

manifest_value() {
  manifest_key=$1
  awk -F= -v wanted="$manifest_key" '
    $1 == wanted { count++; sub(/^[^=]*=/, ""); value = $0 }
    END { if (count != 1 || value == "") exit 1; print value }
  ' "$script_dir/VERSION"
}
version=$(manifest_value version) || die 'VERSION 缺少唯一 version'
commit=$(manifest_value commit) || die 'VERSION 缺少唯一 commit'
go_version=$(manifest_value go_version) || die 'VERSION 缺少唯一 go_version'
build_time=$(manifest_value build_time) || die 'VERSION 缺少唯一 build_time'
case "$version" in *[!A-Za-z0-9._+-]*) die 'VERSION 中的 version 格式无效' ;; esac
case "$commit" in *[!A-Za-z0-9._+-]*) die 'VERSION 中的 commit 格式无效' ;; esac
case "$build_time" in *[!0-9A-Za-z:._+-]*) die 'VERSION 中的 build_time 格式无效' ;; esac
case "$go_version" in go*) ;; *) die 'VERSION 中的 go_version 格式无效' ;; esac

release_numbers=${go_version#go}
release_major=${release_numbers%%.*}
release_remainder=${release_numbers#*.}
[ "$release_remainder" != "$release_numbers" ] || die 'VERSION 中的 go_version 格式无效'
release_minor=${release_remainder%%.*}
release_patch=${release_remainder#*.}
[ "$release_patch" != "$release_remainder" ] || die 'VERSION 中的 go_version 格式无效'
case "$release_patch" in *.*) die 'VERSION 中的 go_version 格式无效' ;; esac
case "$release_major:$release_minor:$release_patch" in
  *[!0-9:]*) die 'VERSION 中的 go_version 格式无效' ;;
esac
[ -n "$release_major" ] && [ -n "$release_minor" ] && [ -n "$release_patch" ] || \
  die 'VERSION 中的 go_version 格式无效'

parse_major_minor() {
  numeric_version=$1
  numeric_version=${numeric_version#go}
  parsed_major=${numeric_version%%.*}
  remainder=${numeric_version#*.}
  [ "$remainder" != "$numeric_version" ] || return 1
  parsed_minor=${remainder%%.*}
  case "$parsed_major:$parsed_minor" in
    *[!0-9:]*) return 1 ;;
    :) return 1 ;;
  esac
  [ -n "$parsed_major" ] && [ -n "$parsed_minor" ]
}

go_directive=$(awk '$1 == "go" { count++; value=$2 } END { if (count != 1) exit 1; print value }' \
  "$script_dir/source/go.mod") || die 'source/go.mod 必须包含唯一 go 指令'
parse_major_minor "$go_directive" || die 'source/go.mod 的 Go 版本格式无效'
required_major=$parsed_major
required_minor=$parsed_minor
[ "$required_major" -eq 1 ] && [ "$required_minor" -eq 25 ] || \
  die 'source/go.mod 必须声明 Go 1.25'

go_bin=${CC_GO_BIN:-}
use_system_go=0
if [ -z "$go_bin" ]; then go_bin=$(command -v go 2>/dev/null || true); fi
if [ -n "$go_bin" ] && [ -x "$go_bin" ]; then
  system_go_output=$("$go_bin" version 2>/dev/null || true)
  system_go_version=$(printf '%s\n' "$system_go_output" | \
    awk '$1 == "go" && $2 == "version" { print $3; exit }')
  if [ -n "$system_go_version" ] && parse_major_minor "$system_go_version"; then
    if [ "$parsed_major" -gt "$required_major" ] || \
       { [ "$parsed_major" -eq "$required_major" ] && \
         [ "$parsed_minor" -ge "$required_minor" ]; }; then
      use_system_go=1
    fi
  fi
fi

if [ "$use_system_go" -eq 0 ]; then
  toolchain_line=$(awk -v wanted_version="$go_version" -v wanted_arch="$arch" '
    $1 == wanted_version && $2 == "darwin" && $3 == wanted_arch {
      count++; line=$0
    }
    END { if (count != 1) exit 1; print line }
  ' "$script_dir/go-toolchains.txt") || \
    die "go-toolchains.txt 未精确匹配唯一行：$go_version darwin $arch"
  set -- $toolchain_line
  [ "$#" -eq 4 ] || die 'go-toolchains.txt 匹配行必须为四列'
  [ "$1" = "$go_version" ] && [ "$2" = darwin ] && [ "$3" = "$arch" ] || \
    die 'go-toolchains.txt 匹配行不一致'
  go_digest=$4
  case "$go_digest" in *[!0123456789abcdefABCDEF]*) die 'Go 工具链 SHA-256 格式无效' ;; esac
  [ "${#go_digest}" -eq 64 ] || die 'Go 工具链 SHA-256 长度无效'

  archive=$work_dir/$go_version.darwin-$arch.tar.gz
  download_url=https://go.dev/dl/$go_version.darwin-$arch.tar.gz
  "$curl_cmd" --fail --location --output "$archive" "$download_url" || \
    die 'Go 工具链下载失败'
  archive_checksum=$archive.sha256
  printf '%s  %s\n' "$go_digest" "$archive" >"$archive_checksum"
  "$shasum_cmd" -a 256 -c "$archive_checksum" >/dev/null || \
    die 'Go 工具链 SHA-256 验证失败'
  toolchain_dir=$work_dir/toolchain
  mkdir "$toolchain_dir"
  "$tar_cmd" -xzf "$archive" -C "$toolchain_dir" || die 'Go 工具链解压失败'
  go_bin=$toolchain_dir/go/bin/go
  [ -f "$go_bin" ] && [ ! -L "$go_bin" ] && [ -x "$go_bin" ] || \
    die '解压后的 Go 程序无效'
  downloaded_go_output=$("$go_bin" version 2>/dev/null) || die '无法运行临时 Go'
  downloaded_go_version=$(printf '%s\n' "$downloaded_go_output" | \
    awk '$1 == "go" && $2 == "version" { print $3; exit }')
  [ "$downloaded_go_version" = "$go_version" ] || die '临时 Go 版本与锁定版本不一致'
fi

candidate=$work_dir/cc-connect
(
  cd "$script_dir/source"
  CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" "$go_bin" build \
    -mod=readonly -trimpath -tags goolm \
    -ldflags "-s -w -X main.version=$version -X main.commit=$commit -X main.buildTime=$build_time" \
    -o "$candidate" ./cmd/cc-connect
) || die 'cc-connect 本地构建失败'
[ -f "$candidate" ] && [ ! -L "$candidate" ] && [ -x "$candidate" ] || \
  die '本地构建未产生有效可执行文件'

binary_build_info=$("$go_bin" version -m "$candidate" 2>/dev/null) || \
  die '无法读取候选程序架构'
case "$binary_build_info" in
  *"darwin/$arch"*) ;;
  *)
    printf '%s\n' "$binary_build_info" | grep -F "GOOS=darwin" >/dev/null && \
      printf '%s\n' "$binary_build_info" | grep -F "GOARCH=$arch" >/dev/null || \
      die "候选程序架构不是 darwin/$arch"
    ;;
esac

verified_version_output() {
  version_candidate=$1
  candidate_output=$("$version_candidate" --version 2>/dev/null) || return 1
  printf '%s\n' "$candidate_output" | awk \
    -v expected_version="$version" \
    -v expected_commit="$commit" \
    -v expected_build_time="$build_time" '
      NR == 1 {
        if (NF != 2 || $1 != "cc-connect" || $2 != expected_version) exit 1
      }
      NR == 2 {
        if (NF != 2 || $1 != "commit:" || $2 != expected_commit) exit 1
      }
      NR == 3 {
        if (NF != 2 || $1 != "built:" || $2 != expected_build_time) exit 1
      }
      NR > 3 { exit 1 }
      END { if (NR != 3) exit 1 }
    ' || return 1
  printf '%s\n' "$candidate_output"
}

unsigned_version_output=$(verified_version_output "$candidate") || \
  die '候选程序 --version 与安装包 VERSION 不一致'

"$codesign_cmd" --force --sign - --identifier com.cc-connect.service "$candidate" || \
  die '候选程序 ad-hoc 签名失败'
"$codesign_cmd" --verify --strict "$candidate" || die '候选程序严格签名验证失败'
signed_version_output=$(verified_version_output "$candidate") || \
  die '签名后的候选程序 --version 与安装包 VERSION 不一致'
[ "$signed_version_output" = "$unsigned_version_output" ] || \
  die '候选程序 --version 在签名后发生变化'

if [ "$activate" -eq 1 ]; then
  "$install_cmd" --binary "$candidate" --activate
else
  "$install_cmd" --binary "$candidate"
fi

populate_source=0
if [ ! -e "$target_source" ]; then
  populate_source=1
elif ! find "$target_source" -mindepth 1 -print -quit | grep . >/dev/null; then
  populate_source=1
fi
if [ "$populate_source" -eq 1 ]; then
  source_next_owned=1
  cp -pR "$script_dir/source" "$source_next"
fi

mkdir -p "$installer_dir"
[ -d "$installer_dir" ] && [ ! -L "$installer_dir" ] || \
  die '安装后的安装材料目录无效'
[ ! -L "$installer_source" ] || die '安装后的发行版 source 快照不能是软链接'
if [ -e "$installer_source" ] && [ ! -d "$installer_source" ]; then
  die '安装后的发行版 source 快照不是目录'
fi
for snapshot_staging_path in "$installer_source_next" "$installer_source_backup"; do
  if [ -e "$snapshot_staging_path" ] || [ -L "$snapshot_staging_path" ]; then
    die "发行版 source 快照候选路径被占用：$snapshot_staging_path"
  fi
done

installer_source_next_owned=1
cp -pR "$script_dir/source" "$installer_source_next"
if [ -d "$installer_source" ]; then
  installer_source_had_old=1
  installer_source_swap_started=1
  installer_source_backup_owned=1
  mv "$installer_source" "$installer_source_backup"
else
  installer_source_had_old=0
  installer_source_swap_started=1
fi
mv "$installer_source_next" "$installer_source"
installer_source_next_owned=0

if [ "$populate_source" -eq 1 ]; then
  if [ -d "$target_source" ]; then rmdir "$target_source"; fi
  mv "$source_next" "$target_source"
  source_next_owned=0
else
  note "已保留现有 source；本发行版快照仍可在 $HOME/cc-connect/installer/source 查看。"
fi

installer_source_swap_started=0
if [ "$installer_source_had_old" -eq 1 ]; then
  rm -rf "$installer_source_backup"
  installer_source_backup_owned=0
fi
