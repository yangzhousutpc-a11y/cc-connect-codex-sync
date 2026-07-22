# 一键交互式安装设计

## 目标

把普通用户的 macOS 安装入口简化为：

```bash
./setup.sh
```

脚本自动完成预检、构建、配置、平台授权、服务激活和诊断。用户只需要选择平台、提供项目名称与工作目录，并亲自完成飞书授权、微信扫码和 macOS 权限确认。

## 选定方案

采用独立的用户入口 `setup.sh`，复用现有 `bootstrap.sh`、平台 `setup` 和 `doctor.sh`；同时新增 `cc-connect config init`，由 Go 程序安全生成 Codex 双向同步所需的最小 TOML 配置。

不采用以下方案：

- 不把交互逻辑塞入 `bootstrap.sh`，避免构建安装器同时承担产品向导职责。
- 不仅依赖 Agent 安装，因为普通终端用户也需要一条命令完成安装。
- 不由 Shell 拼接 TOML，避免项目名和路径转义错误或生成不安全配置。

## 文件职责

- `packaging/macos/setup.sh`：用户交互和流程编排，不解析或改写 TOML。
- `cmd/cc-connect/config_cmd.go`：暴露 `config init` 命令和参数校验。
- `config/config.go`：原子创建最小 Codex 配置，拒绝覆盖已有文件。
- `packaging/macos/bootstrap.sh`、`install.sh`、`doctor.sh`：保留现有构建、事务安装、激活、回滚和诊断职责。
- 安装文档：普通用户只看到 `./setup.sh`；原五段流程保留为高级手动安装。

## `config init` 契约

命令形式：

```bash
cc-connect config init --config <路径> --project <名称> --work-dir <绝对目录>
```

行为：

- 项目名称不能为空。
- 工作目录必须是已存在的绝对目录。
- 目标配置文件不得已存在，也不得是软链接。
- 父目录必须是安全的真实目录；配置以 `0600` 权限原子写入。
- 生成一个项目，Agent 固定为 Codex，并写入：
  - `cmd = "codex"`
  - `backend = "app_server"`
  - `app_server_url = "stdio"`
  - `desktop_live_sync = true`
  - `mode = "suggest"`
  - 用户确认的 `work_dir`
- 初始配置不写平台占位符；飞书和微信平台由各自的 `setup` 命令添加。
- 不提供覆盖参数。已有配置的升级不调用 `config init`。

## 全新安装流程

1. 要求交互式终端，检查 macOS 12+、`arm64`/`x86_64`、Codex CLI 登录、工作空间和网络。
2. 询问启用飞书、个人微信或两者；询问项目名称和绝对工作目录。
3. 运行 `bootstrap.sh`，只安装候选程序，不激活服务。
4. 在 `~/cc-connect/data/` 创建权限为 `0600` 的临时配置 `config.toml.setup`。
5. 使用新 runtime 的 `config init` 初始化临时配置。
6. 在用户选择的工作目录中，对临时配置运行飞书和/或微信 `setup`。凭据仅由现有平台命令处理。
7. 所有平台配置成功后，将临时配置原子提升为正式 `config.toml`。
8. 使用已经验证并安装的 runtime 调用 `install.sh --activate`，只激活、不重复编译；随后运行 `doctor.sh`。
9. 提示用户对已启用平台完成双向收发验收。

若平台授权中断，正式配置仍不存在；脚本只清理本轮创建的 `config.toml.setup`，不影响其他数据。重新运行 `setup.sh` 会从配置步骤重新开始，避免解析或续接不完整的平台状态。

## 已有安装与升级流程

检测到 `~/cc-connect/data/config.toml` 时：

- 明确告知将保留配置、会话、登录状态和日志。
- 用户确认后运行 `bootstrap.sh --activate`，沿用现有事务备份与失败回滚。
- 不重新询问平台凭据，不重新扫码，不调用 `config init`。
- 最后运行 `doctor.sh`。
- 若用户取消，脚本在任何写入之前退出。

## 失败与安全边界

- 不使用 `sudo`，不关闭 Gatekeeper，不扩大文件访问权限。
- 不在命令参数或日志中回显飞书 Secret、微信 Token 等凭据。
- 配置、激活或诊断失败时不删除正式配置和用户数据。
- 新装阶段只有平台配置全部完成后才提升正式配置并激活服务。
- 升级阶段继续使用现有安装器的候选验证、备份、原子切换和回滚。
- `setup.sh` 可重复运行；不允许生成第二套 runtime 或第二个 LaunchAgent。

## 安装包集成

- 源码安装包根目录加入可执行的 `setup.sh`。
- 安装完成后 `~/cc-connect/installer/setup.sh` 也必须存在，便于换电脑、恢复和升级。
- `setup.sh` 纳入安装材料的候选写入、备份、回滚和校验清单。
- Agent 引导式安装改为下载并校验安装包后调用 `./setup.sh`，不再自行编排五段安装流程。

## 文档呈现

README 的普通用户入口只保留两种：

1. 解压安装包后运行 `./setup.sh`。
2. 复制 Agent 引导安装命令。

现有 `bootstrap.sh → config → platform setup → activate → doctor` 过程移动到“高级手动安装”，用于排障和开发，不再作为默认路径。

## 验证标准

- `config init`：正常创建、拒绝覆盖、软链接拒绝、相对路径拒绝、特殊字符安全编码、权限为 `0600`。
- `setup.sh`：全新飞书、全新微信、双平台、已有配置升级、用户取消、平台授权失败、临时配置失败清理、路径含空格、重复运行。
- 安装事务：`setup.sh` 被正确复制、备份、回滚，源码包清单与校验覆盖该文件。
- 原有安装器测试、最小范围检查、Go 全量测试和竞态测试全部通过。
- 实机只进行一次：在不覆盖现有配置的前提下完成升级路径、诊断和服务存活验证。
