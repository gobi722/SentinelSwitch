package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sentinelswitch/fraud-engine/internal/circuitbreaker"
	"github.com/sentinelswitch/fraud-engine/internal/config"
	"github.com/sentinelswitch/fraud-engine/internal/pipeline"
	redisclient "github.com/sentinelswitch/fraud-engine/internal/redis"
	"github.com/sentinelswitch/fraud-engine/internal/risk"
	"github.com/sentinelswitch/fraud-engine/internal/rules"
	"github.com/sentinelswitch/fraud-engine/internal/velocity"
	"go.uber.org/zap"
)

func main() {
	cfgPath := os.Getenv("CONFIG_PATH")
	if cfgPath == "" {
		cfgPath = "config/fraud-engine.yaml"
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	var logger *zap.Logger
	if cfg.Logging.Format == "json" {
		logger, err = zap.NewProduction()
	} else {
		logger, err = zap.NewDevelopment()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync() //nolint:errcheck

	velocityRDB, err := redisclient.NewClient(cfg, redisclient.DBVelocity)
	if err != nil {
		logger.Fatal("velocity redis connect failed", zap.Error(err))
	}
	merchantRDB, err := redisclient.NewClient(cfg, redisclient.DBMerchantRisk)
	if err != nil {
		logger.Fatal("merchant-risk redis connect failed", zap.Error(err))
	}
	cbRDB, err := redisclient.NewClient(cfg, redisclient.DBCircuitBreaker)
	if err != nil {
		logger.Fatal("circuit-breaker redis connect failed", zap.Error(err))
	}

	rulesPath := os.Getenv("RULES_CONFIG")
	if rulesPath == "" {
		rulesPath = cfg.Rules.ConfigFile
	}
	engine, err := rules.NewEngine(rulesPath)
	if err != nil {
		logger.Fatal("failed to load rules engine", zap.Error(err))
	}

	velStore := velocity.NewStore(velocityRDB, logger)
	cb := circuitbreaker.New(cbRDB, cfg, logger)

	riskCli, err := risk.NewClient(cfg, logger)
	if err != nil {
		logger.Fatal("failed to create risk service client", zap.Error(err))
	}
	defer riskCli.Close() //nolint:errcheck

	proc, err := pipeline.NewProcessor(cfg, engine, velStore, merchantRDB, riskCli, cb, logger)
	if err != nil {
		logger.Fatal("failed to create pipeline processor", zap.Error(err))
	}

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
		logger.Info("received shutdown signal", zap.String("signal", sig.String()))
		cancel()
	}()

	logger.Info("fraud-engine starting",
		zap.String("consumer_topic", cfg.Kafka.Consumer.Topic),
		zap.String("producer_topic", cfg.Kafka.Producer.Topic),
	)

	if err := proc.Run(ctx); err != nil {
		logger.Error("pipeline exited with error", zap.Error(err))
		os.Exit(1)
	}

	logger.Info("fraud-engine stopped")
}
