#!/bin/sh
set -eu

bundle=${1:-}
[ -n "$bundle" ] && [ -d "$bundle" ] && [ ! -L "$bundle" ] || {
  printf 'public bundle scan requires a real directory\n' >&2
  exit 2
}

scan_root=$(mktemp -d "${TMPDIR:-/tmp}/cc-connect-public-scan.XXXXXX")
cleanup() { rm -rf "$scan_root"; }
trap cleanup EXIT HUP INT TERM

all_paths=$scan_root/all-paths
if ! find "$bundle" -print >"$all_paths"; then
  printf 'public bundle path scan failed\n' >&2
  exit 1
fi
while IFS= read -r path; do
  if [ -L "$path" ]; then
    printf 'public bundle contains symlink: %s\n' "$path" >&2
    exit 1
  fi
  case "$path" in
    */.wecom|*/data/wecom|*/state/wecom)
      printf 'public bundle contains WeCom local state: %s\n' "$path" >&2
      exit 1
      ;;
  esac
  name=${path##*/}
  case "$name" in
    config.toml|daemon.json|sessions|logs|crons|timers|operations|.git|\
    wecom-state|wecom_state|wecom-session|wecom_sessions|wecom-token|wecom_token)
      printf 'public bundle contains forbidden object: %s\n' "$path" >&2
      exit 1
      ;;
  esac
done <"$all_paths"

maintainer_name=yangzhou
maintainer_path=/Users/$maintainer_name
maintainer_matches=$scan_root/maintainer-matches
grep_status=0
grep -r -n -F "$maintainer_path" "$bundle" >"$maintainer_matches" 2>"$scan_root/maintainer-errors" || \
  grep_status=$?
case "$grep_status" in
  0)
    cat "$maintainer_matches" >&2
    printf 'public bundle contains maintainer absolute path\n' >&2
    exit 1
    ;;
  1) ;;
  *)
    cat "$scan_root/maintainer-errors" >&2
    printf 'public bundle maintainer-path scan failed\n' >&2
    exit 1
    ;;
esac

secret_matches=$scan_root/secret-matches
grep_status=0
grep -r -n -E \
  '(^|[,{][[:space:]]*)(app_secret|api_key|access_token|bot_secret)[[:space:]]*=' \
  "$bundle" >"$secret_matches" 2>"$scan_root/secret-errors" || grep_status=$?
case "$grep_status" in
  0) ;;
  1) ;;
  *)
    cat "$scan_root/secret-errors" >&2
    printf 'public bundle secret scan failed\n' >&2
    exit 1
    ;;
esac

while IFS= read -r assignment; do
  relative=${assignment#"$bundle"/}
  file=${relative%%:*}
  remainder=${relative#*:}
  content=${remainder#*:}
  normalized=$(printf '%s\n' "$content" | sed \
    -e 's/^[[:space:]]*//' \
    -e 's/[[:space:]]*$//')

  case "$normalized" in
    *'=""'*|*'= ""'*|*'${'*|*[Yy][Oo][Uu][Rr][-_*]*|*[Xx][Xx][Xx]*)
      continue
      ;;
  esac

  case "$file|$normalized" in
    source/tests/open_source_installer/source_bundle_test.sh'|assert_scan_rejects '*)
      continue
      ;;
    source/config/config_test.go'|api_key = "sk-primary"'|\
    source/config/config_test.go'|api_key = "sk-backup"'|\
    source/config/config_test.go'|api_key = "sk-shared"'|\
    source/config/config_test.go'|api_key = "key123"'|\
    source/config/config_test.go'|api_key = "key-a"'|\
    source/config/config_test.go'|api_key = "key-b"'|\
    source/config/config_test.go'|app_secret = "old_feishu_secret"'|\
    source/config/config_test.go'|app_secret = "old_lark_secret"'|\
    source/config/config_test.go'|app_secret = "old_secret"'|\
    source/config/config_test.go'|app_secret = "sec_keep"'|\
    source/config/config_test.go'|app_secret = "test"'|\
    source/config/config_test.go'|app_secret = "y"'|\
    source/config/config_test.go'|bot_secret = "secret_old"')
      continue
      ;;
  esac

  printf 'public bundle contains non-placeholder secret assignment: %s\n' \
    "$assignment" >&2
  exit 1
done <"$secret_matches"

bot_id_matches=$scan_root/bot-id-matches
grep_status=0
grep -r -n -E \
  "(^|[,{][[:space:]]*)bot_id[[:space:]]*=[[:space:]]*['\"]aib[A-Za-z0-9_-]{12,}['\"]" \
  "$bundle" >"$bot_id_matches" 2>"$scan_root/bot-id-errors" || grep_status=$?
case "$grep_status" in
  0) ;;
  1) ;;
  *)
    cat "$scan_root/bot-id-errors" >&2
    printf 'public bundle BotID scan failed\n' >&2
    exit 1
    ;;
esac

while IFS= read -r assignment; do
  relative=${assignment#"$bundle"/}
  file=${relative%%:*}
  remainder=${relative#*:}
  content=${remainder#*:}
  normalized=$(printf '%s\n' "$content" | sed \
    -e 's/^[[:space:]]*//' \
    -e 's/[[:space:]]*$//')
  case "$file|$normalized" in
    source/tests/open_source_installer/source_bundle_test.sh'|assert_scan_rejects '*)
      continue
      ;;
  esac
  printf 'public bundle contains a likely real WeCom BotID: %s\n' \
    "$assignment" >&2
  exit 1
done <"$bot_id_matches"
