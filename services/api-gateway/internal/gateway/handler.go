package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sentinelswitch/api-gateway/internal/auth"
	"github.com/sentinelswitch/api-gateway/internal/hashing"
	"github.com/sentinelswitch/api-gateway/internal/idempotency"
	"github.com/sentinelswitch/api-gateway/internal/kafka"
	"github.com/sentinelswitch/api-gateway/internal/ratelimit"
	"github.com/sentinelswitch/api-gateway/internal/txnstatus"
	gatewayv1 "github.com/sentinelswitch/proto/gateway/v1"
	transactionv1 "github.com/sentinelswitch/proto/transactions/v1"
)

// Prometheus metrics
var (
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "sentinel_gateway_requests_total",
		Help: "Total SubmitTransaction requests partitioned by outcome.",
	}, []string{"result"})

	requestDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "sentinel_gateway_request_duration_seconds",
		Help:    "SubmitTransaction handler duration.",
		Buckets: prometheus.DefBuckets,
	})

	idempotencyHitsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "sentinel_gateway_idempotency_hits_total",
		Help: "Total requests rejected as duplicates by the idempotency store.",
	})
)

// Handler implements gatewayv1.GatewayServiceServer.
type Handler struct {
	gatewayv1.UnimplementedGatewayServiceServer

	validator   *Validator
	hasher      *hashing.Hasher
	idempotency *idempotency.Store
	producer    *kafka.Producer
	rateLimiter *ratelimit.Limiter
	txnStatus   *txnstatus.Store
	log         *zap.Logger
}

func NewHandler(
	v *Validator,
	h *hashing.Hasher,
	ids *idempotency.Store,
	p *kafka.Producer,
	rl *ratelimit.Limiter,
	ts *txnstatus.Store,
	log *zap.Logger,
) *Handler {
	return &Handler{
		validator:   v,
		hasher:      h,
		idempotency: ids,
		producer:    p,
		rateLimiter: rl,
		txnStatus:   ts,
		log:         log,
	}
}

func (h *Handler) SubmitTransaction(
	ctx context.Context,
	req *gatewayv1.TransactionRequest,
) (*gatewayv1.TransactionAck, error) {

	// Verified caller identity, injected by auth.UnaryServerInterceptor.
	// Unreachable in practice (the interceptor rejects unauthenticated calls
	// before the handler runs) — defended here rather than trusted blindly.
	clientID, ok := auth.ClientIDFromContext(ctx)
	if !ok || clientID == "" {
		return nil, status.Error(codes.Internal, "missing authenticated client identity")
	}

	start := time.Now()
	result := "accepted"
	defer func() {
		requestsTotal.WithLabelValues(result).Inc()
		requestDuration.Observe(time.Since(start).Seconds())
	}()

	// 1. Rate limit
	if !h.rateLimiter.Allow(ctx) {
		result = "rate_limited"
		return nil, status.Error(codes.ResourceExhausted, "rate limit exceeded")
	}

	// 2. Validate
	if err := h.validator.ValidateSubmit(req); err != nil {
		result = "validation_error"
		return nil, err
	}

	// 3. Hash PAN — card_hash comes from client, we re-derive masked PAN
	cardHash := h.hasher.Hash(req.CardHash)
	maskedPAN := req.PanLast4

	// 4. Idempotency check
	txnID := uuid.NewString()
	original, isDuplicate, err := h.idempotency.CheckAndSet(ctx, req.IdempotencyKey, txnID)
	if err != nil {
		h.log.Error("idempotency store error", zap.Error(err))
		result = "internal_error"
		return nil, status.Error(codes.Internal, "internal error")
	}
	if isDuplicate {
		h.log.Info("duplicate idempotency key",
			zap.String("idempotency_key", req.IdempotencyKey),
			zap.String("original_txn_id", original),
		)
		result = "duplicate"
		idempotencyHitsTotal.Inc()
		return &gatewayv1.TransactionAck{
			TxnId:  original,
			Status: gatewayv1.TransactionStatus_PENDING,
		}, status.Error(codes.AlreadyExists, "duplicate request")
	}

	// 5. Build Kafka event
	now := time.Now().UTC()
	event := &transactionv1.TransactionEvent{
		TxnId:           txnID,
		TxnTimestamp:    now.Format(time.RFC3339),
		CardHash:        cardHash,
		MaskedPan:       maskedPAN,
		AmountMinor:     req.AmountMinor,
		Currency:        req.Currency,
		MerchantId:      req.MerchantId,
		TerminalId:      req.TerminalId,
		Mcc:             req.Mcc,
		TransactionType: transactionv1.TransactionType(req.TransactionType),
		Channel:         transactionv1.Channel(req.Channel),
		ClientId:        clientID,
	}

	// 6. Publish — keyed on card_hash for partition locality
	if err := h.producer.Publish(ctx, cardHash, event); err != nil {
		h.log.Error("kafka publish failed",
			zap.String("txn_id", txnID),
			zap.Error(err),
		)
		result = "internal_error"
		return nil, status.Error(codes.Internal, "failed to enqueue transaction")
	}

	h.log.Info("transaction accepted",
		zap.String("txn_id", txnID),
		zap.String("masked_pan", maskedPAN),
		zap.String("currency", req.Currency),
		zap.Int64("amount_minor", req.AmountMinor),
	)

	return &gatewayv1.TransactionAck{
		TxnId:       txnID,
		Status:      gatewayv1.TransactionStatus_PENDING,
		SubmittedAt: now.Format(time.RFC3339),
	}, nil
}

