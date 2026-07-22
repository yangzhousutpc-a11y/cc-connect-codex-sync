# Agent 引导式安装设计

## 目标

让已经安装并登录 Codex CLI 的 macOS 用户复制一条命令，进入可交互的 Codex 安装会话。Agent 自动完成下载、校验、解压、构建、安装、服务激活和诊断；遇到平台凭据、微信扫码或 macOS 权限时暂停，由用户本人操作后继续。

## 方案选择

采用“远程安装说明 + 现有安装器”两层结构：

1. 一条命令从本仓库读取 `AGENT_INSTALL.md`，将完整内容作为初始提示启动交互式 Codex CLI。
2. Agent 按说明下载 GitHub 最新 Release 中的源码安装包和 `.sha256`，然后调用包内现有 `bootstrap.sh`、平台 `setup` 和 `doctor.sh`。

不采用以下方案：

- `curl | bash`：虽然更短，但缺少交互解释和人工授权停顿。
- 新写第二套安装器：会重复现有 `bootstrap.sh` 的校验、事务切换和回滚逻辑。
- `codex exec`：非交互模式不适合扫码、凭据和系统权限流程。

## 用户入口

README 提供一条可复制命令。命令使用交互式 `codex`，工作目录设为用户主目录，沙箱采用 `workspace-write`，审批策略采用 `on-request`。不使用跳过审批或关闭沙箱的危险参数。

`AGENT_INSTALL.md` 同时进入源码仓库和后续源码安装包，方便审查与离线查看。

## Agent 执行流程

1. 确认系统为受支持的 macOS，并检查 Codex CLI 已登录。
2. 询问用户准备启用飞书、个人微信或两者，以及项目名称和绝对工作目录。
3. 从本项目 GitHub 最新 Release 下载与 `cc-connect-codex-sync-*-macos-source.tar.gz` 匹配的安装包及校验文件。
4. 校验下载来源、文件名和 SHA-256；失败时停止，不绕过校验。
5. 在独立临时目录解压并阅读包内 README，再运行 `bootstrap.sh`。
6. 保留已有 `~/cc-connect/data`；新安装时创建权限为 `600` 的配置文件。
7. 引导用户在本机完成飞书配置、微信扫码和必要的 macOS 权限确认。Agent 不要求用户在聊天中粘贴密钥。
8. 运行 `bootstrap.sh --activate` 和 `doctor.sh`。
9. 提示用户分别发送平台测试消息；只有诊断通过且所选平台完成收发验证后，才报告安装完成。

## 安全与失败处理

- 不使用 `sudo`，不关闭 Gatekeeper，不自动授予完全磁盘访问权限。
- 不读取、回显、上传或提交现有配置、Token、Cookie、微信登录状态、会话和日志。
- 不覆盖已有正式配置；升级复用现有事务安装和回滚机制。
- 下载、校验、构建、登录或诊断失败时保留用户数据，说明失败步骤和安全重试方式。
- 只安装本仓库的 Codex、飞书和个人微信组合，不引入其他 Agent、平台或 Web 管理端。

## 文档改动

- 新增根目录 `AGENT_INSTALL.md`，作为 Agent 的完整安装任务说明。
- 在 `README.zh-CN.md`、`README.md` 和 `INSTALL.md` 中增加 Agent 引导式安装入口，并保留现有手动安装方式。
- 不修改现有安装脚本的行为。

## 验证标准

- 一键命令的 Shell 语法有效，并启动交互式 Codex，而不是非交互 `codex exec`。
- `AGENT_INSTALL.md` 不包含真实凭据、个人路径或跳过安全审批的参数。
- 源码安装包构建后包含 `source/AGENT_INSTALL.md`。
- 文档链接、Markdown 格式和最小开源范围检查通过。
- 全量 CI 通过。
