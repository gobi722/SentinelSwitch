// Package pipeline consumes FraudResultEvents from the internal fraud_results
// topic and republishes each one, unmodified, to a per-client external topic
// (results.<client_id>) so external callers can consume only their own
// results — never anyone else's.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	kafkago "github.com/segmentio/kafka-go"
	fraudpb "github.com/sentinelswitch/proto/fraud/v1"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"github.com/sentinelswitch/result-notifier/internal/config"
	kafkapkg "github.com/sentinelswitch/result-notifier/internal/kafka"
	"github.com/sentinelswitch/result-notifier/internal/router"
)

var (
	messagesRouted = promauto.NewCounter(prometheus.CounterOpts{
		Name: "sentinel_result_notifier_messages_routed_total",
		Help: "Total fraud results successfully republished to a client-specific topic.",
		// Deliberately NOT labeled by client_id — an unbounded/growing tenant
		// count would blow up Prometheus label cardinality. Per-client volume
		// is answered via Kafka consumer lag on each results.<client_id>
		// topic, not a Prometheus label.
	})

	messagesUnrouted = promauto.NewCounter(prometheus.CounterOpts{
		Name: "sentinel_result_notifier_unrouted_total",
		Help: "Total fraud results that could not be routed (bad client_id or publish failure) and were sent to the unrouted DLQ.",
	})
)

// Processor consumes fraud_results and republishes to per-client topics.
type Processor struct {
	cfg    *config.Config
	reader *kafkago.Reader
	writer *kafkago.Writer
	router *router.Router
	dlq    *kafkapkg.DLQProducer
	logger *zap.Logger
}

// New wires all dependencies into a Processor.
func New(cfg *config.Config, rtr *router.Router, dlq *kafkapkg.DLQProducer, logger *zap.Logger) *Processor {
	reader := kafkago.NewReader(kafkago.ReaderConfig{
		Brokers:        cfg.Kafka.Brokers,
		GroupID:        cfg.Kafka.Consumer.GroupID,
		Topic:          cfg.Kafka.Consumer.Topic,
		MinBytes:       1,
		MaxBytes:       10 << 20,
		CommitInterval: 0, // manual commit
	})

	// Topic intentionally left empty: each WriteMessages call sets
	// kafka.Message.Topic per-message, since the destination varies by
	// client_id.
	writer := &kafkago.Writer{
		Addr:         kafkago.TCP(cfg.Kafka.Brokers...),
		Balancer:     &kafkago.LeastBytes{},
		RequiredAcks: kafkago.RequireAll,
		Async:        false,
	}

	return &Processor{
		cfg:    cfg,
		reader: reader,
		writer: writer,
		router: rtr,
		dlq:    dlq,
		logger: logger,
	}
}

// Run starts the consumer loop; blocks until ctx is cancelled.
func (p *Processor) Run(ctx context.Context) error {
	for {
		msg, err := p.reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
				break
			}
			p.logger.Error("kafka: fetch error", zap.Error(err))
			continue
		}

		// Commit only after the message is either successfully routed or
		// safely parked in the unrouted DLQ — never on an unhandled error.
		// Dropping a paying integrator's result silently is worse than a
		// bounded retry-then-DLQ, so (unlike fraud-engine) this does not
		// always-commit.
		if err := p.handle(ctx, msg); err != nil {
			p.logger.Error("pipeline: handle error, will retry on redelivery",
				zap.Error(err),
				zap.Int64("offset", msg.Offset),
			)
			continue
		}

		if err := p.reader.CommitMessages(ctx, msg); err != nil {
			p.logger.Warn("kafka: commit error", zap.Error(err))
		}
	}

	_ = p.reader.Close()
	_ = p.writer.Close()
	return nil
}

// handle routes a single fraud_results message to its client-specific topic,
// or to the unrouted DLQ if it can't be routed or published.
func (p *Processor) handle(ctx context.Context, msg kafkago.Message) error {
	event, err := deserializeFraudResult(msg.Value)
	if err != nil {
		p.logger.Warn("undecodable message — routing to unrouted DLQ",
			zap.Error(err), zap.Int64("offset", msg.Offset))
		return p.toDLQ(ctx, msg, "decode_error")
	}

	topic, err := p.router.ResolveTopic(event.ClientId)
	if err != nil {
		p.logger.Warn("unroutable client_id — routing to unrouted DLQ",
			zap.String("txn_id", event.TxnId), zap.Error(err))
		return p.toDLQ(ctx, msg, "unroutable_client_id")
	}

	if err := p.publishWithRetry(ctx, topic, msg); err != nil {
		p.logger.Error("publish exhausted retries — routing to unrouted DLQ",
			zap.String("txn_id", event.TxnId), zap.String("topic", topic), zap.Error(err))
		return p.toDLQ(ctx, msg, "publish_error")
	}

	messagesRouted.Inc()
	p.logger.Info("fraud result routed",
		zap.String("txn_id", event.TxnId),
		zap.String("client_id", event.ClientId),
		zap.String("topic", topic),
	)
	return nil
}

// publishWithRetry republishes the original (unmodified) message bytes to
// topic, retrying with exponential backoff per cfg.Retry.
func (p *Processor) publishWithRetry(ctx context.Context, topic string, msg kafkago.Message) error {
	delay := time.Duration(p.cfg.Retry.InitialDelayMs) * time.Millisecond
	maxDelay := time.Duration(p.cfg.Retry.MaxDelayMs) * time.Millisecond

	var lastErr error
	for attempt := 1; attempt <= p.cfg.Retry.MaxAttempts; attempt++ {
		err := p.writer.WriteMessages(ctx, kafkago.Message{
			Topic: topic,
			Key:   msg.Key,
			Value: msg.Value,
		})
		if err == nil {
			return nil
		}
		lastErr = err

		if attempt < p.cfg.Retry.MaxAttempts {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
			delay = time.Duration(float64(delay) * p.cfg.Retry.Multiplier)
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
	return fmt.Errorf("publish to %s: %w", topic, lastErr)
}

// toDLQ sends the original message to the unrouted DLQ and, on success,
// signals the caller to commit the original offset (returns nil). If the DLQ
// publish itself fails, the error propagates so the message is retried on
// redelivery rather than silently lost.
func (p *Processor) toDLQ(ctx context.Context, msg kafkago.Message, reason string) error {
	if err := p.dlq.Publish(ctx, msg, reason, 0); err != nil {
		return fmt.Errorf("dlq publish: %w", err)
	}
	messagesUnrouted.Inc()
	return nil
}

// deserializeFraudResult decodes a Confluent wire-format payload into a
// FraudResultEvent. Wire format: [0x00][4-byte schema_id][proto bytes].
//
// Only used to read client_id for routing — the original raw bytes are
// forwarded unmodified, so no re-serialization or Schema Registry access is
// needed on the publish side.
func deserializeFraudResult(raw []byte) (*fraudpb.FraudResultEvent, error) {
	if len(raw) < 5 {
		return nil, fmt.Errorf("message too short: %d bytes", len(raw))
	}
	if raw[0] != 0x00 {
		return nil, fmt.Errorf("unexpected Confluent magic byte: 0x%02x", raw[0])
	}
	var event fraudpb.FraudResultEvent
	if err := proto.Unmarshal(raw[5:], &event); err != nil {
		return nil, fmt.Errorf("proto unmarshal FraudResultEvent: %w", err)
	}
	return &event, nil
}
