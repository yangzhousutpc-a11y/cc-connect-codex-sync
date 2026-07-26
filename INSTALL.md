# 安装说明

## 一键安装（推荐）

要求：macOS 12 或更高版本，并且 Codex CLI 已安装和登录。复制这一条命令：

```bash
sh -c "$(curl -fsSL https://raw.githubusercontent.com/yangzhousutpc-a11y/cc-connect-codex-sync/main/install-macos.sh)"
```

脚本会定位最新稳定 Release、下载源码安装包及 `.sha256`、完成校验和解压，再启动安装向导。向导会完成构建、配置、平台授权、服务激活和诊断；已有安装会保留配置、会话及登录状态并执行安全升级。

如需逐步检查，可在 [Releases](https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/releases) 手动下载两个文件后运行：

```bash
shasum -a 256 -c cc-connect-codex-sync-*-macos-source.tar.gz.sha256
tar -xzf cc-connect-codex-sync-*-macos-source.tar.gz
cd cc-connect-source-install
./setup.sh
```

## Agent 引导式安装（备选）

要求：macOS 12 或更高版本，并且 Codex CLI 已安装和登录。复制这一条命令启动交互式安装：

```bash
CC_CONNECT_AGENT_PROMPT="$(curl -fsSL https://raw.githubusercontent.com/yangzhousutpc-a11y/cc-connect-codex-sync/main/AGENT_INSTALL.md)" && [ -n "$CC_CONNECT_AGENT_PROMPT" ] && codex -C "$HOME" -s workspace-write -a on-request "$CC_CONNECT_AGENT_PROMPT"
```

Agent 下载并校验安装包后，会调用同一个 `./setup.sh` 向导；飞书凭据、微信扫码及 macOS 权限由用户本人确认。完整边界见 [Agent 安装任务](AGENT_INSTALL.md)。

## 高级手动安装

完整安装、配置与迁移步骤见：

- [中文说明](README.zh-CN.md)
- [macOS 源码安装包说明](packaging/macos/README.zh-CN.md)

本仓库只支持 Codex，可配置飞书、个人微信和企业微信三个消息入口。

```text
Agent: Codex
Platforms: Feishu, personal Weixin, WeCom
```

企业微信使用智能机器人的 API 长连接模式。请先完成安装，再按[企业微信指南](docs/wecom.md)运行 `cc-connect wecom setup --bot-name "机器人显示名"`；`--bot-name` 用于精确去除机器人的 @ 前缀，BotID 与 Secret 只保存在本机配置中。
