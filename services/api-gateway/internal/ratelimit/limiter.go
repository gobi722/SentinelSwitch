// Package ratelimit provides a per-client token-bucket rate limiter built on
// golang.org/x/time/rate.
//
// The client identity is extracted from a configurable gRPC metadata header
// (default: "x-client-id").  If the header is absent the caller's peer address
// is used as the key so that unknown callers are still limited.
//
// Limiter entries that have not been used for cleanupInterval are evicted to
// avoid unbounded growth.
package ratelimit

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

const (
	cleanupInterval = 5 * time.Minute
	entryIdleLimit  = 10 * time.Minute
)

// Limiter holds per-client rate limiters.
type Limiter struct {
	mu             sync.Mutex
	entries        map[string]*entry
	rps            rate.Limit
	burst          int
	identityHeader string
}

type entry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// New creates a Limiter.
//   - rps            — sustained requests per second per client
//   - burst          — maximum burst size
//   - identityHeader — gRPC metadata key used to identify the caller
func New(rps int, burst int, identityHeader string) *Limiter {
	l := &Limiter{
		entries:        make(map[string]*entry),
		rps:            rate.Limit(rps),
		burst:          burst,
		identityHeader: identityHeader,
	}
	go l.cleanup()
	return l
}

// Allow returns true if the caller identified from ctx is within its rate limit.
// Call this at the top of each handler.
func (l *Limiter) Allow(ctx context.Context) bool {
	key := l.clientKey(ctx)
	return l.limiterFor(key).Allow()
}

// Wait blocks until a token is available or ctx is cancelled.
// Returns an error if ctx expires before a token is granted.
func (l *Limiter) Wait(ctx context.Context) error {
	key := l.clientKey(ctx)
	return l.limiterFor(key).Wait(ctx)
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

func (l *Limiter) clientKey(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get(l.identityHeader); len(vals) > 0 && vals[0] != "" {
			return vals[0]
		}
	}
	if p, ok := peer.FromContext(ctx); ok {
		return p.Addr.String()
	}
	return "unknown"
}

func (l *Limiter) limiterFor(key string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.entries[key]
	if !ok {
		e = &entry{limiter: rate.NewLimiter(l.rps, l.burst)}
		l.entries[key] = e
	}
	e.lastSeen = time.Now()
	return e.limiter
}

func (l *Limiter) cleanup() {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for range ticker.C {
		l.mu.Lock()
		cutoff := time.Now().Add(-entryIdleLimit)
		for k, e := range l.entries {
			if e.lastSeen.Before(cutoff) {
				delete(l.entries, k)
			}
		}
		l.mu.Unlock()
	}
}
