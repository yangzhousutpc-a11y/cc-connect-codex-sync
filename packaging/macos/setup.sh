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
  */*) setup_path=$0 ;;
  *) die '请通过明确路径运行 setup.sh' ;;
esac
[ ! -L "$setup_path" ] || die 'setup.sh 不能是软链接'
script_parent=$(dirname -- "$setup_path")
script_dir_logical=$(CDPATH= cd -- "$script_parent" 2>/dev/null && pwd -L) || die '无法访问安装包目录'
script_dir=$(CDPATH= cd -- "$script_parent" 2>/dev/null && pwd -P) || die '无法解析安装包目录'
[ "$script_dir_logical" = "$script_dir" ] || die '安装包目录路径不能经过软链接'

root=${CC_CONNECT_ROOT:-$HOME/cc-connect}
case "$root" in
  /*) ;;
  *) die 'CC_CONNECT_ROOT 必须是绝对路径' ;;
esac
[ "$root" != / ] && [ "$root" != "$HOME" ] || die '拒绝使用过宽的安装根目录'

bootstrap_cmd=${CC_TEST_BOOTSTRAP_SH:-$script_dir/bootstrap.sh}
install_cmd=${CC_TEST_INSTALL_SH:-$script_dir/install.sh}
doctor_cmd=${CC_TEST_DOCTOR_SH:-$script_dir/doctor.sh}
runtime=${CC_TEST_RUNTIME:-$root/runtime/cc-connect}
config_path=$root/data/config.toml
staging_config=$root/data/config.toml.setup

for helper in "$bootstrap_cmd" "$install_cmd" "$doctor_cmd"; do
  [ -f "$helper" ] && [ ! -L "$helper" ] && [ -x "$helper" ] || die "安装辅助脚本不可执行：$helper"
done

confirm() {
  prompt=$1
  printf '%s [y/N] ' "$prompt"
  IFS= read -r answer || return 1
  case "$answer" in
    y|Y|yes|YES|Yes) return 0 ;;
    *) return 1 ;;
  esac
}

if [ -L "$config_path" ]; then
  die "正式配置不能是软链接：$config_path"
fi
if [ -f "$config_path" ]; then
  note '检测到已有 cc-connect 配置。升级会保留配置、会话、登录状态和日志。'
  if ! confirm '继续安全升级吗？'; then
    note '已取消，未修改任何文件。'
    exit 0
  fi
  "$bootstrap_cmd" --activate
  "$doctor_cmd"
  note '升级完成，现有配置和平台登录状态已保留。'
  exit 0
fi
if [ -e "$config_path" ]; then
  die "正式配置路径已存在且不是普通文件：$config_path"
fi
if [ -e "$staging_config" ] || [ -L "$staging_config" ]; then
  die "发现上次遗留的临时配置，请确认后删除：$staging_config"
fi
if [ "${CC_TEST_ALLOW_NON_TTY:-0}" != 1 ] && [ ! -t 0 ]; then
  die '首次安装需要在交互式终端中运行 ./setup.sh'
fi

codex_cmd=${CC_TEST_CODEX:-}
if [ -z "$codex_cmd" ]; then
  codex_cmd=$(command -v codex 2>/dev/null || true)
fi
[ -n "$codex_cmd" ] && [ -x "$codex_cmd" ] || die '未找到可执行的 Codex CLI，请先安装并登录 Codex'
"$codex_cmd" --version >/dev/null 2>&1 || die 'Codex CLI 无法正常运行'
"$codex_cmd" login status >/dev/null 2>&1 || die 'Codex CLI 尚未登录，请先运行 codex login'

note '请选择需要连接的平台：'
note '  1) 飞书'
note '  2) 个人微信'
note '  3) 飞书和个人微信'
printf '请输入 1、2 或 3：'
IFS= read -r platform_choice || die '未读取到平台选择'
case "$platform_choice" in
  1) setup_feishu=1; setup_weixin=0 ;;
  2) setup_feishu=0; setup_weixin=1 ;;
  3) setup_feishu=1; setup_weixin=1 ;;
  *) die '平台选择无效，只能输入 1、2 或 3' ;;
esac

printf '请输入项目名称：'
IFS= read -r project || die '未读取到项目名称'
[ -n "$(printf '%s' "$project" | tr -d '[:space:]')" ] || die '项目名称不能为空'

printf '请输入 Codex 工作目录的绝对路径：'
IFS= read -r work_dir || die '未读取到工作目录'
case "$work_dir" in
  /*) ;;
  *) die '工作目录必须是绝对路径' ;;
esac
[ -d "$work_dir" ] || die "工作目录不存在或不是目录：$work_dir"

staging_owned=0
cleanup() {
  status=$?
  trap - EXIT HUP INT TERM
  if [ "$staging_owned" -eq 1 ] && { [ -e "$staging_config" ] || [ -L "$staging_config" ]; }; then
    rm -f "$staging_config" || status=1
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

note '正在校验源码并安装程序…'
"$bootstrap_cmd"
[ -f "$runtime" ] && [ ! -L "$runtime" ] && [ -x "$runtime" ] || die "安装后的运行程序不可用：$runtime"
mkdir -p "$root/data"
"$runtime" config init --config "$staging_config" --project "$project" --work-dir "$work_dir"
staging_owned=1

if [ "$setup_feishu" -eq 1 ]; then
  note '请按照终端提示完成飞书授权。'
  "$runtime" feishu setup --config "$staging_config" --project "$project"
fi
if [ "$setup_weixin" -eq 1 ]; then
  note '请按照终端提示完成微信扫码登录。'
  "$runtime" weixin setup --config "$staging_config" --project "$project"
fi

ln "$staging_config" "$config_path" || die '正式配置已被其他进程创建，拒绝覆盖'
rm -f "$staging_config"
staging_owned=0

"$install_cmd" --binary "$runtime" --activate
"$doctor_cmd"

note '安装完成。请分别从已启用的平台和 Codex App 发送一条消息，确认双向同步。'
