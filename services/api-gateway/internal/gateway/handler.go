package gateway

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sentinelswitch/api-gateway/internal/hashing"
	"github.com/sentinelswitch/api-gateway/internal/idempotency"
	"github.com/sentinelswitch/api-gateway/internal/kafka"
	"github.com/sentinelswitch/api-gateway/internal/ratelimit"
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
	log         *zap.Logger
}

func NewHandler(
	v *Validator,
	h *hashing.Hasher,
	ids *idempotency.Store,
	p *kafka.Producer,
	rl *ratelimit.Limiter,
	log *zap.Logger,
) *Handler {
	return &Handler{
		validator:   v,
		hasher:      h,
		idempotency: ids,
		producer:    p,
		rateLimiter: rl,
		log:         log,
	}
}

func (h *Handler) SubmitTransaction(
	ctx context.Context,
	req *gatewayv1.TransactionRequest,
) (*gatewayv1.TransactionAck, error) {

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
	return nil, status.Error(codes.Unimplemented, "status queries must be directed to the transaction-processor service")
}