#!/bin/sh

umask 077

cc_die() { printf '错误：%s\n' "$*" >&2; exit 1; }
cc_note() { printf '%s\n' "$*"; }
cc_layout_root() { printf '%s\n' "${CC_CONNECT_ROOT:-$HOME/cc-connect}"; }
cc_data_link() { printf '%s\n' "$HOME/.cc-connect"; }
cc_cli_link() { printf '%s\n' "$HOME/.local/bin/cc-connect"; }
cc_plist_path() { printf '%s\n' "$HOME/Library/LaunchAgents/com.cc-connect.service.plist"; }
cc_daemon_meta_path() { printf '%s\n' "$1/data/daemon.json"; }

cc_require_safe_home() {
  [ -n "${HOME:-}" ] || cc_die 'HOME 未设置'
  case "$HOME" in /*) ;; *) cc_die 'HOME 必须是绝对路径' ;; esac
  [ "$HOME" != "/" ] || cc_die '拒绝使用根目录作为 HOME'
}

cc_require_physical_existing_prefix() {
  requested_path=$1
  path_label=$2
  probe_path=$requested_path
  while [ ! -e "$probe_path" ] && [ ! -L "$probe_path" ]; do
    parent_path=$(dirname -- "$probe_path")
    [ "$parent_path" != "$probe_path" ] || break
    probe_path=$parent_path
  done
  [ ! -L "$probe_path" ] || cc_die "$path_label 的已有路径不能是软链接：$probe_path"
  physical_probe=$(CDPATH= cd -- "$probe_path" 2>/dev/null && pwd -P) || cc_die "无法验证 $path_label 的已有路径：$probe_path"
  [ "$physical_probe" = "$probe_path" ] || cc_die "$path_label 的已有路径不是规范物理路径：$probe_path -> $physical_probe"
}

cc_require_safe_root() {
  root_path=$1
  case "$root_path" in /*) ;; *) cc_die 'CC_CONNECT_ROOT 必须是绝对路径' ;; esac
  case "$root_path" in
    /|"$HOME") cc_die 'CC_CONNECT_ROOT 不能是 / 或 HOME 本身' ;;
    *//*|*/|*/./*|*/.|*/../*|*/..) cc_die 'CC_CONNECT_ROOT 必须是规范绝对路径，不能包含 //、尾部 /、. 或 .. 路径段' ;;
  esac
  [ ! -L "$root_path" ] || cc_die "CC_CONNECT_ROOT 不能是软链接：$root_path"
  if [ -e "$root_path" ] && [ ! -d "$root_path" ]; then
    cc_die "CC_CONNECT_ROOT 已存在且不是目录：$root_path"
  fi
  cc_require_physical_existing_prefix "$root_path" CC_CONNECT_ROOT
}

cc_require_regular_or_absent() {
  file_path=$1
  path_label=$2
  [ ! -L "$file_path" ] || cc_die "$path_label 不能是软链接：$file_path"
  if [ -e "$file_path" ] && [ ! -f "$file_path" ]; then
    cc_die "$path_label 必须是普通文件或不存在：$file_path"
  fi
}

cc_require_absent() {
  file_path=$1
  path_label=$2
  if [ -e "$file_path" ] || [ -L "$file_path" ]; then
    cc_die "$path_label 必须不存在：$file_path"
  fi
}

cc_require_executable_regular() {
  file_path=$1
  path_label=$2
  cc_require_regular_or_absent "$file_path" "$path_label"
  [ -f "$file_path" ] || cc_die "$path_label 不存在：$file_path"
  [ -x "$file_path" ] || cc_die "$path_label 不可执行：$file_path"
}

cc_require_safe_layout_paths() {
  root_path=$1
  cc_require_safe_root "$root_path"
  for category in source runtime data installer backups; do
    category_path=$root_path/$category
    [ ! -L "$category_path" ] || cc_die "分类目录不能是软链接：$category_path"
    if [ -e "$category_path" ] && [ ! -d "$category_path" ]; then
      cc_die "分类路径已存在且不是目录：$category_path"
    fi
  done
}

