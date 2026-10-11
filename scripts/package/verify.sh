#!/bin/sh
# Static audit only. Runtime/clean-machine checks remain separate explicit gates.
set -eu
if [ "$#" -ne 1 ]; then
  echo "Usage: verify.sh ABSOLUTE_APP_BUNDLE" >&2
  exit 2
fi
task_package_python="${WA_INDEXER_PACKAGE_PYTHON:-/usr/bin/python3}"
case "$task_package_python" in /*) ;; *) echo "Absolute developer Python path required." >&2; exit 2 ;; esac
task_package_scripts=$(/usr/bin/dirname "$0")
exec "$task_package_python" "$task_package_scripts/verify_macos.py" "$@"
