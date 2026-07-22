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
  README.md LICENSE VERSION checksums.txt bootstrap.sh install.sh doctor.sh uninstall.sh \
  source/go.mod source/README.md source/README.zh-CN.md source/config.example.toml
do
  assert_file "$first/$required"
done

assert_dir "$first/source/agent/codex"
assert_dir "$first/source/platform/feishu"
assert_dir "$first/source/platform/weixin"

for forbidden in source/web source/npm source/assets source/changelogs source/.git source/.superpowers source/docs/superpowers
do
  assert_absent "$first/$forbidden"
done

[ "$(find "$first/source/agent" -mindepth 1 -maxdepth 1 -type d | wc -l | tr -d ' ')" = 1 ] || fail 'bundle contains extra agents'
[ "$(find "$first/source/platform" -mindepth 1 -maxdepth 1 -type d | wc -l | tr -d ' ')" = 2 ] || fail 'bundle contains extra platforms'

grep -F 'module github.com/yangzhousutpc-a11y/cc-connect-codex-sync' "$first/source/go.mod" >/dev/null || fail 'wrong module path'
grep -F 'version=v1.0.0' "$first/VERSION" >/dev/null || fail 'wrong release version'

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

(cd "$first" && find . -type f -print0 | LC_ALL=C sort -z | xargs -0 shasum -a 256) >"$test_root/first.hashes"
(cd "$second" && find . -type f -print0 | LC_ALL=C sort -z | xargs -0 shasum -a 256) >"$test_root/second.hashes"
cmp -s "$test_root/first.hashes" "$test_root/second.hashes" || fail 'source bundle is not reproducible'

printf 'PASS: minimal source bundle\n'
