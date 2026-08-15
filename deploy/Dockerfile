# Grimoire Notes — container image
#   docker build -t grimoire-notes -f deploy/Dockerfile .
#   docker run -p 9111:9111 -v grimoire-vault:/vault grimoire-notes
#
# Configuration is entirely env-driven (see README "Config"): GRIMOIRE_VAULT,
# GRIMOIRE_PORT/HOST, GRIMOIRE_AUTH_TOKEN, GRIMOIRE_OLLAMA_URL, GRIMOIRE_SYNC_*…
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go/go.mod go/go.sum ./go/
RUN cd go && go mod download
COPY go ./go
# CGO_ENABLED=0 keeps this a static binary: the SQLite driver is pure Go, so
# the runtime image needs no libc and no toolchain
RUN cd go \
    && CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/grimoire ./cmd/grimoire \
    && CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/grimoire-mcp ./cmd/grimoire-mcp

FROM alpine:3.20
WORKDIR /app
COPY --from=build /out/grimoire /out/grimoire-mcp /usr/local/bin/
COPY web ./web
COPY plugins ./plugins

# run as a non-root user; the vault volume is the only writable path it needs
RUN adduser -D -u 1000 grimoire \
    && mkdir /vault && chown grimoire:grimoire /vault
USER grimoire

ENV GRIMOIRE_VAULT=/vault \
    GRIMOIRE_PORT=9111 \
    GRIMOIRE_HOST=0.0.0.0 \
    GRIMOIRE_WEB_DIR=/app/web \
    GRIMOIRE_PLUGIN_DIR=/app/plugins
VOLUME /vault
EXPOSE 9111

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
  CMD wget -qO- "http://127.0.0.1:${GRIMOIRE_PORT}/api/health" >/dev/null || exit 1

CMD ["grimoire"]
