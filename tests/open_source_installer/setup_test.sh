#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd -P)
setup_source=$repo_root/packaging/macos/setup.sh
test_root=$(mktemp -d "${TMPDIR:-/tmp}/cc-connect-setup-test.XXXXXX")
test_root=$(CDPATH= cd -- "$test_root" && pwd -P)
cleanup() { rm -rf "$test_root"; }
trap cleanup EXIT HUP INT TERM

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
assert_contains() { grep -F -- "$2" "$1" >/dev/null || fail "$1 missing: $2"; }
assert_absent() { [ ! -e "$1" ] && [ ! -L "$1" ] || fail "unexpected path: $1"; }

[ -f "$setup_source" ] || fail "missing setup source: $setup_source"

new_case() {
  name=$1
  case_root=$test_root/$name
  mkdir -p "$case_root/home" "$case_root/bin" "$case_root/work dir" "$case_root/bundle"
  cp "$setup_source" "$case_root/bundle/setup.sh"
  chmod 755 "$case_root/bundle/setup.sh"
  : >"$case_root/calls"

  cat >"$case_root/bin/bootstrap" <<'EOF'
#!/bin/sh
set -eu
printf 'bootstrap' >>"$CC_TEST_CALLS"
for arg in "$@"; do printf ' <%s>' "$arg" >>"$CC_TEST_CALLS"; done
printf '\n' >>"$CC_TEST_CALLS"
EOF
  cat >"$case_root/bin/install" <<'EOF'
#!/bin/sh
set -eu
printf 'install' >>"$CC_TEST_CALLS"
for arg in "$@"; do printf ' <%s>' "$arg" >>"$CC_TEST_CALLS"; done
printf '\n' >>"$CC_TEST_CALLS"
EOF
  cat >"$case_root/bin/doctor" <<'EOF'
#!/bin/sh
set -eu
printf 'doctor\n' >>"$CC_TEST_CALLS"
EOF
  cat >"$case_root/bin/codex" <<'EOF'
#!/bin/sh
set -eu
case "${1:-}" in
  --version) printf 'codex-cli test\n' ;;
  login) [ "${2:-}" = status ] ;;
  *) exit 2 ;;
esac
EOF
  cat >"$case_root/bin/runtime" <<'EOF'
#!/bin/sh
set -eu
printf 'runtime' >>"$CC_TEST_CALLS"
for arg in "$@"; do printf ' <%s>' "$arg" >>"$CC_TEST_CALLS"; done
printf '\n' >>"$CC_TEST_CALLS"
if [ "${1:-}" = config ] && [ "${2:-}" = init ]; then
  shift 2
  target=
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --config) target=$2; shift 2 ;;
      *) shift ;;
    esac
  done
  [ -n "$target" ]
  install -m 600 /dev/null "$target"
fi
case " $* " in
  *" $CC_TEST_FAIL_PLATFORM setup "*) exit 19 ;;
esac
EOF
  chmod 755 "$case_root/bin/"*
}

run_setup() {
  input=$1
  shift
  printf '%b' "$input" | env \
    HOME="$case_root/home" \
    CC_CONNECT_ROOT="$case_root/home/cc-connect" \
    CC_TEST_ALLOW_NON_TTY=1 \
    CC_TEST_BOOTSTRAP_SH="$case_root/bin/bootstrap" \
    CC_TEST_INSTALL_SH="$case_root/bin/install" \
    CC_TEST_DOCTOR_SH="$case_root/bin/doctor" \
    CC_TEST_RUNTIME="$case_root/bin/runtime" \
    CC_TEST_CODEX="$case_root/bin/codex" \
    CC_TEST_CALLS="$case_root/calls" \
    CC_TEST_FAIL_PLATFORM="${CC_TEST_FAIL_PLATFORM:-never}" \
    "$case_root/bundle/setup.sh" "$@"
}

new_case existing_cancel
mkdir -p "$case_root/home/cc-connect/data"
printf 'keep\n' >"$case_root/home/cc-connect/data/config.toml"
run_setup 'n\n' >/dev/null
[ ! -s "$case_root/calls" ] || fail 'cancelled upgrade performed writes'

new_case existing_upgrade
mkdir -p "$case_root/home/cc-connect/data"
printf 'keep\n' >"$case_root/home/cc-connect/data/config.toml"
run_setup 'y\n' >/dev/null
assert_contains "$case_root/calls" 'bootstrap <--activate>'
assert_contains "$case_root/calls" 'doctor'
[ "$(cat "$case_root/home/cc-connect/data/config.toml")" = keep ] || fail 'upgrade changed config'

for platform_case in feishu weixin both; do
  new_case "$platform_case"
  case "$platform_case" in
    feishu) choice=1 ;;
    weixin) choice=2 ;;
    both) choice=3 ;;
  esac
  run_setup "$choice\ndemo project\n$case_root/work dir\n" >/dev/null
  [ "$(grep -c '^bootstrap$' "$case_root/calls")" = 1 ] || fail "$platform_case built more than once"
  assert_contains "$case_root/calls" 'runtime <config> <init>'
  assert_contains "$case_root/calls" 'install <--binary>'
  assert_contains "$case_root/calls" 'doctor'
  case "$platform_case" in
    feishu) assert_contains "$case_root/calls" 'runtime <feishu> <setup>' ;;
    weixin) assert_contains "$case_root/calls" 'runtime <weixin> <setup>' ;;
    both)
      assert_contains "$case_root/calls" 'runtime <feishu> <setup>'
      assert_contains "$case_root/calls" 'runtime <weixin> <setup>'
      ;;
  esac
  [ -f "$case_root/home/cc-connect/data/config.toml" ] || fail "$platform_case missing final config"
  assert_absent "$case_root/home/cc-connect/data/config.toml.setup"
done

new_case platform_failure
CC_TEST_FAIL_PLATFORM=weixin
if run_setup "2\ndemo\n$case_root/work dir\n" >/dev/null 2>&1; then
  fail 'platform failure unexpectedly succeeded'
fi
unset CC_TEST_FAIL_PLATFORM
assert_absent "$case_root/home/cc-connect/data/config.toml"
assert_absent "$case_root/home/cc-connect/data/config.toml.setup"

new_case repeated
run_setup "1\ndemo\n$case_root/work dir\n" >/dev/null
: >"$case_root/calls"
run_setup 'y\n' >/dev/null
assert_contains "$case_root/calls" 'bootstrap <--activate>'
[ "$(grep -c '^doctor$' "$case_root/calls")" = 1 ] || fail 'repeat did not use upgrade path'

printf 'PASS: guided setup\n'
