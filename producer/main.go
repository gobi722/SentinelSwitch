
package main

import (
	"context"
	"log"
	"time"

	"github.com/segmentio/kafka-go"
)

func main() {

	writer := kafka.NewWriter(kafka.WriterConfig{
		Brokers: []string{
			"localhost:9092",
			"localhost:9093",
			"localhost:9094",
		},
		Topic:        "payments",
		// RequiredAcks: kafka.RequireAll, // acks=all
		Balancer:     &kafka.LeastBytes{},
	})

	for i := 0; i < 100; i++ {
		err := writer.WriteMessages(context.Background(),
			kafka.Message{
				Key:   []byte("txn"),
				Value: []byte("payment message"),
			},
		)
		if err != nil {
			log.Fatal(err)
		}
		time.Sleep(time.Second)
	}
}


// package main

// import (
// 	"context"
// 	"fmt"
// 	"log"
// 	"time"

// 	"github.com/segmentio/kafka-go"
// )

// func main() {

// 	writer := kafka.NewWriter(kafka.WriterConfig{
// 		Brokers:  []string{"localhost:9092"},
// 		Topic:    "test-topic",
// 		Balancer: &kafka.Hash{},
// 	})
// 	defer writer.Close()

// 	for i := 0; i < 20; i++ {

// 		var value string

// 		// Inject failure at message-5
// 		if i == 5 {
// 			value = "fail"
// 		} else {
// 			value = fmt.Sprintf("message-%d", i)
// 		}

// 		msg := kafka.Message{
// 			Key:   []byte(fmt.Sprintf("key-%d", i)),
// 			Value: []byte(value),
// 		}

// 		err := writer.WriteMessages(context.Background(), msg)
// 		if err != nil {
// 			log.Fatal("Write error:", err)
// 		}

// 		fmt.Println("Produced:", value)
// 		time.Sleep(500 * time.Millisecond)
// 	}
// }