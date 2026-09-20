# ---------------------------------------------------------------------------
# Build context MUST be the repo root, not this directory — go.mod's
# `replace github.com/sentinelswitch/proto => ../proto-gen` reaches outside
# services/persistence-svc/. Build with:
#   docker build -f services/persistence-svc/Dockerfile -t sentinel-persistence-svc .
# ---------------------------------------------------------------------------

# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /workspace

# Shared proto module — sibling of the service dir, matching the relative
# ../proto-gen path in go.mod's replace directive.
COPY services/proto-gen ./proto-gen

WORKDIR /workspace/persistence-svc

COPY services/persistence-svc/go.mod services/persistence-svc/go.sum ./
RUN go mod download

COPY services/persistence-svc/. .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /persistence-svc ./cmd/main.go

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /persistence-svc /persistence-svc

USER 65532:65532

EXPOSE 9093 8083

ENTRYPOINT ["/persistence-svc"]
