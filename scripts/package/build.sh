#!/bin/sh
# Developer tooling: assemble audited prebuilt inputs, never native-build/download.
set -eu
if [ "$#" -ne 3 ]; then
  echo "Usage: build.sh AUDITED_MANIFEST INPUT_ROOT NEW_OUTPUT.app" >&2
  exit 2
fi
task_package_python="${WA_INDEXER_PACKAGE_PYTHON:-/usr/bin/python3}"
case "$task_package_python" in /*) ;; *) echo "Absolute developer Python path required." >&2; exit 2 ;; esac
task_package_scripts=$(/usr/bin/dirname "$0")
exec "$task_package_python" "$task_package_scripts/assemble_macos.py" "$@"
