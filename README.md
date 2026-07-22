# cc-connect Codex Sync

English | [简体中文](README.zh-CN.md)

[![CI](https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/actions/workflows/ci.yml/badge.svg)](https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/yangzhousutpc-a11y/cc-connect-codex-sync)](https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/releases)
[![License](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A standalone source project that connects only **Codex, Feishu, and personal Weixin**, with stable conversation routing and bidirectional message visibility across all three entry points.

## Core capabilities

- **Feishu ↔ Codex App:** Feishu group messages enter the matching Codex conversation; Codex App user messages and Codex replies are relayed to the original group.
- **Weixin ↔ Codex App:** personal Weixin messages enter an independent Codex conversation; later App messages and replies return to the original Weixin chat.
- **Strict one-to-one routing:** one Feishu group or Weixin chat binds to one Codex conversation, preventing duplicate conversations and cross-chat leakage.
- **Feishu `/new [name]`:** creates one new Feishu group and one new Codex conversation without changing the original group, conversation, history, or context.
- **Visible provenance:** conversation names use `[飞书-Codex]` and `[微信-Codex]`; relayed messages use `✣ Codex App · 你` or `✣ Codex · 回复`.
- **Reliable recovery:** preserves the first recovered message, retries failed deliveries, does not skip messages while disconnected, and restores multi-workspace bindings after restart.
- **Compatibility diagnostics:** `/doctor` reports Codex CLI and Codex App bidirectional-sync status.

## Repository scope

```text
agent/codex
platform/feishu
platform/weixin
core + config + daemon + cmd
packaging/macos
```

This repository does not ship other agents, other chat platforms, a web admin UI, or upstream distribution channels. Configuration is handled through `config.toml` and the CLI.

## Synchronization model

| Entry point | Codex App conversation | Relay destination |
| --- | --- | --- |
| Feishu group | `[飞书-Codex] Group name` | Original Feishu group |
| Personal Weixin | `[微信-Codex] Conversation name` | Original Weixin chat |
| Feishu `/new [name]` | New `[飞书-Codex] Name` | One newly created Feishu group |

Principle: **the platform conversation is a stable routing key; creating a new conversation must not rewrite an existing binding.**

## macOS installation

Requirements: macOS 12 or later, network access, and an installed and authenticated Codex CLI. Go does not need to be preinstalled; the installer downloads and verifies a temporary toolchain when necessary.

Download the latest archive and matching `.sha256` file from [Releases](https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/releases):

```bash
shasum -a 256 -c cc-connect-codex-sync-*-macos-source.tar.gz.sha256
tar -xzf cc-connect-codex-sync-*-macos-source.tar.gz
cd cc-connect-source-install
./bootstrap.sh

install -m 600 ~/cc-connect/data/config.example.toml ~/cc-connect/data/config.toml
# Edit ~/cc-connect/data/config.toml with the real project name and absolute work directory.
~/cc-connect/runtime/cc-connect feishu setup --project my-project
~/cc-connect/runtime/cc-connect weixin setup --project my-project

./bootstrap.sh --activate
./doctor.sh
```

See the [macOS local source installation guide](packaging/macos/README.zh-CN.md) for permissions and recovery. See the [Feishu guide](docs/feishu.md) and [Weixin guide](docs/weixin.md) for platform setup.

## Source verification

```bash
go mod verify
go build ./...
go test ./...
go test -race ./...
make test-open-source-installer
```

Report security issues through [Private Vulnerability Reporting](https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/security/advisories/new). Never publish configuration, tokens, Weixin login state, conversation content, complete logs, or personal filesystem paths.

## Origin and license

This project is built on [cc-connect](https://github.com/chenhg5/cc-connect) and released under the [MIT License](LICENSE).
