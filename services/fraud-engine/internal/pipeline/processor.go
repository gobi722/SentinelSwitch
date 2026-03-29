package pipeline

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	kafkago "github.com/segmentio/kafka-go"
	"github.com/sentinelswitch/fraud-engine/internal/circuitbreaker"
	"github.com/sentinelswitch/fraud-engine/internal/config"
	"github.com/sentinelswitch/fraud-engine/internal/features"
	riskClient "github.com/sentinelswitch/fraud-engine/internal/risk"
	"github.com/sentinelswitch/fraud-engine/internal/rules"
	"github.com/sentinelswitch/fraud-engine/internal/velocity"
	fraudpb "github.com/sentinelswitch/proto/fraud/v1"
	riskpb "github.com/sentinelswitch/proto/risk/v1"
	transactionspb "github.com/sentinelswitch/proto/transactions/v1"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"github.com/redis/go-redis/v9"
)

// Prometheus metrics
var (
	messagesProcessed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fraud_engine_messages_processed_total",
		Help: "Total transaction messages processed by decision.",
	}, []string{"decision"})

	processingDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "fraud_engine_processing_duration_seconds",
		Help:    "End-to-end processing duration per message.",
		Buckets: prometheus.DefBuckets,
	})

	riskCallErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fraud_engine_risk_call_errors_total",
		Help: "Risk service call errors, including circuit-open events.",
	})
)

// Processor consumes TransactionEvents from Kafka, runs the fraud pipeline, and
// publishes FraudResultEvents.
type Processor struct {
	cfg        *config.Config
	reader     *kafkago.Reader
	writer     *kafkago.Writer
	engine     *rules.Engine
	velStore   *velocity.Store
	merchantDB *redis.Client
	risk       *riskClient.Client
	cb         *circuitbreaker.Breaker
	logger     *zap.Logger

	schemaID   uint32
	schemaErr  error
	schemaOnce sync.Once

	sem chan struct{} // concurrency limiter for risk calls
}

// NewProcessor wires all dependencies into a Processor.
func NewProcessor(
	cfg *config.Config,
	engine *rules.Engine,
	velStore *velocity.Store,
	merchantDB *redis.Client,
	risk *riskClient.Client,
	cb *circuitbreaker.Breaker,
	logger *zap.Logger,
) (*Processor, error) {
	reader := kafkago.NewReader(kafkago.ReaderConfig{
		Brokers:        cfg.Kafka.Brokers,
		GroupID:        cfg.Kafka.Consumer.GroupID,
		Topic:          cfg.Kafka.Consumer.Topic,
		MinBytes:       1,
		MaxBytes:       10 << 20,
		CommitInterval: 0, // manual commit
	})

	writer := &kafkago.Writer{
		Addr:         kafkago.TCP(cfg.Kafka.Brokers...),
		Topic:        cfg.Kafka.Producer.Topic,
		Balancer:     &kafkago.LeastBytes{},
		RequiredAcks: kafkago.RequireAll,
		Async:        false,
	}

	return &Processor{
		cfg:        cfg,
		reader:     reader,
		writer:     writer,
		engine:     engine,
		velStore:   velStore,
		merchantDB: merchantDB,
		risk:       risk,
		cb:         cb,
		logger:     logger,
		sem:        make(chan struct{}, cfg.Concurrency.MaxConcurrentRisk),
	}, nil
}

// Run starts the consumer loop; blocks until ctx is cancelled.
func (p *Processor) Run(ctx context.Context) error {
	workerSem := make(chan struct{}, p.cfg.Concurrency.WorkerPoolSize)
	var wg sync.WaitGroup

	for {
		msg, err := p.reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
				break
			}
			p.logger.Error("kafka: fetch error", zap.Error(err))
			continue
		}

		workerSem <- struct{}{}
		wg.Add(1)
		go func(m kafkago.Message) {
			defer func() { <-workerSem; wg.Done() }()
			if err := p.handle(ctx, m); err != nil {
				p.logger.Error("pipeline: handle error",
					zap.Error(err),
					zap.Int64("offset", m.Offset),
				)
			}
			// Always commit to avoid poison-pill replay.
			if err := p.reader.CommitMessages(ctx, m); err != nil {
				p.logger.Warn("kafka: commit error", zap.Error(err))
			}
		}(msg)
	}

	wg.Wait()
	_ = p.reader.Close()
	_ = p.writer.Close()
	return nil
}

