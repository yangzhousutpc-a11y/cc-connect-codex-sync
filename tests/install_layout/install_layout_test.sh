#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/cc-connect-layout-test.XXXXXX")
test_root=$(CDPATH= cd -- "$test_root" && pwd -P)
trap 'rm -rf "$test_root"' EXIT HUP INT TERM
original_path=$PATH
failures=0

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
assert_file() { [ -f "$1" ] || fail "missing file: $1"; }
assert_dir() { [ -d "$1" ] && [ ! -L "$1" ] || fail "missing real directory: $1"; }
assert_not_exists() { [ ! -e "$1" ] && [ ! -L "$1" ] || fail "unexpected path: $1"; }
assert_link_target() {
  [ -L "$1" ] || fail "not a symlink: $1"
  [ "$(readlink "$1")" = "$2" ] || fail "wrong target for $1"
}
assert_mode() {
  actual_mode=$(stat -f '%Lp' "$1")
  [ "$actual_mode" = "$2" ] || fail "mode for $1 is $actual_mode, want $2"
}
sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
expect_failure() {
  description=$1
  shift
  if "$@"; then fail "$description unexpectedly succeeded"; fi
}

write_fake_binary() {
  output=$1
  marker=$2
  cat >"$output" <<EOF
#!/bin/sh
# $marker
set -eu
state_file=\${CC_TEST_STATE_FILE:?}
daemon_log=\${CC_TEST_DAEMON_LOG:?}
plist="\$HOME/Library/LaunchAgents/com.cc-connect.service.plist"
meta="\${CC_CONNECT_ROOT:?}/data/daemon.json"
case "\${1:-}" in
  --version)
    [ "\${CC_TEST_VERSION_FAIL:-0}" != 1 ] || exit 23
    printf 'cc-connect test $marker\n'
    ;;
  daemon)
    printf '%s\n' "\$*" >>"\$daemon_log"
    case "\${2:-}" in
      install)
        work_dir=
        log_file=
        shift 2
        while [ "\$#" -gt 0 ]; do
          case "\$1" in
            --work-dir) work_dir=\$2; shift 2 ;;
            --log-file) log_file=\$2; shift 2 ;;
            *) shift ;;
          esac
        done
        mkdir -p "\$(dirname -- "\$plist")" "\$(dirname -- "\$meta")"
        if [ "\${CC_TEST_SIGNAL_PARENT:-0}" = 1 ]; then
          printf 'interrupted plist\n' >"\$plist"
          printf 'interrupted metadata\n' >"\$meta"
          printf 'gui\n' >"\$state_file"
          kill -TERM "\$PPID"
          exit 143
        fi
        if [ "\${CC_TEST_INSTALL_FAIL:-0}" = 1 ]; then
          printf 'failed plist\n' >"\$plist"
          printf 'failed metadata\n' >"\$meta"
          printf 'gui\n' >"\$state_file"
          exit 29
        fi
        cat >"\$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>com.cc-connect.service</string>
<key>ProgramArguments</key><array><string>\${CC_CONNECT_ROOT}/runtime/cc-connect</string></array>
<key>WorkingDirectory</key><string>\$work_dir</string>
<key>EnvironmentVariables</key><dict><key>CC_LOG_FILE</key><string>\$log_file</string></dict>
<key>StandardOutPath</key><string>/dev/null</string>
<key>StandardErrorPath</key><string>/dev/null</string>
</dict></plist>
PLIST
        cat >"\$meta" <<META
{"binary_path":"\${CC_CONNECT_ROOT}/runtime/cc-connect","work_dir":"\$work_dir","log_file":"\$log_file"}
META
        printf 'gui\n' >"\$state_file"
        ;;
      status)
        status=\${CC_TEST_STATUS_OVERRIDE:-}
        if [ -z "\$status" ]; then
          if [ -s "\$state_file" ]; then status=Running; else status='Not installed'; fi
        fi
        printf 'cc-connect daemon status\n\n  Status:    %s\n' "\$status"
        [ "\$status" != Running ] || printf '  PID:       4242\n'
        ;;
      uninstall)
        [ "\${CC_TEST_UNINSTALL_FAIL:-0}" != 1 ] || exit 31
        if [ "\${CC_TEST_UNINSTALL_LEAVES_RUNNING:-0}" != 1 ]; then
          rm -f "\$plist" "\$meta" "\$state_file"
        fi
        ;;
      *) exit 2 ;;
    esac
    ;;
  *) exit 0 ;;
esac
EOF
  chmod 755 "$output"
}

