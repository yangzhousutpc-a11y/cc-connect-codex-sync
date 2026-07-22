#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/cc-connect-bundle-test.XXXXXX")
test_root=$(CDPATH= cd -- "$test_root" && pwd -P)
trap 'rm -rf "$test_root"' EXIT HUP INT TERM

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
binary="$test_root/cc-connect"
dist="$test_root/dist"
cat >"$binary" <<'EOF'
#!/bin/sh
printf 'cc-connect test\n'
EOF
chmod 755 "$binary"

make -C "$repo_root" local-install-bundle BINARY="$binary" DIST="$dist" >/dev/null
bundle="$dist/cc-connect-clean-install"
actual="$test_root/actual-manifest"
expected="$test_root/expected-manifest"
find "$bundle" -mindepth 1 -print | sed "s#^$bundle/##" | LC_ALL=C sort >"$actual"
cat >"$expected" <<'EOF'
README.md
config.example.toml
doctor.sh
install.sh
lib.sh
runtime
runtime/cc-connect
uninstall.sh
EOF
cmp -s "$expected" "$actual" || {
  diff -u "$expected" "$actual" >&2 || true
  fail 'clean bundle manifest differs from the exact allowlist'
}

for forbidden in config.toml token tokens sessions logs weixin daemon.json backups staging; do
  if find "$bundle" -iname "$forbidden" -print -quit | grep . >/dev/null; then
    fail "clean bundle contains sensitive object: $forbidden"
  fi
done

printf 'PASS: clean bundle manifest\n'
