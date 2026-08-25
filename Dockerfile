# Build stage.
#
# Pinned to BUILDPLATFORM on purpose: the builder always runs natively and Go
# cross-compiles to TARGETOS/TARGETARCH itself. Letting the builder run under QEMU
# instead would emulate the entire Go toolchain, which turns a multi-arch build
# from seconds into many minutes for no benefit — the output is identical.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG BUILT_AT=unknown

# CGO is not needed — the whole binary is stdlib plus go-redis — so a static build
# runs on any base image. The version is stamped in so the binary can say which
# build it is.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.builtAt=${BUILT_AT}" \
      -o /out/redis-monitor ./cmd/redis-monitor

# scratch has no shell and no adduser, so the non-root ownership is set up here,
# in the builder, and copied over verbatim.
RUN mkdir -p /out/data && chown -R 10001:10001 /out/data

# Run stage. The UI is embedded in the binary, the binary is CGO-free and talks to
# Redis over plain TCP (no TLS, no timezone lookups), so nothing from a distro
# base — libc, CA bundle, tzdata, a shell — is actually needed at runtime.
FROM scratch

COPY --from=build --chown=10001:10001 /out/data /data
COPY --from=build /out/redis-monitor /usr/local/bin/redis-monitor

# Numeric UID: no /etc/passwd entry is required for USER to work.
USER 10001
WORKDIR /data

# The recorded trend lives here. Mount a volume if the series should outlive the
# container.
ENV REDIS_MONITOR_DATA_DIR=/data
ENV HTTP_ADDR=:8088

EXPOSE 8088

# Liveness only: readiness (/readyz) reports on Redis, and a monitor must not be
# restarted merely because the server it watches is down. There is no wget in
# this image, so the binary checks itself instead of shelling out to one.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD ["/usr/local/bin/redis-monitor", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/redis-monitor"]

# Labels last, so changing them cannot invalidate any build cache above.
ARG VERSION
ARG COMMIT
LABEL org.opencontainers.image.title="Redis Monitor" \
      org.opencontainers.image.description="Standalone Redis key browser, metrics dashboard and connected-clients view" \
      org.opencontainers.image.source="https://github.com/snyype/redis-monitor" \
      org.opencontainers.image.licenses="NOASSERTION" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"
