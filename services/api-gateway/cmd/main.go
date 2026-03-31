package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_zap "github.com/grpc-ecosystem/go-grpc-middleware/logging/zap"
	grpc_recovery "github.com/grpc-ecosystem/go-grpc-middleware/recovery"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"github.com/sentinelswitch/api-gateway/internal/config"
	"github.com/sentinelswitch/api-gateway/internal/gateway"
	"github.com/sentinelswitch/api-gateway/internal/hashing"
	"github.com/sentinelswitch/api-gateway/internal/idempotency"
	"github.com/sentinelswitch/api-gateway/internal/kafka"
	"github.com/sentinelswitch/api-gateway/internal/ratelimit"
	gatewayv1 "github.com/sentinelswitch/proto/gateway/v1"
)

func main() {
	// -------------------------------------------------------------------------
	// Logger
	// -------------------------------------------------------------------------
	log, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create logger: %v\n", err)
		os.Exit(1)
	}
	defer log.Sync() //nolint:errcheck

	// -------------------------------------------------------------------------
	// Config
	// -------------------------------------------------------------------------
	cfgPath := envOr("CONFIG_PATH", "config/api-gateway.yaml")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatal("config load failed", zap.String("path", cfgPath), zap.Error(err))
	}
	log.Info("config loaded", zap.String("path", cfgPath))

	// -------------------------------------------------------------------------
	// Build dependencies
	// -------------------------------------------------------------------------
	// PAN hashing
	hasher, err := hashing.New(cfg.Hashing)
	if err != nil {
		log.Fatal("hasher init failed", zap.Error(err))
	}

	// Idempotency store (Redis)
	idStore := idempotency.New(
		cfg.Redis.Host,
		cfg.Redis.Password,
		cfg.Redis.DB,
		cfg.Idempotency.KeyPrefix,
		cfg.Idempotency.TTLSeconds,
		cfg.Idempotency.OnRedisUnavailable,
	)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := idStore.Ping(ctx); err != nil {
		log.Warn("redis ping failed — continuing per on_redis_unavailable policy", zap.Error(err))
	} else {
		log.Info("redis connected", zap.String("addr", cfg.Redis.Host))
	}
	cancel()

	// Kafka producer
	producer := kafka.New(kafka.ProducerConfig{
		Brokers:           cfg.Kafka.Brokers,
		Topic:             cfg.Kafka.Topic,
		Acks:              cfg.Kafka.Producer.Acks,
		EnableIdempotence: cfg.Kafka.Producer.EnableIdempotence,
		Retries:           cfg.Kafka.Producer.Retries,
		DeliveryTimeoutMs: cfg.Kafka.Producer.DeliveryTimeoutMs,
		LingerMs:          cfg.Kafka.Producer.LingerMs,
		BatchSizeBytes:    cfg.Kafka.Producer.BatchSizeBytes,
		CompressionType:   cfg.Kafka.Producer.CompressionType,
		ClientID:          cfg.Kafka.Producer.ClientID,
		SchemaRegistryURL: cfg.Kafka.Producer.SchemaRegistryURL,
		SchemaSubject:     cfg.Kafka.Producer.SchemaSubject,
	})

	// Rate limiter
	rl := ratelimit.New(
		cfg.RateLimiting.RequestsPerSecond,
		cfg.RateLimiting.Burst,
		cfg.RateLimiting.IdentityHeader,
	)

	// Validator
	validator, err := gateway.NewValidator(cfg.Validation)
	if err != nil {
		log.Fatal("validator init failed", zap.Error(err))
	}

	// Handler
	handler := gateway.NewHandler(validator, hasher, idStore, producer, rl, log)

	// -------------------------------------------------------------------------
	// gRPC server
	// -------------------------------------------------------------------------
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(
			grpc_zap.UnaryServerInterceptor(log),
			grpc_recovery.UnaryServerInterceptor(),
		)),
	)

	gatewayv1.RegisterGatewayServiceServer(grpcServer, handler)

	// gRPC health
	healthSrv := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthSrv)
	healthSrv.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)

	// gRPC reflection (dev / grpcurl)
	if cfg.Server.EnableReflection {
		reflection.Register(grpcServer)
	}

	grpcLis, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Server.GRPC.Port))
	if err != nil {
		log.Fatal("grpc listen failed", zap.Int("port", cfg.Server.GRPC.Port), zap.Error(err))
	}

	// -------------------------------------------------------------------------
	// Metrics + health HTTP server
	// -------------------------------------------------------------------------
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	httpSrv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Server.Metrics.Port),
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// -------------------------------------------------------------------------
	// Start
	// -------------------------------------------------------------------------
	go func() {
		log.Info("gRPC server starting", zap.Int("port", cfg.Server.GRPC.Port))
		if err := grpcServer.Serve(grpcLis); err != nil {
			log.Fatal("gRPC serve error", zap.Error(err))
		}
	}()

	go func() {
		log.Info("HTTP server starting", zap.Int("port", cfg.Server.Metrics.Port))
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("HTTP serve error", zap.Error(err))
		}
	}()

	// -------------------------------------------------------------------------
	// Graceful shutdown
	// -------------------------------------------------------------------------
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("shutdown signal received")

	healthSrv.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutCancel()

	grpcServer.GracefulStop()

	if err := httpSrv.Shutdown(shutCtx); err != nil {
		log.Error("HTTP shutdown error", zap.Error(err))
	}
	if err := producer.Close(); err != nil {
		log.Error("kafka producer close error", zap.Error(err))
	}
	if err := idStore.Close(); err != nil {
		log.Error("idempotency store close error", zap.Error(err))
	}

	log.Info("shutdown complete")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
