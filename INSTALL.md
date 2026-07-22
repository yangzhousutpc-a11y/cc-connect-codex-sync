# 安装说明

## 一键安装（推荐）

要求：macOS 12 或更高版本，并且 Codex CLI 已安装和登录。下载 Release 的源码安装包及 `.sha256`，校验并解压后只需运行：

```bash
cd cc-connect-source-install
./setup.sh
```

向导会完成构建、配置、平台授权、服务激活和诊断；已有安装会保留配置、会话及登录状态并执行安全升级。

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

本仓库只支持 Codex，并可同时配置飞书和个人微信两个消息入口。
