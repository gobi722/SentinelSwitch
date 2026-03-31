package gateway

import (
	"fmt"
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sentinelswitch/api-gateway/internal/config"
	gatewayv1 "github.com/sentinelswitch/proto/gateway/v1"
)

type Validator struct {
	cfg        config.ValidationConfig
	mccRE      *regexp.Regexp
	currencies map[string]struct{}
}

func NewValidator(cfg config.ValidationConfig) (*Validator, error) {
	mccRE, err := regexp.Compile(cfg.MccPattern)
	if err != nil {
		return nil, fmt.Errorf("invalid mcc_pattern %q: %w", cfg.MccPattern, err)
	}

	toSet := func(ss []string) map[string]struct{} {
		m := make(map[string]struct{}, len(ss))
		for _, s := range ss {
			m[strings.ToUpper(s)] = struct{}{}
		}
		return m
	}

	return &Validator{
		cfg:        cfg,
		mccRE:      mccRE,
		currencies: toSet(cfg.SupportedCurrencies),
	}, nil
}

func (v *Validator) ValidateSubmit(req *gatewayv1.TransactionRequest) error {
	var errs []string
	add := func(msg string) { errs = append(errs, msg) }

	// --- Idempotency key ---
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		add("idempotency_key: required")
	}

	// --- Card hash (raw PAN never sent — client sends card_hash) ---
	if strings.TrimSpace(req.CardHash) == "" {
		add("card_hash: required")
	}

	// --- PAN last4 ---
	if len(req.PanLast4) != 4 || !allDigits(req.PanLast4) {
		add("pan_last4: must be exactly 4 digits")
	}

	// --- Amount ---
	if req.AmountMinor < v.cfg.AmountMinMinor || req.AmountMinor > v.cfg.AmountMaxMinor {
		add(fmt.Sprintf("amount_minor: must be %d–%d", v.cfg.AmountMinMinor, v.cfg.AmountMaxMinor))
	}

	// --- Currency ---
	if _, ok := v.currencies[strings.ToUpper(req.Currency)]; !ok {
		add(fmt.Sprintf("currency: unsupported value %q", req.Currency))
	}

	// --- Transaction type (enum — 0 means unspecified) ---
	if req.TransactionType == gatewayv1.TransactionType_TRANSACTION_TYPE_UNSPECIFIED {
		add("transaction_type: required")
	}

	// --- Channel (enum — 0 means unspecified) ---
	if req.Channel == gatewayv1.Channel_CHANNEL_UNSPECIFIED {
		add("channel: required")
	}

	// --- MCC (optional but must match pattern if provided) ---
	if req.Mcc != "" && !v.mccRE.MatchString(req.Mcc) {
		add(fmt.Sprintf("mcc: does not match pattern %s", v.cfg.MccPattern))
	}

	// --- Terminal / Merchant IDs ---
	if len(req.TerminalId) > v.cfg.TerminalIDMaxLength {
		add(fmt.Sprintf("terminal_id: exceeds max length %d", v.cfg.TerminalIDMaxLength))
	}
	if len(req.MerchantId) > v.cfg.MerchantIDMaxLength {
		add(fmt.Sprintf("merchant_id: exceeds max length %d", v.cfg.MerchantIDMaxLength))
	}

	if len(errs) > 0 {
		return status.Errorf(codes.InvalidArgument, "validation failed: %s", strings.Join(errs, "; "))
	}
	return nil
}

// allDigits returns true if every character in s is 0–9.
func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}