write_fake_launchctl() {
  output=$1
  cat >"$output" <<'EOF'
#!/bin/sh
set -eu
state_file=${CC_TEST_STATE_FILE:?}
[ -z "${CC_TEST_LAUNCHCTL_LOG:-}" ] || printf '%s\n' "$*" >>"$CC_TEST_LAUNCHCTL_LOG"
case "${1:-}" in
  print)
    target=${2:-}
    state=$(cat "$state_file" 2>/dev/null || true)
    case "$target:$state" in
      gui/*/com.cc-connect.service:gui|gui/*/com.cc-connect.service:both|user/*/com.cc-connect.service:user|user/*/com.cc-connect.service:both)
        printf 'state = running\npid = 4242\n'
        ;;
      *) exit 113 ;;
    esac
    ;;
  bootout)
    rm -f "$state_file"
    ;;
  bootstrap)
    case "${2:-}" in gui/*) printf 'gui\n' >"$state_file" ;; *) printf 'user\n' >"$state_file" ;; esac
    ;;
  kickstart) exit 0 ;;
  *) exit 2 ;;
esac
EOF
  chmod 755 "$output"
}

write_fake_pgrep() {
  output=$1
  cat >"$output" <<'EOF'
#!/bin/sh
set -eu
if [ -n "${CC_TEST_EXPECT_PGREP_PATTERN:-}" ] && [ "${2:-}" != "$CC_TEST_EXPECT_PGREP_PATTERN" ]; then
  exit 64
fi
if [ -n "${CC_TEST_PIDS:-}" ]; then
  printf '%b\n' "$CC_TEST_PIDS"
  exit 0
fi
[ -s "${CC_TEST_STATE_FILE:?}" ] || exit 1
printf '4242\n'
EOF
  chmod 755 "$output"
}

setup_case() {
  case_name=$1
  case_root="$test_root/$case_name"
  fake_home="$case_root/home"
  bundle="$case_root/bundle"
  tools_dir="$case_root/tools"
  mkdir -p "$fake_home" "$bundle/runtime" "$tools_dir"
  cp "$repo_root/packaging/macos/"*.sh "$bundle/"
  cp "$repo_root/config.example.toml" "$bundle/config.example.toml"
  cp "$repo_root/packaging/macos/README.zh-CN.md" "$bundle/README.md"
  write_fake_binary "$bundle/runtime/cc-connect" candidate
  write_fake_launchctl "$tools_dir/launchctl"
  write_fake_pgrep "$tools_dir/pgrep"
  chmod 755 "$bundle/"*.sh
  HOME=$fake_home
  CC_CONNECT_ROOT="$fake_home/cc-connect"
  CC_TEST_STATE_FILE="$case_root/launchd.state"
  CC_TEST_DAEMON_LOG="$case_root/daemon.log"
  CC_TEST_LAUNCHCTL_LOG="$case_root/launchctl.log"
  CC_LAUNCHCTL="$tools_dir/launchctl"
  CC_PGREP="$tools_dir/pgrep"
  PATH="$HOME/.local/bin:$tools_dir:$original_path"
  export HOME CC_CONNECT_ROOT CC_TEST_STATE_FILE CC_TEST_DAEMON_LOG CC_TEST_LAUNCHCTL_LOG CC_LAUNCHCTL CC_PGREP PATH
  : >"$CC_TEST_DAEMON_LOG"
  : >"$CC_TEST_LAUNCHCTL_LOG"
}

install_staged() {
  "$bundle/install.sh" --binary "$bundle/runtime/cc-connect"
}

install_active() {
  install_staged
  install -m 600 "$bundle/config.example.toml" "$CC_CONNECT_ROOT/data/config.toml"
  "$bundle/install.sh" --binary "$bundle/runtime/cc-connect" --activate
}

case_basic_layout_and_readme() {
  setup_case basic_layout_and_readme
  install_staged
  for dir in source runtime data installer backups; do assert_dir "$CC_CONNECT_ROOT/$dir"; done
  assert_file "$CC_CONNECT_ROOT/runtime/cc-connect"
  assert_file "$CC_CONNECT_ROOT/data/config.example.toml"
  assert_file "$CC_CONNECT_ROOT/installer/README.md"
  assert_link_target "$HOME/.cc-connect" "$CC_CONNECT_ROOT/data"
  assert_link_target "$HOME/.local/bin/cc-connect" "$CC_CONNECT_ROOT/runtime/cc-connect"
  [ ! -s "$CC_TEST_DAEMON_LOG" ] || fail 'staged install activated daemon'
  "$bundle/doctor.sh"
}

case_source_readme_compatibility() {
  setup_case source_readme_compatibility
  mv "$bundle/README.md" "$bundle/README.zh-CN.md"
  install_staged
  assert_file "$CC_CONNECT_ROOT/installer/README.md"
}

case_installed_source_can_update_itself() {
  setup_case installed_source_can_update_itself
  install_staged
  candidate="$case_root/new-candidate"
  write_fake_binary "$candidate" self-update
  candidate_hash=$(sha256 "$candidate")
  "$CC_CONNECT_ROOT/installer/install.sh" --binary "$candidate" || fail 'installed installer could not update itself'
  [ "$(sha256 "$CC_CONNECT_ROOT/runtime/cc-connect")" = "$candidate_hash" ] || fail 'installed source did not promote the candidate runtime'
  for leaf in config.example.toml README.md install.sh doctor.sh uninstall.sh lib.sh; do
    assert_file "$CC_CONNECT_ROOT/installer/$leaf"
  done
}

case_material_leaf_preflight_preserves_runtime() {
  leaf_index=0
  for relative_leaf in \
    installer/config.example.toml \
    data/config.example.toml \
    installer/README.md \
    installer/install.sh \
    installer/doctor.sh \
    installer/uninstall.sh \
    installer/lib.sh
  do
    leaf_index=$((leaf_index + 1))
    setup_case "material_leaf_$leaf_index"
    install_staged
    old_hash=$(sha256 "$CC_CONNECT_ROOT/runtime/cc-connect")
    target="$CC_CONNECT_ROOT/$relative_leaf"
    rm "$target"
    mkdir "$target"
    candidate="$case_root/new-candidate"
    write_fake_binary "$candidate" "leaf-$leaf_index"
    expect_failure "material leaf directory: $relative_leaf" "$bundle/install.sh" --binary "$candidate"
    [ "$(sha256 "$CC_CONNECT_ROOT/runtime/cc-connect")" = "$old_hash" ] || fail "material leaf failure changed runtime: $relative_leaf"
  done
}

case_candidate_version_preflight() {
  setup_case candidate_version_preflight
  CC_TEST_VERSION_FAIL=1
  export CC_TEST_VERSION_FAIL
  expect_failure 'invalid candidate' "$bundle/install.sh" --binary "$bundle/runtime/cc-connect"
  assert_not_exists "$CC_CONNECT_ROOT"
}

case_collision_preserves_runtime() {
  setup_case collision_preserves_runtime
  mkdir -p "$CC_CONNECT_ROOT/runtime" "$HOME/.cc-connect"
  write_fake_binary "$CC_CONNECT_ROOT/runtime/cc-connect" stable
  old_hash=$(sha256 "$CC_CONNECT_ROOT/runtime/cc-connect")
  expect_failure 'data entry collision' "$bundle/install.sh" --binary "$bundle/runtime/cc-connect"
  [ "$(sha256 "$CC_CONNECT_ROOT/runtime/cc-connect")" = "$old_hash" ] || fail 'collision changed stable runtime'
}

case_rejects_unsafe_runtime_targets() {
  setup_case rejects_unsafe_runtime_targets
  mkdir -p "$CC_CONNECT_ROOT/runtime/cc-connect"
  expect_failure 'runtime destination directory' "$bundle/install.sh" --binary "$bundle/runtime/cc-connect"
  assert_not_exists "$CC_CONNECT_ROOT/runtime/cc-connect/cc-connect.next"

  rm -rf "$CC_CONNECT_ROOT/runtime/cc-connect"
  external_runtime="$case_root/external-runtime"
  write_fake_binary "$external_runtime" external
  external_hash=$(sha256 "$external_runtime")
  ln -s "$external_runtime" "$CC_CONNECT_ROOT/runtime/cc-connect"
  expect_failure 'runtime destination symlink' "$bundle/install.sh" --binary "$bundle/runtime/cc-connect"
  [ "$(sha256 "$external_runtime")" = "$external_hash" ] || fail 'unsafe runtime target changed external file'

  rm "$CC_CONNECT_ROOT/runtime/cc-connect"
  write_fake_binary "$CC_CONNECT_ROOT/runtime/cc-connect" stable
  old_hash=$(sha256 "$CC_CONNECT_ROOT/runtime/cc-connect")
  external_next="$case_root/external-next"
  printf 'external next\n' >"$external_next"
  next_hash=$(sha256 "$external_next")
  ln -s "$external_next" "$CC_CONNECT_ROOT/runtime/cc-connect.next"
  expect_failure 'symlink candidate staging path' "$bundle/install.sh" --binary "$bundle/runtime/cc-connect"
  [ "$(sha256 "$CC_CONNECT_ROOT/runtime/cc-connect")" = "$old_hash" ] || fail 'unsafe staging changed stable runtime'
  [ "$(sha256 "$external_next")" = "$next_hash" ] || fail 'unsafe staging changed external target'

  rm "$CC_CONNECT_ROOT/runtime/cc-connect.next"
  mkdir "$CC_CONNECT_ROOT/runtime/cc-connect.next"
  expect_failure 'candidate staging directory' "$bundle/install.sh" --binary "$bundle/runtime/cc-connect"
  [ "$(sha256 "$CC_CONNECT_ROOT/runtime/cc-connect")" = "$old_hash" ] || fail 'staging directory changed stable runtime'
}

case_missing_config_preserves_runtime() {
  setup_case missing_config_preserves_runtime
  mkdir -p "$CC_CONNECT_ROOT/runtime"
  write_fake_binary "$CC_CONNECT_ROOT/runtime/cc-connect" stable
  old_hash=$(sha256 "$CC_CONNECT_ROOT/runtime/cc-connect")
  expect_failure 'activation without config' "$bundle/install.sh" --binary "$bundle/runtime/cc-connect" --activate
  [ "$(sha256 "$CC_CONNECT_ROOT/runtime/cc-connect")" = "$old_hash" ] || fail 'missing config changed stable runtime'
}

case_config_symlink_preserves_external_file() {
  setup_case config_symlink_preserves_external_file
  mkdir -p "$CC_CONNECT_ROOT/runtime" "$CC_CONNECT_ROOT/data"
  write_fake_binary "$CC_CONNECT_ROOT/runtime/cc-connect" stable
  external_config="$case_root/external-config.toml"
  cp "$bundle/config.example.toml" "$external_config"
  chmod 644 "$external_config"
  ln -s "$external_config" "$CC_CONNECT_ROOT/data/config.toml"
  old_hash=$(sha256 "$CC_CONNECT_ROOT/runtime/cc-connect")
  expect_failure 'activation with symlink config' "$bundle/install.sh" --binary "$bundle/runtime/cc-connect" --activate
  [ "$(sha256 "$CC_CONNECT_ROOT/runtime/cc-connect")" = "$old_hash" ] || fail 'symlink config changed stable runtime'
  assert_mode "$external_config" 644
}

case_rejects_unsafe_roots_before_writes() {
  setup_case rejects_unsafe_roots_before_writes
  old_pwd=$PWD
  cd "$case_root"
  expect_failure 'relative root' env HOME="$HOME" CC_CONNECT_ROOT=relative-root CC_TEST_STATE_FILE="$CC_TEST_STATE_FILE" CC_TEST_DAEMON_LOG="$CC_TEST_DAEMON_LOG" "$bundle/install.sh" --binary "$bundle/runtime/cc-connect"
  assert_not_exists "$case_root/relative-root"
  expect_failure 'HOME as root' env HOME="$HOME" CC_CONNECT_ROOT="$HOME" CC_TEST_STATE_FILE="$CC_TEST_STATE_FILE" CC_TEST_DAEMON_LOG="$CC_TEST_DAEMON_LOG" "$bundle/install.sh" --binary "$bundle/runtime/cc-connect"
  assert_not_exists "$HOME/runtime"
  expect_failure 'root containing dot-dot' env HOME="$HOME" CC_CONNECT_ROOT="$case_root/path/../root" CC_TEST_STATE_FILE="$CC_TEST_STATE_FILE" CC_TEST_DAEMON_LOG="$CC_TEST_DAEMON_LOG" "$bundle/install.sh" --binary "$bundle/runtime/cc-connect"
  assert_not_exists "$case_root/root"
  expect_failure 'root with trailing slash' env HOME="$HOME" CC_CONNECT_ROOT="$case_root/root/" CC_TEST_STATE_FILE="$CC_TEST_STATE_FILE" CC_TEST_DAEMON_LOG="$CC_TEST_DAEMON_LOG" "$bundle/install.sh" --binary "$bundle/runtime/cc-connect"
  assert_not_exists "$case_root/root"
  cd "$old_pwd"

  dangerous_tools="$case_root/dangerous-tools"
  mkdir -p "$dangerous_tools"
  cat >"$dangerous_tools/mkdir" <<EOF
#!/bin/sh
printf 'called\n' >"$case_root/write-command-called"
exit 97
EOF
  chmod 755 "$dangerous_tools/mkdir"
  expect_failure 'filesystem root' env HOME="$HOME" CC_CONNECT_ROOT=/ CC_TEST_STATE_FILE="$CC_TEST_STATE_FILE" CC_TEST_DAEMON_LOG="$CC_TEST_DAEMON_LOG" PATH="$dangerous_tools:$PATH" "$bundle/install.sh" --binary "$bundle/runtime/cc-connect"
  assert_not_exists "$case_root/write-command-called"
}

case_rejects_symlink_layout() {
  setup_case rejects_symlink_layout
  root_target="$case_root/root-target"
  mkdir -p "$root_target"
  ln -s "$root_target" "$CC_CONNECT_ROOT"
  expect_failure 'symlink root' "$bundle/install.sh" --binary "$bundle/runtime/cc-connect"
  [ -z "$(find "$root_target" -mindepth 1 -print -quit)" ] || fail 'symlink root target was modified'

  rm "$CC_CONNECT_ROOT"
  mkdir -p "$CC_CONNECT_ROOT" "$case_root/runtime-target"
  write_fake_binary "$case_root/runtime-target/cc-connect" stable
  old_hash=$(sha256 "$case_root/runtime-target/cc-connect")
  ln -s "$case_root/runtime-target" "$CC_CONNECT_ROOT/runtime"
  expect_failure 'symlink category' "$bundle/install.sh" --binary "$bundle/runtime/cc-connect"
  [ "$(sha256 "$case_root/runtime-target/cc-connect")" = "$old_hash" ] || fail 'symlink category target was modified'

  parent_target="$case_root/parent-target"
  mkdir -p "$parent_target"
  ln -s "$parent_target" "$case_root/root-parent-link"
  expect_failure 'symlink parent component' env HOME="$HOME" CC_CONNECT_ROOT="$case_root/root-parent-link/cc-connect" CC_TEST_STATE_FILE="$CC_TEST_STATE_FILE" CC_TEST_DAEMON_LOG="$CC_TEST_DAEMON_LOG" "$bundle/install.sh" --binary "$bundle/runtime/cc-connect"
  [ -z "$(find "$parent_target" -mindepth 1 -print -quit)" ] || fail 'symlink parent target was modified'
}

case_permissions_are_private() {
  setup_case permissions_are_private
  old_umask=$(umask)
  umask 022
  install_staged
  assert_mode "$CC_CONNECT_ROOT" 700
  assert_mode "$CC_CONNECT_ROOT/data" 700
  assert_mode "$CC_CONNECT_ROOT/backups" 700
  cp "$bundle/config.example.toml" "$CC_CONNECT_ROOT/data/config.toml"
  chmod 644 "$CC_CONNECT_ROOT/data/config.toml"
  "$bundle/install.sh" --binary "$bundle/runtime/cc-connect" --activate
  assert_mode "$CC_CONNECT_ROOT/data/config.toml" 600
  chmod 400 "$CC_CONNECT_ROOT/data/config.toml"
  "$bundle/install.sh" --binary "$bundle/runtime/cc-connect" --activate
  assert_mode "$CC_CONNECT_ROOT/data/config.toml" 400
  umask "$old_umask"
}

case_upgrade_backup_and_activation_rollback() {
  setup_case upgrade_backup_and_activation_rollback
  mkdir -p "$CC_CONNECT_ROOT/runtime" "$CC_CONNECT_ROOT/data" "$HOME/Library/LaunchAgents"
  write_fake_binary "$CC_CONNECT_ROOT/runtime/cc-connect" stable
  install -m 600 "$bundle/config.example.toml" "$CC_CONNECT_ROOT/data/config.toml"
  old_hash=$(sha256 "$CC_CONNECT_ROOT/runtime/cc-connect")
  printf 'old plist\n' >"$HOME/Library/LaunchAgents/com.cc-connect.service.plist"
  printf 'old metadata\n' >"$CC_CONNECT_ROOT/data/daemon.json"
  printf 'gui\n' >"$CC_TEST_STATE_FILE"
  mkdir -p "$CC_CONNECT_ROOT/backups"
  for backup_name in install-20000101000001-sentinel install-20000101000002-sentinel install-20000101000003-sentinel install-20000101000004-sentinel; do
    mkdir "$CC_CONNECT_ROOT/backups/$backup_name"
    printf 'keep\n' >"$CC_CONNECT_ROOT/backups/$backup_name/sentinel"
  done
  CC_TEST_INSTALL_FAIL=1
  export CC_TEST_INSTALL_FAIL
  expect_failure 'daemon activation failure' "$bundle/install.sh" --binary "$bundle/runtime/cc-connect" --activate
  [ "$(sha256 "$CC_CONNECT_ROOT/runtime/cc-connect")" = "$old_hash" ] || fail 'activation rollback did not restore runtime'
  grep -Fx 'old plist' "$HOME/Library/LaunchAgents/com.cc-connect.service.plist" >/dev/null || fail 'activation rollback did not restore plist'
  grep -Fx 'old metadata' "$CC_CONNECT_ROOT/data/daemon.json" >/dev/null || fail 'activation rollback did not restore daemon metadata'
  grep -F 'bootstrap gui/' "$CC_TEST_LAUNCHCTL_LOG" >/dev/null || fail 'activation rollback did not reload the previously running service'
  assert_not_exists "$HOME/.cc-connect"
  assert_not_exists "$HOME/.local/bin/cc-connect"
  assert_not_exists "$CC_CONNECT_ROOT/data/config.example.toml"
  for leaf in config.example.toml README.md install.sh doctor.sh uninstall.sh lib.sh; do
    assert_not_exists "$CC_CONNECT_ROOT/installer/$leaf"
  done
  for backup_name in install-20000101000001-sentinel install-20000101000002-sentinel install-20000101000003-sentinel install-20000101000004-sentinel; do
    assert_file "$CC_CONNECT_ROOT/backups/$backup_name/sentinel"
  done
  backup_match=
  for backup in "$CC_CONNECT_ROOT"/backups/*/runtime-cc-connect; do
    [ -f "$backup" ] || continue
    if [ "$(sha256 "$backup")" = "$old_hash" ]; then backup_match=$backup; fi
  done
  [ -n "$backup_match" ] || fail 'upgrade did not preserve old runtime in backups'
}

