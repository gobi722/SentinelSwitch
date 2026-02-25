package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/segmentio/kafka-go"
)

func main() {

	writer := kafka.NewWriter(kafka.WriterConfig{
		Brokers:  []string{"localhost:9092"},
		Topic:    "test-topic",
		Balancer: &kafka.Hash{}, // important for key-based partitioning
	})
	defer writer.Close()

	for i := 0; i < 20; i++ {

		msg := kafka.Message{
			Key:   []byte(fmt.Sprintf("key-%d", i)),
			Value: []byte(fmt.Sprintf("message-%d", i)),
		}

		err := writer.WriteMessages(context.Background(), msg)
		if err != nil {
			log.Fatal("Write error:", err)
		}

		fmt.Println("Produced:", string(msg.Value))
		time.Sleep(500 * time.Millisecond)
	}
}