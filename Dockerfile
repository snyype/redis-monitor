# Build stage.
FROM golang:1.24-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO is not needed — the whole binary is stdlib plus go-redis — so a static build
# can run on scratch.
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/redis-monitor ./cmd/redis-monitor

# Run stage. The UI is embedded in the binary, so there is nothing else to copy.
FROM alpine:3.20

RUN adduser -D -u 10001 monitor \
 && mkdir -p /data \
 && chown monitor:monitor /data

COPY --from=build /out/redis-monitor /usr/local/bin/redis-monitor

USER monitor
WORKDIR /data

# The recorded trend lives here. Mount a volume if the series should outlive the
# container.
ENV REDIS_MONITOR_DATA_DIR=/data
ENV HTTP_ADDR=:8088

EXPOSE 8088

# Liveness only: readiness (/readyz) reports on Redis, and a monitor must not be
# restarted merely because the server it watches is down.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8088/healthz >/dev/null || exit 1

ENTRYPOINT ["redis-monitor"]
