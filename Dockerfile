# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
# Independent build-time helper layer: application source changes reuse it.
# No helper download, package manager, shell or Python in the runtime images.
FROM --platform=$BUILDPLATFORM golang:1.27.1-trixie@sha256:8f58fd67ea075142d947a60e0caa4317746a55118d312f027793d382c7741734 AS cloudflared
ARG TARGETOS
ARG TARGETARCH
# The pinned builder already supplies Python 3 and CA roots; only the helper
# and its notices cross into runtime.
WORKDIR /opt/cloudflared
COPY packaging/cloudflared-manifest.json ./manifest.json
COPY scripts/package/verify_cloudflared.py ./verify.py
RUN mkdir -p /out && python3 verify.py /opt/cloudflared/manifest.json "$TARGETOS/$TARGETARCH" /out/cloudflared
COPY packaging/cloudflared-notices/ /out/notices/cloudflared/
COPY packaging/cloudflared-manifest.json /out/notices/cloudflared/manifest.json

FROM golang:1.27.1-trixie@sha256:8f58fd67ea075142d947a60e0caa4317746a55118d312f027793d382c7741734 AS build
ARG TARGETARCH
ARG TARGETOS
ARG BUILD_CACHE_SCOPE=default
ENV CGO_ENABLED=1 GOTOOLCHAIN=go1.27.1
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,id=wa-indexer-modules-${BUILD_CACHE_SCOPE},target=/go/pkg/mod go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY LICENSE THIRD_PARTY_NOTICES ./
RUN --mount=type=cache,id=wa-indexer-modules-${BUILD_CACHE_SCOPE},target=/go/pkg/mod --mount=type=cache,id=wa-indexer-go1.27.1-${TARGETOS}-${TARGETARCH}-${BUILD_CACHE_SCOPE},target=/root/.cache/go-build     go build -tags sqlite_fts5,ts_omit_logtail -trimpath -ldflags='-s -w' -o /out/wa-indexer ./cmd/wa-indexer
RUN --mount=type=cache,id=wa-indexer-modules-${BUILD_CACHE_SCOPE},target=/go/pkg/mod mkdir -p /private/var/lib/wa-indexer/data /private/var/lib/wa-indexer/demo &&     chmod 0700 /private/var/lib/wa-indexer /private/var/lib/wa-indexer/data /private/var/lib/wa-indexer/demo &&     chown -R 10001:10001 /private/var/lib/wa-indexer &&     mkdir -p /out/notices && cp LICENSE THIRD_PARTY_NOTICES /out/notices/ &&     go version > /out/notices/compiler.txt && dpkg-query -W libc6 gcc > /out/notices/build-packages.txt && \
    go list -m all > /out/notices/go-modules.txt && \
    go list -m -f '{{.Dir}}' all | while IFS= read -r dir; do \
      if [ -n "$dir" ]; then dest="/out/notices/modules/${dir#/go/pkg/mod/}"; mkdir -p "$dest"; \
      find "$dir" -type f \( -iname 'license*' -o -iname 'copying*' -o -iname 'notice*' -o -iname 'patents*' \) -exec sh -c 'base=$1; dest=$2; shift 2; for src do rel=${src#"$base"/}; target="$dest/$rel"; mkdir -p "${target%/*}"; cp "$src" "$target"; done' sh "$dir" "$dest" '{}' \;; fi; \
    done

# Developer-only helpers: never part of either application runtime.
FROM build AS harness-build
COPY scripts/docker/helper/ ./scripts/docker/helper/
RUN --mount=type=cache,id=wa-indexer-modules-${BUILD_CACHE_SCOPE},target=/go/pkg/mod --mount=type=cache,id=wa-indexer-helper-go1.27.1-${TARGETOS}-${TARGETARCH},target=/root/.cache/go-build     CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/fs-helper ./scripts/docker/helper/fs
FROM gcr.io/distroless/base-nossl-debian13:nonroot@sha256:8c563c1fb5e120606f0d85733049775faed6192e2bd2223ef283a5393eec22b9 AS harness
COPY --from=harness-build /out/fs-helper /fs-helper
USER 10001:10001
ENTRYPOINT ["/fs-helper"]

# Compatibility benchmark: exactly the same application binary and private roots.
FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f AS slim
ARG SOURCE_REVISION=unknown
LABEL org.opencontainers.image.revision=$SOURCE_REVISION org.wa-indexer.toolchain="go1.27.1" org.wa-indexer.build.gcc="4:14.2.0-1" org.wa-indexer.runtime.libc6="2.41-12+deb13u4" org.wa-indexer.runtime="debian13-slim" org.wa-indexer.qualification="source-build-preview"
COPY --from=build /out/wa-indexer /usr/local/bin/wa-indexer
COPY --from=cloudflared /out/cloudflared /usr/local/bin/cloudflared
COPY --from=cloudflared /out/notices/ /usr/share/doc/wa-indexer/
COPY --from=build /out/notices/ /usr/share/doc/wa-indexer/
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=10001:10001 /private/var/lib/ /var/lib/
USER 10001:10001
EXPOSE 8123
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=3s --start-period=30s --start-interval=1s CMD ["/usr/local/bin/wa-indexer", "health", "--data-dir", "/var/lib/wa-indexer/data", "--connect", "127.0.0.1:8123"]
ENTRYPOINT ["/usr/local/bin/wa-indexer"]

FROM gcr.io/distroless/base-nossl-debian13:nonroot@sha256:8c563c1fb5e120606f0d85733049775faed6192e2bd2223ef283a5393eec22b9 AS runtime
ARG SOURCE_REVISION=unknown
LABEL org.opencontainers.image.revision=$SOURCE_REVISION org.wa-indexer.toolchain="go1.27.1" org.wa-indexer.build.gcc="4:14.2.0-1" org.wa-indexer.runtime.libc6="2.41-12+deb13u4" org.wa-indexer.runtime="distroless-base-nossl-debian13" org.wa-indexer.qualification="source-build-preview"
COPY --from=build /out/wa-indexer /usr/local/bin/wa-indexer
COPY --from=cloudflared /out/cloudflared /usr/local/bin/cloudflared
COPY --from=cloudflared /out/notices/ /usr/share/doc/wa-indexer/
COPY --from=build /out/notices/ /usr/share/doc/wa-indexer/
COPY --from=build --chown=10001:10001 /private/var/lib/ /var/lib/
USER 10001:10001
EXPOSE 8123
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=3s --start-period=30s --start-interval=1s CMD ["/usr/local/bin/wa-indexer", "health", "--data-dir", "/var/lib/wa-indexer/data", "--connect", "127.0.0.1:8123"]
ENTRYPOINT ["/usr/local/bin/wa-indexer"]
