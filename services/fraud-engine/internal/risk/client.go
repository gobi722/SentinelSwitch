package risk

import (
	"context"
	"fmt"
	"time"

	riskpb "github.com/sentinelswitch/proto/risk/v1"
	"github.com/sentinelswitch/fraud-engine/internal/config"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

type Client struct {
	stub    riskpb.RiskServiceClient
	conn    *grpc.ClientConn
	timeout time.Duration
	logger  *zap.Logger
}

func NewClient(cfg *config.Config, logger *zap.Logger) (*Client, error) {
	addr := fmt.Sprintf("%s:%d", cfg.RiskService.Host, cfg.RiskService.Port)

	//nolint:staticcheck // grpc.Dial is deprecated in v1.63+ but works on all versions
	conn, err := grpc.Dial(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("risk: dial %s: %w", addr, err)
	}

	timeout := time.Duration(cfg.RiskService.CallTimeoutMs) * time.Millisecond
	if timeout == 0 {
		timeout = 500 * time.Millisecond
	}

	return &Client{
		stub:    riskpb.NewRiskServiceClient(conn),
		conn:    conn,
		timeout: timeout,
		logger:  logger,
	}, nil
}

func (c *Client) CalculateRisk(ctx context.Context, req *riskpb.RiskRequest) (*riskpb.RiskResponse, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.stub.CalculateRisk(callCtx, req)
	if err != nil {
		return nil, fmt.Errorf("risk: CalculateRisk: %w", err)
	}
	return resp, nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}