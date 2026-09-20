// live-dashboard — demo tool: streams decoded FraudResultEvents from a
// client's results.<client_id> Kafka topic to a browser in real time over
// Server-Sent Events, for showing a non-technical audience "submit via
// Postman -> decision arrives on your private channel" live.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	fraudv1 "github.com/sentinelswitch/proto/fraud/v1"
	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl/scram"
	"google.golang.org/protobuf/proto"
)

type displayEvent struct {
	TxnID          string   `json:"txnId"`
	MerchantID     string   `json:"merchantId"`
	TerminalID     string   `json:"terminalId"`
	AmountDisplay  string   `json:"amountDisplay"`
	Decision       string   `json:"decision"`
	RiskScore      int32    `json:"riskScore"`
	TriggeredRules []string `json:"triggeredRules"`
	ProcessedAt    string   `json:"processedAt"`
	ClientID       string   `json:"clientId"`
}

var minorDecimals = map[string]int{
	"OMR": 3, "BHD": 3, "KWD": 3,
}

func formatAmount(minor int64, currency string) string {
	dec := minorDecimals[currency]
	if dec == 0 {
		dec = 2
	}
	div := int64(1)
	for i := 0; i < dec; i++ {
		div *= 10
	}
	whole := minor / div
	frac := minor % div
	return fmt.Sprintf("%d.%0*d %s", whole, dec, frac, currency)
}

// decode strips the 5-byte Confluent wire-format header (1 magic byte + 4-byte
// big-endian schema id — no message-index prefix, single-message-per-file
// proto) and unmarshals the remaining bytes.
func decode(raw []byte) (*fraudv1.FraudResultEvent, error) {
	const headerLen = 1 + 4
	if len(raw) < headerLen {
		return nil, fmt.Errorf("payload too short (%d bytes)", len(raw))
	}
	if raw[0] != 0x0 {
		return nil, fmt.Errorf("unexpected magic byte 0x%x", raw[0])
	}
	event := &fraudv1.FraudResultEvent{}
	if err := proto.Unmarshal(raw[headerLen:], event); err != nil {
		return nil, err
	}
	return event, nil
}

type hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func newHub() *hub { return &hub{subs: make(map[chan []byte]struct{})} }

func (h *hub) subscribe() chan []byte {
	ch := make(chan []byte, 8)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
	close(ch)
}

func (h *hub) broadcast(msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

func main() {
	broker := flag.String("broker", "localhost:9096", "Kafka PUBLIC listener")
	client := flag.String("client", "", "client_id (also the topic suffix / SCRAM username)")
	password := flag.String("password", "", "SCRAM password for this client_id")
	httpPort := flag.String("http-port", "7070", "local port to serve the dashboard on")
	flag.Parse()

	if *client == "" || *password == "" {
		log.Fatal("usage: live-dashboard -client <client_id> -password <scram_password> [-http-port 7070]")
	}

	topic := "results." + *client
	group := fmt.Sprintf("%s.demo-%d", *client, time.Now().Unix())

	mechanism, err := scram.Mechanism(scram.SHA256, *client, *password)
	if err != nil {
		log.Fatalf("scram mechanism: %v", err)
	}

	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:     []string{*broker},
		Topic:       topic,
		GroupID:     group,
		StartOffset: kafka.LastOffset, // only show results that arrive during this demo run
		Dialer: &kafka.Dialer{
			Timeout:       10 * time.Second,
			SASLMechanism: mechanism,
		},
	})

	h := newHub()

	go func() {
		for {
			m, err := reader.ReadMessage(context.Background())
			if err != nil {
				log.Printf("kafka read error: %v", err)
				time.Sleep(time.Second)
				continue
			}
			event, err := decode(m.Value)
			if err != nil {
				log.Printf("decode error at offset %d: %v", m.Offset, err)
				continue
			}
			disp := displayEvent{
				TxnID:          event.GetTxnId(),
				MerchantID:     event.GetMerchantId(),
				TerminalID:     event.GetTerminalId(),
				AmountDisplay:  formatAmount(event.GetAmountMinor(), event.GetCurrency()),
				Decision:       event.GetDecision().String(),
				RiskScore:      event.GetRiskScore(),
				TriggeredRules: event.GetTriggeredRules(),
				ProcessedAt:    event.GetProcessedAt(),
				ClientID:       event.GetClientId(),
			}
			b, _ := json.Marshal(disp)
			log.Printf("delivered txn=%s decision=%s risk=%d", disp.TxnID, disp.Decision, disp.RiskScore)
			h.broadcast(b)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		ch := h.subscribe()
		defer h.unsubscribe(ch)

		fmt.Fprintf(w, "event: ready\ndata: {\"topic\":%q,\"clientId\":%q}\n\n", topic, *client)
		flusher.Flush()

		ctx := r.Context()
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-ch:
				fmt.Fprintf(w, "data: %s\n\n", msg)
				flusher.Flush()
			case <-ticker.C:
				fmt.Fprint(w, ": keepalive\n\n")
				flusher.Flush()
			}
		}
	})

	wd, _ := os.Getwd()
	mux.Handle("/", http.FileServer(http.Dir(wd)))

	addr := "localhost:" + *httpPort
	log.Printf("SentinelSwitch live dashboard: http://%s  (client=%s, topic=%s, group=%s)", addr, *client, topic, group)
	log.Fatal(http.ListenAndServe(addr, mux))
}
