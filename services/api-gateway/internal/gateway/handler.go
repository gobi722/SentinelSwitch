package gateway

import (
	"context"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sentinelswitch/api-gateway/internal/hashing"
	"github.com/sentinelswitch/api-gateway/internal/idempotency"
	"github.com/sentinelswitch/api-gateway/internal/kafka"
	"github.com/sentinelswitch/api-gateway/internal/ratelimit"
	gatewayv1 "github.com/sentinelswitch/proto/gateway/v1"
	transactionv1 "github.com/sentinelswitch/proto/transaction/v1"
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

// NewHandler wires all dependencies together.
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

// SubmitTransaction validates, deduplicates, hashes PAN, and publishes the
// transaction event to Kafka.
//
// Response codes:
//   - OK (200)         — accepted for processing; txn_id returned
//   - ALREADY_EXISTS   — duplicate idempotency_key; original txn_id returned
//   - INVALID_ARGUMENT — validation failed
//   - RESOURCE_EXHAUSTED — rate-limit exceeded
//   - INTERNAL         — unexpected server error
func (h *Handler) SubmitTransaction(
	ctx context.Context,
	req *gatewayv1.SubmitTransactionRequest,
) (*gatewayv1.SubmitTransactionResponse, error) {
	// 1. Rate limit
	if !h.rateLimiter.Allow(ctx) {
		return nil, status.Error(codes.ResourceExhausted, "rate limit exceeded")
	}

	// 2. Validate
	if err := h.validator.ValidateSubmit(req); err != nil {
		return nil, err
	}

	// 3. Hash PAN before doing anything else
	cardHash := h.hasher.Hash(req.Pan)
	maskedPAN := h.hasher.MaskedPAN(req.Pan)

	// 4. Idempotency check
	txnID := uuid.NewString()
	original, isDuplicate, err := h.idempotency.CheckAndSet(ctx, req.IdempotencyKey, txnID)
	if err != nil {
		h.log.Error("idempotency store error", zap.Error(err))
		return nil, status.Error(codes.Internal, "internal error")
	}
	if isDuplicate {
		h.log.Info("duplicate idempotency key",
			zap.String("idempotency_key", req.IdempotencyKey),
			zap.String("original_txn_id", original),
		)
		return &gatewayv1.SubmitTransactionResponse{
			TxnId:     original,
			Duplicate: true,
		}, status.Error(codes.AlreadyExists, "duplicate request")
	}

	// 5. Build Kafka event (PAN never leaves this method)
	event := &transactionv1.TransactionEvent{
		TxnId:                    txnID,
		IdempotencyKey:           req.IdempotencyKey,
		CardHash:                 cardHash,
		MaskedPan:                maskedPAN,
		ExpiryMonth:              req.ExpiryMonth,
		ExpiryYear:               req.ExpiryYear,
		AmountMinor:              req.AmountMinor,
		CurrencyCode:             req.CurrencyCode,
		TransactionType:          req.TransactionType,
		Channel:                  req.Channel,
		MerchantCategoryCode:     req.MerchantCategoryCode,
		TerminalId:               req.TerminalId,
		MerchantId:               req.MerchantId,
		RetrievalReferenceNumber: req.RetrievalReferenceNumber,
		ReceivedAt:               timestamppb.New(time.Now().UTC()),
	}

	// 6. Publish — keyed on card_hash for partition locality
	if err := h.producer.Publish(ctx, cardHash, event); err != nil {
		h.log.Error("kafka publish failed",
			zap.String("txn_id", txnID),
			zap.Error(err),
		)
		return nil, status.Error(codes.Internal, "failed to enqueue transaction")
	}

	h.log.Info("transaction accepted",
		zap.String("txn_id", txnID),
		zap.String("masked_pan", maskedPAN),
		zap.String("currency", req.CurrencyCode),
		zap.Int64("amount_minor", req.AmountMinor),
	)

	return &gatewayv1.SubmitTransactionResponse{TxnId: txnID}, nil
}

// GetTransactionStatus is a stub — status queries are served by the
// transaction-processor service.  The gateway delegates via gRPC forwarding
// or a BFF layer; this endpoint exists for direct client compatibility.
func (h *Handler) GetTransactionStatus(
	ctx context.Context,
	req *gatewayv1.GetTransactionStatusRequest,
) (*gatewayv1.GetTransactionStatusResponse, error) {
	if req.TxnId == "" {
		return nil, status.Error(codes.InvalidArgument, "txn_id: required")
	}
	// Downstream delegation would happen here.
	return nil, status.Error(codes.Unimplemented, "status queries must be directed to the transaction-processor service")
}
