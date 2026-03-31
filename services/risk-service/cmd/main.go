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
	grpc_recovery "github.com/grpc-ecosystem/go-grpc-middleware/recovery"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	riskpb "github.com/sentinelswitch/proto/risk/v1"
	"github.com/sentinelswitch/risk-service/internal/config"
	"github.com/sentinelswitch/risk-service/internal/scoring"
	"github.com/sentinelswitch/risk-service/internal/server"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

func main() {
	cfgPath := os.Getenv("CONFIG_PATH")
	if cfgPath == "" {
		cfgPath = "../../../config/risk-service.yaml"
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

	// Build scoring.Config from YAML
	scoringCfg := scoring.Config{
		ApproveBelowScore:           int32(cfg.DecisionThresholds.ApproveBelow),
		DeclineAboveScore:           int32(cfg.DecisionThresholds.DeclineAbove),
		ContributingFactorThreshold: cfg.Scoring.ContributingFactorThreshold,
	}
	for _, f := range cfg.Scoring.Features {
		scoringCfg.Features = append(scoringCfg.Features, scoring.FeatureConfig{
			Name:   f.Name,
			Weight: f.Weight,
			Normalise: scoring.NormaliseConfig{
				Method: f.Normalise.Method,
				Cap:    f.Normalise.Cap,
			},
		})
	}

	scorer, err := scoring.New(scoringCfg)
	if err != nil {
		logger.Fatal("failed to create scorer", zap.Error(err))
	}

	grpcHandler := server.NewHandler(scorer, logger)

	var grpcOpts []grpc.ServerOption
	grpcOpts = append(grpcOpts, grpc.UnaryInterceptor(
		grpc_middleware.ChainUnaryServer(
			grpc_recovery.UnaryServerInterceptor(),
		),
	))
	if cfg.Server.GRPC.MaxConcurrentStreams > 0 {
		grpcOpts = append(grpcOpts,
			grpc.MaxConcurrentStreams(uint32(cfg.Server.GRPC.MaxConcurrentStreams)),
		)
	}
	grpcSrv := grpc.NewServer(grpcOpts...)
	riskpb.RegisterRiskServiceServer(grpcSrv, grpcHandler)
	if cfg.Server.GRPC.EnableReflection {
		reflection.Register(grpcSrv)
	}

	lis, err := net.Listen("tcp", fmt.Sprintf("%s:%d", cfg.Server.GRPC.Host, cfg.Server.GRPC.Port))
	if err != nil {
		logger.Fatal("failed to listen", zap.Error(err))
	}

	// Metrics server
	metricsMux := http.NewServeMux()
	metricsMux.Handle(cfg.Server.Metrics.Path, promhttp.Handler())
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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("shutdown signal received", zap.String("signal", sig.String()))
		grpcSrv.GracefulStop()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = metricsServer.Shutdown(shutCtx)
		_ = healthServer.Shutdown(shutCtx)
	}()

	logger.Info("risk-service gRPC server starting",
		zap.String("addr", lis.Addr().String()),
	)
	if err := grpcSrv.Serve(lis); err != nil {
		logger.Error("gRPC server exited", zap.Error(err))
		os.Exit(1)
	}

	logger.Info("risk-service stopped")
}
