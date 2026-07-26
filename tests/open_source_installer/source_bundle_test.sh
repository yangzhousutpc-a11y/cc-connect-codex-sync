#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd -P)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/cc-connect-source-bundle-test.XXXXXX")
cleanup() { rm -rf "$test_root"; }
trap cleanup EXIT HUP INT TERM

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
assert_file() { [ -f "$1" ] || fail "missing file: $1"; }
assert_dir() { [ -d "$1" ] || fail "missing directory: $1"; }
assert_absent() { [ ! -e "$1" ] && [ ! -L "$1" ] || fail "unexpected path: $1"; }

first_dist=$test_root/first
second_dist=$test_root/second
mkdir "$first_dist" "$second_dist"
make -C "$repo_root" open-source-install-bundle DIST="$first_dist" >/dev/null
make -C "$repo_root" open-source-install-bundle DIST="$second_dist" >/dev/null

first=$first_dist/cc-connect-source-install
second=$second_dist/cc-connect-source-install

for required in \
  README.md LICENSE VERSION checksums.txt setup.sh bootstrap.sh install.sh doctor.sh uninstall.sh \
  source/go.mod source/README.md source/README.zh-CN.md source/AGENT_INSTALL.md source/install-macos.sh source/config.example.toml \
  source/docs/feishu.md source/docs/weixin.md source/docs/wecom.md
do
  assert_file "$first/$required"
done
[ -x "$first/setup.sh" ] || fail 'setup.sh is not executable'

assert_dir "$first/source/agent/codex"
assert_dir "$first/source/platform/feishu"
assert_dir "$first/source/platform/weixin"
assert_dir "$first/source/platform/wecom"

for forbidden in source/web source/npm source/assets source/changelogs source/.git source/.superpowers source/docs/superpowers
do
  assert_absent "$first/$forbidden"
done

[ "$(find "$first/source/agent" -mindepth 1 -maxdepth 1 -type d | wc -l | tr -d ' ')" = 1 ] || fail 'bundle contains extra agents'
[ "$(find "$first/source/platform" -mindepth 1 -maxdepth 1 -type d | wc -l | tr -d ' ')" = 3 ] || fail 'bundle contains extra platforms'

grep -F 'module github.com/yangzhousutpc-a11y/cc-connect-codex-sync' "$first/source/go.mod" >/dev/null || fail 'wrong module path'
grep -F 'version=v1.1.0' "$first/VERSION" >/dev/null || fail 'wrong release version'
grep -F './setup.sh' "$first/README.md" >/dev/null || fail 'bundle README does not use guided setup'
grep -F './setup.sh' "$first/source/AGENT_INSTALL.md" >/dev/null || fail 'Agent install does not delegate to guided setup'
grep -F '## 高级手动安装' "$first/source/README.zh-CN.md" >/dev/null || fail 'Chinese README does not separate advanced manual installation'
grep -F '## Advanced manual installation' "$first/source/README.md" >/dev/null || fail 'English README does not separate advanced manual installation'
grep -F '## 三种会话模型' "$first/source/README.zh-CN.md" >/dev/null || fail 'Chinese README does not document three conversation models'
grep -F '## Three conversation models' "$first/source/README.md" >/dev/null || fail 'English README does not document three conversation models'

wecom_doc=$first/source/docs/wecom.md
for required_text in \
  '[企业微信-Codex]' \
  'cc-connect wecom setup' \
  'mode = "websocket"' \
  'bot_id' \
  'bot_secret' \
  '群内需要 @机器人' \
  '/new 不会自动创建企业微信群' \
  '不需要公网 URL、CorpID、AgentID、Token、EncodingAESKey 或 cloudflared'
do
  grep -F "$required_text" "$wecom_doc" >/dev/null ||
    fail "Enterprise WeChat guide is missing: $required_text"
done

wecom_guide_requires_unrelated_field() {
  awk '
    BEGIN {
      chinese[1] = "公网 URL"; english[1] = "public url"
      chinese[2] = "CorpID"; english[2] = "corpid"
      chinese[3] = "AgentID"; english[3] = "agentid"
      chinese[4] = "Token"; english[4] = "token"
      chinese[5] = "EncodingAESKey"; english[5] = "encodingaeskey"
      chinese[6] = "cloudflared"; english[6] = "cloudflared"
    }
    {
      for (i = 1; i <= 6; i++) {
        chinese_text = $0
        while (sub("不需要[[:space:]]*(配置[[:space:]]*)?" chinese[i], "", chinese_text)) {}
        while (sub("无需[[:space:]]*(配置[[:space:]]*)?" chinese[i], "", chinese_text)) {}
        if (chinese_text ~ ("需要[[:space:]]*(配置[[:space:]]*)?" chinese[i]) ||
            chinese_text ~ ("必须[[:space:]]*(配置|提供)[[:space:]]*" chinese[i])) {
          found = 1
        }

        english_text = tolower($0)
        while (sub("does[[:space:]]+not[[:space:]]+require([[:space:]]+configuration)?([[:space:]]+of)?[[:space:]]+" english[i], "", english_text)) {}
        while (sub("do[[:space:]]+not[[:space:]]+require([[:space:]]+configuration)?([[:space:]]+of)?[[:space:]]+" english[i], "", english_text)) {}
        if (english_text ~ ("requires?([[:space:]]+configuration)?([[:space:]]+of)?[[:space:]]+" english[i])) {
          found = 1
        }
      }
    }
    END { exit found ? 0 : 1 }
  '
}

