# cc-connect Codex Sync

[English](README.md) | 简体中文

[![CI](https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/actions/workflows/ci.yml/badge.svg)](https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/yangzhousutpc-a11y/cc-connect-codex-sync)](https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/releases)
[![License](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

一份只连接 **Codex、飞书和个人微信** 的独立源码项目，让三个入口共享稳定的会话路由，并保持消息双向可见。

## 核心能力

- **飞书 ↔ Codex App**：飞书群消息进入对应 Codex 会话；Codex App 中的用户消息与 Codex 回复同步回原群。
- **微信 ↔ Codex App**：个人微信消息进入独立 Codex 会话；Codex App 中的后续消息与回复同步回原微信会话。
- **严格一对一**：一个飞书群或微信会话只绑定一个 Codex 会话，避免重复会话和跨群串线。
- **飞书 `/new [名称]`**：新建一个飞书群和一个 Codex 会话；原群、原会话、历史与上下文保持不变。
- **来源与命名**：会话名使用 `[飞书-Codex]`、`[微信-Codex]`；回传消息使用 `✣ Codex App · 你` 或 `✣ Codex · 回复`。
- **可靠恢复**：保留恢复后的首条消息；发送失败自动重试；断线时不跳过消息；重启后恢复多工作区绑定。
- **兼容性诊断**：`/doctor` 显示 Codex CLI 与 Codex App 双向同步状态。

## 项目范围

```text
agent/codex
platform/feishu
platform/weixin
core + config + daemon + cmd
packaging/macos
```

本仓库不包含其他 Agent、其他聊天平台、Web 管理后台或上游发行渠道。配置通过 `config.toml` 和命令行完成。

## 同步模型

| 入口 | Codex App 会话 | 回传目标 |
| --- | --- | --- |
| 飞书群 | `[飞书-Codex] 群名` | 原飞书群 |
| 个人微信 | `[微信-Codex] 会话名` | 原微信会话 |
| 飞书 `/new [名称]` | 新建 `[飞书-Codex] 名称` | 新建的一个飞书群 |

原则：**平台会话是稳定路由键，新建操作不能改写已有绑定。**

## macOS 安装

要求：macOS 12 或更高版本、网络连接，以及已经安装并登录的 Codex CLI。用户不需要预装 Go；缺少兼容 Go 时，安装器会下载并校验临时工具链。

从 [Releases](https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/releases) 下载最新压缩包和对应 `.sha256` 文件：

```bash
shasum -a 256 -c cc-connect-codex-sync-*-macos-source.tar.gz.sha256
tar -xzf cc-connect-codex-sync-*-macos-source.tar.gz
cd cc-connect-source-install
./bootstrap.sh

install -m 600 ~/cc-connect/data/config.example.toml ~/cc-connect/data/config.toml
# 编辑 ~/cc-connect/data/config.toml，将项目名和绝对工作目录改为真实值
~/cc-connect/runtime/cc-connect feishu setup --project my-project
~/cc-connect/runtime/cc-connect weixin setup --project my-project

./bootstrap.sh --activate
./doctor.sh
```

完整权限与故障恢复说明见 [macOS 本地源码安装指南](packaging/macos/README.zh-CN.md)。平台配置见 [飞书指南](docs/feishu.md)和[微信指南](docs/weixin.md)。

## 源码验证

```bash
go mod verify
go build ./...
go test ./...
go test -race ./...
make test-open-source-installer
```

安全问题请使用 [Private Vulnerability Reporting](https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/security/advisories/new)。不要公开配置、Token、微信登录状态、会话内容、完整日志或个人路径。

## 来源与许可证

本项目基于 [cc-connect](https://github.com/chenhg5/cc-connect) 改造，按照 [MIT License](LICENSE) 发布。
