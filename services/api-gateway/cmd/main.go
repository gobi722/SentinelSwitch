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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/sentinelswitch/api-gateway/internal/auth"
	"github.com/sentinelswitch/api-gateway/internal/config"
	"github.com/sentinelswitch/api-gateway/internal/gateway"
	"github.com/sentinelswitch/api-gateway/internal/hashing"
	"github.com/sentinelswitch/api-gateway/internal/idempotency"
	"github.com/sentinelswitch/api-gateway/internal/kafka"
	"github.com/sentinelswitch/api-gateway/internal/logging"
	"github.com/sentinelswitch/api-gateway/internal/ratelimit"
	"github.com/sentinelswitch/api-gateway/internal/txnstatus"
	gatewayv1 "github.com/sentinelswitch/proto/gateway/v1"
)

func main() {
	godotenv.Load(".env")
	// -------------------------------------------------------------------------
	// Config
	// -------------------------------------------------------------------------
	cfgPath := envOr("CONFIG_PATH", "../../config/api-gateway.yaml")
	redisPath := envOr("CONFIG_PATH", "../../config/redis.yaml")
	cfg, err := config.Load(cfgPath, redisPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config load failed: %v\n", err)
		os.Exit(1)
	}

	// -------------------------------------------------------------------------
	// Logger — everything goes to cfg.Logging.Dir (hourly-rotated); the
	// terminal only echoes startup-phase logs until stopConsole() is called
	// further down.
	// -------------------------------------------------------------------------
	log, stopConsole, err := logging.New(cfg.Logging.Format, cfg.Logging.Level, cfg.Logging.Dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create logger: %v\n", err)
		os.Exit(1)
	}
	defer log.Sync() //nolint:errcheck
	log.Info("config loaded", zap.String("path", cfgPath))

	// -------------------------------------------------------------------------
	// Build dependencies
	// -------------------------------------------------------------------------
	// PAN hashing
	hasher, err := hashing.New(cfg.PanHashing.SecretEnv, cfg.PanHashing.MaskedPanPrefixLen,
		cfg.PanHashing.MaskedPanSuffixLen, cfg.PanHashing.MaskChar)
	if err != nil {
		log.Fatal("hasher init failed", zap.Error(err))
	}

	// Auth: Postgres pool (api_clients registry)
	pgDSN := fmt.Sprintf(
		"host=%s port=%d dbname=%s user=%s password=%s sslmode=%s connect_timeout=%d",
		cfg.Postgres.Host,
		cfg.Postgres.Port,
		cfg.Postgres.Database,
		cfg.Postgres.Username,
		cfg.Postgres.Password,
		cfg.Postgres.SSLMode,
		cfg.Postgres.Pool.ConnectTimeoutMs/1000,
	)
	pgPoolCfg, err := pgxpool.ParseConfig(pgDSN)
	if err != nil {
		log.Fatal("failed to parse postgres dsn", zap.Error(err))
	}
	pgPoolCfg.MaxConns = int32(cfg.Postgres.Pool.MaxOpenConns)
	pgPoolCfg.MinConns = int32(cfg.Postgres.Pool.MaxIdleConns)
	pgPoolCfg.MaxConnLifetime = time.Duration(cfg.Postgres.Pool.ConnMaxLifetime) * time.Second

	pgPool, err := pgxpool.NewWithConfig(context.Background(), pgPoolCfg)
	if err != nil {
		log.Fatal("failed to create postgres pool for auth", zap.Error(err))
	}

	authStore := auth.NewStore(pgPool)
	authCache := auth.NewCache(
		authStore,
		fmt.Sprintf("%s:%d", cfg.Redis.Host, cfg.Redis.Port),
		cfg.Redis.Password,
		cfg.Auth.CacheRedisDB,
		cfg.Auth.CacheTTLSeconds,
	)

	// GetTransactionStatus lookups — same Postgres pool as auth, different table.
	txnStatusStore := txnstatus.NewStore(pgPool)

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
	handler := gateway.NewHandler(validator, hasher, idStore, producer, rl, txnStatusStore, log)

	// -------------------------------------------------------------------------
	// gRPC server
	// -------------------------------------------------------------------------
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(
			grpc_zap.UnaryServerInterceptor(log),
			grpc_recovery.UnaryServerInterceptor(),
			auth.UnaryServerInterceptor(authCache, cfg.Auth.ApiKeyHeader, cfg.RateLimiting.IdentityHeader, log),
		)),
	)

	gatewayv1.RegisterGatewayServiceServer(grpcServer, handler)

	// gRPC health
	healthSrv := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthSrv)
	healthSrv.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)

	// gRPC reflection (dev / grpcurl)
	// if cfg.Server.EnableReflection {
	// 	reflection.Register(grpcServer)
	// }

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
	// Readiness: unlike /healthz (always 200 once the process is up), this
	// checks the backing stores auth depends on — a caller routed here while
	// they're down would just get fail-closed Unavailable responses anyway.
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := authStore.Ping(ctx); err != nil {
			http.Error(w, "postgres (auth store) unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := authCache.Ping(ctx); err != nil {
			http.Error(w, "redis (auth cache) unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := idStore.Ping(ctx); err != nil {
			http.Error(w, "redis (idempotency store) unavailable", http.StatusServiceUnavailable)
			return
		}
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
	// Logged synchronously (not inside the goroutines below) so these are
	// guaranteed to hit the terminal before stopConsole() takes effect.
	log.Info("gRPC server starting", zap.Int("port", cfg.Server.GRPC.Port))
	go func() {
		if err := grpcServer.Serve(grpcLis); err != nil {
			log.Fatal("gRPC serve error", zap.Error(err))
		}
	}()

	log.Info("HTTP server starting", zap.Int("port", cfg.Server.Metrics.Port))
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("HTTP serve error", zap.Error(err))
		}
	}()

	// Startup is done — from here on, logs (including every request the
	// grpc_zap interceptor logs) only go to cfg.Logging.Dir, not the terminal.
	stopConsole()

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
	if err := authCache.Close(); err != nil {
		log.Error("auth cache close error", zap.Error(err))
	}
	authStore.Close()

	log.Info("shutdown complete")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
