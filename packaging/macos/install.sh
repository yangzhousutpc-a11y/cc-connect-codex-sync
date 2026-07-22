#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
. "$script_dir/lib.sh"
cc_require_safe_home

binary_path=
activate=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --binary) [ "$#" -ge 2 ] || cc_die '--binary 缺少路径'; binary_path=$2; shift 2 ;;
    --activate) activate=1; shift ;;
    *) cc_die "未知参数：$1" ;;
  esac
done
[ -n "$binary_path" ] || binary_path="$script_dir/runtime/cc-connect"

root=$(cc_layout_root)
runtime_path="$root/runtime/cc-connect"
data_link=$(cc_data_link)
cli_link=$(cc_cli_link)
plist_path=$(cc_plist_path)
meta_path=$(cc_daemon_meta_path "$root")
config_path="$root/data/config.toml"
installer_config="$root/installer/config.example.toml"
data_example="$root/data/config.example.toml"
installer_readme="$root/installer/README.md"
installer_install="$root/installer/install.sh"
installer_doctor="$root/installer/doctor.sh"
installer_uninstall="$root/installer/uninstall.sh"
installer_lib="$root/installer/lib.sh"

# 全部可预见失败必须在首次写入之前发生。
cc_require_safe_layout_paths "$root"
cc_require_regular_or_absent "$runtime_path" '稳定 runtime'
if [ -e "$runtime_path" ]; then cc_require_executable_regular "$runtime_path" '稳定 runtime'; fi
cc_require_absent "$runtime_path.next" 'runtime 候选暂存路径'
cc_require_owned_or_absent_link "$data_link" "$root/data"
cc_require_owned_or_absent_link "$cli_link" "$runtime_path"
cc_require_executable_regular "$binary_path" '候选运行程序'
"$binary_path" --version >/dev/null 2>&1 || cc_die "候选运行程序未通过 --version 验证：$binary_path"
[ -f "$script_dir/config.example.toml" ] || cc_die "缺少配置模板：$script_dir/config.example.toml"
readme_path=$(cc_find_readme "$script_dir") || cc_die "缺少安装说明：$script_dir/README.md 或 README.zh-CN.md"
for source_script in "$script_dir/install.sh" "$script_dir/doctor.sh" "$script_dir/uninstall.sh" "$script_dir/lib.sh"; do
  [ -f "$source_script" ] && [ ! -L "$source_script" ] && [ -x "$source_script" ] || cc_die "安装脚本源必须是非软链接普通可执行文件：$source_script"
done
for material_target in "$installer_config" "$data_example" "$installer_readme" "$installer_install" "$installer_doctor" "$installer_uninstall" "$installer_lib"; do
  cc_require_regular_or_absent "$material_target" '安装材料目标'
  cc_require_absent "$material_target.next" '安装材料候选暂存路径'
done

old_service_loaded=0
old_runtime_running=0
if cc_launchd_target_loaded gui || cc_launchd_target_loaded user; then old_service_loaded=1; fi
if [ -f "$runtime_path" ] && cc_pgrep_runtime "$runtime_path" >/dev/null 2>&1; then old_runtime_running=1; fi
if [ "$activate" -eq 0 ] && { [ "$old_service_loaded" -eq 1 ] || [ "$old_runtime_running" -eq 1 ]; }; then
  cc_die '检测到正在运行的服务；请使用 --activate 完成同一事务，拒绝 staged update 造成 CLI/daemon 版本分叉'
fi
if [ "$activate" -eq 1 ]; then
  [ ! -L "$config_path" ] || cc_die "活动配置不能是软链接：$config_path"
  [ -f "$config_path" ] || cc_die "请先创建 $config_path"
  cc_require_physical_existing_prefix "$(dirname -- "$plist_path")" 'LaunchAgent 目录'
  cc_require_regular_or_absent "$plist_path" LaunchAgent
  cc_require_regular_or_absent "$meta_path" daemon.json
fi

