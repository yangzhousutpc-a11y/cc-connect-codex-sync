# cc-connect Codex Sync

[English](README.md) | 简体中文

[![CI](https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/actions/workflows/ci.yml/badge.svg)](https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/yangzhousutpc-a11y/cc-connect-codex-sync)](https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/releases)
[![License](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

这是一个基于 [CC Connect](https://github.com/chenhg5/cc-connect) 改造的 **Codex 专用双向同步项目**。它不只让飞书或微信远程触发 Agent，而是把 **Codex App、飞书和个人微信** 连接成同一套会话系统的三个入口：平台消息能进入正确的 Codex 会话，Codex App 中的用户消息和回复也能回到原来的平台会话。

## 这个项目解决什么问题

原版 CC Connect 已经提供了“消息平台调用 Agent、结果返回消息平台”的基础能力。本项目在这个基础上，重点解决桌面端与消息平台之间的双向可见、不同平台的会话模型、并发路由正确性，以及故障和重启后的连续性。

- **真正双向可见：** 飞书、个人微信和 Codex App 任一入口产生的会话内容，都能同步到对应入口。
- **按平台设计路由：** 飞书有独立群聊容器，采用“一群一 Codex 会话”；个人微信通常只有一个长期聊天入口，采用“单入口、多逻辑 Codex 会话”。
- **把正确性放在首位：** 防止双群、重复会话、跨群串线、恢复时丢首条消息，以及断线或发送失败造成的消息跳过。

## 在原版 CC Connect 基础上做了什么

| 改造方向 | 本项目的实现 |
| --- | --- |
| Codex App 双向同步 | 平台消息进入 Codex App；Codex App 中的用户消息和 Codex 回复同步回原飞书群或微信会话。 |
| 飞书一群一会话 | 每个飞书群稳定绑定一个 Codex 会话；并发群消息互不串线。 |
| 飞书 `/new` | 在 A 群执行 `/new 名称`，只创建一个 B 群和一个新 Codex 会话；A 群仍保留原会话、历史和上下文。 |
| 新会话即时可见 | 新群创建成功后立即让对应会话出现在 Codex App，无需先发送业务消息。 |
| 微信多逻辑会话 | 在同一个个人微信入口中，通过 `/new`、`/back`、`/list`、`/switch` 和 `/current` 管理多个 Codex 会话。 |
| 来源感知 | 新会话使用 `[飞书-Codex]` 或 `[微信-Codex]` 命名，回传消息明确标出来自 Codex App 的用户输入或 Codex 回复。 |
| 可恢复创建 | `/new` 分阶段持久化；中断或重启后复用已创建资源，避免双群、双会话和首条消息丢失。 |
| 可靠消息中继 | 未确认送达的消息保留并重试；平台断线恢复后继续处理；多工作区绑定在重启后恢复。 |
| Codex 兼容哨兵 | 监测 Codex CLI/app-server 事件兼容性；`/doctor` 展示同步状态，无法安全识别来源时停止错误转发。 |
| macOS 本地安装 | 源码、唯一运行程序、配置数据、安装材料和备份统一到 `~/cc-connect`；候选程序验证后再切换，失败可回滚。 |

## 两种会话模型

### 飞书：一群一会话

- A 群始终绑定原 Codex 会话，历史和上下文保持不变。
- 在 A 群执行 `/new 名称`，系统创建一个新的 B 群和一个新的 Codex 会话。
- A 群后续消息继续进入旧会话，B 群消息只进入新会话。
- 新飞书群和对应 Codex 会话使用 `[飞书-Codex] 名称`。

### 个人微信：单入口、多逻辑会话

个人微信不创建新的外部聊天，而是在同一个微信入口中切换多个 Codex 会话：

| 命令 | 行为 |
| --- | --- |
| `/new [主题]` | 创建新的 Codex 会话并切换 |
| `/back` | 返回上一个逻辑会话 |
| `/list` | 查看可以切换的会话 |
| `/switch <序号>` | 切换到指定 Codex 会话 |
| `/current` | 查看当前活跃会话 |

新微信会话使用 `[微信-Codex] 主题名称`，每条文字回复都会带当前会话简称，降低误用上下文的风险。

### 同步消息标识

```text
✣ Codex App · 你
用户在 Codex App 输入的内容

✣ Codex · 回复
Codex 返回的内容
```

这些标识只说明消息来源，不进入群名，也不参与路由。

## 开源范围

```text
agent/codex
platform/feishu
platform/weixin
core + config + daemon + cmd
packaging/macos
```

本仓库只保留 Codex、飞书、个人微信及其必要基础设施，不包含其他 Agent、其他聊天平台、Web 管理后台或 npm 分发层。

## 如何选择

- **普通用户：** 从 [Releases](https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/releases) 下载 `cc-connect-codex-sync-*-macos-source.tar.gz`，解压后按照下方安装步骤操作。对应的 `.sha256` 文件仅用于校验下载是否完整，不是第二个安装包。
- **开发者：** 克隆本仓库，在源码目录中构建、测试或参与开发。Release 安装包属于发布产物，不提交到源码仓库。

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
