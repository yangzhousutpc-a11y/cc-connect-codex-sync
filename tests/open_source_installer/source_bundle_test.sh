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
  source/docs/wecom.md
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
grep -F 'version=v1.0.2' "$first/VERSION" >/dev/null || fail 'wrong release version'
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
  grep -Eiv '不需要|无需|does not require|do not require|not required' |
    grep -Ei \
      '(^|[[:space:]：:，,；;。])(需要([[:space:]]*配置)?|必须配置|必须提供)[[:space:]]*(公网 URL|CorpID|AgentID|Token|EncodingAESKey|cloudflared)|(^|[[:space:][:punct:]])requires?([[:space:]]+configuration)?([[:space:]]+of)?[[:space:]]+(a[[:space:]]+)?(public URL|CorpID|AgentID|Token|EncodingAESKey|cloudflared)'
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
done

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