had_runtime=0
had_data_link=0
had_cli_link=0
had_plist=0
had_meta=0
had_installer_config=0
had_data_example=0
had_installer_readme=0
had_installer_install=0
had_installer_doctor=0
had_installer_uninstall=0
had_installer_lib=0
[ -f "$runtime_path" ] && had_runtime=1
[ -L "$data_link" ] && had_data_link=1
[ -L "$cli_link" ] && had_cli_link=1
[ -f "$plist_path" ] && had_plist=1
[ -f "$meta_path" ] && had_meta=1
[ -f "$installer_config" ] && had_installer_config=1
[ -f "$data_example" ] && had_data_example=1
[ -f "$installer_readme" ] && had_installer_readme=1
[ -f "$installer_install" ] && had_installer_install=1
[ -f "$installer_doctor" ] && had_installer_doctor=1
[ -f "$installer_uninstall" ] && had_installer_uninstall=1
[ -f "$installer_lib" ] && had_installer_lib=1

mkdir -p "$root/source" "$root/runtime" "$root/data/logs" "$root/installer" "$root/backups"
cc_restrict_private_dir "$root"
cc_restrict_private_dir "$root/data"
cc_restrict_private_dir "$root/backups"

needs_backup=0
for existing_flag in "$had_runtime" "$had_plist" "$had_meta" "$had_installer_config" "$had_data_example" "$had_installer_readme" "$had_installer_install" "$had_installer_doctor" "$had_installer_uninstall" "$had_installer_lib"; do
  [ "$existing_flag" -eq 0 ] || needs_backup=1
done
backup_dir=
if [ "$needs_backup" -eq 1 ]; then
  backup_dir="$root/backups/install-$(date '+%Y%m%d%H%M%S')-$$"
  mkdir -p "$backup_dir/materials/installer" "$backup_dir/materials/data"
  [ "$had_runtime" -eq 0 ] || cp -p "$runtime_path" "$backup_dir/runtime-cc-connect"
  [ "$had_plist" -eq 0 ] || cp -p "$plist_path" "$backup_dir/com.cc-connect.service.plist"
  [ "$had_meta" -eq 0 ] || cp -p "$meta_path" "$backup_dir/daemon.json"
  [ "$had_installer_config" -eq 0 ] || cp -p "$installer_config" "$backup_dir/materials/installer/config.example.toml"
  [ "$had_data_example" -eq 0 ] || cp -p "$data_example" "$backup_dir/materials/data/config.example.toml"
  [ "$had_installer_readme" -eq 0 ] || cp -p "$installer_readme" "$backup_dir/materials/installer/README.md"
  [ "$had_installer_install" -eq 0 ] || cp -p "$installer_install" "$backup_dir/materials/installer/install.sh"
  [ "$had_installer_doctor" -eq 0 ] || cp -p "$installer_doctor" "$backup_dir/materials/installer/doctor.sh"
  [ "$had_installer_uninstall" -eq 0 ] || cp -p "$installer_uninstall" "$backup_dir/materials/installer/uninstall.sh"
  [ "$had_installer_lib" -eq 0 ] || cp -p "$installer_lib" "$backup_dir/materials/installer/lib.sh"
fi

transaction_active=1
daemon_install_started=0

restore_material() {
  existed=$1
  backup_path=$2
  target_path=$3
  if [ "$existed" -eq 1 ]; then cp -p "$backup_path" "$target_path"; else rm -f "$target_path"; fi
}

