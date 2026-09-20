# ---------------------------------------------------------------------------
# Build context MUST be the repo root, not this directory — go.mod's
# `replace github.com/sentinelswitch/proto => ../proto-gen` reaches outside
# services/risk-service/. Build with:
#   docker build -f services/risk-service/Dockerfile -t sentinel-risk-service .
# ---------------------------------------------------------------------------

# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /workspace

# Shared proto module — sibling of the service dir, matching the relative
# ../proto-gen path in go.mod's replace directive.
COPY services/proto-gen ./proto-gen

WORKDIR /workspace/risk-service

COPY services/risk-service/go.mod services/risk-service/go.sum ./
RUN go mod download

COPY services/risk-service/. .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /risk-service ./cmd/main.go

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /risk-service /risk-service

USER 65532:65532

EXPOSE 50052 9094 8084

ENTRYPOINT ["/risk-service"]