case_running_service_rejects_staged_update() {
  setup_case running_service_rejects_staged_update
  install_active
  old_hash=$(sha256 "$CC_CONNECT_ROOT/runtime/cc-connect")
  candidate="$case_root/new-candidate"
  write_fake_binary "$candidate" staged-split
  expect_failure 'staged update while daemon is running' "$bundle/install.sh" --binary "$candidate"
  [ "$(sha256 "$CC_CONNECT_ROOT/runtime/cc-connect")" = "$old_hash" ] || fail 'running service staged update changed runtime'
}

case_successful_updates_keep_three_backups() {
  setup_case successful_updates_keep_three_backups
  install_active
  update_index=1
  while [ "$update_index" -le 4 ]; do
    candidate="$case_root/candidate-$update_index"
    write_fake_binary "$candidate" "update-$update_index"
    "$bundle/install.sh" --binary "$candidate" --activate
    update_index=$((update_index + 1))
  done
  backup_count=$(find "$CC_CONNECT_ROOT/backups" -mindepth 1 -maxdepth 1 -type d -name 'install-*' | wc -l | tr -d ' ')
  [ "$backup_count" -eq 3 ] || fail "successful updates kept $backup_count install backups, want 3"
}

case_backup_pruning_preserves_path_boundaries() {
  setup_case backup_pruning_preserves_path_boundaries
  newline=$(printf '\n_')
  newline=${newline%_}
  external_root="$case_root/external-root"
  newline_root="$case_root/newline-prefix${newline}$external_root"
  newline_backups="$newline_root/backups"
  mkdir -p "$newline_backups" "$external_root/backups"
  for suffix in 1 2 3 4; do
    backup_name="install-2000010100000$suffix-boundary"
    mkdir -p "$newline_backups/$backup_name" "$external_root/backups/$backup_name"
    printf 'keep\n' >"$external_root/backups/$backup_name/sentinel"
  done

  . "$bundle/lib.sh"
  cc_prune_install_backups "$newline_backups"

  for suffix in 1 2 3 4; do
    assert_file "$external_root/backups/install-2000010100000$suffix-boundary/sentinel"
  done
  backup_count=0
  for backup_path in "$newline_backups"/install-*; do
    [ -d "$backup_path" ] && [ ! -L "$backup_path" ] || continue
    backup_count=$((backup_count + 1))
  done
  [ "$backup_count" -eq 3 ] || fail "newline-root pruning kept $backup_count direct backups, want 3"
}

