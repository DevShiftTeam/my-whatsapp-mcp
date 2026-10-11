#!/bin/sh
set -eu
umask 077
root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd -P)
sh "$root/scripts/app/build.sh"
exec "$root/bin/wa-indexer" serve "$@"
