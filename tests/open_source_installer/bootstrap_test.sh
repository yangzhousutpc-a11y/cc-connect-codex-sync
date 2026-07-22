#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
bootstrap_source="$repo_root/packaging/macos/bootstrap.sh"
test_root=$(mktemp -d "${TMPDIR:-/tmp}/cc-connect-bootstrap-test.XXXXXX")
test_root=$(CDPATH= cd -- "$test_root" && pwd -P)
trap 'rm -rf "$test_root"' EXIT HUP INT TERM

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
assert_file() { [ -f "$1" ] || fail "missing file: $1"; }
assert_not_exists() { [ ! -e "$1" ] && [ ! -L "$1" ] || fail "unexpected path: $1"; }
assert_empty_file() { [ ! -s "$1" ] || fail "expected empty file: $1"; }
assert_nonempty_file() { [ -s "$1" ] || fail "expected non-empty file: $1"; }
assert_line_count() {
  expected=$1
  candidate=$2
  actual=$(wc -l <"$candidate" | tr -d ' ')
  [ "$actual" -eq "$expected" ] || \
    fail "expected $expected lines in $candidate, got $actual"
}

snapshot_tree() {
  tree_root=$1
  snapshot_file=$2
  if [ ! -e "$tree_root" ] && [ ! -L "$tree_root" ]; then
    printf 'MISSING\n' >"$snapshot_file"
    return
  fi
  : >"$snapshot_file"
  LC_ALL=C find "$tree_root" -print | LC_ALL=C sort | while IFS= read -r entry; do
    if [ "$entry" = "$tree_root" ]; then
      relative=.
    else
      relative=."${entry#"$tree_root"}"
    fi
    if [ -L "$entry" ]; then
      entry_type=symlink
      entry_mode=$(stat -f '%Lp' "$entry")
      payload=$(readlink "$entry")
    elif [ -d "$entry" ]; then
      entry_type=directory
      entry_mode=$(stat -f '%Lp' "$entry")
      payload=-
    elif [ -f "$entry" ]; then
      entry_type=file
      entry_mode=$(stat -f '%Lp' "$entry")
      payload=$(/usr/bin/shasum -a 256 "$entry" | awk '{print $1}')
    else
      entry_type=other
      entry_mode=$(stat -f '%Lp' "$entry")
      payload=-
    fi
    printf '%s\t%s\t%s\t%s\n' \
      "$relative" "$entry_type" "$entry_mode" "$payload" >>"$snapshot_file"
  done
}

assert_snapshot_matches() {
  expected_snapshot=$1
  actual_root=$2
  label=$3
  actual_snapshot="$case_root/$label.actual.snapshot"
  snapshot_tree "$actual_root" "$actual_snapshot"
  cmp -s "$expected_snapshot" "$actual_snapshot" || {
    diff -u "$expected_snapshot" "$actual_snapshot" >&2 || true
    fail "$label tree snapshot changed in $case_name"
  }
}

unset CC_CONNECT_ROOT

