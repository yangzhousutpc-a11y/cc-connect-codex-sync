#!/bin/sh
set -eu
set -f

umask 077

die() {
  printf '错误：%s\n' "$*" >&2
  exit 1
}

note() {
  printf '%s\n' "$*"
}

[ "$(uname -s)" = Darwin ] || die '一键安装仅支持 macOS'
macos_version=$(sw_vers -productVersion 2>/dev/null) || die '无法读取 macOS 版本'
macos_major=${macos_version%%.*}
case "$macos_major" in
  ''|*[!0-9]*) die '无法识别 macOS 版本' ;;
esac
[ "$macos_major" -ge 12 ] || die '需要 macOS 12 或更高版本'

for required_command in curl tar shasum grep mktemp sed wc; do
  command -v "$required_command" >/dev/null 2>&1 ||
    die "缺少系统命令：$required_command"
done

repository_url=https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync
latest_url=$(curl -fsSL --proto '=https' --tlsv1.2 \
  --connect-timeout 15 --max-time 60 \
  -o /dev/null -w '%{url_effective}' \
  "$repository_url/releases/latest") ||
  die '无法定位最新 GitHub Release'

case "$latest_url" in
  "$repository_url"/releases/tag/*) tag=${latest_url##*/} ;;
  *) die '最新 Release 跳转到了非预期地址' ;;
esac
printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' ||
  die '最新 Release 标签格式异常'

archive_name=cc-connect-codex-sync-$tag-macos-source.tar.gz
checksum_name=$archive_name.sha256
download_base=$repository_url/releases/download/$tag

temp_root=
cleanup() {
  cleanup_status=$?
  trap - EXIT HUP INT TERM
  if [ -n "$temp_root" ]; then
    temp_parent=$(dirname -- "$temp_root")
    case "$temp_root" in
      "$temp_parent"/cc-connect-install.*)
        [ "$temp_parent" != / ] || exit 1
        rm -rf -- "$temp_root" || cleanup_status=1
        ;;
      *) cleanup_status=1 ;;
    esac
  fi
  exit "$cleanup_status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

temp_logical=$(mktemp -d "${TMPDIR:-/tmp}/cc-connect-install.XXXXXX") ||
  die '无法创建安装临时目录'
case "$temp_logical" in
  /*) ;;
  *) die '安装临时目录不是绝对路径' ;;
esac
temp_root=$(CDPATH= cd -- "$temp_logical" && pwd -P) ||
  die '无法解析安装临时目录'
temp_parent=$(dirname -- "$temp_root")
case "$temp_root" in
  "$temp_parent"/cc-connect-install.*) ;;
  *) die '安装临时目录名称异常' ;;
esac
[ "$temp_parent" != / ] || die '拒绝在根目录创建安装临时目录'

archive_path=$temp_root/$archive_name
checksum_path=$temp_root/$checksum_name

note "正在下载 $tag 源码安装包…"
curl -fL --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 300 \
  --retry 2 -o "$archive_path" "$download_base/$archive_name" ||
  die '源码安装包下载失败'
curl -fL --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 60 \
  --retry 2 -o "$checksum_path" "$download_base/$checksum_name" ||
  die 'SHA-256 校验文件下载失败'

[ "$(wc -l <"$checksum_path" | tr -d '[:space:]')" = 1 ] ||
  die 'SHA-256 校验文件必须只有一条记录'
IFS=' ' read -r expected_hash expected_name unexpected_field <"$checksum_path" ||
  die '无法读取 SHA-256 校验文件'
[ "${#expected_hash}" -eq 64 ] || die 'SHA-256 摘要长度异常'
case "$expected_hash" in
  *[!0-9A-Fa-f]*) die 'SHA-256 摘要格式异常' ;;
esac
[ "$expected_name" = "$archive_name" ] ||
  die 'SHA-256 校验文件中的文件名不匹配'
[ -z "${unexpected_field:-}" ] || die 'SHA-256 校验文件包含多余字段'

(cd "$temp_root" && shasum -a 256 -c "$checksum_name") ||
  die '源码安装包 SHA-256 校验失败'

archive_list=$temp_root/archive.list
tar -tzf "$archive_path" >"$archive_list" ||
  die '无法读取源码安装包'
[ -s "$archive_list" ] || die '源码安装包为空'
while IFS= read -r archive_entry; do
  [ -n "$archive_entry" ] || die '源码安装包包含空路径'
  case "$archive_entry" in
    /*|../*|*/../*|*/..|*//*)
      die '源码安装包包含不安全路径'
      ;;
    cc-connect-source-install|cc-connect-source-install/*) ;;
    *) die '源码安装包包含非预期顶层目录' ;;
  esac
done <"$archive_list"

tar -xzf "$archive_path" -C "$temp_root" ||
  die '源码安装包解压失败'
bundle=$temp_root/cc-connect-source-install
setup_path=$bundle/setup.sh
[ -d "$bundle" ] && [ ! -L "$bundle" ] ||
  die '源码安装包目录无效'
[ -f "$setup_path" ] && [ ! -L "$setup_path" ] && [ -x "$setup_path" ] ||
  die '源码安装包缺少可执行的 setup.sh'

note '下载和校验完成，正在启动安装向导…'
"$setup_path"
