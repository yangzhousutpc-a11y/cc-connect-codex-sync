#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd -P)
installer=$repo_root/install-macos.sh
test_root=$(mktemp -d "${TMPDIR:-/tmp}/cc-connect-one-command-test.XXXXXX")
test_root=$(CDPATH= cd -- "$test_root" && pwd -P)
cleanup() { rm -rf "$test_root"; }
trap cleanup EXIT HUP INT TERM

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
assert_contains() { grep -F -- "$2" "$1" >/dev/null || fail "$1 missing: $2"; }
assert_absent() { [ ! -e "$1" ] && [ ! -L "$1" ] || fail "unexpected path: $1"; }

[ -x "$installer" ] || fail 'missing one-command installer'

new_case() {
  name=$1
  case_root=$test_root/$name
  fixture=$case_root/fixture
  bundle=$case_root/bundle/cc-connect-source-install
  fake_bin=$case_root/bin
  temp_parent=$case_root/tmp
  calls=$case_root/calls
  mkdir -p "$fixture" "$bundle" "$fake_bin" "$temp_parent"
  : >"$calls"

  cat >"$bundle/setup.sh" <<'EOF'
#!/bin/sh
set -eu
printf 'setup\n' >>"$CC_TEST_CALLS"
exit "${CC_TEST_SETUP_EXIT:-0}"
EOF
  chmod 755 "$bundle/setup.sh"
  printf 'version=v1.0.2\n' >"$bundle/VERSION"
  tar -C "$case_root/bundle" -czf "$fixture/archive.tar.gz" cc-connect-source-install
  (
    cd "$fixture"
    shasum -a 256 archive.tar.gz |
      sed 's/archive\.tar\.gz/cc-connect-codex-sync-v1.0.2-macos-source.tar.gz/' \
      >archive.tar.gz.sha256
  )

  cat >"$fake_bin/uname" <<'EOF'
#!/bin/sh
printf 'Darwin\n'
EOF
  cat >"$fake_bin/sw_vers" <<'EOF'
#!/bin/sh
[ "${1:-}" = -productVersion ]
printf '%s\n' "${CC_TEST_MACOS_VERSION:-14.0}"
EOF
  cat >"$fake_bin/curl" <<'EOF'
#!/bin/sh
set -eu
output=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o|--output) output=$2; shift 2 ;;
    -w|--write-out|--connect-timeout|--max-time|--retry|--proto)
      shift 2
      ;;
    --tlsv1.2|-f|-L|-s|-S|-fsSL) shift ;;
    http://*|https://*) url=$1; shift ;;
    *) shift ;;
  esac
done
printf 'curl <%s>\n' "$url" >>"$CC_TEST_CALLS"
case "$url" in
  */releases/latest)
    printf 'https://github.com/yangzhousutpc-a11y/cc-connect-codex-sync/releases/tag/%s' \
      "${CC_TEST_TAG:-v1.0.2}"
    ;;
  */cc-connect-codex-sync-*-macos-source.tar.gz)
    cp "$CC_TEST_FIXTURE/archive.tar.gz" "$output"
    ;;
  */cc-connect-codex-sync-*-macos-source.tar.gz.sha256)
    cp "$CC_TEST_FIXTURE/archive.tar.gz.sha256" "$output"
    ;;
  *) exit 22 ;;
esac
EOF
  chmod 755 "$fake_bin/"*
}

run_installer() {
  env \
    TMPDIR="$temp_parent/" \
    PATH="$fake_bin:/usr/bin:/bin" \
    CC_TEST_CALLS="$calls" \
    CC_TEST_FIXTURE="$fixture" \
    CC_TEST_TAG="${CC_TEST_TAG:-v1.0.2}" \
    CC_TEST_SETUP_EXIT="${CC_TEST_SETUP_EXIT:-0}" \
    "$installer"
}

assert_cleaned() {
  remaining=$(find "$temp_parent" -mindepth 1 -maxdepth 1 -print -quit)
  [ -z "$remaining" ] || fail "temporary directory was not cleaned: $remaining"
}

new_case success
run_installer >/dev/null
assert_contains "$calls" 'setup'
assert_cleaned

new_case invalid_tag
CC_TEST_TAG=latest
if run_installer >/dev/null 2>&1; then
  fail 'invalid release tag unexpectedly succeeded'
fi
unset CC_TEST_TAG
if grep -F setup "$calls" >/dev/null; then
  fail 'invalid release tag invoked setup'
fi
assert_cleaned

new_case bad_checksum
printf '0%.0s' $(jot 64 1 64) >"$fixture/archive.tar.gz.sha256"
printf '  cc-connect-codex-sync-v1.0.2-macos-source.tar.gz\n' >>"$fixture/archive.tar.gz.sha256"
if run_installer >/dev/null 2>&1; then
  fail 'bad checksum unexpectedly succeeded'
fi
if grep -F setup "$calls" >/dev/null; then
  fail 'bad checksum invoked setup'
fi
assert_cleaned

new_case bad_top_level
rm -rf "$case_root/bundle"
mkdir -p "$case_root/bundle/unexpected"
printf 'bad\n' >"$case_root/bundle/unexpected/file"
tar -C "$case_root/bundle" -czf "$fixture/archive.tar.gz" unexpected
(
  cd "$fixture"
  shasum -a 256 archive.tar.gz |
    sed 's/archive\.tar\.gz/cc-connect-codex-sync-v1.0.2-macos-source.tar.gz/' \
    >archive.tar.gz.sha256
)
if run_installer >/dev/null 2>&1; then
  fail 'bad top-level directory unexpectedly succeeded'
fi
assert_cleaned

new_case symlink_setup
rm "$bundle/setup.sh"
ln -s VERSION "$bundle/setup.sh"
tar -C "$case_root/bundle" -czf "$fixture/archive.tar.gz" cc-connect-source-install
(
  cd "$fixture"
  shasum -a 256 archive.tar.gz |
    sed 's/archive\.tar\.gz/cc-connect-codex-sync-v1.0.2-macos-source.tar.gz/' \
    >archive.tar.gz.sha256
)
if run_installer >/dev/null 2>&1; then
  fail 'symlink setup unexpectedly succeeded'
fi
assert_cleaned

new_case setup_failure
CC_TEST_SETUP_EXIT=23
set +e
run_installer >/dev/null 2>&1
setup_status=$?
set -e
unset CC_TEST_SETUP_EXIT
[ "$setup_status" -eq 23 ] || fail "setup exit status = $setup_status, want 23"
assert_contains "$calls" 'setup'
assert_cleaned

printf 'PASS: one-command macOS installer\n'
