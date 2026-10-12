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
env CGO_ENABLED=1 "${GO:-go}" build -tags fts5,sqlite_fts5,ts_omit_logtail -trimpath \
  -o bin/wa-indexer ./cmd/wa-indexer

mkdir -p bin/notices
cp LICENSE THIRD_PARTY_NOTICES bin/notices/

# Build/package-time acquisition only. This script produces a local developer
# build; it does not qualify a downloadable native application release.
task_helper_platform=$("${GO:-go}" env GOOS)/$("${GO:-go}" env GOARCH)
case "$task_helper_platform" in linux/amd64|linux/arm64|darwin/arm64) ;; *) echo "Cloudflare helper is unavailable for this platform; the local archive build remains usable." >&2; exit 0 ;; esac
task_helper_dir=$(mktemp -d "$root/bin/.cloudflared.XXXXXX")
trap 'rm -rf "$task_helper_dir"' EXIT HUP INT TERM
if [ -n "${WA_INDEXER_CLOUDFLARED_ASSET:-}" ]; then
  "${PYTHON:-python3}" scripts/package/verify_cloudflared.py packaging/cloudflared-manifest.json "$task_helper_platform" "$task_helper_dir/cloudflared" --asset "$WA_INDEXER_CLOUDFLARED_ASSET" --probe
else
  "${PYTHON:-python3}" scripts/package/verify_cloudflared.py packaging/cloudflared-manifest.json "$task_helper_platform" "$task_helper_dir/cloudflared" --probe
fi
mv "$task_helper_dir/cloudflared" bin/cloudflared
mkdir -p bin/notices/cloudflared
cp -R packaging/cloudflared-notices/. bin/notices/cloudflared/
cp packaging/cloudflared-manifest.json bin/notices/cloudflared/manifest.json
