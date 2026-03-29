package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	kafkago "github.com/segmentio/kafka-go"
	"github.com/sentinelswitch/persistence-svc/internal/config"
	kafkapkg "github.com/sentinelswitch/persistence-svc/internal/kafka"
	"github.com/sentinelswitch/persistence-svc/internal/pipeline"
	"github.com/sentinelswitch/persistence-svc/internal/store"
	"go.uber.org/zap"
)

func main() {
	cfgPath := os.Getenv("CONFIG_PATH")
	if cfgPath == "" {
		cfgPath = "config/persistence-svc.yaml"
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config load failed: %v\n", err)
		os.Exit(1)
	}

	var logger *zap.Logger
	if cfg.Logging.Format == "json" {
		logger, err = zap.NewProduction()
	} else {
		logger, err = zap.NewDevelopment()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "logger build failed: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync() //nolint:errcheck

	// PostgreSQL pool
	dsn := fmt.Sprintf(
		"host=%s port=%d dbname=%s user=%s password=%s sslmode=%s connect_timeout=%d",
		cfg.Postgres.Host,
		cfg.Postgres.Port,
		cfg.Postgres.Database,
		cfg.Postgres.Username,
		cfg.Postgres.Password,
		cfg.Postgres.SSLMode,
		cfg.Postgres.Pool.ConnectTimeoutMs/1000,
	)

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		logger.Fatal("failed to parse postgres dsn", zap.Error(err))
	}
	poolCfg.MaxConns = int32(cfg.Postgres.Pool.MaxOpenConns)
	poolCfg.MinConns = int32(cfg.Postgres.Pool.MaxIdleConns)
	poolCfg.MaxConnLifetime = time.Duration(cfg.Postgres.Pool.ConnMaxLifetime) * time.Second

	pgStore, err := store.New(context.Background(), poolCfg, logger)
	if err != nil {
		logger.Fatal("failed to connect to postgres", zap.Error(err))
	}
	defer pgStore.Close()

	if err := pgStore.Ping(context.Background()); err != nil {
		logger.Fatal("postgres ping failed", zap.Error(err))
	}
	logger.Info("postgres connected")

	// Kafka reader
	reader := kafkago.NewReader(kafkago.ReaderConfig{
		Brokers:        cfg.Kafka.Brokers,
		GroupID:        cfg.Kafka.Consumer.GroupID,
		Topic:          cfg.Kafka.Consumer.Topic,
		MinBytes:       1,
		MaxBytes:       10 << 20,
		CommitInterval: 0, // manual commit
	})

	// DLQ producer
	dlqProducer := kafkapkg.NewDLQProducer(cfg.Kafka.Brokers, cfg.Kafka.DLQProducer.Topic)
	defer dlqProducer.Close() //nolint:errcheck

	// Pipeline processor
	proc := pipeline.New(
		reader,
		pgStore,
		dlqProducer,
		pipeline.Config{
			MaxBatchSize:    cfg.Postgres.Batch.MaxBatchSize,
			FlushIntervalMs: cfg.Postgres.Batch.FlushIntervalMs,
			MaxAttempts:     cfg.Retry.MaxAttempts,
			InitialDelayMs:  cfg.Retry.InitialDelayMs,
			Multiplier:      cfg.Retry.Multiplier,
			MaxDelayMs:      cfg.Retry.MaxDelayMs,
		},
		logger,
	)

	// Metrics server
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Server.Metrics.Port),
		Handler: metricsMux,
	}
	go func() {
		logger.Info("metrics server listening", zap.Int("port", cfg.Server.Metrics.Port))
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("metrics server error", zap.Error(err))
		}
	}()

	// Health server
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	healthServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Server.Health.Port),
		Handler: healthMux,
	}
	go func() {
		logger.Info("health server listening", zap.Int("port", cfg.Server.Health.Port))
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

	logger.Info("persistence-svc starting",
		zap.String("topic", cfg.Kafka.Consumer.Topic),
		zap.String("dlq_topic", cfg.Kafka.DLQProducer.Topic),
	)
	proc.Run(ctx)

	logger.Info("persistence-svc stopped")
}
