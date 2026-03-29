# ---------------------------------------------------------------------------
# Build stage
# ---------------------------------------------------------------------------
FROM golang:1.23-alpine AS builder

# Install git for private module fetching (if any) and CA certs
RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /workspace

# Copy go.mod / go.sum first to cache dependency downloads separately
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source
COPY . .

# Build a static binary.
# CGO_ENABLED=0 removes the libc dependency.
# ldflags trim the binary size and embed version info from build args.
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
      -ldflags="-s -w \
        -X main.version=${VERSION} \
        -X main.commit=${COMMIT} \
        -X main.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      -o /api-gateway \
      ./cmd/main.go

# ---------------------------------------------------------------------------
# Runtime stage
# ---------------------------------------------------------------------------
FROM scratch

# Pull in timezone data and certs from builder
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Drop binary into an empty image
COPY --from=builder /api-gateway /api-gateway

# Default config path (override via CONFIG_PATH env or volume)
COPY config/api-gateway.yaml /config/api-gateway.yaml

# gRPC
EXPOSE 9090
# Metrics / health
EXPOSE 9091

# Run as non-root (UID 65532 = "nonroot" convention used with distroless/scratch)
USER 65532:65532

ENTRYPOINT ["/api-gateway"]
