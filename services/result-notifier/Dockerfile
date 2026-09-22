# ---------------------------------------------------------------------------
# Build context MUST be the repo root, not this directory — go.mod's
# `replace github.com/sentinelswitch/proto => ../proto-gen` reaches outside
# services/result-notifier/. Build with:
#   docker build -f services/result-notifier/Dockerfile -t sentinel-result-notifier .
# ---------------------------------------------------------------------------

# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /workspace

# Shared proto module — sibling of the service dir, matching the relative
# ../proto-gen path in go.mod's replace directive.
COPY services/proto-gen ./proto-gen

WORKDIR /workspace/result-notifier

COPY services/result-notifier/go.mod services/result-notifier/go.sum ./
RUN go mod download

COPY services/result-notifier/. .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /result-notifier ./cmd/main.go

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /result-notifier /result-notifier

# Default config path (override via CONFIG_PATH env or volume). main.go's
# default CONFIG_PATH ("../../config/result-notifier.yaml") resolves from
# "/" (this image's default WORKDIR) to exactly this path.
COPY config/result-notifier.yaml /config/result-notifier.yaml

USER 65532:65532

EXPOSE 9098 8085

ENTRYPOINT ["/result-notifier"]
