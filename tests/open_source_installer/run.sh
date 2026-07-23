#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
sh "$script_dir/source_bundle_test.sh"
sh "$script_dir/bootstrap_test.sh"
sh "$script_dir/setup_test.sh"
sh "$script_dir/one_command_test.sh"
