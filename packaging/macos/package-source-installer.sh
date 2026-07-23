#!/bin/sh
set -eu

umask 022

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd -P)
output=${1:-}
[ -n "$output" ] || {
  printf 'usage: %s OUTPUT\n' "$0" >&2
  exit 2
}
case "$output" in
  /*) output_path=$output ;;
  *) output_path=$repo_root/$output ;;
esac
[ "$(basename -- "$output_path")" = cc-connect-source-install ] || {
  printf 'refusing unexpected output path: %s\n' "$output_path" >&2
  exit 2
}

if ! git -C "$repo_root" diff --quiet --ignore-submodules -- ||
   ! git -C "$repo_root" diff --cached --quiet --ignore-submodules --; then
  printf 'refusing to package a dirty tracked/index state\n' >&2
  exit 1
fi

commit=$(git -C "$repo_root" rev-parse HEAD)
source_epoch=$(git -C "$repo_root" show -s --format=%ct HEAD)
build_time=$(date -u -r "$source_epoch" '+%Y-%m-%dT%H:%M:%SZ')

output_parent=$(dirname -- "$output_path")
mkdir -p "$output_parent"
stage=$(mktemp -d "$output_parent/.cc-connect-source-installer.XXXXXX")
backup_path=
swap_started=0
swap_committed=0
cleanup() {
  status=$?
  trap - EXIT HUP INT TERM

  if [ "$status" -ne 0 ] && [ "$swap_started" -eq 1 ] && \
     [ "$swap_committed" -eq 0 ] && \
     { [ -e "$backup_path" ] || [ -L "$backup_path" ]; }; then
    can_restore=1
    if [ -e "$output_path" ] || [ -L "$output_path" ]; then
      if ! rm -rf "$output_path"; then can_restore=0; fi
    fi
    if [ "$can_restore" -eq 1 ] && ! mv "$backup_path" "$output_path"; then
      can_restore=0
    fi
    if [ "$can_restore" -eq 0 ]; then
      printf 'failed to restore previous bundle from %s\n' "$backup_path" >&2
      status=1
    fi
  fi

  rm -rf "$stage" || status=1
  exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

bundle=$stage/bundle
source_dir=$bundle/source
mkdir -p "$source_dir"

git -C "$repo_root" archive --format=tar HEAD \
  ':(exclude).superpowers' \
  ':(exclude)docs/superpowers' | tar -xf - -C "$source_dir"

install -m 755 \
  "$repo_root/packaging/macos/setup.sh" \
  "$repo_root/packaging/macos/bootstrap.sh" \
  "$repo_root/packaging/macos/install.sh" \
  "$repo_root/packaging/macos/doctor.sh" \
  "$repo_root/packaging/macos/uninstall.sh" \
  "$repo_root/packaging/macos/lib.sh" \
  "$bundle/"

install -m 644 "$repo_root/LICENSE" "$bundle/LICENSE"
install -m 644 "$repo_root/LICENSE" "$source_dir/LICENSE"
install -m 644 "$repo_root/packaging/macos/README.zh-CN.md" "$bundle/README.md"
install -m 644 "$repo_root/config.example.toml" "$bundle/config.example.toml"
install -m 644 "$repo_root/packaging/macos/go-toolchains.txt" "$bundle/go-toolchains.txt"
printf '%s\n' \
  'version=v1.0.2' \
  "commit=$commit" \
  'go_version=go1.26.5' \
  "source_date_epoch=$source_epoch" \
  "build_time=$build_time" >"$bundle/VERSION"
chmod 644 "$bundle/VERSION"

"$repo_root/packaging/macos/scan-public-bundle.sh" "$bundle"

(
  cd "$bundle"
  find . -type f ! -name checksums.txt -print | LC_ALL=C sort | while IFS= read -r file; do
    shasum -a 256 "$file"
  done >checksums.txt
)

if [ -e "$output_path" ] || [ -L "$output_path" ]; then
  backup_path=$(mktemp -d "$output_parent/.cc-connect-source-installer.previous.XXXXXX")
  rmdir "$backup_path"
  swap_started=1
  mv "$output_path" "$backup_path"
fi
mv "$bundle" "$output_path"
swap_committed=1
if [ "$swap_started" -eq 1 ]; then
  rm -rf "$backup_path"
  swap_started=0
fi
printf 'Created %s\n' "$output_path"
