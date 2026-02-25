package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/segmentio/kafka-go"
)

const (
	broker = "localhost:9092"
	topic  = "test-topic"
	group  = "test-group"
)

func createTopic() {
	conn, err := kafka.Dial("tcp", broker)
	if err != nil {
		log.Fatal("Failed to dial broker:", err)
	}
	defer conn.Close()

	controller, err := conn.Controller()
	if err != nil {
		log.Fatal("Failed to get controller:", err)
	}

	controllerConn, err := kafka.Dial("tcp", fmt.Sprintf("%s:%d", controller.Host, controller.Port))
	if err != nil {
		log.Fatal("Failed to dial controller:", err)
	}
	defer controllerConn.Close()

	topicConfigs := []kafka.TopicConfig{
		{
			Topic:             topic,
			NumPartitions:     1,
			ReplicationFactor: 1,
		},
	}

	err = controllerConn.CreateTopics(topicConfigs...)
	if err != nil {
		log.Println("Topic might already exist:", err)
	} else {
		fmt.Println("Topic created successfully")
	}
}

func produceMessage() {
	writer := kafka.NewWriter(kafka.WriterConfig{
		Brokers: []string{broker},
		Topic:   topic,
	})

	defer writer.Close()

	msg := kafka.Message{
		Key:   []byte("Key-1"),
		Value: []byte("Hello Kafka from Go!"),
	}

	err := writer.WriteMessages(context.Background(), msg)
	if err != nil {
		log.Fatal("Failed to write message:", err)
	}

	fmt.Println("Message produced successfully")
}

func consumeMessage() {
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers: []string{broker},
		GroupID: group,
		Topic:   topic,
	})

	defer reader.Close()

	fmt.Println("Waiting for message...")

	msg, err := reader.ReadMessage(context.Background())
	if err != nil {
		log.Fatal("Failed to read message:", err)
	}

	fmt.Printf("Received message: %s\n", string(msg.Value))
}

func main() {
	createTopic()
	time.Sleep(2 * time.Second)

	produceMessage()
	time.Sleep(2 * time.Second)

	consumeMessage()
}