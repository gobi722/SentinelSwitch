# ---------------------------------------------------------------------------
# Build context MUST be the repo root, not this directory — go.mod's
# `replace github.com/sentinelswitch/proto => ../proto-gen` reaches outside
# services/fraud-engine/. Build with:
#   docker build -f services/fraud-engine/Dockerfile -t sentinel-fraud-engine .
# ---------------------------------------------------------------------------

# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /workspace

# Shared proto module — sibling of the service dir, matching the relative
# ../proto-gen path in go.mod's replace directive.
COPY services/proto-gen ./proto-gen

WORKDIR /workspace/fraud-engine

COPY services/fraud-engine/go.mod services/fraud-engine/go.sum ./
RUN go mod download

COPY services/fraud-engine/. .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /fraud-engine ./cmd/main.go

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /fraud-engine /fraud-engine

# Default config path (override via CONFIG_PATH env or volume). main.go's
# default CONFIG_PATH ("../../config/fraud-engine.yaml") resolves from "/"
# (this image's default WORKDIR) to exactly this path. fraud-rules.yaml is
# also needed — rules.config_file defaults to "config/fraud-rules.yaml",
# which resolves the same way from "/".
COPY config/fraud-engine.yaml /config/fraud-engine.yaml
COPY config/fraud-rules.yaml /config/fraud-rules.yaml

USER 65532:65532

EXPOSE 9095 8082

ENTRYPOINT ["/fraud-engine"]