case_backup_pruning_reports_delete_failure() {
  setup_case backup_pruning_reports_delete_failure
  mkdir -p "$CC_CONNECT_ROOT/backups"
  for suffix in 1 2 3 4; do
    mkdir "$CC_CONNECT_ROOT/backups/install-2000010100000$suffix-delete-failure"
  done
  cat >"$tools_dir/rm" <<'EOF'
#!/bin/sh
exit 88
EOF
  chmod 755 "$tools_dir/rm"

  . "$bundle/lib.sh"
  expect_failure 'backup prune delete failure' cc_prune_install_backups "$CC_CONNECT_ROOT/backups"
  /bin/rm "$tools_dir/rm"

  backup_count=0
  for backup_path in "$CC_CONNECT_ROOT/backups"/install-*; do
    [ -d "$backup_path" ] && [ ! -L "$backup_path" ] || continue
    backup_count=$((backup_count + 1))
  done
  [ "$backup_count" -eq 4 ] || fail "failed prune changed backup count to $backup_count, want 4"
}

case_signal_rolls_back_transaction() {
  setup_case signal_rolls_back_transaction
  install_staged
  install -m 600 "$bundle/config.example.toml" "$CC_CONNECT_ROOT/data/config.toml"
  old_runtime_hash=$(sha256 "$CC_CONNECT_ROOT/runtime/cc-connect")
  old_readme_hash=$(sha256 "$CC_CONNECT_ROOT/installer/README.md")
  candidate="$case_root/signal-candidate"
  write_fake_binary "$candidate" signal-candidate
  CC_TEST_SIGNAL_PARENT=1
  export CC_TEST_SIGNAL_PARENT

  expect_failure 'TERM during daemon activation' "$bundle/install.sh" --binary "$candidate" --activate

  unset CC_TEST_SIGNAL_PARENT
  [ "$(sha256 "$CC_CONNECT_ROOT/runtime/cc-connect")" = "$old_runtime_hash" ] || fail 'TERM rollback did not restore runtime'
  [ "$(sha256 "$CC_CONNECT_ROOT/installer/README.md")" = "$old_readme_hash" ] || fail 'TERM rollback did not restore install materials'
  assert_not_exists "$HOME/Library/LaunchAgents/com.cc-connect.service.plist"
  assert_not_exists "$CC_CONNECT_ROOT/data/daemon.json"
  assert_link_target "$HOME/.cc-connect" "$CC_CONNECT_ROOT/data"
  assert_link_target "$HOME/.local/bin/cc-connect" "$CC_CONNECT_ROOT/runtime/cc-connect"
}

