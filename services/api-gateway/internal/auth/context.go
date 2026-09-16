package auth

import "context"

type contextKey int

const clientIDKey contextKey = 0

// WithClientID returns a new context carrying the verified caller identity.
func WithClientID(ctx context.Context, clientID string) context.Context {
	return context.WithValue(ctx, clientIDKey, clientID)
}

// ClientIDFromContext returns the verified client_id injected by the auth
// interceptor, if present.
func ClientIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(clientIDKey).(string)
	return v, ok
}
