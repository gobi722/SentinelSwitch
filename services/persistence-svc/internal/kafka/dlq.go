package kafka

import (
	"context"
	"strconv"
	"time"

	"github.com/segmentio/kafka-go"
)

// DLQProducer publishes failed messages to a dead-letter-queue topic with
// diagnostic headers.
type DLQProducer struct {
	writer *kafka.Writer
}

// NewDLQProducer constructs a DLQProducer for the given topic.
func NewDLQProducer(brokers []string, topic string) *DLQProducer {
	return &DLQProducer{
		writer: &kafka.Writer{
			Addr:         kafka.TCP(brokers...),
			Topic:        topic,
			Balancer:     &kafka.LeastBytes{},
			RequiredAcks: kafka.RequireAll,
			Async:        false,
		},
	}
}

// Publish sends originalMsg to the DLQ topic with diagnostic headers.
func (d *DLQProducer) Publish(
	ctx context.Context,
	originalMsg kafka.Message,
	failureReason string,
	retryCount int,
) error {
	headers := []kafka.Header{
		{Key: "dlq-original-topic", Value: []byte("fraud_results")},
		{Key: "dlq-original-partition", Value: []byte(strconv.Itoa(originalMsg.Partition))},
		{Key: "dlq-original-offset", Value: []byte(strconv.FormatInt(originalMsg.Offset, 10))},
		{Key: "dlq-failure-reason", Value: []byte(failureReason)},
		{Key: "dlq-retry-count", Value: []byte(strconv.Itoa(retryCount))},
		{Key: "dlq-failed-at", Value: []byte(time.Now().UTC().Format(time.RFC3339))},
	}

	return d.writer.WriteMessages(ctx, kafka.Message{
		Key:     originalMsg.Key,
		Value:   originalMsg.Value,
		Headers: headers,
	})
}

// Close flushes pending writes and closes the underlying Kafka writer.
func (d *DLQProducer) Close() error {
	return d.writer.Close()
}