if wecom_guide_requires_unrelated_field <"$wecom_doc" >/dev/null; then
  fail 'Enterprise WeChat guide incorrectly requires unrelated connection fields'
fi

assert_wecom_requirement_rejected() {
  text=$1
  if ! printf '%s\n' "$text" | wecom_guide_requires_unrelated_field >/dev/null; then
    fail "Enterprise WeChat requirement policy accepted: $text"
  fi
}

assert_wecom_requirement_allowed() {
  text=$1
  if printf '%s\n' "$text" | wecom_guide_requires_unrelated_field >/dev/null; then
    fail "Enterprise WeChat requirement policy rejected: $text"
  fi
}

for field_pair in \
  '公网 URL|public URL' \
  'CorpID|CorpID' \
  'AgentID|AgentID' \
  'Token|Token' \
  'EncodingAESKey|EncodingAESKey' \
  'cloudflared|cloudflared'
do
  chinese_field=${field_pair%%|*}
  english_field=${field_pair#*|}
  assert_wecom_requirement_rejected "需要配置 $chinese_field"
  assert_wecom_requirement_rejected "需要 $chinese_field"
  assert_wecom_requirement_rejected "requires configuration of $english_field"
  assert_wecom_requirement_rejected "requires $english_field"
  assert_wecom_requirement_allowed "不需要 $chinese_field"
  assert_wecom_requirement_allowed "无需 $chinese_field"
  assert_wecom_requirement_allowed "does not require $english_field"
  assert_wecom_requirement_allowed "does not require configuration of $english_field"
done

assert_wecom_requirement_rejected 'API长连接模式需要配置 CorpID。'
assert_wecom_requirement_rejected '不需要 Token，但需要 CorpID。'
assert_wecom_requirement_rejected 'does not require Token but requires CorpID'

wecom_example=$first/source/config.example.toml
if grep -E '^[[:space:]]*type[[:space:]]*=[[:space:]]*"wecom"' "$wecom_example" >/dev/null; then
  fail 'Enterprise WeChat example must not be enabled'
fi
for commented_line in \
  '# [[projects.platforms]]' \
  '# type = "wecom"' \
  '# [projects.platforms.options]' \
  '# mode = "websocket"' \
  '# bot_id = "your-wecom-bot-id"' \
  '# bot_secret = "your-wecom-bot-secret"'
do
  grep -F "$commented_line" "$wecom_example" >/dev/null ||
    fail "Enterprise WeChat example line must stay commented: $commented_line"
done

for public_entry in \
  source/README.md \
  source/README.zh-CN.md \
  source/INSTALL.md \
  source/config.example.toml \
  source/packaging/macos/README.zh-CN.md \
  source/cmd/cc-connect/main.go
do
  grep -F 'Agent: Codex' "$first/$public_entry" >/dev/null ||
    fail "$public_entry does not identify Codex as the only agent"
  grep -F 'Platforms: Feishu, personal Weixin, WeCom' "$first/$public_entry" >/dev/null ||
    fail "$public_entry does not list the three supported platforms"
done

grep -F '文字消息和结构化图片会自动双向同步' "$first/source/README.zh-CN.md" >/dev/null ||
  fail 'Chinese README does not bound automatic desktop sync'
grep -F 'Text messages and structured images sync automatically' "$first/source/README.md" >/dev/null ||
  fail 'English README does not bound automatic desktop sync'
grep -F '企业微信入站文件' "$first/source/README.zh-CN.md" >/dev/null ||
  fail 'Chinese README does not document inbound WeCom files'
grep -F 'Inbound WeCom files' "$first/source/README.md" >/dev/null ||
  fail 'English README does not document inbound WeCom files'
grep -F 'Files block' "$first/source/README.md" >/dev/null ||
  fail 'English README does not reject implicit Files block delivery'
grep -F 'send this file to the current chat' "$first/source/README.md" >/dev/null ||
  fail 'English README does not document explicit file delivery'
grep -F 'cc-connect send --file' "$first/source/README.md" >/dev/null ||
  fail 'English README does not document the safe file command'
grep -F 'Never include the local path in the confirmation message' "$first/source/README.md" >/dev/null ||
  fail 'English README does not protect local paths'

for safe_file_doc in \
  source/README.zh-CN.md \
  source/docs/feishu.md \
  source/docs/weixin.md \
  source/docs/wecom.md \
  source/AGENTS.md
do
  grep -F 'Files block' "$first/$safe_file_doc" >/dev/null ||
    fail "$safe_file_doc does not reject implicit Files block delivery"
  grep -F '把这个文件发送到当前群' "$first/$safe_file_doc" >/dev/null ||
    fail "$safe_file_doc does not document explicit file delivery"
  grep -F 'cc-connect send --file' "$first/$safe_file_doc" >/dev/null ||
    fail "$safe_file_doc does not document the safe file command"
  grep -F '不要在确认消息中展示本机路径' "$first/$safe_file_doc" >/dev/null ||
    fail "$safe_file_doc does not protect local paths"
done

agent_prompt=$first/source/core/interfaces.go
grep -F 'Files block' "$agent_prompt" >/dev/null ||
  fail 'Agent help does not reject implicit Files block delivery'
grep -F 'send this file to the current chat' "$agent_prompt" >/dev/null ||
  fail 'Agent help does not require explicit file delivery'
grep -F 'cc-connect send --file' "$agent_prompt" >/dev/null ||
  fail 'Agent help does not document the safe file command'
grep -F 'whether it appears in a Codex App Files block or you generated it' "$agent_prompt" >/dev/null ||
  fail 'Agent help does not require authorization for every regular file'
for unsafe_file_help in \
  'generate a local image or file that should be sent' \
  'generated file that should be sent' \
  'generated attachments that clearly need delivery'
do
  if grep -F "$unsafe_file_help" "$agent_prompt" >/dev/null; then
    fail "Agent help grants discretion to send regular files: $unsafe_file_help"
  fi
done

verify_checksums() {
  bundle=$1
  expected=$test_root/expected
  actual=$test_root/actual
  (cd "$bundle" && find . -type f ! -name checksums.txt -print | LC_ALL=C sort) >"$expected"
  sed 's/^[0-9a-fA-F]*  //' "$bundle/checksums.txt" | LC_ALL=C sort >"$actual"
  cmp -s "$expected" "$actual" || fail 'checksum manifest does not cover every file exactly'
  (cd "$bundle" && shasum -a 256 -c checksums.txt >/dev/null) || fail 'checksum verification failed'
}

verify_checksums "$first"
verify_checksums "$second"
"$repo_root/packaging/macos/scan-public-bundle.sh" "$first"

assert_scan_rejects() {
  name=$1
  content=$2
  fixture=$test_root/privacy-reject-$name
  mkdir "$fixture"
  printf '%s\n' "$content" >"$fixture/config.example.toml"
  if "$repo_root/packaging/macos/scan-public-bundle.sh" "$fixture" >/dev/null 2>&1; then
    fail "privacy scan accepted $name"
  fi
}

assert_scan_accepts() {
  name=$1
  content=$2
  fixture=$test_root/privacy-accept-$name
  mkdir "$fixture"
  printf '%s\n' "$content" >"$fixture/config.example.toml"
  "$repo_root/packaging/macos/scan-public-bundle.sh" "$fixture" >/dev/null ||
    fail "privacy scan rejected placeholder $name"
}

secret_key=bot_secret
bot_id_key=bot_id
real_secret=real-secret-value
real_bot_id=aib0123456789ABCDEF
secret_placeholder=your-wecom-bot-secret
secret_env_placeholder='${WECOM_BOT_SECRET}'
bot_id_placeholder=your-wecom-bot-id
bot_id_env_placeholder='${WECOM_BOT_ID}'

assert_scan_rejects secret-plain-double "$secret_key = \"$real_secret\""
assert_scan_rejects secret-plain-single "$secret_key = '$real_secret'"
assert_scan_rejects secret-inline-double "options = { mode = \"websocket\", $secret_key = \"$real_secret\" }"
assert_scan_rejects secret-inline-single "options = { mode = 'websocket', $secret_key = '$real_secret' }"
assert_scan_rejects bot-id-plain-double "$bot_id_key = \"$real_bot_id\""
assert_scan_rejects bot-id-plain-single "$bot_id_key = '$real_bot_id'"
assert_scan_rejects bot-id-inline-double "options = { $bot_id_key = \"$real_bot_id\", mode = \"websocket\" }"
assert_scan_rejects bot-id-inline-single "options = { $bot_id_key = '$real_bot_id', mode = 'websocket' }"

assert_scan_accepts secret-placeholder-double "$secret_key = \"$secret_placeholder\""
assert_scan_accepts secret-placeholder-single "$secret_key = '$secret_env_placeholder'"
assert_scan_accepts secret-placeholder-inline "options = { $secret_key = \"$secret_placeholder\" }"
assert_scan_accepts bot-id-placeholder-double "$bot_id_key = \"$bot_id_placeholder\""
assert_scan_accepts bot-id-placeholder-single "$bot_id_key = '$bot_id_env_placeholder'"
assert_scan_accepts bot-id-placeholder-inline "options = { $bot_id_key = \"$bot_id_placeholder\" }"

(cd "$first" && find . -type f -print0 | LC_ALL=C sort -z | xargs -0 shasum -a 256) >"$test_root/first.hashes"
(cd "$second" && find . -type f -print0 | LC_ALL=C sort -z | xargs -0 shasum -a 256) >"$test_root/second.hashes"
cmp -s "$test_root/first.hashes" "$test_root/second.hashes" || fail 'source bundle is not reproducible'

printf 'PASS: minimal source bundle\n'
