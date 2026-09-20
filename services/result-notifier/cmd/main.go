package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/sentinelswitch/result-notifier/internal/config"
	kafkapkg "github.com/sentinelswitch/result-notifier/internal/kafka"
	"github.com/sentinelswitch/result-notifier/internal/logging"
	"github.com/sentinelswitch/result-notifier/internal/pipeline"
	"github.com/sentinelswitch/result-notifier/internal/router"
)

func main() {
	godotenv.Load(".env")
	cfgPath := os.Getenv("CONFIG_PATH")
	if cfgPath == "" {
		cfgPath = "../../config/result-notifier.yaml"
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config load failed: %v\n", err)
		os.Exit(1)
	}

	// Everything goes to cfg.Logging.Dir (hourly-rotated); the terminal only
	// echoes startup-phase logs until stopConsole() is called further down.
	logger, stopConsole, err := logging.New(cfg.Logging.Format, cfg.Logging.Level, cfg.Logging.Dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logger build failed: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync() //nolint:errcheck

	rtr := router.New(cfg.Router.TopicPrefix)

	dlqProducer := kafkapkg.NewDLQProducer(cfg.Kafka.Brokers, cfg.Kafka.DLQProducer.Topic)
	defer dlqProducer.Close() //nolint:errcheck

	proc := pipeline.New(cfg, rtr, dlqProducer, logger)

	// Metrics server
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Server.Metrics.Port),
		Handler: metricsMux,
	}
	logger.Info("metrics server listening", zap.Int("port", cfg.Server.Metrics.Port))
	go func() {
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("metrics server error", zap.Error(err))
		}
	}()

	// Health server
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	healthServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Server.Health.Port),
		Handler: healthMux,
	}
	logger.Info("health server listening", zap.Int("port", cfg.Server.Health.Port))
	go func() {
		if err := healthServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("health server error", zap.Error(err))
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("shutdown signal received", zap.String("signal", sig.String()))
		cancel()
		_ = metricsServer.Close()
		_ = healthServer.Close()
	}()

	logger.Info("result-notifier starting",
		zap.String("consumer_topic", cfg.Kafka.Consumer.Topic),
		zap.String("topic_prefix", cfg.Router.TopicPrefix),
		zap.String("unrouted_dlq_topic", cfg.Kafka.DLQProducer.Topic),
	)

	// Startup is done — from here on, logs (including every message the
	// pipeline processes) only go to cfg.Logging.Dir, not the terminal.
	stopConsole()

	proc.Run(ctx) //nolint:errcheck

	logger.Info("result-notifier stopped")
}