case_service_metadata_symlinks_preserve_external_files() {
  setup_case service_metadata_symlinks_preserve_external_files
  mkdir -p "$CC_CONNECT_ROOT/runtime" "$CC_CONNECT_ROOT/data" "$HOME/Library/LaunchAgents"
  write_fake_binary "$CC_CONNECT_ROOT/runtime/cc-connect" stable
  install -m 600 "$bundle/config.example.toml" "$CC_CONNECT_ROOT/data/config.toml"
  old_hash=$(sha256 "$CC_CONNECT_ROOT/runtime/cc-connect")
  external_plist="$case_root/external.plist"
  external_meta="$case_root/external-daemon.json"
  printf 'external plist\n' >"$external_plist"
  printf 'external metadata\n' >"$external_meta"
  chmod 644 "$external_plist" "$external_meta"
  plist_hash=$(sha256 "$external_plist")
  meta_hash=$(sha256 "$external_meta")
  ln -s "$external_plist" "$HOME/Library/LaunchAgents/com.cc-connect.service.plist"
  ln -s "$external_meta" "$CC_CONNECT_ROOT/data/daemon.json"
  expect_failure 'symlink service metadata' "$bundle/install.sh" --binary "$bundle/runtime/cc-connect" --activate
  [ "$(sha256 "$CC_CONNECT_ROOT/runtime/cc-connect")" = "$old_hash" ] || fail 'service metadata preflight changed stable runtime'
  [ "$(sha256 "$external_plist")" = "$plist_hash" ] || fail 'service metadata preflight changed external plist'
  [ "$(sha256 "$external_meta")" = "$meta_hash" ] || fail 'service metadata preflight changed external daemon metadata'
  assert_mode "$external_plist" 644
  assert_mode "$external_meta" 644

  rm "$HOME/Library/LaunchAgents/com.cc-connect.service.plist"
  expect_failure 'symlink daemon metadata' "$bundle/install.sh" --binary "$bundle/runtime/cc-connect" --activate
  [ "$(sha256 "$external_meta")" = "$meta_hash" ] || fail 'daemon metadata preflight changed external file'
}

