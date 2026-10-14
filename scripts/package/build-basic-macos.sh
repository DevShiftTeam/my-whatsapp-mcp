#!/bin/sh
# Offline BASIC candidate packaging; no native build or application execution.
set -eu
task_package_python="${WA_INDEXER_PACKAGE_PYTHON:-/usr/bin/python3}"
case "$task_package_python" in /*) ;; *) echo "Absolute developer Python path required." >&2; exit 2 ;; esac
task_package_scripts=$(/usr/bin/dirname "$0")
exec "$task_package_python" "$task_package_scripts/build-basic-macos.py" "$@"
