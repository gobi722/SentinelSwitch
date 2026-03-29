package server

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	riskpb "github.com/sentinelswitch/proto/risk/v1"
	"github.com/sentinelswitch/risk-service/internal/scoring"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	requestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sentinel_risk_requests_total",
			Help: "Total number of risk scoring requests partitioned by decision.",
		},
		[]string{"decision"},
	)

	scoreHistogram = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "sentinel_risk_score_histogram",
			Help:    "Distribution of risk scores.",
			Buckets: []float64{100, 200, 300, 400, 500, 600, 700, 800, 900, 1000},
		},
	)
)

// Handler implements the gRPC RiskService.
type Handler struct {
	riskpb.UnimplementedRiskServiceServer
	scorer *scoring.Scorer
	log    *zap.Logger
}

// NewHandler constructs a Handler with the given scorer and logger.
func NewHandler(scorer *scoring.Scorer, log *zap.Logger) *Handler {
	return &Handler{scorer: scorer, log: log}
}

// CalculateRisk scores a transaction and returns the risk decision.
func (h *Handler) CalculateRisk(ctx context.Context, req *riskpb.RiskRequest) (*riskpb.RiskResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request must not be nil")
	}
	if req.TxnId == "" {
		return nil, status.Error(codes.InvalidArgument, "txn_id must not be empty")
	}
	if req.Features == nil {
		return nil, status.Error(codes.InvalidArgument, "features must not be nil")
	}

	result := h.scorer.Score(req.Features, req.AmountMinor)

	var decision riskpb.Decision
	switch result.Decision {
	case "APPROVE":
		decision = riskpb.Decision_APPROVE
	case "DECLINE":
		decision = riskpb.Decision_DECLINE
	default:
		decision = riskpb.Decision_REVIEW
	}

	requestsTotal.WithLabelValues(result.Decision).Inc()
	scoreHistogram.Observe(float64(result.Score))

	h.log.Debug("risk scored",
		zap.String("txn_id", req.TxnId),
		zap.Int32("score", result.Score),
		zap.String("decision", result.Decision),
	)

	return &riskpb.RiskResponse{
		RiskScore:      result.Score,
		Decision:       decision,
		TriggeredRules: result.ContribFactors,
	}, nil
}
