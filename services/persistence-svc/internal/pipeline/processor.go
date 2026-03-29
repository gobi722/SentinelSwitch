package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	kafkago "github.com/segmentio/kafka-go"
	fraudpb "github.com/sentinelswitch/proto/fraud/v1"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	kafkapkg "github.com/sentinelswitch/persistence-svc/internal/kafka"
	"github.com/sentinelswitch/persistence-svc/internal/store"
)

var (
	upsertsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "sentinel_persistence_upserts_total",
		Help: "Total upsert operations partitioned by status (success/retry/dlq).",
	}, []string{"status"})

	batchSizeHistogram = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "sentinel_persistence_batch_size_histogram",
		Help:    "Distribution of batch sizes processed by the pipeline.",
		Buckets: prometheus.LinearBuckets(1, 50, 11),
	})
)

// Config holds tunable parameters for the Processor.
type Config struct {
	MaxBatchSize    int
	FlushIntervalMs int
	MaxAttempts     int
	InitialDelayMs  int
	Multiplier      float64
	MaxDelayMs      int
}

// Processor is the core Kafka → PostgreSQL batch pipeline.
type Processor struct {
	reader *kafkago.Reader
	store  *store.Store
	dlq    *kafkapkg.DLQProducer
	cfg    Config
	log    *zap.Logger
}

// New constructs a Processor.
func New(
	reader *kafkago.Reader,
	store *store.Store,
	dlq *kafkapkg.DLQProducer,
	cfg Config,
	log *zap.Logger,
) *Processor {
	return &Processor{reader: reader, store: store, dlq: dlq, cfg: cfg, log: log}
}

// Run starts the batch-processing loop. It returns when ctx is cancelled.
func (p *Processor) Run(ctx context.Context) {
	flushInterval := time.Duration(p.cfg.FlushIntervalMs) * time.Millisecond
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	var (
		batch  []kafkago.Message
		events []*fraudpb.FraudResultEvent
	)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		p.processBatch(ctx, batch, events)
		batch = batch[:0]
		events = events[:0]
	}

	for {
		fetchCtx, fetchCancel := context.WithTimeout(ctx, flushInterval)
		msg, err := p.reader.FetchMessage(fetchCtx)
		fetchCancel()

		if err != nil {
			if ctx.Err() != nil {
				p.log.Info("context cancelled — flushing remaining batch", zap.Int("size", len(batch)))
				flush()
				return
			}
			// Fetch timeout — ticker will handle flush
		} else {
			event, decodeErr := decodeConfluentProto(msg.Value)
			if decodeErr != nil {
				p.log.Warn("deserialization failure — routing to DLQ",
					zap.String("reason", decodeErr.Error()),
					zap.Int("partition", msg.Partition),
					zap.Int64("offset", msg.Offset),
				)
				if pubErr := p.dlq.Publish(ctx, msg, decodeErr.Error(), 0); pubErr != nil {
					p.log.Error("dlq publish failed for invalid message", zap.Error(pubErr))
				}
				upsertsTotal.WithLabelValues("dlq").Inc()
				if commitErr := p.reader.CommitMessages(ctx, msg); commitErr != nil {
					p.log.Error("failed to commit poison-pill offset", zap.Error(commitErr))
				}
			} else {
				batch = append(batch, msg)
				events = append(events, event)
			}

			if len(batch) >= p.cfg.MaxBatchSize {
				flush()
			}
		}

		select {
		case <-ticker.C:
			flush()
		default:
		}
	}
}

// processBatch upserts events into PostgreSQL with exponential-backoff retry.
// On exhausted retries every message is published to the DLQ.
func (p *Processor) processBatch(
	ctx context.Context,
	msgs []kafkago.Message,
	events []*fraudpb.FraudResultEvent,
) {
	if len(events) == 0 {
		return
	}

	batchSizeHistogram.Observe(float64(len(events)))

	var lastErr error
	attempt := 0

	err := p.withRetry(ctx, func() error {
		attempt++
		if upsertErr := p.store.UpsertBatch(ctx, events); upsertErr != nil {
			p.log.Warn("upsert batch failed",
				zap.Int("attempt", attempt),
				zap.Int("batch_size", len(events)),
				zap.Error(upsertErr),
			)
			upsertsTotal.WithLabelValues("retry").Inc()
			lastErr = upsertErr
			return upsertErr
		}
		return nil
	})

	if err != nil {
		p.log.Error("all retry attempts exhausted — routing batch to DLQ",
			zap.Int("batch_size", len(msgs)),
			zap.Error(lastErr),
		)
		reason := fmt.Sprintf("db_error after %d attempts: %s", p.cfg.MaxAttempts, lastErr)
		for _, msg := range msgs {
			if pubErr := p.dlq.Publish(ctx, msg, reason, p.cfg.MaxAttempts); pubErr != nil {
				p.log.Error("dlq publish failed", zap.Error(pubErr))
			}
			upsertsTotal.WithLabelValues("dlq").Inc()
		}
		if commitErr := p.reader.CommitMessages(ctx, msgs...); commitErr != nil {
			p.log.Error("failed to commit offsets after DLQ", zap.Error(commitErr))
		}
		return
	}

	upsertsTotal.WithLabelValues("success").Add(float64(len(events)))
	if commitErr := p.reader.CommitMessages(ctx, msgs...); commitErr != nil {
		p.log.Error("failed to commit offsets after successful upsert", zap.Error(commitErr))
	}
	p.log.Info("batch upserted and committed",
		zap.Int("batch_size", len(events)),
		zap.Int("attempts", attempt),
	)
}

// withRetry executes fn with exponential-backoff retry up to MaxAttempts times.
func (p *Processor) withRetry(ctx context.Context, fn func() error) error {
	delay := time.Duration(p.cfg.InitialDelayMs) * time.Millisecond
	var err error

	for attempt := 1; attempt <= p.cfg.MaxAttempts; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if attempt == p.cfg.MaxAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		next := time.Duration(float64(delay) * p.cfg.Multiplier)
		if maxDelay := time.Duration(p.cfg.MaxDelayMs) * time.Millisecond; next > maxDelay {
			next = maxDelay
		}
		delay = next
	}
	return err
}

// decodeConfluentProto strips the 5-byte Confluent Schema Registry wire-format
// header and unmarshals the remaining bytes as a FraudResultEvent.
//
// Wire format: [0x00][4-byte schema_id big-endian][proto payload]
func decodeConfluentProto(raw []byte) (*fraudpb.FraudResultEvent, error) {
	if len(raw) < 5 {
		return nil, fmt.Errorf("message too short (%d bytes)", len(raw))
	}
	if raw[0] != 0x00 {
		return nil, fmt.Errorf("invalid Confluent magic byte: 0x%02x", raw[0])
	}

	var event fraudpb.FraudResultEvent
	if err := proto.Unmarshal(raw[5:], &event); err != nil {
		return nil, fmt.Errorf("proto unmarshal FraudResultEvent: %w", err)
	}
	return &event, nil
}