func (h *Handler) GetTransactionStatus(
	ctx context.Context,
	req *gatewayv1.StatusRequest,
) (*gatewayv1.TransactionStatusResponse, error) {
	if req.TxnId == "" {
		return nil, status.Error(codes.InvalidArgument, "txn_id: required")
	}

	clientID, ok := auth.ClientIDFromContext(ctx)
	if !ok {
		// Should be unreachable — the auth interceptor rejects unauthenticated
		// calls before the handler ever runs.
		return nil, status.Error(codes.Internal, "missing verified client identity")
	}

	row, err := h.txnStatus.Lookup(ctx, req.TxnId, clientID)
	if err != nil {
		if errors.Is(err, txnstatus.ErrNotFound) {
			// Deliberately identical response whether the txn_id is genuinely
			// unknown, still being processed (no row exists until Fraud Engine
			// decides), or belongs to a different client — distinguishing the
			// last case would leak another tenant's transaction existence.
			return &gatewayv1.TransactionStatusResponse{
				TxnId:  req.TxnId,
				Status: gatewayv1.TransactionStatus_PENDING,
			}, nil
		}
		h.log.Error("txn status lookup failed", zap.String("txn_id", req.TxnId), zap.Error(err))
		return nil, status.Error(codes.Unavailable, "status lookup temporarily unavailable")
	}

	resp := &gatewayv1.TransactionStatusResponse{
		TxnId:       req.TxnId,
		RiskScore:   row.RiskScore,
		SubmittedAt: row.SubmittedAt.UTC().Format(time.RFC3339),
	}
	switch row.Decision {
	case "APPROVED":
		resp.Status = gatewayv1.TransactionStatus_APPROVED
		resp.FraudDecision = "APPROVE"
	case "DECLINED":
		resp.Status = gatewayv1.TransactionStatus_DECLINED
		resp.FraudDecision = "DECLINE"
	case "REVIEW":
		resp.Status = gatewayv1.TransactionStatus_REVIEW
		resp.FraudDecision = "REVIEW"
	default:
		resp.Status = gatewayv1.TransactionStatus_ERROR
	}
	if !row.DecidedAt.IsZero() {
		resp.DecidedAt = row.DecidedAt.UTC().Format(time.RFC3339)
	}
	return resp, nil
}