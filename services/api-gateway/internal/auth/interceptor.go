package auth

import (
	"context"
	"errors"
	"strings"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// healthCheckPrefix is exempted from auth so container/orchestrator health
// probes (registered separately in cmd/main.go) keep working unauthenticated.
const healthCheckPrefix = "/grpc.health.v1.Health/"

// adminServicePrefix is exempted from this per-CLIENT interceptor because it
// has its own, separate admin-key check (see gateway.AdminHandler), not a
// per-client x-api-key — an operator provisioning a brand-new client cannot,
// by definition, already hold that client's key.
const adminServicePrefix = "/sentinel.gateway.v1.AdminService/"

// UnaryServerInterceptor validates the caller's API key on every RPC except
// health checks, and injects the verified client_id into the request context.
//
//   - apiKeyHeader    — gRPC metadata key carrying the API key (config: "x-api-key")
//   - clientIDHeader  — gRPC metadata key the rate limiter reads for its bucket
//     key (config: rate_limiting.identity_header, typically "x-client-id").
//     This interceptor overwrites it with the VERIFIED client_id so the
//     existing rate limiter — which otherwise trusts this header blindly —
//     becomes authenticated for free, with no changes to it.
func UnaryServerInterceptor(cache *Cache, apiKeyHeader, clientIDHeader string, log *zap.Logger) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		if strings.HasPrefix(info.FullMethod, healthCheckPrefix) ||
			strings.HasPrefix(info.FullMethod, adminServicePrefix) {
			return handler(ctx, req)
		}

		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing request metadata")
		}

		vals := md.Get(apiKeyHeader)
		if len(vals) == 0 || vals[0] == "" {
			return nil, status.Errorf(codes.Unauthenticated, "missing %s", apiKeyHeader)
		}

		clientID, err := cache.Lookup(ctx, Hash(vals[0]))
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, status.Error(codes.Unauthenticated, "invalid or inactive api key")
			}
			log.Error("auth backing store unavailable", zap.Error(err))
			return nil, status.Error(codes.Unavailable, "auth temporarily unavailable")
		}

		ctx = WithClientID(ctx, clientID)

		// Rewrite (never trust the caller's own value for) the identity
		// header the rate limiter keys off, so it reads the verified
		// client_id instead of a self-declared, unverified value.
		newMD := md.Copy()
		newMD.Set(clientIDHeader, clientID)
		ctx = metadata.NewIncomingContext(ctx, newMD)

		return handler(ctx, req)
	}
}