case_doctor_rejects_not_installed() {
  setup_case doctor_rejects_not_installed
  install_staged
  install -m 600 "$bundle/config.example.toml" "$CC_CONNECT_ROOT/data/config.toml"
  expect_failure 'doctor with daemon not installed' "$bundle/doctor.sh"
}

case_doctor_rejects_stopped() {
  setup_case doctor_rejects_stopped
  install_active
  : >"$CC_TEST_STATE_FILE"
  CC_TEST_STATUS_OVERRIDE=Stopped
  export CC_TEST_STATUS_OVERRIDE
  expect_failure 'doctor with stopped daemon' "$bundle/doctor.sh"
}

case_doctor_rejects_old_plist() {
  setup_case doctor_rejects_old_plist
  install_active
  sed -i '' 's#<string>.*/runtime/cc-connect</string>#<string>/old/runtime/cc-connect</string>#' "$HOME/Library/LaunchAgents/com.cc-connect.service.plist"
  expect_failure 'doctor with stale plist' "$bundle/doctor.sh"
}

case_doctor_rejects_old_path() {
  setup_case doctor_rejects_old_path
  install_active
  old_bin="$case_root/old-bin"
  mkdir -p "$old_bin"
  write_fake_binary "$old_bin/cc-connect" old-path
  PATH="$old_bin:$PATH"
  export PATH
  expect_failure 'doctor with stale PATH entry' "$bundle/doctor.sh"
}

