FROM golang:1.26-alpine AS build
WORKDIR /src
# BuildKit cache mounts keep rebuilds fast and the build layer small:
# the module cache survives across builds (no re-download), and the
# go-build cache speeds recompiles without bloating the final image.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build go mod download
COPY . .
# VERSION is injected from the build host (docker-compose passes
# `git describe --tags`); the .git dir is excluded from the build context
# so it cannot be derived here. Matches GoReleaser's -X main.version.
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/freebucks-proxy ./backend/cmd/freebucks-proxy

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 1000 app \
    && adduser -S -u 1000 -G app app \
    && mkdir -p /app/data /app/dump /app/logs \
    && chown -R app:app /app
WORKDIR /app
COPY --from=build /out/freebucks-proxy /usr/local/bin/freebucks-proxy
USER app
EXPOSE 3457
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=10s CMD wget -qO- http://127.0.0.1:3457/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/freebucks-proxy"]
