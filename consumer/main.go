package main

import (
	"context"
	"fmt"
	"log"

	"github.com/segmentio/kafka-go"
)

func main() {

	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers: []string{"localhost:9092"},
		GroupID: "test-group",
		Topic:   "test-topic",
	})

	defer reader.Close()

	fmt.Println("Consumer started...")

	for {
		msg, err := reader.ReadMessage(context.Background())
		if err != nil {
			log.Println("Error:", err)
			continue
		}

		fmt.Printf("Consumer got: %s | Partition: %d\n",
			string(msg.Value),
			msg.Partition)
	}
}