#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
. "$script_dir/lib.sh"
cc_require_safe_home
root=$(cc_layout_root)
cc_require_safe_layout_paths "$root"

runtime_path="$root/runtime/cc-connect"
data_link=$(cc_data_link)
cli_link=$(cc_cli_link)
plist_path=$(cc_plist_path)
meta_path=$(cc_daemon_meta_path "$root")

if [ -e "$runtime_path" ] || [ -L "$runtime_path" ]; then
  cc_require_executable_regular "$runtime_path" '统一 runtime'
  if ! "$runtime_path" daemon uninstall; then
    cc_die 'daemon 卸载失败；服务和两个入口均已保留'
  fi
  daemon_status=$($runtime_path daemon status 2>&1 || true)
  if ! printf '%s\n' "$daemon_status" | grep -E '^[[:space:]]*Status:[[:space:]]+Not installed[[:space:]]*$' >/dev/null; then
    cc_die 'daemon 卸载后仍显示已安装或运行；两个入口均已保留'
  fi
elif [ -e "$plist_path" ] || [ -e "$meta_path" ]; then
  cc_die '统一 runtime 不可用，但服务元数据仍存在；为避免失联，两个入口均已保留'
fi

if cc_launchd_target_loaded gui || cc_launchd_target_loaded user || [ -e "$plist_path" ]; then
  cc_die 'launchd 服务仍存在或运行；两个入口均已保留'
fi
remaining_pids=$(cc_pgrep_runtime "$runtime_path" 2>/dev/null | awk '/^[0-9]+$/ && !seen[$0]++ {print}' || true)
if [ -n "$remaining_pids" ]; then
  cc_die "仍有统一 runtime 进程未退出（PID: $(printf '%s\n' "$remaining_pids" | tr '\n' ' ')）；两个入口均已保留"
fi

cc_remove_owned_link "$cli_link" "$runtime_path"
cc_remove_owned_link "$data_link" "$root/data"
printf '已确认服务未安装且未运行；受管入口已移除，数据仍保留在 %s/data\n' "$root"