write_bare_command_sentinel() {
  output=$1
  cat >"$output" <<'EOF'
#!/bin/sh
set -eu
command_name=${0##*/}
printf '%s\n' "$command_name" >>"$CC_TEST_BARE_COMMAND_LOG"
exit 97
EOF
  chmod 755 "$output"
}

prepare_controlled_path() {
  controlled_path=$1
  mkdir -p "$controlled_path"
  for allowed_command in \
    awk basename cat chmod cmp cp cut date dd dirname env expr file find grep \
    head id ln mkdir mktemp mv od printf pwd readlink rm rmdir sed sh sleep \
    sort stat tail tee test touch tr uniq wc xargs
  do
    resolved_command=$(command -v "$allowed_command" 2>/dev/null || true)
    case "$resolved_command" in
      /*) ln -s "$resolved_command" "$controlled_path/$allowed_command" ;;
    esac
  done
  write_bare_command_sentinel "$controlled_path/bare-command-sentinel"
  for forbidden_command in \
    brew codesign curl fetch git go install make openssl port shasum sha256sum \
    sudo tar uname wget xcrun
  do
    ln -s bare-command-sentinel "$controlled_path/$forbidden_command"
  done
}

write_fake_uname() {
  output=$1
  cat >"$output" <<'EOF'
#!/bin/sh
set -eu
case "${1:-}" in
  -s) printf '%s\n' "${CC_TEST_OS:-Darwin}" ;;
  -m) printf '%s\n' "${CC_TEST_MACHINE:-arm64}" ;;
  *) printf '%s\n' "${CC_TEST_OS:-Darwin}" ;;
esac
EOF
  chmod 755 "$output"
}

write_fake_curl() {
  output=$1
  cat >"$output" <<'EOF'
#!/bin/sh
set -eu
printf 'CALL\n' >>"$CC_TEST_CURL_LOG"
argument_index=0
for argument in "$@"; do
  argument_index=$((argument_index + 1))
  printf 'ARG\t%s\t%s\n' "$argument_index" "$argument" >>"$CC_TEST_CURL_LOG"
done
printf 'END\n' >>"$CC_TEST_CURL_LOG"
download=
url=
fail_enabled=0
location_enabled=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o|--output) download=$2; shift 2 ;;
    --output=*) download=${1#--output=}; shift ;;
    --fail|--fail-with-body) fail_enabled=1; shift ;;
    --location) location_enabled=1; shift ;;
    -?*)
      short_flags=${1#-}
      case "$short_flags" in *f*) fail_enabled=1 ;; esac
      case "$short_flags" in *L*) location_enabled=1 ;; esac
      shift
      ;;
    http://*|https://*) url=$1; shift ;;
    *) shift ;;
  esac
done
[ -n "$download" ] && [ -n "$url" ] || exit 64
[ "$fail_enabled" -eq 1 ] && [ "$location_enabled" -eq 1 ] || exit 69
printf '%s\n' "$download" >>"$CC_TEST_CURL_OUTPUT_LOG"
case "${CC_TEST_CURL_SIGNAL:-}" in
  TERM|INT) kill -"$CC_TEST_CURL_SIGNAL" "$PPID"; exit 128 ;;
esac
[ "${CC_TEST_CURL_FAIL:-0}" != 1 ] || exit 22
mkdir -p "$(dirname -- "$download")"
printf 'fake Go archive for %s\n' "$url" >"$download"
EOF
  chmod 755 "$output"
}

write_fake_shasum() {
  output=$1
  cat >"$output" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$CC_TEST_SHASUM_LOG"
expected_go_digest() {
  case "$1" in
    *go1.26.5.darwin-arm64.tar.gz)
      printf '%s\n' efb87ff28af9a188d0536ef5d42e63dd52ba8263cd7344a993cc48dd11dedb6a
      ;;
    *go1.26.5.darwin-amd64.tar.gz)
      printf '%s\n' 6231d8d3b8f5552ec6cbf6d685bdd5482e1e703214b120e89b3bf0d7bf1ef725
      ;;
    *) return 1 ;;
  esac
}
verify_go_checksum_line() {
  checksum_line=$1
  digest=${checksum_line%% *}
  prefix="$digest  "
  case "$checksum_line" in
    "$prefix"*) archive_member=${checksum_line#"$prefix"} ;;
    *) return 1 ;;
  esac
  [ "${#digest}" -eq 64 ] || return 1
  case "$digest" in *[!0123456789abcdefABCDEF]*) return 1 ;; esac
  expected_digest=$(expected_go_digest "$archive_member") || return 1
  [ "$digest" = "$expected_digest" ] || return 1
  [ -f "$archive_member" ] && [ ! -L "$archive_member" ] || return 1
  [ "${CC_TEST_SHA_FAIL:-0}" != 1 ] || return 1
}
check_file=
previous=
archive=
for argument in "$@"; do
  if [ "$previous" = -c ]; then check_file=$argument; fi
  case "$argument" in *go1.26.5.darwin-*.tar.gz) archive=$argument ;; esac
  previous=$argument
done

if [ -n "$check_file" ] && [ "$check_file" != - ] && [ -f "$check_file" ]; then
  if grep -F 'go1.26.5.darwin-' "$check_file" >/dev/null 2>&1; then
    archive_lines=$(grep -F 'go1.26.5.darwin-' "$check_file")
    [ "$(printf '%s\n' "$archive_lines" | wc -l | tr -d ' ')" -eq 1 ] || exit 1
    verify_go_checksum_line "$archive_lines" || exit 1
    exit 0
  fi
fi
if [ "$check_file" = - ]; then
  input=$(cat)
  case "$input" in
    *go1.26.5.darwin-*) verify_go_checksum_line "$input" || exit 1; exit 0 ;;
  esac
  printf '%s\n' "$input" | /usr/bin/shasum -a 256 -c -
  exit $?
fi
if [ -n "$archive" ]; then
  [ -f "$archive" ] && [ ! -L "$archive" ] || exit 1
  digest=$(expected_go_digest "$archive") || exit 65
  if [ "${CC_TEST_SHA_FAIL:-0}" = 1 ]; then
    digest=0000000000000000000000000000000000000000000000000000000000000000
  fi
  printf '%s  %s\n' "$digest" "$archive"
  exit 0
fi
exec /usr/bin/shasum "$@"
EOF
  chmod 755 "$output"
}

write_fake_go() {
  output=$1
  version=$2
  label=$3
  cat >"$output" <<EOF
#!/bin/sh
set -eu
CC_TEST_FAKE_GO_VERSION=$version
CC_TEST_FAKE_GO_LABEL=$label
export CC_TEST_FAKE_GO_VERSION CC_TEST_FAKE_GO_LABEL
exec "\$CC_TEST_FAKE_GO_DRIVER" "\$@"
EOF
  chmod 755 "$output"
}

write_fake_go_driver() {
  output=$1
  cat >"$output" <<'EOF'
#!/bin/sh
set -eu
case "${1:-}" in
  version)
    printf 'go version %s darwin/%s\n' \
      "$CC_TEST_FAKE_GO_VERSION" "${CC_TEST_GO_ARCH:-arm64}"
    ;;
  build)
    shift
    printf 'CALL\nGO\t%s\nENV\tCGO_ENABLED\t%s\nENV\tGOOS\t%s\nENV\tGOARCH\t%s\n' \
      "$CC_TEST_FAKE_GO_LABEL" "${CGO_ENABLED:-}" "${GOOS:-}" \
      "${GOARCH:-}" >>"$CC_TEST_BUILD_LOG"
    candidate=
    previous=
    argument_index=0
    for argument in "$@"; do
      argument_index=$((argument_index + 1))
      printf 'ARG\t%s\t%s\n' "$argument_index" "$argument" \
        >>"$CC_TEST_BUILD_LOG"
      if [ "$previous" = -o ]; then candidate=$argument; fi
      previous=$argument
    done
    printf 'END\n' >>"$CC_TEST_BUILD_LOG"
    [ -n "$candidate" ] || exit 66
    printf '%s\n' "$candidate" >>"$CC_TEST_BUILD_OUTPUT_LOG"
    case "${CC_TEST_BUILD_SIGNAL:-}" in
      TERM|INT) kill -"$CC_TEST_BUILD_SIGNAL" "$PPID"; exit 128 ;;
    esac
    [ "${CC_TEST_BUILD_FAIL:-0}" != 1 ] || exit 23
    mkdir -p "$(dirname -- "$candidate")"
    cat >"$candidate" <<'BINARY'
#!/bin/sh
set -eu
case "${1:-}" in
  --version)
    candidate_version=${CC_TEST_CANDIDATE_VERSION:-v1.0.0}
    candidate_commit=${CC_TEST_CANDIDATE_COMMIT:-0123456789abcdef}
    candidate_build_time=${CC_TEST_CANDIDATE_BUILD_TIME:-2026-07-22T00:00:00Z}
    if [ -n "${CC_TEST_SIGNED_MARKER:-}" ] && \
       [ -f "$CC_TEST_SIGNED_MARKER" ]; then
      candidate_version=${CC_TEST_POST_SIGN_VERSION:-$candidate_version}
    fi
    printf 'cc-connect %s\ncommit:  %s\nbuilt:   %s\n' \
      "$candidate_version" "$candidate_commit" "$candidate_build_time"
    ;;
  *) exit 0 ;;
esac
BINARY
    chmod 755 "$candidate"
    printf 'build\t%s\n' "$candidate" >>"$CC_TEST_EVENT_LOG"
    ;;
  *) exit 67 ;;
esac
EOF
  chmod 755 "$output"
}

write_fake_tar() {
  output=$1
  cat >"$output" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$CC_TEST_TAR_LOG"
[ "${CC_TEST_TAR_FAIL:-0}" != 1 ] || exit 2
destination=
previous=
for argument in "$@"; do
  if [ "$previous" = -C ]; then destination=$argument; fi
  previous=$argument
done
[ -n "$destination" ] || exit 68
mkdir -p "$destination/go/bin"
cat >"$destination/go/bin/go" <<'GOEOF'
#!/bin/sh
set -eu
CC_TEST_FAKE_GO_VERSION=go1.26.5
CC_TEST_FAKE_GO_LABEL=temporary
export CC_TEST_FAKE_GO_VERSION CC_TEST_FAKE_GO_LABEL
exec "$CC_TEST_FAKE_GO_DRIVER" "$@"
GOEOF
chmod 755 "$destination/go/bin/go"
EOF
  chmod 755 "$output"
}

write_fake_codesign() {
  output=$1
  cat >"$output" <<'EOF'
#!/bin/sh
set -eu
printf 'CALL\n' >>"$CC_TEST_CODESIGN_LOG"
argument_index=0
for argument in "$@"; do
  argument_index=$((argument_index + 1))
  printf 'ARG\t%s\t%s\n' "$argument_index" "$argument" \
    >>"$CC_TEST_CODESIGN_LOG"
done
printf 'END\n' >>"$CC_TEST_CODESIGN_LOG"
candidate=
for argument in "$@"; do candidate=$argument; done
[ -n "$candidate" ] && [ -f "$candidate" ] && [ ! -L "$candidate" ] && \
  [ -x "$candidate" ] || exit 70
"$candidate" --version >/dev/null 2>&1 || exit 71
case "${1:-}" in
  --force) event_name=codesign-sign ;;
  --verify) event_name=codesign-verify ;;
  *) exit 72 ;;
esac
printf '%s\t%s\n' "$event_name" "$candidate" >>"$CC_TEST_EVENT_LOG"
if [ "$event_name" = codesign-sign ] && \
   [ "${CC_TEST_MUTATE_AFTER_SIGN:-0}" = 1 ]; then
  : >"$CC_TEST_SIGNED_MARKER"
fi
exit 0
EOF
  chmod 755 "$output"
}

write_install_recorder() {
  output=$1
  cat >"$output" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$CC_TEST_INSTALL_LOG"
candidate=
previous=
for argument in "$@"; do
  if [ "$previous" = --binary ]; then candidate=$argument; fi
  previous=$argument
done
[ -n "$candidate" ] && [ -f "$candidate" ] && [ ! -L "$candidate" ] && \
  [ -x "$candidate" ] || exit 73
"$candidate" --version >/dev/null 2>&1 || exit 74
printf 'install\t%s\n' "$candidate" >>"$CC_TEST_EVENT_LOG"
exit 0
EOF
  chmod 755 "$output"
}

refresh_checksums() {
  rm -f "$bundle/checksums.txt"
  (
    cd "$bundle"
    find . -type f ! -name checksums.txt -print | LC_ALL=C sort | \
      while IFS= read -r member; do
        /usr/bin/shasum -a 256 "$member"
      done
  ) >"$bundle/checksums.txt"
}

setup_case() {
  case_name=$1
  case_root="$test_root/$case_name"
  fake_home="$case_root/home"
  bundle="$case_root/bundle"
  tools_dir="$case_root/tools"
  path_dir="$case_root/path"
  case_tmp="$case_root/tmp"
  mkdir -p "$fake_home/cc-connect/runtime/assets" \
    "$bundle/source/cmd/cc-connect" "$bundle/source/internal/empty" \
    "$tools_dir" "$case_tmp"

  printf 'stable runtime\n' >"$fake_home/cc-connect/runtime/cc-connect"
  printf 'stable sidecar\n' >"$fake_home/cc-connect/runtime/assets/metadata"
  ln -s ../cc-connect "$fake_home/cc-connect/runtime/assets/current"
  chmod 750 "$fake_home/cc-connect/runtime" "$fake_home/cc-connect/runtime/assets"
  chmod 700 "$fake_home/cc-connect/runtime/cc-connect"
  chmod 640 "$fake_home/cc-connect/runtime/assets/metadata"
  runtime_before_snapshot="$case_root/runtime.before.snapshot"
  snapshot_tree "$fake_home/cc-connect/runtime" "$runtime_before_snapshot"
  if [ -f "$bootstrap_source" ]; then
    cp "$bootstrap_source" "$bundle/bootstrap.sh"
  else
    printf '#!/bin/sh\nexit 1\n' >"$bundle/bootstrap.sh"
  fi
  cp "$repo_root/packaging/macos/go-toolchains.txt" "$bundle/go-toolchains.txt"
  printf '%s\n' \
    'version=v1.0.0' \
    'commit=0123456789abcdef' \
    'go_version=go1.26.5' \
    'source_date_epoch=1700000000' \
    'build_time=2026-07-22T00:00:00Z' >"$bundle/VERSION"
  printf '%s\n' 'module github.com/yangzhousutpc-a11y/cc-connect-codex-sync' '' 'go 1.25' \
    >"$bundle/source/go.mod"
  : >"$bundle/source/go.sum"
  printf 'package main\nfunc main() {}\n' >"$bundle/source/cmd/cc-connect/main.go"
  printf 'verified bundled source\n' >"$bundle/source/release-marker.txt"
  printf 'nested source data\n' >"$bundle/source/internal/data.txt"
  printf '#!/bin/sh\nexit 97\n' >"$bundle/install.sh"
  printf '#!/bin/sh\nexit 96\n' >"$bundle/setup.sh"
  chmod 755 "$bundle/bootstrap.sh" "$bundle/install.sh" "$bundle/setup.sh"
  chmod 755 "$bundle/source" "$bundle/source/cmd" "$bundle/source/internal"
  chmod 750 "$bundle/source/cmd/cc-connect" "$bundle/source/internal/empty"
  chmod 640 "$bundle/source/go.mod" "$bundle/source/internal/data.txt"
  chmod 600 "$bundle/source/go.sum"
  chmod 755 "$bundle/source/cmd/cc-connect/main.go"
  chmod 644 "$bundle/source/release-marker.txt"
  bundled_source_snapshot="$case_root/bundled-source.snapshot"
  snapshot_tree "$bundle/source" "$bundled_source_snapshot"

  write_fake_uname "$tools_dir/uname"
  write_fake_curl "$tools_dir/curl"
  write_fake_shasum "$tools_dir/shasum"
  write_fake_tar "$tools_dir/tar"
  write_fake_codesign "$tools_dir/codesign"
  write_fake_go_driver "$tools_dir/fake-go-driver"
  write_fake_go "$tools_dir/system-go" go1.26.5 system
  write_install_recorder "$tools_dir/install-recorder"
  refresh_checksums

  CC_TEST_CURL_LOG="$case_root/curl.log"
  CC_TEST_CURL_OUTPUT_LOG="$case_root/curl-output.log"
  CC_TEST_SHASUM_LOG="$case_root/shasum.log"
  CC_TEST_TAR_LOG="$case_root/tar.log"
  CC_TEST_CODESIGN_LOG="$case_root/codesign.log"
  CC_TEST_BUILD_LOG="$case_root/build.log"
  CC_TEST_BUILD_OUTPUT_LOG="$case_root/build-output.log"
  CC_TEST_INSTALL_LOG="$case_root/install.log"
  CC_TEST_EVENT_LOG="$case_root/events.log"
  CC_TEST_BARE_COMMAND_LOG="$case_root/bare-command.log"
  CC_TEST_FAKE_GO_DRIVER="$tools_dir/fake-go-driver"
  prepare_controlled_path "$path_dir"
  CC_TEST_CONTROLLED_PATH=$path_dir
  for log_file in \
    "$CC_TEST_CURL_LOG" "$CC_TEST_CURL_OUTPUT_LOG" \
    "$CC_TEST_SHASUM_LOG" "$CC_TEST_TAR_LOG" \
    "$CC_TEST_CODESIGN_LOG" "$CC_TEST_BUILD_LOG" \
    "$CC_TEST_BUILD_OUTPUT_LOG" "$CC_TEST_INSTALL_LOG" \
    "$CC_TEST_EVENT_LOG" "$CC_TEST_BARE_COMMAND_LOG"
  do
    : >"$log_file"
  done
  HOME=$fake_home
  TMPDIR=$case_tmp
  CC_UNAME="$tools_dir/uname"
  CC_CURL="$tools_dir/curl"
  CC_SHASUM="$tools_dir/shasum"
  CC_TAR="$tools_dir/tar"
  CC_CODESIGN="$tools_dir/codesign"
  CC_GO_BIN="$tools_dir/system-go"
  CC_TEST_INSTALL_SH="$tools_dir/install-recorder"
  CC_TEST_OS=Darwin
  CC_TEST_MACHINE=arm64
  CC_TEST_GO_ARCH=arm64
  CC_TEST_CURL_FAIL=0
  CC_TEST_SHA_FAIL=0
  CC_TEST_TAR_FAIL=0
  CC_TEST_BUILD_FAIL=0
  CC_TEST_CURL_SIGNAL=
  CC_TEST_BUILD_SIGNAL=
  CC_TEST_CANDIDATE_VERSION=v1.0.0
  CC_TEST_CANDIDATE_COMMIT=0123456789abcdef
  CC_TEST_CANDIDATE_BUILD_TIME=2026-07-22T00:00:00Z
  CC_TEST_POST_SIGN_VERSION=v1.0.0
  CC_TEST_MUTATE_AFTER_SIGN=0
  CC_TEST_SIGNED_MARKER="$case_root/candidate-signed"
  export HOME TMPDIR CC_UNAME CC_CURL CC_SHASUM CC_TAR CC_CODESIGN \
    CC_GO_BIN CC_TEST_INSTALL_SH CC_TEST_OS CC_TEST_MACHINE CC_TEST_GO_ARCH \
    CC_TEST_CURL_FAIL CC_TEST_SHA_FAIL CC_TEST_TAR_FAIL CC_TEST_BUILD_FAIL \
    CC_TEST_CURL_SIGNAL CC_TEST_BUILD_SIGNAL CC_TEST_CURL_LOG \
    CC_TEST_CURL_OUTPUT_LOG \
    CC_TEST_SHASUM_LOG CC_TEST_TAR_LOG CC_TEST_CODESIGN_LOG \
    CC_TEST_BUILD_LOG CC_TEST_BUILD_OUTPUT_LOG CC_TEST_INSTALL_LOG \
    CC_TEST_EVENT_LOG CC_TEST_BARE_COMMAND_LOG CC_TEST_FAKE_GO_DRIVER \
    CC_TEST_CONTROLLED_PATH CC_TEST_CANDIDATE_VERSION \
    CC_TEST_CANDIDATE_COMMIT CC_TEST_CANDIDATE_BUILD_TIME \
    CC_TEST_POST_SIGN_VERSION CC_TEST_MUTATE_AFTER_SIGN \
    CC_TEST_SIGNED_MARKER
}

run_bootstrap() {
  PATH=$CC_TEST_CONTROLLED_PATH \
    "$bundle/bootstrap.sh" "$@" >"$case_root/stdout" 2>"$case_root/stderr"
}

expect_failure() {
  label=$1
  shift
  if run_bootstrap "$@"; then fail "$label unexpectedly succeeded"; fi
}

assert_runtime_unchanged() {
  assert_snapshot_matches "$runtime_before_snapshot" \
    "$fake_home/cc-connect/runtime" stable-runtime
}

assert_source_matches_bundle() {
  assert_snapshot_matches "$bundled_source_snapshot" \
    "$fake_home/cc-connect/source" unified-source
}

assert_installer_source_matches_bundle() {
  assert_snapshot_matches "$bundled_source_snapshot" \
    "$fake_home/cc-connect/installer/source" installer-source
}

assert_no_build_or_install() {
  assert_empty_file "$CC_TEST_BUILD_LOG"
  assert_empty_file "$CC_TEST_INSTALL_LOG"
  assert_empty_file "$CC_TEST_EVENT_LOG"
}

assert_no_bare_command_bypass() {
  assert_empty_file "$CC_TEST_BARE_COMMAND_LOG"
}

assert_temp_clean() {
  assert_not_exists "$fake_home/cc-connect/source.next-bootstrap"
  assert_not_exists "$fake_home/cc-connect/installer/source.next-bootstrap"
  assert_not_exists "$fake_home/cc-connect/installer/source.previous-bootstrap"
  if find "$case_tmp" -mindepth 1 -print -quit | grep . >/dev/null; then
    find "$case_tmp" -mindepth 1 -maxdepth 3 -print >&2
    fail "temporary paths remain in $case_name"
  fi
}

assert_recorded_temp_removed() {
  if [ -s "$CC_TEST_CURL_OUTPUT_LOG" ]; then
    while IFS= read -r download_path; do
      assert_not_exists "$(dirname -- "$download_path")"
    done <"$CC_TEST_CURL_OUTPUT_LOG"
  fi
  if [ -s "$CC_TEST_BUILD_OUTPUT_LOG" ]; then
    while IFS= read -r candidate; do
      assert_not_exists "$(dirname -- "$candidate")"
    done <"$CC_TEST_BUILD_OUTPUT_LOG"
  fi
  if [ -s "$CC_TEST_TAR_LOG" ]; then
    while IFS= read -r tar_args; do
      destination=
      previous=
      for argument in $tar_args; do
        if [ "$previous" = -C ]; then destination=$argument; fi
        previous=$argument
      done
      [ -z "$destination" ] || assert_not_exists "$destination"
    done <"$CC_TEST_TAR_LOG"
  fi
  assert_temp_clean
}

assert_curl_contract() {
  expected_arch=$1
  assert_line_count 1 "$CC_TEST_CURL_OUTPUT_LOG"
  recorded_download=$(cat "$CC_TEST_CURL_OUTPUT_LOG")
  call_count=0
  end_count=0
  argument_count=0
  expected_index=1
  url_count=0
  output_count=0
  fail_enabled=0
  location_enabled=0
  output_pending=0
  parsed_download=
  tab=$(printf '\t')
  while IFS="$tab" read -r record field value; do
    case "$record" in
      CALL) call_count=$((call_count + 1)) ;;
      END) end_count=$((end_count + 1)) ;;
      ARG)
        [ "$field" -eq "$expected_index" ] || \
          fail "curl argv index is not contiguous in $case_name"
        expected_index=$((expected_index + 1))
        argument_count=$((argument_count + 1))
        if [ "$output_pending" -eq 1 ]; then
          parsed_download=$value
          output_pending=0
          continue
        fi
        case "$value" in
          http://*|https://*)
            url_count=$((url_count + 1))
            [ "$value" = \
                "https://go.dev/dl/go1.26.5.darwin-$expected_arch.tar.gz" ] || \
              fail "curl used a non-official Go URL in $case_name"
            ;;
          -o|--output)
            output_count=$((output_count + 1))
            output_pending=1
            ;;
          --output=*)
            output_count=$((output_count + 1))
            parsed_download=${value#--output=}
            ;;
          --fail|--fail-with-body) fail_enabled=1 ;;
          --location) location_enabled=1 ;;
          --*) ;;
          -?*)
            short_flags=${value#-}
            case "$short_flags" in *f*) fail_enabled=1 ;; esac
            case "$short_flags" in *L*) location_enabled=1 ;; esac
            ;;
        esac
        ;;
      *) fail "unknown curl log record in $case_name: $record" ;;
    esac
  done <"$CC_TEST_CURL_LOG"
  [ "$call_count" -eq 1 ] && [ "$end_count" -eq 1 ] || \
    fail "curl must be invoked exactly once in $case_name"
  [ "$argument_count" -gt 0 ] || fail "curl received no argv in $case_name"
  [ "$url_count" -eq 1 ] || \
    fail "curl must receive exactly one http(s) URL in $case_name"
  [ "$fail_enabled" -eq 1 ] || \
    fail "curl must enable fail-on-HTTP-error semantics in $case_name"
  [ "$location_enabled" -eq 1 ] || \
    fail "curl must follow redirects in $case_name"
  [ "$output_count" -eq 1 ] && [ "$output_pending" -eq 0 ] || \
    fail "curl must receive exactly one complete output option in $case_name"
  [ "$parsed_download" = "$recorded_download" ] || \
    fail "curl output argv does not match the fake output record in $case_name"
  case "$parsed_download" in
    "$case_tmp"/*) ;;
    *) fail "curl output escaped the current test temp directory in $case_name" ;;
  esac
  relative_download=${parsed_download#"$case_tmp"/}
  case "/$relative_download/" in
    */../*|*/./*)
      fail "curl output contains path traversal in $case_name"
      ;;
  esac
}

assert_build_contract() {
  expected_arch=$1
  expected_label=$2
  assert_line_count 1 "$CC_TEST_BUILD_OUTPUT_LOG"
  candidate=$(cat "$CC_TEST_BUILD_OUTPUT_LOG")
  [ "$(basename -- "$candidate")" = cc-connect ] || \
    fail "wrong build output name in $case_name"
  expected_build_log="$case_root/build.expected.log"
  expected_ldflags='-s -w -X main.version=v1.0.0 -X main.commit=0123456789abcdef -X main.buildTime=2026-07-22T00:00:00Z'
  printf '%b\n' \
    CALL \
    "GO\t$expected_label" \
    "ENV\tCGO_ENABLED\t0" \
    "ENV\tGOOS\tdarwin" \
    "ENV\tGOARCH\t$expected_arch" \
    "ARG\t1\t-mod=readonly" \
    "ARG\t2\t-trimpath" \
    "ARG\t3\t-tags" \
    "ARG\t4\tgoolm" \
    "ARG\t5\t-ldflags" \
    "ARG\t6\t$expected_ldflags" \
    "ARG\t7\t-o" \
    "ARG\t8\t$candidate" \
    "ARG\t9\t./cmd/cc-connect" \
    END >"$expected_build_log"
  cmp -s "$expected_build_log" "$CC_TEST_BUILD_LOG" || {
    diff -u "$expected_build_log" "$CC_TEST_BUILD_LOG" >&2 || true
    fail "build argv or environment differs from exact contract in $case_name"
  }
}

assert_codesign_contract() {
  candidate=$1
  expected_codesign_log="$case_root/codesign.expected.log"
  printf '%b\n' \
    CALL \
    "ARG\t1\t--force" \
    "ARG\t2\t--sign" \
    "ARG\t3\t-" \
    "ARG\t4\t--identifier" \
    "ARG\t5\tcom.cc-connect.service" \
    "ARG\t6\t$candidate" \
    END \
    CALL \
    "ARG\t1\t--verify" \
    "ARG\t2\t--strict" \
    "ARG\t3\t$candidate" \
    END >"$expected_codesign_log"
  cmp -s "$expected_codesign_log" "$CC_TEST_CODESIGN_LOG" || {
    diff -u "$expected_codesign_log" "$CC_TEST_CODESIGN_LOG" >&2 || true
    fail "codesign argv differs from exact contract in $case_name"
  }
}

assert_event_contract() {
  candidate=$1
  expected_event_log="$case_root/events.expected.log"
  printf '%b\n' \
    "build\t$candidate" \
    "codesign-sign\t$candidate" \
    "codesign-verify\t$candidate" \
    "install\t$candidate" >"$expected_event_log"
  cmp -s "$expected_event_log" "$CC_TEST_EVENT_LOG" || {
    diff -u "$expected_event_log" "$CC_TEST_EVENT_LOG" >&2 || true
    fail "build/sign/verify/install event order differs in $case_name"
  }
}

assert_success_contract() {
  expected_arch=$1
  expected_label=$2
  expected_activate=$3
  assert_build_contract "$expected_arch" "$expected_label"
  assert_line_count 1 "$CC_TEST_INSTALL_LOG"
  candidate=$(cat "$CC_TEST_BUILD_OUTPUT_LOG")
  assert_codesign_contract "$candidate"
  expected_install="--binary $candidate"
  [ "$expected_activate" -eq 0 ] || expected_install="$expected_install --activate"
  [ "$(cat "$CC_TEST_INSTALL_LOG")" = "$expected_install" ] || \
    fail "wrong install delegation in $case_name"
  assert_event_contract "$candidate"
  assert_no_bare_command_bypass
  assert_runtime_unchanged
  assert_installer_source_matches_bundle
  assert_recorded_temp_removed
}

assert_preflight_rejected() {
  label=$1
  expect_failure "$label"
  assert_no_build_or_install
  assert_no_bare_command_bypass
  assert_runtime_unchanged
  assert_temp_clean
}

case_rejects_linux() {
  setup_case rejects_linux
  CC_TEST_OS=Linux; export CC_TEST_OS
  assert_preflight_rejected Linux
}

case_rejects_unsupported_architecture() {
  setup_case rejects_unsupported_architecture
  CC_TEST_MACHINE=ppc64; export CC_TEST_MACHINE
  assert_preflight_rejected 'unsupported architecture'
}

case_rejects_missing_go_mod() {
  setup_case rejects_missing_go_mod
  rm "$bundle/source/go.mod"
  refresh_checksums
  assert_preflight_rejected 'missing source/go.mod'
}

case_rejects_critical_symlink_inputs() {
  input_index=0
  for relative_input in \
    VERSION \
    checksums.txt \
    go-toolchains.txt \
    setup.sh \
    install.sh \
    source/go.mod \
    source/release-marker.txt
  do
    input_index=$((input_index + 1))
    setup_case "rejects_symlink_input_$input_index"
    target="$bundle/$relative_input"
    external="$case_root/external-input-$input_index"
    cp -p "$target" "$external"
    rm "$target"
    ln -s "$external" "$target"
    assert_preflight_rejected "symlink input: $relative_input"
  done
}

case_rejects_parent_directory_symlink_escape() {
  parent_index=0
  for relative_parent in source source/cmd; do
    parent_index=$((parent_index + 1))
    setup_case "rejects_parent_symlink_$parent_index"
    parent_path="$bundle/$relative_parent"
    external_parent="$case_root/external-parent-$parent_index"
    mv "$parent_path" "$external_parent"
    ln -s "$external_parent" "$parent_path"
    if [ "$relative_parent" = source ]; then
      [ -f "$bundle/source/go.mod" ] && [ ! -L "$bundle/source/go.mod" ] || \
        fail 'parent symlink fixture must leave source/go.mod as a regular leaf'
    fi
    assert_preflight_rejected "parent directory symlink escape: $relative_parent"
  done
}

case_rejects_tampered_checksum_member() {
  setup_case rejects_tampered_checksum_member
  printf 'tampered\n' >"$bundle/source/release-marker.txt"
  assert_preflight_rejected 'tampered checksum member'
}

case_rejects_checksum_path_traversal() {
  setup_case rejects_checksum_path_traversal
  printf 'outside bundle\n' >"$case_root/outside"
  outside_digest=$(/usr/bin/shasum -a 256 "$case_root/outside" | awk '{print $1}')
  printf '%s  ../outside\n' "$outside_digest" >>"$bundle/checksums.txt"
  assert_preflight_rejected 'checksum path traversal'
}

case_rejects_unlisted_bundle_symlink() {
  setup_case rejects_unlisted_bundle_symlink
  ln -s main.go "$bundle/source/cmd/cc-connect/extra.go"
  assert_preflight_rejected 'unlisted bundle symlink'
}

case_reuses_go_1_26_5_and_populates_absent_source() {
  setup_case reuses_go_1_26_5_and_populates_absent_source
  run_bootstrap
  assert_success_contract arm64 system 0
  assert_empty_file "$CC_TEST_CURL_LOG"
  assert_empty_file "$CC_TEST_CURL_OUTPUT_LOG"
  assert_empty_file "$CC_TEST_TAR_LOG"
  assert_source_matches_bundle
}

case_reuses_go_1_25_0_and_preserves_nonempty_source() {
  setup_case reuses_go_1_25_0_and_preserves_nonempty_source
  write_fake_go "$tools_dir/system-go" go1.25.0 system
  CC_TEST_MACHINE=x86_64
  CC_TEST_GO_ARCH=amd64
  export CC_TEST_MACHINE CC_TEST_GO_ARCH
  mkdir -p "$fake_home/cc-connect/source/private/empty"
  printf 'keep me\n' >"$fake_home/cc-connect/source/local.txt"
  printf 'private source\n' >"$fake_home/cc-connect/source/private/data"
  ln -s ../local.txt "$fake_home/cc-connect/source/private/current"
  chmod 710 "$fake_home/cc-connect/source" "$fake_home/cc-connect/source/private"
  chmod 700 "$fake_home/cc-connect/source/private/empty"
  chmod 600 "$fake_home/cc-connect/source/local.txt"
  chmod 640 "$fake_home/cc-connect/source/private/data"
  existing_source_snapshot="$case_root/existing-source.before.snapshot"
  snapshot_tree "$fake_home/cc-connect/source" "$existing_source_snapshot"
  run_bootstrap
  assert_success_contract amd64 system 0
  assert_empty_file "$CC_TEST_CURL_LOG"
  assert_empty_file "$CC_TEST_CURL_OUTPUT_LOG"
  assert_empty_file "$CC_TEST_TAR_LOG"
  assert_snapshot_matches "$existing_source_snapshot" \
    "$fake_home/cc-connect/source" existing-unified-source
  assert_installer_source_matches_bundle
}

case_replaces_installer_snapshot_when_run_from_installer() {
  setup_case replaces_installer_snapshot_when_run_from_installer
  mkdir -p "$fake_home/cc-connect/source"
  printf 'keep unified source\n' >"$fake_home/cc-connect/source/local.txt"
  existing_source_snapshot="$case_root/existing-source.before.snapshot"
  snapshot_tree "$fake_home/cc-connect/source" "$existing_source_snapshot"
  mv "$bundle" "$fake_home/cc-connect/installer"
  bundle="$fake_home/cc-connect/installer"
  run_bootstrap
  assert_success_contract arm64 system 0
  assert_snapshot_matches "$existing_source_snapshot" \
    "$fake_home/cc-connect/source" existing-unified-source
  assert_installer_source_matches_bundle
}

case_installer_snapshot_promotion_failure_restores_old_snapshot() {
  setup_case installer_snapshot_promotion_failure_restores_old_snapshot
  mkdir -p "$fake_home/cc-connect/source" \
    "$fake_home/cc-connect/installer/source/private/empty"
  printf 'keep unified source\n' >"$fake_home/cc-connect/source/local.txt"
  printf 'old release snapshot\n' \
    >"$fake_home/cc-connect/installer/source/old.txt"
  printf 'old private snapshot\n' \
    >"$fake_home/cc-connect/installer/source/private/data"
  chmod 710 "$fake_home/cc-connect/installer/source" \
    "$fake_home/cc-connect/installer/source/private"
  chmod 700 "$fake_home/cc-connect/installer/source/private/empty"
  chmod 600 "$fake_home/cc-connect/installer/source/old.txt"
  chmod 640 "$fake_home/cc-connect/installer/source/private/data"
  existing_source_snapshot="$case_root/existing-source.before.snapshot"
  old_installer_snapshot="$case_root/old-installer-source.before.snapshot"
  snapshot_tree "$fake_home/cc-connect/source" "$existing_source_snapshot"
  snapshot_tree "$fake_home/cc-connect/installer/source" \
    "$old_installer_snapshot"

  rm "$path_dir/mv"
  cat >"$path_dir/mv" <<'EOF'
#!/bin/sh
set -eu
if [ "${1:-}" = "$HOME/cc-connect/installer/source.next-bootstrap" ] && \
   [ "${2:-}" = "$HOME/cc-connect/installer/source" ]; then
  exit 91
fi
exec /bin/mv "$@"
EOF
  chmod 755 "$path_dir/mv"

  expect_failure 'installer snapshot promotion failure'
  assert_build_contract arm64 system
  assert_line_count 1 "$CC_TEST_INSTALL_LOG"
  built_candidate=$(cat "$CC_TEST_BUILD_OUTPUT_LOG")
  assert_codesign_contract "$built_candidate"
  assert_event_contract "$built_candidate"
  assert_snapshot_matches "$existing_source_snapshot" \
    "$fake_home/cc-connect/source" existing-unified-source
  assert_snapshot_matches "$old_installer_snapshot" \
    "$fake_home/cc-connect/installer/source" restored-installer-source
  assert_no_bare_command_bypass
  assert_runtime_unchanged
  assert_recorded_temp_removed
}

case_go_1_24_9_downloads_arm64_and_populates_empty_source() {
  setup_case go_1_24_9_downloads_arm64_and_populates_empty_source
  write_fake_go "$tools_dir/system-go" go1.24.9 system
  mkdir -p "$fake_home/cc-connect/source"
  chmod 700 "$fake_home/cc-connect/source"
  run_bootstrap
  assert_success_contract arm64 temporary 0
  assert_curl_contract arm64
  assert_source_matches_bundle
}

case_missing_go_downloads_amd64_and_activates() {
  setup_case missing_go_downloads_amd64_and_activates
  CC_GO_BIN="$tools_dir/missing-go"
  CC_TEST_MACHINE=x86_64
  CC_TEST_GO_ARCH=amd64
  export CC_GO_BIN CC_TEST_MACHINE CC_TEST_GO_ARCH
  run_bootstrap --activate
  assert_success_contract amd64 temporary 1
  assert_curl_contract amd64
  assert_source_matches_bundle
}

assert_download_failure_contract() {
  label=$1
  expect_failure "$label"
  assert_empty_file "$CC_TEST_INSTALL_LOG"
  assert_empty_file "$CC_TEST_EVENT_LOG"
  assert_no_bare_command_bypass
  assert_runtime_unchanged
  assert_recorded_temp_removed
}

case_curl_failure_cleans_temp() {
  setup_case curl_failure_cleans_temp
  write_fake_go "$tools_dir/system-go" go1.24.9 system
  CC_TEST_CURL_FAIL=1; export CC_TEST_CURL_FAIL
  assert_download_failure_contract 'curl failure'
  assert_curl_contract arm64
  assert_empty_file "$CC_TEST_BUILD_LOG"
}

case_sha_failure_cleans_temp() {
  setup_case sha_failure_cleans_temp
  write_fake_go "$tools_dir/system-go" go1.24.9 system
  CC_TEST_SHA_FAIL=1; export CC_TEST_SHA_FAIL
  assert_download_failure_contract 'SHA failure'
  assert_curl_contract arm64
  grep -F 'go1.26.5.darwin-arm64.tar.gz' "$CC_TEST_SHASUM_LOG" >/dev/null || \
    fail 'SHA failure case did not start archive verification'
  assert_empty_file "$CC_TEST_BUILD_LOG"
}

case_tar_failure_cleans_temp() {
  setup_case tar_failure_cleans_temp
  write_fake_go "$tools_dir/system-go" go1.24.9 system
  CC_TEST_TAR_FAIL=1; export CC_TEST_TAR_FAIL
  assert_download_failure_contract 'tar failure'
  assert_curl_contract arm64
  grep -F 'go1.26.5.darwin-arm64.tar.gz' "$CC_TEST_SHASUM_LOG" >/dev/null || \
    fail 'tar failure case did not complete archive verification'
  assert_nonempty_file "$CC_TEST_TAR_LOG"
  assert_empty_file "$CC_TEST_BUILD_LOG"
}

case_build_failure_cleans_temp() {
  setup_case build_failure_cleans_temp
  CC_TEST_BUILD_FAIL=1; export CC_TEST_BUILD_FAIL
  expect_failure 'build failure'
  assert_build_contract arm64 system
  assert_empty_file "$CC_TEST_CODESIGN_LOG"
  assert_empty_file "$CC_TEST_INSTALL_LOG"
  assert_empty_file "$CC_TEST_EVENT_LOG"
  assert_no_bare_command_bypass
  assert_runtime_unchanged
  assert_recorded_temp_removed
}

assert_candidate_metadata_rejected_before_codesign() {
  label=$1
  expect_failure "$label"
  assert_build_contract arm64 system
  assert_empty_file "$CC_TEST_CODESIGN_LOG"
  assert_empty_file "$CC_TEST_INSTALL_LOG"
  expected_event_log="$case_root/metadata-rejected.expected.log"
  candidate=$(cat "$CC_TEST_BUILD_OUTPUT_LOG")
  printf 'build\t%s\n' "$candidate" >"$expected_event_log"
  cmp -s "$expected_event_log" "$CC_TEST_EVENT_LOG" || \
    fail "$label performed an action after build"
  assert_no_bare_command_bypass
  assert_runtime_unchanged
  assert_recorded_temp_removed
}

case_rejects_candidate_version_mismatch() {
  setup_case rejects_candidate_version_mismatch
  CC_TEST_CANDIDATE_VERSION=v9.9.9; export CC_TEST_CANDIDATE_VERSION
  assert_candidate_metadata_rejected_before_codesign 'candidate version mismatch'
}

case_rejects_candidate_commit_mismatch() {
  setup_case rejects_candidate_commit_mismatch
  CC_TEST_CANDIDATE_COMMIT=ffffffffffffffff; export CC_TEST_CANDIDATE_COMMIT
  assert_candidate_metadata_rejected_before_codesign 'candidate commit mismatch'
}

case_rejects_candidate_build_time_mismatch() {
  setup_case rejects_candidate_build_time_mismatch
  CC_TEST_CANDIDATE_BUILD_TIME=2026-07-22T01:02:03Z
  export CC_TEST_CANDIDATE_BUILD_TIME
  assert_candidate_metadata_rejected_before_codesign 'candidate build time mismatch'
}

case_rejects_version_changed_after_codesign() {
  setup_case rejects_version_changed_after_codesign
  CC_TEST_MUTATE_AFTER_SIGN=1
  CC_TEST_POST_SIGN_VERSION=v9.9.9
  export CC_TEST_MUTATE_AFTER_SIGN CC_TEST_POST_SIGN_VERSION
  expect_failure 'candidate version changed after codesign'
  assert_build_contract arm64 system
  candidate=$(cat "$CC_TEST_BUILD_OUTPUT_LOG")
  assert_codesign_contract "$candidate"
  assert_empty_file "$CC_TEST_INSTALL_LOG"
  expected_event_log="$case_root/post-sign-rejected.expected.log"
  printf '%b\n' \
    "build\t$candidate" \
    "codesign-sign\t$candidate" \
    "codesign-verify\t$candidate" >"$expected_event_log"
  cmp -s "$expected_event_log" "$CC_TEST_EVENT_LOG" || \
    fail 'post-sign version mismatch performed install'
  assert_no_bare_command_bypass
  assert_runtime_unchanged
  assert_recorded_temp_removed
}

case_term_cleans_temp() {
  setup_case term_cleans_temp
  write_fake_go "$tools_dir/system-go" go1.24.9 system
  CC_TEST_CURL_SIGNAL=TERM; export CC_TEST_CURL_SIGNAL
  assert_download_failure_contract TERM
  assert_curl_contract arm64
  assert_empty_file "$CC_TEST_BUILD_LOG"
}

case_int_cleans_temp() {
  setup_case int_cleans_temp
  CC_TEST_BUILD_SIGNAL=INT; export CC_TEST_BUILD_SIGNAL
  expect_failure INT
  assert_build_contract arm64 system
  assert_empty_file "$CC_TEST_CODESIGN_LOG"
  assert_empty_file "$CC_TEST_INSTALL_LOG"
  assert_empty_file "$CC_TEST_EVENT_LOG"
  assert_no_bare_command_bypass
  assert_runtime_unchanged
  assert_recorded_temp_removed
}

case_fake_tool_harness_is_operational() {
  setup_case fake_tool_harness_is_operational
  for bare_command in curl go codesign sudo; do
    if PATH=$CC_TEST_CONTROLLED_PATH "$bare_command" >/dev/null 2>&1; then
      fail "bare-command sentinel unexpectedly succeeded: $bare_command"
    fi
  done
  expected_bare_log="$case_root/bare-command.expected.log"
  printf '%s\n' curl go codesign sudo >"$expected_bare_log"
  cmp -s "$expected_bare_log" "$CC_TEST_BARE_COMMAND_LOG" || \
    fail 'controlled PATH did not record all bare-command bypasses'
  : >"$CC_TEST_BARE_COMMAND_LOG"
  [ "$("$CC_UNAME" -s)" = Darwin ] || fail 'fake uname OS is broken'
  [ "$("$CC_UNAME" -m)" = arm64 ] || fail 'fake uname architecture is broken'
  [ "$("$CC_GO_BIN" version)" = 'go version go1.26.5 darwin/arm64' ] || \
    fail 'fake system Go version is broken'
  (
    cd "$bundle"
    "$CC_SHASUM" -a 256 -c checksums.txt >/dev/null
  ) || fail 'fake shasum cannot verify a valid bundle'
  archive="$case_tmp/go1.26.5.darwin-arm64.tar.gz"
  if "$CC_CURL" -o "$archive" \
      'https://go.dev/dl/go1.26.5.darwin-arm64.tar.gz' >/dev/null 2>&1; then
    fail 'fake curl accepted a download without fail/redirect semantics'
  fi
  : >"$CC_TEST_CURL_LOG"
  : >"$CC_TEST_CURL_OUTPUT_LOG"
  "$CC_CURL" --location \
    'https://go.dev/dl/go1.26.5.darwin-arm64.tar.gz' \
    --output="$archive" --fail
  assert_curl_contract arm64
  expected_arm64_digest=efb87ff28af9a188d0536ef5d42e63dd52ba8263cd7344a993cc48dd11dedb6a
  archive_checksum=$("$CC_SHASUM" -a 256 "$archive")
  [ "$archive_checksum" = "$expected_arm64_digest  $archive" ] || \
    fail 'fake Go archive checksum is broken'
  correct_checksum_file="$case_tmp/correct-go-checksum"
  wrong_checksum_file="$case_tmp/wrong-go-checksum"
  printf '%s  %s\n' "$expected_arm64_digest" "$archive" \
    >"$correct_checksum_file"
  "$CC_SHASUM" -a 256 -c "$correct_checksum_file" >/dev/null || \
    fail 'fake shasum rejected the exact pinned arm64 digest'
  printf '%064d  %s\n' 0 "$archive" >"$wrong_checksum_file"
  if "$CC_SHASUM" -a 256 -c "$wrong_checksum_file" >/dev/null 2>&1; then
    fail 'fake shasum accepted a wrong 64-byte Go archive digest'
  fi
  amd64_archive="$case_tmp/go1.26.5.darwin-amd64.tar.gz"
  printf 'fake amd64 archive\n' >"$amd64_archive"
  expected_amd64_digest=6231d8d3b8f5552ec6cbf6d685bdd5482e1e703214b120e89b3bf0d7bf1ef725
  amd64_checksum=$("$CC_SHASUM" -a 256 "$amd64_archive")
  [ "$amd64_checksum" = "$expected_amd64_digest  $amd64_archive" ] || \
    fail 'fake shasum did not bind the amd64 digest to the amd64 archive'
  toolchain="$case_tmp/toolchain"
  "$CC_TAR" -xzf "$archive" -C "$toolchain"
  [ "$("$toolchain/go/bin/go" version)" = \
      'go version go1.26.5 darwin/arm64' ] || \
    fail 'fake temporary Go version is broken'
  CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 \
    "$toolchain/go/bin/go" build -mod=readonly -trimpath -tags goolm \
      -ldflags '-s -w -X main.version=v1.0.0 -X main.commit=0123456789abcdef -X main.buildTime=2026-07-22T00:00:00Z' \
      -o "$case_tmp/cc-connect" ./cmd/cc-connect
  assert_build_contract arm64 temporary
  "$CC_CODESIGN" --force --sign - --identifier com.cc-connect.service \
    "$case_tmp/cc-connect"
  "$CC_CODESIGN" --verify --strict "$case_tmp/cc-connect"
  assert_codesign_contract "$case_tmp/cc-connect"
  "$CC_TEST_INSTALL_SH" --binary "$case_tmp/cc-connect"
  assert_event_contract "$case_tmp/cc-connect"
  assert_nonempty_file "$CC_TEST_CODESIGN_LOG"
  assert_nonempty_file "$CC_TEST_INSTALL_LOG"
  rm -rf "$case_tmp"
  mkdir -p "$case_tmp"
  assert_runtime_unchanged
  assert_no_bare_command_bypass
  assert_temp_clean
}

case_fake_tool_harness_is_operational
assert_file "$bootstrap_source"

case_rejects_linux
case_rejects_unsupported_architecture
case_rejects_missing_go_mod
case_rejects_critical_symlink_inputs
case_rejects_parent_directory_symlink_escape
case_rejects_tampered_checksum_member
case_rejects_checksum_path_traversal
case_rejects_unlisted_bundle_symlink
case_reuses_go_1_26_5_and_populates_absent_source
case_reuses_go_1_25_0_and_preserves_nonempty_source
case_replaces_installer_snapshot_when_run_from_installer
case_installer_snapshot_promotion_failure_restores_old_snapshot
case_go_1_24_9_downloads_arm64_and_populates_empty_source
case_missing_go_downloads_amd64_and_activates
case_curl_failure_cleans_temp
case_sha_failure_cleans_temp
case_tar_failure_cleans_temp
case_build_failure_cleans_temp
case_rejects_candidate_version_mismatch
case_rejects_candidate_commit_mismatch
case_rejects_candidate_build_time_mismatch
case_rejects_version_changed_after_codesign
case_term_cleans_temp
case_int_cleans_temp

printf 'PASS: macOS source bootstrap\n'