cc_require_owned_or_absent_link() {
  link_path=$1
  target_path=$2
  if [ -L "$link_path" ]; then
    [ "$(readlink "$link_path")" = "$target_path" ] || cc_die "$link_path 已指向其他位置；请先迁移数据后再安装"
    return
  fi
  [ ! -e "$link_path" ] || cc_die "$link_path 已存在且不是软链接"
}

cc_replace_owned_link() {
  link_path=$1
  target_path=$2
  cc_require_owned_or_absent_link "$link_path" "$target_path"
  [ -L "$link_path" ] && return
  mkdir -p "$(dirname -- "$link_path")"
  ln -s "$target_path" "$link_path"
}

cc_remove_owned_link() {
  link_path=$1
  target_path=$2
  if [ -L "$link_path" ] && [ "$(readlink "$link_path")" = "$target_path" ]; then rm "$link_path"; fi
}

cc_find_readme() {
  source_dir=$1
  if [ -f "$source_dir/README.md" ]; then
    printf '%s\n' "$source_dir/README.md"
  elif [ -f "$source_dir/README.zh-CN.md" ]; then
    printf '%s\n' "$source_dir/README.zh-CN.md"
  else
    return 1
  fi
}

cc_restrict_private_dir() { chmod go-rwx "$1"; }
cc_restrict_config() { chmod u-x,go-rwx "$1"; }

cc_launchctl() { "${CC_LAUNCHCTL:-launchctl}" "$@"; }
cc_pgrep() { "${CC_PGREP:-pgrep}" "$@"; }
cc_plutil() { "${CC_PLUTIL:-plutil}" "$@"; }
cc_sha256() { "${CC_SHASUM:-shasum}" -a 256 "$1" | awk '{print $1}'; }
cc_pgrep_runtime() {
  runtime_path=$1
  runtime_pattern=$(printf '%s\n' "$runtime_path" | sed 's/[][\\.^$*+?(){}|]/\\&/g')
  cc_pgrep -f "$runtime_pattern"
}

cc_prune_install_backups() {
  backups_root=$1
  backup_count=0
  for backup_path in "$backups_root"/install-*; do
    [ -d "$backup_path" ] && [ ! -L "$backup_path" ] || continue
    backup_count=$((backup_count + 1))
  done
  remove_count=$((backup_count - 3))
  [ "$remove_count" -gt 0 ] || return 0
  # install-* 名称以固定宽度时间戳开头；shell glob 按名称排序且保留路径边界。
  for backup_path in "$backups_root"/install-*; do
    [ -d "$backup_path" ] && [ ! -L "$backup_path" ] || continue
    [ "$remove_count" -gt 0 ] || break
    rm -rf "$backup_path" || return 1
    remove_count=$((remove_count - 1))
  done
}

cc_launchd_target_loaded() {
  domain=$1
  uid=$(id -u)
  cc_launchctl print "$domain/$uid/com.cc-connect.service" >/dev/null 2>&1
}

cc_bootout_launchagent() {
  uid=$(id -u)
  cc_launchctl bootout "gui/$uid/com.cc-connect.service" >/dev/null 2>&1 || true
  cc_launchctl bootout "user/$uid/com.cc-connect.service" >/dev/null 2>&1 || true
}

cc_restore_launchagent() {
  plist_path=$1
  uid=$(id -u)
  cc_bootout_launchagent
  if cc_launchctl bootstrap "gui/$uid" "$plist_path" >/dev/null 2>&1; then
    cc_launchctl kickstart -kp "gui/$uid/com.cc-connect.service" >/dev/null 2>&1
    return
  fi
  cc_launchctl bootstrap "user/$uid" "$plist_path" >/dev/null 2>&1
  cc_launchctl kickstart -kp "user/$uid/com.cc-connect.service" >/dev/null 2>&1
}