// handle processes a single Kafka message through the full fraud pipeline.
func (p *Processor) handle(ctx context.Context, msg kafkago.Message) error {
	start := time.Now()

	// Step 1 — Deserialize TransactionEvent
	txn, err := deserializeTransaction(msg.Value)
	if err != nil {
		return fmt.Errorf("deserialize: %w", err)
	}

	// Step 2 — Merchant risk from Redis
	merchantRisk := p.getMerchantRisk(ctx, txn.MerchantId)

	// Step 3 — Rule engine
	triggeredRules := p.engine.Evaluate(txn)

	// Step 4 — Velocity checks
	velResult, err := p.velStore.Record(ctx, txn.CardHash, txn.TxnId, txn.MerchantId, txn.TerminalId, txn.AmountMinor)
	if err != nil {
		p.logger.Warn("velocity record failed, using zero values", zap.Error(err))
		velResult = &velocity.Result{}
	}

	// Step 5 — Build FraudFeatures
	ff := features.Build(txn, velResult, merchantRisk, triggeredRules)

	// Step 6 — Call Risk Service with circuit breaker
	riskScore, decision := p.callRiskService(ctx, txn, ff)

	// Step 7 — Map score to decision when not returned by Risk Service
	if decision == riskpb.Decision_DECISION_UNSPECIFIED {
		decision = p.scoreToDecision(riskScore)
	}

	// Step 8 — Build FraudResultEvent
	triggeredStrs := make([]string, len(triggeredRules))
	for i, r := range triggeredRules {
		triggeredStrs[i] = string(r)
	}

	processedAt := time.Now().UTC().Format(time.RFC3339)

	result := &fraudpb.FraudResultEvent{
		TxnId:           txn.TxnId,
		TxnTimestamp:    txn.TxnTimestamp,
		ProcessedAt:     processedAt,
		CardHash:        txn.CardHash,
		MaskedPan:       txn.MaskedPan,
		AmountMinor:     txn.AmountMinor,
		Currency:        txn.Currency,
		MerchantId:      txn.MerchantId,
		TerminalId:      txn.TerminalId,
		Mcc:             txn.Mcc,
		TransactionType: fraudpb.TransactionType(txn.TransactionType),
		Channel:         fraudpb.Channel(txn.Channel),
		Scheme:          fraudpb.Scheme(txn.Scheme),
		Rrn:             txn.Rrn,
		Decision:        fraudpb.Decision(decision),
		RiskScore:       riskScore,
		TriggeredRules:  triggeredStrs,
	}

	// Step 9 — Serialize and publish to fraud_results topic
	payload, err := p.serializeFraudResult(ctx, result)
	if err != nil {
		return fmt.Errorf("serialize fraud result: %w", err)
	}

	if err := p.writer.WriteMessages(ctx, kafkago.Message{
		Key:   []byte(txn.CardHash), // partition by card_hash (consistent with design)
		Value: payload,
	}); err != nil {
		return fmt.Errorf("kafka write: %w", err)
	}

	// Step 10 — Offset committed by the outer goroutine in Run().

	dur := time.Since(start).Seconds()
	processingDuration.Observe(dur)
	messagesProcessed.WithLabelValues(decision.String()).Inc()

	p.logger.Info("txn processed",
		zap.String("txn_id", txn.TxnId),
		zap.String("decision", decision.String()),
		zap.Int32("risk_score", riskScore),
		zap.Strings("triggered_rules", triggeredStrs),
		zap.Float64("duration_s", dur),
	)

	return nil
}

// getMerchantRisk retrieves the pre-computed merchant risk score from Redis.
// Defaults to 0.5 (neutral) when the key is absent or unparseable.
func (p *Processor) getMerchantRisk(ctx context.Context, merchantID string) float64 {
	val, err := p.merchantDB.Get(ctx, "merchant_risk:"+merchantID).Result()
	if err != nil {
		return 0.5
	}
	score, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return 0.5
	}
	return score
}

