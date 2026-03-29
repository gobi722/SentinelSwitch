// Package kafka wraps the segmentio/kafka-go writer and provides a
// Confluent-compatible Schema Registry wire-format framing for Protobuf
// messages.
//
// Wire format (Confluent):
//   byte 0         : magic byte 0x00
//   bytes 1-4      : schema_id (big-endian int32)
//   bytes 5+       : Protobuf serialised payload
//
// Schema lookup is lazy: the schema ID is fetched once on the first Publish
// call and cached for the lifetime of the producer.
package kafka

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"
	"google.golang.org/protobuf/proto"
)

const confluentMagicByte = 0x00

// Producer publishes protobuf messages to a single Kafka topic using the
// Confluent wire format.
type Producer struct {
	writer         *kafka.Writer
	schemaSubject  string
	schemaRegistry string
	httpClient     *http.Client

	mu       sync.Mutex
	schemaID int32
	fetched  bool
}

// ProducerConfig holds the configuration needed to build a Producer.
type ProducerConfig struct {
	Brokers           string
	Topic             string
	Acks              string // "all", "1", "0"
	EnableIdempotence bool
	Retries           int
	DeliveryTimeoutMs int
	LingerMs          int
	BatchSizeBytes    int
	CompressionType   string
	ClientID          string
	SchemaRegistryURL string
	SchemaSubject     string
}

// New creates a Producer.
func New(cfg ProducerConfig) *Producer {
	requiredAcks := kafka.RequireAll
	switch cfg.Acks {
	case "1":
		requiredAcks = kafka.RequireOne
	case "0":
		requiredAcks = kafka.RequireNone
	}

	var comp kafka.Compression
	switch cfg.CompressionType {
	case "snappy":
		comp = kafka.Snappy
	case "lz4":
		comp = kafka.Lz4
	case "zstd":
		comp = kafka.Zstd
	default:
		comp = kafka.Gzip
	}

	w := &kafka.Writer{
		Addr:                   kafka.TCP(cfg.Brokers),
		Topic:                  cfg.Topic,
		Balancer:               &kafka.LeastBytes{},
		RequiredAcks:           kafka.RequiredAcks(requiredAcks),
		MaxAttempts:            cfg.Retries + 1,
		WriteTimeout:           time.Duration(cfg.DeliveryTimeoutMs) * time.Millisecond,
		BatchTimeout:           time.Duration(cfg.LingerMs) * time.Millisecond,
		BatchBytes:             int64(cfg.BatchSizeBytes),
		Compression:            comp,
		AllowAutoTopicCreation: false,
		Async:                  false,
	}

	return &Producer{
		writer:         w,
		schemaSubject:  cfg.SchemaSubject,
		schemaRegistry: cfg.SchemaRegistryURL,
		httpClient: &http.Client{
			Timeout: 3 * time.Second,
		},
	}
}

// Publish serialises msg using the Confluent wire format and writes it to Kafka.
// The key is used for partition routing (typically the hashed PAN or txn_id).
func (p *Producer) Publish(ctx context.Context, key string, msg proto.Message) error {
	schemaID, err := p.getSchemaID(ctx)
	if err != nil {
		return fmt.Errorf("kafka: fetch schema id: %w", err)
	}

	payload, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("kafka: marshal proto: %w", err)
	}

	frame := make([]byte, 5+len(payload))
	frame[0] = confluentMagicByte
	binary.BigEndian.PutUint32(frame[1:5], uint32(schemaID))
	copy(frame[5:], payload)

	return p.writer.WriteMessages(ctx, kafka.Message{
		Key:   []byte(key),
		Value: frame,
	})
}

// Close flushes and closes the underlying writer.
func (p *Producer) Close() error {
	return p.writer.Close()
}

// ---------------------------------------------------------------------------
// Schema Registry
// ---------------------------------------------------------------------------

type schemaResponse struct {
	ID int32 `json:"id"`
}

// getSchemaID fetches the schema ID from the Confluent Schema Registry.
// The result is cached after the first successful fetch.
func (p *Producer) getSchemaID(ctx context.Context) (int32, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.fetched {
		return p.schemaID, nil
	}

	url := fmt.Sprintf("%s/subjects/%s/versions/latest", p.schemaRegistry, p.schemaSubject)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.schemaregistry.v1+json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("schema registry unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("schema registry HTTP %d: %s", resp.StatusCode, body)
	}

	var sr schemaResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return 0, fmt.Errorf("schema registry decode: %w", err)
	}

	p.schemaID = sr.ID
	p.fetched = true
	return p.schemaID, nil
}
