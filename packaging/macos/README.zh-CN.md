# cc-connect macOS 本地源码安装包

这个安装包面向希望在本机从公开源码构建 cc-connect 的 macOS 用户。普通用户只需运行 `./setup.sh`；向导会调用现有的校验、构建、安装和诊断组件，不使用安装包外的预编译 cc-connect 程序。

## 安装要求与边界

- macOS 12 或更高版本；支持 Intel（`x86_64`）和 Apple Silicon（`arm64`）。
- 网络连接；安装过程中需要联网。没有兼容 Go 时需要从 `go.dev` 下载锁定版本，首次构建也可能需要下载 Go 模块。
- 已安装并登录 Codex CLI；开始前运行 `codex --version`，并确认 Codex CLI 能正常启动会话。
- 至少预留约 1 GB 临时空间。
- 用户不需要预装 Go。系统没有兼容 Go 时，脚本会下载经过 SHA-256 校验的临时工具链，并在本轮结束时清理它。

向导会安全生成 Codex 最小配置，但不会替你生成平台凭据。首次安装仍需本人完成飞书授权或微信扫码。安装包不包含任何旧电脑的账号密钥、Token、微信登录状态、历史会话、日志或定时任务。

## 一键安装流程

解压发行包后，目录名应为 `cc-connect-source-install`：

```bash
cd cc-connect-source-install
./setup.sh
```

首次安装时选择飞书、个人微信或两者，输入项目名称和 Codex 工作目录，然后完成平台授权。检测到已有 `~/cc-connect/data/config.toml` 时，向导会先征求确认，再保留配置、会话、日志和登录状态进行安全升级。

## 高级手动安装

仅在排障或开发时按顺序执行底层命令：

```bash
./bootstrap.sh
install -m 600 ~/cc-connect/data/config.example.toml ~/cc-connect/data/config.toml
# 编辑 ~/cc-connect/data/config.toml 后，将 my-project 替换为配置中的项目名
~/cc-connect/runtime/cc-connect feishu setup --project my-project
~/cc-connect/runtime/cc-connect weixin setup --project my-project
./bootstrap.sh --activate
./doctor.sh
```

详细平台步骤见安装包内的 `source/docs/feishu.md` 和 `source/docs/weixin.md`。以后可以从当前安装包目录重新运行 `./setup.sh`；安装后也会在 `~/cc-connect/installer` 保留该入口。

## 以后需要卸载时

安装验收时不要执行下面的卸载命令；只有以后确实不再使用 cc-connect 时，才在安装包目录运行：

```bash
./uninstall.sh
```

`./uninstall.sh` 会确认服务已经卸载且进程已经退出，再移除受管命令入口；卸载会保留 `~/cc-connect/data` 中的配置、会话和日志，便于恢复或手工备份。如果要彻底删除这些数据，请先自行确认备份，再单独处理该目录。

## 安全说明

- **校验与本地编译：** `bootstrap.sh` 会验证安装包文件清单和 SHA-256，再从包内 `source` 源码构建与当前 Intel 或 Apple Silicon 架构匹配的程序。不要修改文件后绕过校验；需要定制源码时请使用仓库的常规源码构建方式。
- **临时 Go：** 系统中没有满足版本要求的 Go 时，脚本只把锁定版本下载到本轮临时目录，校验官方归档后使用，并在成功、失败或中断退出时清理。用户已有的 Go 不会被替换；Go 自身使用的标准模块或构建缓存可能继续保留。
- **本地签名：** 编译结果会进行 ad-hoc 本地签名并再次验证。它不是 Apple Developer ID 签名，也不代表 Apple 公证。
- **Gatekeeper：** 不要关闭 Gatekeeper，也不要全局降低 macOS 安全设置。如果 macOS 阻止运行，先确认安装包来自项目官方发布页且校验无误，再到“系统设置 → 隐私与安全性”查看拦截信息，只对确认可信的这一个项目选择“仍要打开”。来源不明时应停止安装并重新下载。
- **目录权限：** 如果 Codex 要访问“文稿（Documents）”或“桌面（Desktop）”中的项目，请在“系统设置 → 隐私与安全性”中为实际运行的 `~/cc-connect/runtime/cc-connect` 授予相应目录权限。仅在确有需要时授予“完全磁盘访问权限”。
- **Issue 脱敏：** 提交 Issue 时可以提供 macOS 版本、Intel/Apple Silicon 架构、失败步骤、`./doctor.sh` 的脱敏结果和错误原文，但不要上传 `config.toml`、Token、`app_secret`、微信登录数据、会话内容或完整日志。请遮盖用户目录、应用 ID、群聊/用户 ID 和其他可识别信息。

## 故障恢复

所有阶段都可以在修复原因后重新运行同一条命令；不要通过关闭安全机制或跳过校验来继续。

- **checksum / 校验失败：** 停止使用当前目录。从项目官方发布页重新下载并完整解压，不要混用两次下载的文件，也不要手改 `checksums.txt`。
- **download / 下载失败：** 检查网络、代理、DNS 和对 `go.dev` 的访问；清理磁盘空间后重试。失败退出会清理本轮下载的临时 Go。
- **build / 构建失败：** 确认 macOS 和架构受支持、可用临时空间约 1 GB，并允许 Go 获取模块；保留完整错误信息，修复网络或空间问题后再次运行 `./setup.sh`。
- **login / Codex 登录失败：** 先在当前 macOS 用户的终端完成 Codex CLI 官方登录流程，并确认能正常启动会话，再重新激活。后台服务不能代替你完成登录。
- **config / 配置失败：** 将 `~/cc-connect/data/config.toml` 与 `~/cc-connect/data/config.example.toml` 对照，修正 TOML 层级和必填项，并保持权限为 `600`。已有配置不要用示例文件覆盖。
- **platform / 平台失败：** 飞书检查应用凭据、事件订阅和机器人权限；微信重新完成扫码授权并确认登录状态有效。每次只排查一个平台，修复后先发送测试消息。
- **doctor / 诊断失败：** 按 `./doctor.sh` 标出的失败项处理，检查 `~/.local/bin` 是否在 `PATH`、launchd 服务是否运行，以及 `~/cc-connect/data/logs/cc-connect.log` 的脱敏错误。修复后重新运行 `./setup.sh`；仍无法恢复时再提交脱敏 Issue。

## 安装后的目录

- 源码快照：`~/cc-connect/source`
- 当前运行程序：`~/cc-connect/runtime/cc-connect`
- 配置、会话和日志：`~/cc-connect/data`
- 可重复使用的安装材料：`~/cc-connect/installer`
- 回滚备份：`~/cc-connect/backups`

安装脚本会收紧过宽的文件权限，但不会放宽已有的更严格权限。