// callRiskService calls the Risk Service with circuit-breaker protection.
// Returns (fallbackScore, DECISION_UNSPECIFIED) when the circuit is open or
// the call fails.
func (p *Processor) callRiskService(
	ctx context.Context,
	txn *transactionspb.TransactionEvent,
	ff *riskpb.FraudFeatures,
) (int32, riskpb.Decision) {
	p.sem <- struct{}{}
	defer func() { <-p.sem }()

	if err := p.cb.Allow(ctx); err != nil {
		riskCallErrors.Inc()
		p.logger.Warn("circuit open — using fallback score", zap.String("txn_id", txn.TxnId))
		return p.cb.FallbackRiskScore(), riskpb.Decision_DECISION_UNSPECIFIED
	}

	req := &riskpb.RiskRequest{
		TxnId:           txn.TxnId,
		CardHash:        txn.CardHash,
		AmountMinor:     txn.AmountMinor,
		Currency:        txn.Currency,
		MerchantId:      txn.MerchantId,
		Mcc:             txn.Mcc,
		TransactionType: riskpb.TransactionType(txn.TransactionType),
		Channel:         riskpb.Channel(txn.Channel),
		Features:        ff,
	}

	resp, err := p.risk.CalculateRisk(ctx, req)
	if err != nil {
		riskCallErrors.Inc()
		p.cb.RecordFailure(ctx)
		p.logger.Warn("risk call failed — using fallback score",
			zap.String("txn_id", txn.TxnId),
			zap.Error(err),
		)
		return p.cb.FallbackRiskScore(), riskpb.Decision_DECISION_UNSPECIFIED
	}

	p.cb.RecordSuccess(ctx)
	return resp.RiskScore, resp.Decision
}

// scoreToDecision maps a numeric risk score to a Decision enum using thresholds
// from config.
func (p *Processor) scoreToDecision(score int32) riskpb.Decision {
	switch {
	case score < p.cfg.DecisionThresholds.ApproveBelow:
		return riskpb.Decision_APPROVE
	case score > p.cfg.DecisionThresholds.DeclineAbove:
		return riskpb.Decision_DECLINE
	default:
		return riskpb.Decision_REVIEW
	}
}

// deserializeTransaction decodes a Confluent wire-format payload into a
// TransactionEvent. Wire format: [0x00][4-byte schema_id][proto bytes].
func deserializeTransaction(raw []byte) (*transactionspb.TransactionEvent, error) {
	if len(raw) < 5 {
		return nil, fmt.Errorf("message too short: %d bytes", len(raw))
	}
	if raw[0] != 0x00 {
		return nil, fmt.Errorf("unexpected Confluent magic byte: 0x%02x", raw[0])
	}
	// raw[1:5] = schema_id (ignored on consume side)
	var txn transactionspb.TransactionEvent
	if err := proto.Unmarshal(raw[5:], &txn); err != nil {
		return nil, fmt.Errorf("proto unmarshal TransactionEvent: %w", err)
	}
	return &txn, nil
}

// serializeFraudResult encodes a FraudResultEvent into Confluent wire format.
// The schema ID is fetched once from the Schema Registry and cached.
func (p *Processor) serializeFraudResult(ctx context.Context, result *fraudpb.FraudResultEvent) ([]byte, error) {
	p.schemaOnce.Do(func() {
		id, err := p.fetchSchemaID(ctx, "fraud-results-value")
		if err != nil {
			p.schemaErr = fmt.Errorf("fetch schema id: %w", err)
			return
		}
		p.schemaID = id
	})
	if p.schemaErr != nil {
		return nil, p.schemaErr
	}

	protoBytes, err := proto.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("proto marshal FraudResultEvent: %w", err)
	}

	out := make([]byte, 5+len(protoBytes))
	out[0] = 0x00
	binary.BigEndian.PutUint32(out[1:5], p.schemaID)
	copy(out[5:], protoBytes)
	return out, nil
}

// schemaRegistryResponse is the minimal JSON returned by Confluent Schema Registry.
type schemaRegistryResponse struct {
	ID int `json:"id"`
}

// fetchSchemaID queries the Schema Registry for the latest version of subject.
func (p *Processor) fetchSchemaID(ctx context.Context, subject string) (uint32, error) {
	url := fmt.Sprintf("%s/subjects/%s/versions/latest",
		p.cfg.Kafka.Producer.SchemaRegistryURL, subject)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}

	httpCli := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpCli.Do(req)
	if err != nil {
		return 0, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("schema registry returned HTTP %d for %s", resp.StatusCode, subject)
	}

	var srResp schemaRegistryResponse
	if err := json.NewDecoder(resp.Body).Decode(&srResp); err != nil {
		return 0, fmt.Errorf("decode schema registry response: %w", err)
	}
	if srResp.ID <= 0 {
		return 0, fmt.Errorf("schema registry returned invalid id %d for %s", srResp.ID, subject)
	}

	p.logger.Info("schema registry: schema id cached",
		zap.String("subject", subject),
		zap.Int("schema_id", srResp.ID),
	)
	return uint32(srResp.ID), nil
}
