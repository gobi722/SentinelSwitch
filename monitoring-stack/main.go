package main

import (
	"log"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	requestCount = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total API Requests",
		},
		[]string{"method", "endpoint"},
	)

	requestLatency = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name: "http_request_duration_seconds",
			Help: "Request latency",
		},
	)
)

func init() {
	prometheus.MustRegister(requestCount)
	prometheus.MustRegister(requestLatency)
}

func main() {

	app := fiber.New()

	app.Use(func(c *fiber.Ctx) error {

		start := time.Now()

		err := c.Next()

		duration := time.Since(start).Seconds()

		requestCount.WithLabelValues(
			c.Method(),
			c.Path(),
		).Inc()

		requestLatency.Observe(duration)

		return err
	})

	app.Get("/pay", func(c *fiber.Ctx) error {

		time.Sleep(50 * time.Millisecond)

		return c.SendString("Payment OK")
	})

	// run metrics server
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		log.Fatal(http.ListenAndServe(":9091", nil))
	}()

	app.Listen(":8080")
}