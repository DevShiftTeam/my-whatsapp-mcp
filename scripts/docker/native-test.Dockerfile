FROM golang:1.27.1-trixie@sha256:8f58fd67ea075142d947a60e0caa4317746a55118d312f027793d382c7741734 AS build
ENV CGO_ENABLED=1 GOTOOLCHAIN=go1.27.1
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY internal/ ./internal/
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,id=wa-indexer-native-tests,target=/root/.cache/go-build go test -race -c -o /appdata.test ./internal/appdata
FROM gcr.io/distroless/base-nossl-debian13:nonroot@sha256:8c563c1fb5e120606f0d85733049775faed6192e2bd2223ef283a5393eec22b9
COPY --from=build /appdata.test /appdata.test
USER 10001:10001
ENTRYPOINT ["/appdata.test"]
