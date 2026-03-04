package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/segmentio/kafka-go"
)

func main() {

	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:  []string{"localhost:9092"},
		GroupID:  "test-group",
		Topic:    "test-topic",
		MinBytes: 10e3,
		MaxBytes: 10e6,
	})

	defer reader.Close()

	fmt.Println("Reliable Consumer started...")

	ctx := context.Background()

	for {
		// 1 Fetch (NO auto commit)
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			log.Println("Fetch error:", err)
			continue
		}

		// 2️ Process
		err = processMessage(msg)
		if err != nil {
			log.Println("Processing failed. Will retry:", err)
			continue // offset NOT committed
		}

		// 3️⃣ Commit ONLY if success
		err = reader.CommitMessages(ctx, msg)
		if err != nil {
			log.Println("Commit failed:", err)
			continue
		}

		fmt.Printf("Processed & committed: %s | Partition: %d | Offset: %d\n",
			string(msg.Value),
			msg.Partition,
			msg.Offset)
	}
}

func processMessage(msg kafka.Message) error {

	fmt.Println("Processing:", string(msg.Value))

	// Simulate failure
	if string(msg.Value) == "fail" {
		return fmt.Errorf("simulated failure")
	}

	// Simulate work
	time.Sleep(2 * time.Second)

	
	return nil
}