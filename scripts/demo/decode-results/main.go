// decode-results — dev-only helper: consumes a per-client results.<client_id>
// topic on the SASL PUBLIC listener (localhost:9096) and pretty-prints each
// FraudResultEvent as JSON, by stripping the Confluent wire-format header
// (magic byte + 4-byte schema id + 1-byte message-index) before unmarshalling
// with the real generated proto type.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"time"

	fraudv1 "github.com/sentinelswitch/proto/fraud/v1"
	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl/scram"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func main() {
	broker := flag.String("broker", "localhost:9096", "Kafka PUBLIC listener")
	topic := flag.String("topic", "", "results.<client_id> topic to read")
	group := flag.String("group", "", "consumer group id (must start with <client_id>.)")
	username := flag.String("username", "", "SCRAM username (client_id)")
	password := flag.String("password", "", "SCRAM password")
	timeout := flag.Duration("timeout", 8*time.Second, "how long to wait for messages")
	flag.Parse()

	if *topic == "" || *group == "" || *username == "" || *password == "" {
		log.Fatal("usage: decode-results -topic results.<client_id> -group <client_id>.debug -username <client_id> -password <scram_password>")
	}

	mechanism, err := scram.Mechanism(scram.SHA256, *username, *password)
	if err != nil {
		log.Fatalf("scram mechanism: %v", err)
	}

	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:     []string{*broker},
		Topic:       *topic,
		GroupID:     *group,
		StartOffset: kafka.FirstOffset,
		Dialer: &kafka.Dialer{
			Timeout:       10 * time.Second,
			SASLMechanism: mechanism,
		},
	})
	defer reader.Close()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	marshaler := protojson.MarshalOptions{Multiline: true, Indent: "  ", EmitUnpopulated: true}

	count := 0
	for {
		m, err := reader.ReadMessage(ctx)
		if err != nil {
			break // context deadline: done draining the topic
		}
		event, decErr := decode(m.Value)
		if decErr != nil {
			n := len(m.Value)
			if n > 16 {
				n = 16
			}
			fmt.Printf("--- offset %d: decode error: %v (first bytes: %s) ---\n", m.Offset, decErr, hex.EncodeToString(m.Value[:n]))
			continue
		}
		out, _ := marshaler.Marshal(event)
		fmt.Printf("--- offset %d ---\n%s\n", m.Offset, out)
		count++
	}
	fmt.Printf("\ndecoded %d message(s) from %s\n", count, *topic)
}

// decode strips the 5-byte Confluent wire-format header: 1 magic byte (0x0) +
// 4-byte big-endian schema id — then unmarshals the remaining bytes directly
// as FraudResultEvent (no message-index prefix is emitted by this project's
// producers; see docs/MULTI_TENANT_RESULT_DELIVERY.md).
func decode(raw []byte) (*fraudv1.FraudResultEvent, error) {
	const headerLen = 1 + 4
	if len(raw) < headerLen {
		return nil, fmt.Errorf("payload too short (%d bytes) to contain the Confluent header", len(raw))
	}
	if raw[0] != 0x0 {
		return nil, fmt.Errorf("unexpected magic byte 0x%x (expected 0x0)", raw[0])
	}
	body := raw[headerLen:]
	event := &fraudv1.FraudResultEvent{}
	if err := proto.Unmarshal(body, event); err != nil {
		return nil, err
	}
	return event, nil
}