rollback_transaction() {
  set +e
  rm -f "$runtime_path.next" "$installer_config.next" "$data_example.next" "$installer_readme.next" \
    "$installer_install.next" "$installer_doctor.next" "$installer_uninstall.next" "$installer_lib.next"
  if [ "$daemon_install_started" -eq 1 ]; then cc_bootout_launchagent; fi
  if [ "$had_runtime" -eq 1 ]; then cp -p "$backup_dir/runtime-cc-connect" "$runtime_path"; else rm -f "$runtime_path"; fi
  restore_material "$had_installer_config" "$backup_dir/materials/installer/config.example.toml" "$installer_config"
  restore_material "$had_data_example" "$backup_dir/materials/data/config.example.toml" "$data_example"
  restore_material "$had_installer_readme" "$backup_dir/materials/installer/README.md" "$installer_readme"
  restore_material "$had_installer_install" "$backup_dir/materials/installer/install.sh" "$installer_install"
  restore_material "$had_installer_doctor" "$backup_dir/materials/installer/doctor.sh" "$installer_doctor"
  restore_material "$had_installer_uninstall" "$backup_dir/materials/installer/uninstall.sh" "$installer_uninstall"
  restore_material "$had_installer_lib" "$backup_dir/materials/installer/lib.sh" "$installer_lib"
  [ "$had_data_link" -eq 1 ] || cc_remove_owned_link "$data_link" "$root/data"
  [ "$had_cli_link" -eq 1 ] || cc_remove_owned_link "$cli_link" "$runtime_path"
  if [ "$daemon_install_started" -eq 1 ]; then
    if [ "$had_plist" -eq 1 ]; then cp -p "$backup_dir/com.cc-connect.service.plist" "$plist_path"; else rm -f "$plist_path"; fi
    if [ "$had_meta" -eq 1 ]; then cp -p "$backup_dir/daemon.json" "$meta_path"; else rm -f "$meta_path"; fi
    if [ "$had_plist" -eq 1 ] && [ "$old_service_loaded" -eq 1 ]; then
      cc_restore_launchagent "$plist_path" || cc_note '警告：旧 LaunchAgent 文件已恢复，但自动重新加载失败，请手动检查服务。'
    fi
  fi
  set -e
}

transaction_exit() {
  transaction_status=$1
  trap - 0 HUP INT TERM
  if [ "$transaction_active" -eq 1 ]; then rollback_transaction; fi
  exit "$transaction_status"
}
trap 'transaction_exit $?' 0
trap 'transaction_exit 129' HUP
trap 'transaction_exit 130' INT
trap 'transaction_exit 143' TERM

# 所有候选先独立落盘；source=destination 也始终通过 .next 替换。
install -m 700 "$binary_path" "$runtime_path.next"
"$runtime_path.next" --version >/dev/null 2>&1 || cc_die '已复制的候选运行程序未通过 --version 验证'
install -m 644 "$script_dir/config.example.toml" "$installer_config.next"
install -m 644 "$script_dir/config.example.toml" "$data_example.next"
install -m 644 "$readme_path" "$installer_readme.next"
install -m 700 "$script_dir/install.sh" "$installer_install.next"
install -m 700 "$script_dir/doctor.sh" "$installer_doctor.next"
install -m 700 "$script_dir/uninstall.sh" "$installer_uninstall.next"
install -m 700 "$script_dir/lib.sh" "$installer_lib.next"

# 安装材料和入口先完成，runtime 最后提升；之后的失败统一走事务回滚。
mv "$installer_config.next" "$installer_config"
mv "$data_example.next" "$data_example"
mv "$installer_readme.next" "$installer_readme"
mv "$installer_install.next" "$installer_install"
mv "$installer_doctor.next" "$installer_doctor"
mv "$installer_uninstall.next" "$installer_uninstall"
mv "$installer_lib.next" "$installer_lib"
cc_replace_owned_link "$data_link" "$root/data"
cc_replace_owned_link "$cli_link" "$runtime_path"
mv "$runtime_path.next" "$runtime_path"

if [ "$activate" -eq 1 ]; then
  cc_restrict_config "$config_path"
  daemon_install_started=1
  "$runtime_path" daemon install --force --work-dir "$root/data" --log-file "$root/data/logs/cc-connect.log" || cc_die 'daemon 激活失败；正在回滚完整安装事务'
  daemon_status=$("$runtime_path" daemon status 2>&1 || true)
  printf '%s\n' "$daemon_status" | grep -E '^[[:space:]]*Status:[[:space:]]+Running[[:space:]]*$' >/dev/null || cc_die 'daemon 激活后未处于 Running；正在回滚完整安装事务'
fi

transaction_active=0
trap - 0 HUP INT TERM
if [ "$activate" -eq 1 ]; then
  cc_prune_install_backups "$root/backups" || cc_note '警告：已验证新版本，但旧安装备份裁剪失败，请手动检查。'
else
  cc_note "目录已准备。使用 install -m 600 创建 $root/data/config.toml 后重新运行 install.sh --activate。"
fi
