# One-command macOS Installer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让普通 macOS 用户复制 README 中的一条命令，即可安全下载、校验、解压最新 Release 并进入现有 `setup.sh` 交互向导。

**Architecture:** 根目录新增一个职责单一的 POSIX shell 引导脚本。它通过 GitHub `releases/latest` 重定向发现稳定版本，在物理临时目录下载源码包及校验文件，完成结构和摘要校验后调用安装包内的 `setup.sh`；配置、升级和平台授权继续由现有向导负责。

**Tech Stack:** POSIX shell、curl、tar、shasum、GitHub Releases、现有 macOS 安装器测试框架。

---

### Task 1: 为公开下载引导器建立失败测试

**Files:**
- Create: `tests/open_source_installer/one_command_test.sh`
- Modify: `tests/open_source_installer/run.sh`

- [ ] **Step 1: 写入缺少实现时会失败的测试**

测试创建假的 `curl`、`sw_vers` 和安装包资产，运行仓库根目录的 `install-macos.sh`，断言：

```sh
[ -x "$repo_root/install-macos.sh" ] || fail 'missing one-command installer'
CC_TEST_CALLS="$calls" PATH="$fake_bin:$PATH" "$repo_root/install-macos.sh"
assert_contains "$calls" 'setup'
assert_absent "$temp_parent/cc-connect-install."
```

同一测试脚本还覆盖非法标签、摘要不匹配、异常顶层目录、软链接 `setup.sh`、向导退出码传递和临时目录清理。

- [ ] **Step 2: 接入安装器测试入口**

在 `tests/open_source_installer/run.sh` 中加入：

```sh
sh "$script_dir/one_command_test.sh"
```

- [ ] **Step 3: 运行测试并确认正确失败**

Run: `sh tests/open_source_installer/one_command_test.sh`

Expected: `FAIL: missing one-command installer`

- [ ] **Step 4: 提交失败测试**

```bash
git add tests/open_source_installer/one_command_test.sh tests/open_source_installer/run.sh
git commit -m "test one-command macOS installer"
```

### Task 2: 实现最小安全下载引导器

**Files:**
- Create: `install-macos.sh`
- Test: `tests/open_source_installer/one_command_test.sh`

- [ ] **Step 1: 实现系统和依赖预检**

脚本使用 `set -eu`、`set -f` 和 `umask 077`，拒绝非 Darwin、低于 macOS 12 或缺少 `curl`、`tar`、`shasum`、`grep`、`mktemp` 的环境。

- [ ] **Step 2: 实现 Release 发现和严格标签校验**

使用：

```sh
latest_url=$(curl -fsSL -o /dev/null -w '%{url_effective}' \
  https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/releases/latest)
tag=${latest_url##*/}
printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' ||
  die '最新 Release 标签格式异常'
```

只构造同仓库、同标签下的源码安装包和 `.sha256` URL。

- [ ] **Step 3: 实现临时目录、下载和校验**

使用系统默认 `mktemp -d` 创建物理目录；退出陷阱只删除本轮已解析且名称匹配 `cc-connect-install.*` 的目录。下载两个资产后，确认校验文件只有一个目标且文件名与压缩包完全一致，再运行：

```sh
(cd "$temp_root" && shasum -a 256 -c "$checksum_name")
```

- [ ] **Step 4: 实现压缩包结构校验和向导调用**

解压前确认所有条目都位于 `cc-connect-source-install` 下，拒绝绝对路径和 `..` 路径段。解压后确认 `setup.sh` 是非软链接的普通可执行文件，再执行：

```sh
"$temp_root/cc-connect-source-install/setup.sh"
```

保留向导退出状态并清理临时目录。

- [ ] **Step 5: 运行引导器测试并确认通过**

Run: `sh tests/open_source_installer/one_command_test.sh`

Expected: `PASS: one-command macOS installer`

- [ ] **Step 6: 提交实现**

