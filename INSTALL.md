# 安装说明

## Agent 引导式安装（推荐）

要求：macOS 12 或更高版本，并且 Codex CLI 已安装和登录。复制这一条命令启动交互式安装：

```bash
CC_CONNECT_AGENT_PROMPT="$(curl -fsSL https://raw.githubusercontent.com/yangzhousutpc-a11y/cc-connect-codex-sync/main/AGENT_INSTALL.md)" && [ -n "$CC_CONNECT_AGENT_PROMPT" ] && codex -C "$HOME" -s workspace-write -a on-request "$CC_CONNECT_AGENT_PROMPT"
```

Agent 自动执行下载、校验、构建、安装、激活和诊断；飞书凭据、微信扫码及 macOS 权限由用户本人确认。完整边界见 [Agent 安装任务](AGENT_INSTALL.md)。

## 手动安装

完整安装、配置与迁移步骤见：

- [中文说明](README.zh-CN.md)
- [macOS 源码安装包说明](packaging/macos/README.zh-CN.md)

本仓库只支持 Codex，并可同时配置飞书和个人微信两个消息入口。
