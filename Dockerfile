# ── Build stage ────────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

WORKDIR /build

# Download deps first for better layer caching.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false -ldflags="-s -w" -o miroxy ./cmd/miroxy

# ── Runtime stage ──────────────────────────────────────────────────────────────
FROM alpine:3.21

# CA certs for upstream TLS connections.
RUN apk add --no-cache ca-certificates && \
    addgroup -S miroxy && adduser -S -G miroxy miroxy && \
    mkdir -p /app/config /data && \
    chown -R miroxy:miroxy /app /data /var/log

WORKDIR /app
COPY --from=builder /build/miroxy /app/miroxy

# /data is used by the CCR persistent store (bbolt) and log files.
VOLUME ["/data"]

# proxy port | admin port (localhost-only in production — bind via compose/k8s)
EXPOSE 8080 8090

# Liveness probe: hits the lightweight /health endpoint on the proxy port.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://localhost:8080/health || exit 1

USER miroxy

ENTRYPOINT ["/app/miroxy", "serve"]
# Default config resolves to /app/config/config.yaml (WORKDIR + serve -c default).
# Override: docker run ... miroxy serve -c /etc/miroxy/config.yaml