case_doctor_rejects_old_daemon_metadata() {
  setup_case doctor_rejects_old_daemon_metadata
  install_active
  sed -i '' 's#"binary_path":"[^"]*"#"binary_path":"/old/runtime/cc-connect"#' "$CC_CONNECT_ROOT/data/daemon.json"
  expect_failure 'doctor with stale daemon metadata' "$bundle/doctor.sh"
}

case_doctor_rejects_duplicate_launchd_jobs() {
  setup_case doctor_rejects_duplicate_launchd_jobs
  install_active
  printf 'both\n' >"$CC_TEST_STATE_FILE"
  expect_failure 'doctor with duplicate launchd jobs' "$bundle/doctor.sh"
}

case_doctor_rejects_multiple_instances() {
  setup_case doctor_rejects_multiple_instances
  install_active
  CC_TEST_PIDS='4242\n5252'
  export CC_TEST_PIDS
  expect_failure 'doctor with multiple service processes' "$bundle/doctor.sh"
}

case_doctor_accepts_healthy_service() {
  setup_case doctor_accepts_healthy_service
  install_active
  "$bundle/doctor.sh"
}

case_doctor_escapes_runtime_path_for_pgrep() {
  setup_case doctor_escapes_runtime_path_for_pgrep
  CC_CONNECT_ROOT="$fake_home/cc-connect[one]+"
  export CC_CONNECT_ROOT
  CC_TEST_EXPECT_PGREP_PATTERN=$(printf '%s\n' "$CC_CONNECT_ROOT/runtime/cc-connect" | sed 's/[][\\.^$*+?(){}|]/\\&/g')
  export CC_TEST_EXPECT_PGREP_PATTERN
  install_active
  "$bundle/doctor.sh"
}

case_doctor_uninstall_reject_unsafe_runtime() {
  setup_case unsafe_runtime_symlink
  install_staged
  install -m 600 "$bundle/config.example.toml" "$CC_CONNECT_ROOT/data/config.toml"
  rm "$CC_CONNECT_ROOT/runtime/cc-connect"
  external_runtime="$case_root/external-runtime"
  write_fake_binary "$external_runtime" external-runtime
  ln -s "$external_runtime" "$CC_CONNECT_ROOT/runtime/cc-connect"
  : >"$CC_TEST_DAEMON_LOG"
  expect_failure 'doctor runtime symlink' "$bundle/doctor.sh"
  [ ! -s "$CC_TEST_DAEMON_LOG" ] || fail 'doctor executed runtime symlink target'
  expect_failure 'uninstall runtime symlink' "$bundle/uninstall.sh"
  [ ! -s "$CC_TEST_DAEMON_LOG" ] || fail 'uninstall executed runtime symlink target'
  assert_link_target "$HOME/.cc-connect" "$CC_CONNECT_ROOT/data"
  assert_link_target "$HOME/.local/bin/cc-connect" "$CC_CONNECT_ROOT/runtime/cc-connect"

  setup_case unsafe_runtime_directory
  install_staged
  rm "$CC_CONNECT_ROOT/runtime/cc-connect"
  mkdir "$CC_CONNECT_ROOT/runtime/cc-connect"
  expect_failure 'doctor runtime directory' "$bundle/doctor.sh"
  expect_failure 'uninstall runtime directory' "$bundle/uninstall.sh"
  assert_link_target "$HOME/.cc-connect" "$CC_CONNECT_ROOT/data"
  assert_link_target "$HOME/.local/bin/cc-connect" "$CC_CONNECT_ROOT/runtime/cc-connect"

  setup_case unsafe_runtime_not_executable
  install_staged
  chmod 600 "$CC_CONNECT_ROOT/runtime/cc-connect"
  expect_failure 'doctor non-executable runtime' "$bundle/doctor.sh"
  expect_failure 'uninstall non-executable runtime' "$bundle/uninstall.sh"
  assert_link_target "$HOME/.cc-connect" "$CC_CONNECT_ROOT/data"
  assert_link_target "$HOME/.local/bin/cc-connect" "$CC_CONNECT_ROOT/runtime/cc-connect"
}

