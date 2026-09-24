package gateway

import (
	"context"
	"crypto/hmac"
	"errors"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/sentinelswitch/api-gateway/internal/auth"
	"github.com/sentinelswitch/api-gateway/internal/provisioning"
	gatewayv1 "github.com/sentinelswitch/proto/gateway/v1"
)

var provisionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "sentinel_gateway_client_provisions_total",
	Help: "Total ProvisionClient requests partitioned by outcome.",
}, []string{"result"})

// AdminHandler implements gatewayv1.AdminServiceServer.
//
// Deliberately separate from Handler (GatewayServiceServer): this surface is
// exempt from the per-client x-api-key interceptor (see
// auth.adminServicePrefix) and instead checks its own admin-key header here,
// directly in the handler — there is exactly one RPC on this service, so a
// dedicated interceptor would be more indirection, not less.
type AdminHandler struct {
	gatewayv1.UnimplementedAdminServiceServer

	provisioner    *provisioning.Service
	adminKeyHeader string
	adminKeySecret []byte
	log            *zap.Logger
}

func NewAdminHandler(p *provisioning.Service, adminKeyHeader, adminKeySecret string, log *zap.Logger) *AdminHandler {
	return &AdminHandler{
		provisioner:    p,
		adminKeyHeader: adminKeyHeader,
		adminKeySecret: []byte(adminKeySecret),
		log:            log,
	}
}

func (h *AdminHandler) ProvisionClient(
	ctx context.Context,
	req *gatewayv1.ProvisionClientRequest,
) (*gatewayv1.ProvisionClientResponse, error) {
	if err := h.checkAdminKey(ctx); err != nil {
		provisionsTotal.WithLabelValues("unauthenticated").Inc()
		return nil, err
	}

	if req.ClientId == "" || req.DisplayName == "" {
		provisionsTotal.WithLabelValues("validation_error").Inc()
		return nil, status.Error(codes.InvalidArgument, "client_id and display_name are required")
	}

	result, err := h.provisioner.Provision(ctx, req.ClientId, req.DisplayName)
	if err != nil {
		switch {
		case errors.Is(err, provisioning.ErrInvalidClientID):
			provisionsTotal.WithLabelValues("validation_error").Inc()
			return nil, status.Error(codes.InvalidArgument, err.Error())
		case errors.Is(err, auth.ErrClientExists):
			provisionsTotal.WithLabelValues("already_exists").Inc()
			return nil, status.Errorf(codes.AlreadyExists, "client_id %q is already provisioned", req.ClientId)
		default:
			h.log.Error("client provisioning failed",
				zap.String("client_id", req.ClientId),
				zap.Error(err),
			)
			provisionsTotal.WithLabelValues("internal_error").Inc()
			return nil, status.Error(codes.Internal, "provisioning failed — see server logs for which step, and whether manual cleanup is needed")
		}
	}

	provisionsTotal.WithLabelValues("success").Inc()
	h.log.Info("client provisioned via admin api", zap.String("client_id", result.ClientID))

	return &gatewayv1.ProvisionClientResponse{
		ClientId:            result.ClientID,
		ApiKey:              result.APIKey,
		KafkaTopic:          result.KafkaTopic,
		ScramUsername:       result.SCRAMUsername,
		ScramPassword:       result.SCRAMPassword,
		ConsumerGroupPrefix: result.ConsumerGroupPrefix,
	}, nil
}

// checkAdminKey reads h.adminKeyHeader from gRPC metadata and compares it to
// the configured secret in constant time (hmac.Equal — same rationale as
// hashing.Verify for PAN comparisons: this value gates a privileged
// operation, so timing side-channels matter here even though the secret
// itself isn't a password).
func (h *AdminHandler) checkAdminKey(ctx context.Context) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Errorf(codes.Unauthenticated, "missing %s", h.adminKeyHeader)
	}
	vals := md.Get(h.adminKeyHeader)
	if len(vals) == 0 || vals[0] == "" {
		return status.Errorf(codes.Unauthenticated, "missing %s", h.adminKeyHeader)
	}
	if !hmac.Equal([]byte(vals[0]), h.adminKeySecret) {
		return status.Error(codes.PermissionDenied, "invalid admin key")
	}
	return nil
}
