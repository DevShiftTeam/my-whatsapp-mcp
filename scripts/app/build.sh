#!/bin/sh
set -eu
umask 077
root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd -P)
cd "$root"
test "$#" -eq 0 || exit 2
mkdir -p bin
case "$(uname -s)" in
  Darwin)
    MACOSX_DEPLOYMENT_TARGET=${MACOSX_DEPLOYMENT_TARGET:-14.0}
    CGO_CFLAGS="${CGO_CFLAGS:--O2 -g} -mmacosx-version-min=$MACOSX_DEPLOYMENT_TARGET"
    CGO_CXXFLAGS="${CGO_CXXFLAGS:--O2 -g} -mmacosx-version-min=$MACOSX_DEPLOYMENT_TARGET"
    CGO_LDFLAGS="${CGO_LDFLAGS:--O2 -g} -mmacosx-version-min=$MACOSX_DEPLOYMENT_TARGET"
    export MACOSX_DEPLOYMENT_TARGET CGO_CFLAGS CGO_CXXFLAGS CGO_LDFLAGS
    ;;
esac
exec env CGO_ENABLED=1 "${GO:-go}" build -tags fts5,sqlite_fts5 -trimpath \
  -o bin/wa-indexer ./cmd/wa-indexer
