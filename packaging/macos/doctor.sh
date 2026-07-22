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
expected_log="$root/data/logs/cc-connect.log"
failures=0

cc_require_executable_regular "$runtime_path" '统一 runtime'

pass() { printf '✅ %s\n' "$*"; }
fail_check() { printf '❌ %s\n' "$*"; failures=$((failures + 1)); }
check_dir() {
  if [ -d "$1" ] && [ ! -L "$1" ]; then pass "$1"; else fail_check "$1 必须是真实目录"; fi
}
check_link() {
  if [ -L "$1" ] && [ "$(readlink "$1")" = "$2" ]; then
    pass "$1 -> $2"
  else
    fail_check "$1 应指向 $2"
  fi
}
plist_value() { cc_plutil -extract "$2" raw -o - "$1" 2>/dev/null; }
check_structured_value() {
  file_path=$1
  key_path=$2
  expected=$3
  label=$4
  actual=$(plist_value "$file_path" "$key_path" || true)
  if [ "$actual" = "$expected" ]; then pass "$label = $expected"; else fail_check "$label 应为 ${expected}，实际为 ${actual:-<缺失>}"; fi
}

for dir in source runtime data installer backups; do check_dir "$root/$dir"; done
pass "运行程序是非软链接普通可执行文件：$runtime_path"
check_link "$data_link" "$root/data"
check_link "$cli_link" "$runtime_path"
for forbidden in config.toml sessions logs weixin daemon.json backups; do
  [ ! -e "$root/installer/$forbidden" ] || fail_check "installer 含活动数据：$forbidden"
done

resolved_cli=$(command -v cc-connect 2>/dev/null || true)
resolved_program=$resolved_cli
if [ -n "$resolved_cli" ] && [ -L "$resolved_cli" ]; then resolved_program=$(readlink "$resolved_cli"); fi
if [ "$resolved_program" = "$runtime_path" ]; then
  pass "PATH 中的 cc-connect 解析到统一 runtime"
else
  fail_check "PATH 中的 cc-connect 解析为 ${resolved_cli:-<未找到>}，而不是 $runtime_path"
fi
if [ -n "$resolved_cli" ] && [ -x "$resolved_cli" ] && [ -x "$runtime_path" ]; then
  cli_hash=$(cc_sha256 "$resolved_cli" 2>/dev/null || true)
  runtime_hash=$(cc_sha256 "$runtime_path" 2>/dev/null || true)
  [ -n "$cli_hash" ] && [ "$cli_hash" = "$runtime_hash" ] && pass '命令行程序与统一 runtime 校验值一致' || fail_check '命令行程序与统一 runtime 校验值不一致'
fi

if [ ! -f "$root/data/config.toml" ]; then
  printf '⚠️ 尚未创建 config.toml，服务未激活\n'
  [ "$failures" -eq 0 ] || exit 1
  printf '✅ 统一目录检查通过（配置待完成）\n'
  exit 0
fi

daemon_status=$($runtime_path daemon status 2>&1 || true)
if printf '%s\n' "$daemon_status" | grep -E '^[[:space:]]*Status:[[:space:]]+Running[[:space:]]*$' >/dev/null; then
  pass 'daemon status = Running'
else
  fail_check 'daemon 未安装或未运行'
fi
status_pid=$(printf '%s\n' "$daemon_status" | awk '/^[[:space:]]*PID:[[:space:]]*[0-9]+[[:space:]]*$/ {print $2; exit}')

if [ -f "$plist_path" ] && [ ! -L "$plist_path" ]; then
  pass "LaunchAgent 存在：$plist_path"
  check_structured_value "$plist_path" ProgramArguments.0 "$runtime_path" 'LaunchAgent ProgramArguments[0]'
  check_structured_value "$plist_path" WorkingDirectory "$root/data" 'LaunchAgent WorkingDirectory'
  check_structured_value "$plist_path" EnvironmentVariables.CC_LOG_FILE "$expected_log" 'LaunchAgent CC_LOG_FILE'
  check_structured_value "$plist_path" StandardOutPath /dev/null 'LaunchAgent StandardOutPath'
  check_structured_value "$plist_path" StandardErrorPath /dev/null 'LaunchAgent StandardErrorPath'
else
  fail_check "LaunchAgent 缺失或是软链接：$plist_path"
fi

if [ -f "$meta_path" ] && [ ! -L "$meta_path" ]; then
  pass "daemon 元数据存在：$meta_path"
  check_structured_value "$meta_path" binary_path "$runtime_path" 'daemon.json binary_path'
  check_structured_value "$meta_path" work_dir "$root/data" 'daemon.json work_dir'
  check_structured_value "$meta_path" log_file "$expected_log" 'daemon.json log_file'
else
  fail_check "daemon.json 缺失或是软链接：$meta_path"
fi

loaded_count=0
launch_pid=
uid=$(id -u)
for domain in gui user; do
  if launch_output=$(cc_launchctl print "$domain/$uid/com.cc-connect.service" 2>/dev/null); then
    loaded_count=$((loaded_count + 1))
    if ! printf '%s\n' "$launch_output" | grep -E 'state = running' >/dev/null; then
      fail_check "launchd $domain 服务未处于 running"
    fi
    current_pid=$(printf '%s\n' "$launch_output" | awk '/^[[:space:]]*pid = [0-9]+[[:space:]]*$/ {print $3; exit}')
    if [ -z "$current_pid" ]; then
      fail_check "launchd $domain 服务缺少有效 PID"
    elif [ -n "$launch_pid" ] && [ "$launch_pid" != "$current_pid" ]; then
      fail_check '不同 launchd 域报告了不同 PID'
    else
      launch_pid=$current_pid
    fi
  fi
done
[ "$loaded_count" -eq 1 ] && pass '只有一个 launchd 服务实例' || fail_check "launchd 已加载实例数为 ${loaded_count}，预期 1"

process_pids=$(cc_pgrep_runtime "$runtime_path" 2>/dev/null | awk '/^[0-9]+$/ && !seen[$0]++ {print}' || true)
process_count=$(printf '%s\n' "$process_pids" | awk 'NF {count++} END {print count+0}')
if [ "$process_count" -eq 1 ]; then
  process_pid=$(printf '%s\n' "$process_pids" | awk 'NF {print; exit}')
  if [ -n "$launch_pid" ] && [ -n "$status_pid" ] && [ "$process_pid" = "$launch_pid" ] && [ "$status_pid" = "$launch_pid" ]; then
    pass "唯一服务进程 PID = $process_pid"
  else
    fail_check 'daemon status、launchd 与进程 PID 不一致'
  fi
else
  fail_check "统一 runtime 的进程数为 ${process_count}，预期 1"
fi

if [ -f "$plist_path" ]; then
  service_program=$(plist_value "$plist_path" ProgramArguments.0 || true)
  if [ -x "$service_program" ]; then
    service_hash=$(cc_sha256 "$service_program" 2>/dev/null || true)
    runtime_hash=$(cc_sha256 "$runtime_path" 2>/dev/null || true)
    [ -n "$service_hash" ] && [ "$service_hash" = "$runtime_hash" ] && pass '服务程序与统一 runtime 校验值一致' || fail_check '服务程序与统一 runtime 校验值不一致'
  fi
fi

[ "$failures" -eq 0 ] || exit 1
printf '✅ 统一目录检查通过\n'