case_uninstall_failure_preserves_entries() {
  setup_case uninstall_failure_preserves_entries
  install_active
  CC_TEST_UNINSTALL_FAIL=1
  export CC_TEST_UNINSTALL_FAIL
  expect_failure 'daemon uninstall failure' "$bundle/uninstall.sh"
  assert_link_target "$HOME/.cc-connect" "$CC_CONNECT_ROOT/data"
  assert_link_target "$HOME/.local/bin/cc-connect" "$CC_CONNECT_ROOT/runtime/cc-connect"
}

case_uninstall_verifies_service_stopped() {
  setup_case uninstall_verifies_service_stopped
  install_active
  CC_TEST_UNINSTALL_LEAVES_RUNNING=1
  export CC_TEST_UNINSTALL_LEAVES_RUNNING
  expect_failure 'daemon still running after uninstall' "$bundle/uninstall.sh"
  assert_link_target "$HOME/.cc-connect" "$CC_CONNECT_ROOT/data"
  assert_link_target "$HOME/.local/bin/cc-connect" "$CC_CONNECT_ROOT/runtime/cc-connect"
}

case_uninstall_rejects_orphan_process() {
  setup_case uninstall_rejects_orphan_process
  install_active
  CC_TEST_PIDS=7777
  export CC_TEST_PIDS
  expect_failure 'orphan runtime process after uninstall' "$bundle/uninstall.sh"
  assert_link_target "$HOME/.cc-connect" "$CC_CONNECT_ROOT/data"
  assert_link_target "$HOME/.local/bin/cc-connect" "$CC_CONNECT_ROOT/runtime/cc-connect"
}

case_uninstall_not_installed_and_nonowned_entries() {
  setup_case uninstall_not_installed_and_nonowned_entries
  install_staged
  "$bundle/uninstall.sh"
  assert_not_exists "$HOME/.cc-connect"
  assert_not_exists "$HOME/.local/bin/cc-connect"

  mkdir -p "$HOME/.local/bin" "$case_root/legacy-data"
  ln -s "$case_root/legacy-data" "$HOME/.cc-connect"
  ln -s "$bundle/runtime/cc-connect" "$HOME/.local/bin/cc-connect"
  "$bundle/uninstall.sh"
  assert_link_target "$HOME/.cc-connect" "$case_root/legacy-data"
  assert_link_target "$HOME/.local/bin/cc-connect" "$bundle/runtime/cc-connect"
}

all_cases='basic_layout_and_readme source_readme_compatibility installed_source_can_update_itself material_leaf_preflight_preserves_runtime candidate_version_preflight collision_preserves_runtime rejects_unsafe_runtime_targets missing_config_preserves_runtime config_symlink_preserves_external_file rejects_unsafe_roots_before_writes rejects_symlink_layout permissions_are_private upgrade_backup_and_activation_rollback running_service_rejects_staged_update successful_updates_keep_three_backups backup_pruning_preserves_path_boundaries backup_pruning_reports_delete_failure signal_rolls_back_transaction service_metadata_symlinks_preserve_external_files doctor_rejects_not_installed doctor_rejects_stopped doctor_rejects_old_plist doctor_rejects_old_path doctor_rejects_old_daemon_metadata doctor_rejects_duplicate_launchd_jobs doctor_rejects_multiple_instances doctor_accepts_healthy_service doctor_escapes_runtime_path_for_pgrep doctor_uninstall_reject_unsafe_runtime uninstall_failure_preserves_entries uninstall_verifies_service_stopped uninstall_rejects_orphan_process uninstall_not_installed_and_nonowned_entries'

run_case() {
  name=$1
  if ("case_$name"); then
    printf 'PASS: %s\n' "$name"
  else
    printf 'FAIL CASE: %s\n' "$name" >&2
    failures=$((failures + 1))
  fi
}

if [ "$#" -gt 0 ]; then
  for name in "$@"; do run_case "$name"; done
else
  for name in $all_cases; do run_case "$name"; done
fi

[ "$failures" -eq 0 ] || exit 1
printf 'PASS: install layout\n'
