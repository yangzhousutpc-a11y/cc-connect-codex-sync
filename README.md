# cc-connect Codex Sync

English | [简体中文](README.zh-CN.md)

[![CI](https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/actions/workflows/ci.yml/badge.svg)](https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/yangzhousutpc-a11y/cc-connect-codex-sync)](https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/releases)
[![License](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

This is a **Codex-focused bidirectional sync project** built on [CC Connect](https://github.com/chenhg5/cc-connect). It goes beyond remotely triggering an agent from Feishu or Weixin: it turns **Codex App, Feishu, and personal Weixin** into three entry points to one conversation system. Platform messages reach the correct Codex conversation, while user messages and replies created in Codex App return to the originating platform chat.

## What this project solves

CC Connect already provides the foundation for invoking an agent from a messaging platform and returning its output. This project extends that foundation with desktop-to-platform synchronization, platform-specific conversation models, concurrency-safe routing, and continuity across failures and restarts.

- **True bidirectional visibility:** conversation content created from Feishu, personal Weixin, or Codex App is relayed to the matching entry point.
- **Platform-aware routing:** Feishu has independent group containers, so it uses one Codex conversation per group. Personal Weixin usually has one long-lived chat entry, so it uses multiple logical Codex conversations inside that entry.
- **Correctness before convenience:** the implementation prevents duplicate groups, duplicate conversations, cross-group routing, lost first messages during recovery, and skipped messages after delivery or connectivity failures.

## What changed from CC Connect

| Area | This project's implementation |
| --- | --- |
| Codex App bidirectional sync | Platform messages appear in Codex App; Codex App user messages and Codex replies return to the originating Feishu group or Weixin chat. |
| One Feishu group, one conversation | Every Feishu group keeps a stable Codex binding, including under concurrent group traffic. |
| Feishu `/new` | Running `/new name` in group A creates exactly one group B and one new Codex conversation; group A keeps its original conversation, history, and context. |
| Immediate conversation visibility | A newly created group is materialized in Codex App immediately, without waiting for the first business message. |
| Multiple logical Weixin conversations | `/new`, `/back`, `/list`, `/switch`, and `/current` manage multiple Codex conversations inside one personal Weixin entry. |
| Source-aware naming | New conversations use `[飞书-Codex]` or `[微信-Codex]`; relayed messages distinguish Codex App user input from Codex replies. |
| Recoverable creation | `/new` persists progress in stages and reuses created resources after interruption or restart, preventing duplicate groups, duplicate conversations, and lost first messages. |
| Reliable relay | Unacknowledged messages remain retryable, disconnected platforms resume processing, and multi-workspace bindings recover after restart. |
| Codex compatibility sentinel | Codex CLI/app-server event compatibility is monitored; `/doctor` reports sync health, and unsafe source identification fails closed. |
| Local macOS installation | Source, the single runtime, data, installer materials, and backups live under `~/cc-connect`; verified candidates are activated transactionally with rollback on failure. |

## Two conversation models

### Feishu: one group, one conversation

- Group A always remains bound to its original Codex conversation, history, and context.
- Running `/new name` in group A creates a new group B and one new Codex conversation.
- Later messages from A continue in the old conversation; messages from B enter only the new conversation.
- New Feishu groups and matching Codex conversations use `[飞书-Codex] Name`.

### Personal Weixin: one entry, multiple logical conversations

Personal Weixin does not create new external chats. It switches among logical Codex conversations inside the same Weixin entry:

| Command | Behavior |
| --- | --- |
| `/new [topic]` | Create and switch to a new Codex conversation |
| `/back` | Return to the previous logical conversation |
| `/list` | List available conversations |
| `/switch <number>` | Switch to a selected Codex conversation |
| `/current` | Show the active conversation |

New Weixin conversations use `[微信-Codex] Topic`. Every text reply includes the current conversation's short name to reduce accidental context mistakes.

### Relay markers

```text
✣ Codex App · 你
Content entered by the user in Codex App

✣ Codex · 回复
Content returned by Codex
```

These markers identify message provenance only. They are not part of group names or routing keys.

## Open-source scope

```text
agent/codex
platform/feishu
platform/weixin
core + config + daemon + cmd
packaging/macos
```

The repository contains only Codex, Feishu, personal Weixin, and their required infrastructure. It does not ship other agents, other messaging platforms, a web admin UI, or an npm distribution layer.

## Choose the right installation

- **Regular users:** use the one-command installer below. It downloads the latest [Release](https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/releases), verifies its SHA-256 checksum, and starts `./setup.sh`.
- **Developers:** clone this repository to build, test, or contribute from source. Release archives are distribution artifacts and are not committed to the source repository.

## One-command macOS installation (recommended)

Requirements: macOS 12 or later, network access, and an installed and authenticated Codex CLI. Go does not need to be preinstalled; the installer downloads and verifies a temporary toolchain when necessary.

Copy this single command:

```bash
sh -c "$(curl -fsSL https://raw.githubusercontent.com/yangzhousutpc-a11y/cc-connect-codex-sync/main/install-macos.sh)"
```

The downloader uses the latest stable GitHub Release, verifies the published SHA-256 file, extracts into a private temporary directory, and starts the existing guided setup. It does not use `sudo`. Feishu credentials, Weixin QR login, and macOS permission decisions remain interactive.

### Manual verified download (alternative)

If you prefer to inspect every step, download the latest archive and matching `.sha256` file from [Releases](https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/releases), then run:

```bash
shasum -a 256 -c cc-connect-codex-sync-*-macos-source.tar.gz.sha256
tar -xzf cc-connect-codex-sync-*-macos-source.tar.gz
cd cc-connect-source-install
./setup.sh
```

On a fresh installation, the wizard asks which platform to enable, the project name, and the Codex work directory, then pauses for Feishu authorization or Weixin QR login. When a configuration already exists, it asks for confirmation and performs a safe upgrade while preserving configuration, sessions, and login state.

## Agent-guided installation (alternative)

For macOS users who already have an installed and authenticated Codex CLI. Copy this single command to start an interactive Codex installation session:

```bash
CC_CONNECT_AGENT_PROMPT="$(curl -fsSL https://raw.githubusercontent.com/yangzhousutpc-a11y/cc-connect-codex-sync/main/AGENT_INSTALL.md)" && [ -n "$CC_CONNECT_AGENT_PROMPT" ] && codex -C "$HOME" -s workspace-write -a on-request "$CC_CONNECT_AGENT_PROMPT"
```

The Agent downloads and verifies the release, then invokes the same `./setup.sh` wizard. It pauses for Feishu credentials, Weixin QR login, and macOS permission decisions. See the complete [Agent installation task](AGENT_INSTALL.md) for behavior and safety boundaries. This method does not bypass Codex approvals or disable sandboxing.

## Advanced manual installation

Use these lower-level steps only for development or troubleshooting:

```bash
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