```bash
git add install-macos.sh
git commit -m "add one-command macOS installer"
```

### Task 3: 更新公开文档和 v1.0.2 版本

**Files:**
- Modify: `README.md`
- Modify: `README.zh-CN.md`
- Modify: `INSTALL.md`
- Modify: `Makefile`
- Modify: `packaging/macos/package-source-installer.sh`
- Modify: `tests/open_source_installer/source_bundle_test.sh`
- Modify: `tests/open_source_installer/one_command_test.sh`

- [ ] **Step 1: 先增加文档一致性失败断言**

在 `one_command_test.sh` 中确认三份文档都只展示同一条公开命令：

```sh
expected='sh -c "$(curl -fsSL https://raw.githubusercontent.com/yangzhousutpc-a11y/cc-connect-codex-sync/main/install-macos.sh)"'
grep -F "$expected" "$repo_root/README.md"
grep -F "$expected" "$repo_root/README.zh-CN.md"
grep -F "$expected" "$repo_root/INSTALL.md"
```

Run: `sh tests/open_source_installer/one_command_test.sh`

Expected: FAIL，因为文档尚未包含新命令。

- [ ] **Step 2: 更新三份安装说明**

把“一键安装”主入口替换为：

```bash
sh -c "$(curl -fsSL https://raw.githubusercontent.com/yangzhousutpc-a11y/cc-connect-codex-sync/main/install-macos.sh)"
```

保留下载 `.tar.gz`、校验和运行 `./setup.sh` 的手动方式，并明确远程脚本会在飞书凭据、微信扫码和 macOS 权限处暂停。

- [ ] **Step 3: 将发布版本升级到 v1.0.2**

把 `Makefile` 默认版本、`package-source-installer.sh` 的 `VERSION` 以及源码包测试断言统一改为 `v1.0.2`。

- [ ] **Step 4: 运行文档与安装器测试**

Run: `sh tests/open_source_installer/one_command_test.sh`

Expected: `PASS: one-command macOS installer`

- [ ] **Step 5: 提交文档和版本**

```bash
git add README.md README.zh-CN.md INSTALL.md Makefile packaging/macos/package-source-installer.sh tests/open_source_installer/source_bundle_test.sh tests/open_source_installer/one_command_test.sh
git commit -m "document one-command v1.0.2 install"
```

### Task 4: 全量验证、发布和公网验收

**Files:**
- Generated release assets outside the repository:
  - `cc-connect-codex-sync-v1.0.2-macos-source.tar.gz`
  - `cc-connect-codex-sync-v1.0.2-macos-source.tar.gz.sha256`

- [ ] **Step 1: 运行全量验证**

```bash
export PATH=/Users/yangzhou/.cache/cc-connect-toolchains/go1.26.5/bin:$PATH
go mod verify
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test ./...
go test -race ./...
sh tests/minimal_scope_test.sh
make test-open-source-installer
```

Expected: 全部退出码为 0，安装器输出三个 `PASS`。

- [ ] **Step 2: 生成并核验发布资产**

从干净提交生成 `cc-connect-source-install`，打包 v1.0.2，生成 SHA-256，确认 `VERSION` 指向当前提交并包含可执行 `setup.sh`。

- [ ] **Step 3: 推送 main 并发布 v1.0.2**

推送本地 `main` 到 `origin/main`，创建正式 Release，只上传源码安装包和对应 `.sha256`。

- [ ] **Step 4: 从 GitHub 公网执行 README 原始单行命令**

在物理隔离临时目录中运行公开命令，通过隔离安装根目录验收到平台选择或人工授权边界；确认下载、校验、解压、Go 工具链准备和源码构建均来自公网 v1.0.2。

- [ ] **Step 5: 核对同步与清理**

确认本地 HEAD、`origin/main`、v1.0.2 Release 目标提交一致，GitHub push/release CI 成功，仓库工作区干净，隔离临时目录已删除，现有 `~/.cc-connect` 仍指向原数据目录。
