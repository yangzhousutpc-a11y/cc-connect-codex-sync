#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

[ "$(find "$repo_root/agent" -mindepth 1 -maxdepth 1 -type d | wc -l | tr -d ' ')" = 1 ] || fail 'agent directory must contain only codex'
[ -d "$repo_root/agent/codex" ] || fail 'agent/codex missing'

[ "$(find "$repo_root/platform" -mindepth 1 -maxdepth 1 -type d | wc -l | tr -d ' ')" = 2 ] || fail 'platform directory must contain only feishu and weixin'
[ -d "$repo_root/platform/feishu" ] || fail 'platform/feishu missing'
[ -d "$repo_root/platform/weixin" ] || fail 'platform/weixin missing'

expected_plugins='plugin_agent_codex.go
plugin_platform_feishu.go
plugin_platform_weixin.go'
actual_plugins=$(find "$repo_root/cmd/cc-connect" -maxdepth 1 -name 'plugin_*.go' -exec basename {} \; | LC_ALL=C sort)
[ "$actual_plugins" = "$expected_plugins" ] || fail "unexpected plugin surface:\n$actual_plugins"

for forbidden in web npm assets changelogs
do
  [ ! -e "$repo_root/$forbidden" ] || fail "unexpected top-level path: $forbidden"
done

grep -F 'module github.com/yangzhousutpc-a11y/cc-connect-codex-sync' "$repo_root/go.mod" >/dev/null || fail 'wrong module path'
if git -C "$repo_root" grep -n 'github.com/chenhg5/cc-connect' -- '*.go' go.mod; then
  fail 'old module path remains in Go source'
fi

for readme in README.md README.zh-CN.md
do
  grep -F 'agent/codex' "$repo_root/$readme" >/dev/null || fail "$readme does not describe Codex scope"
  grep -F 'platform/feishu' "$repo_root/$readme" >/dev/null || fail "$readme does not describe Feishu scope"
  grep -F 'platform/weixin' "$repo_root/$readme" >/dev/null || fail "$readme does not describe Weixin scope"
done

printf 'PASS: minimal Codex Feishu Weixin scope\n'
