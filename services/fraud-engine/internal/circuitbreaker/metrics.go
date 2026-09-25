package circuitbreaker

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// These were documented (docs/PROJECT_OVERVIEW.md §4 Case 2) as if already
// implemented, but no metric was ever actually wired up — nothing here
// existed until this file. Only the two explicit state transitions are
// counted (CLOSED->OPEN in RecordFailure, HALF_OPEN->CLOSED in
// RecordSuccess); OPEN->HALF_OPEN happens implicitly via Redis TTL expiry
// with no code path to hook, so it isn't counted as a discrete event.
var (
	stateTransitionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "sentinel_cb_state_transitions_total",
		Help: "Circuit breaker state transitions, partitioned by from_state and to_state.",
	}, []string{"from_state", "to_state"})

	fallbackDecisionsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "sentinel_cb_fallback_decisions_total",
		Help: "Total times a fallback risk score was used because the circuit was open or the Risk Service call failed.",
	})

	openDurationSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "sentinel_cb_open_duration_seconds",
		Help: "How long the circuit has been continuously OPEN, in seconds. 0 when not OPEN.",
	})
)
