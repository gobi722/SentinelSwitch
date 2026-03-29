// Package gateway contains the gRPC handler and its supporting types.
package gateway

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sentinelswitch/api-gateway/internal/config"
	gatewayv1 "github.com/sentinelswitch/proto/gateway/v1"
)

// Validator applies field-level business rules to SubmitTransactionRequest.
type Validator struct {
	cfg        config.ValidationConfig
	mccRE      *regexp.Regexp
	rrnRE      *regexp.Regexp
	currencies map[string]struct{}
	txnTypes   map[string]struct{}
	channels   map[string]struct{}
}

// NewValidator builds a Validator from config.
func NewValidator(cfg config.ValidationConfig) (*Validator, error) {
	mccRE, err := regexp.Compile(cfg.MccPattern)
	if err != nil {
		return nil, fmt.Errorf("invalid mcc_pattern %q: %w", cfg.MccPattern, err)
	}
	rrnRE, err := regexp.Compile(cfg.RRNPattern)
	if err != nil {
		return nil, fmt.Errorf("invalid rrn_pattern %q: %w", cfg.RRNPattern, err)
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
		rrnRE:      rrnRE,
		currencies: toSet(cfg.SupportedCurrencies),
		txnTypes:   toSet(cfg.SupportedTransactionTypes),
		channels:   toSet(cfg.SupportedChannels),
	}, nil
}

// ValidateSubmit checks all required fields of a SubmitTransactionRequest.
// Returns a gRPC INVALID_ARGUMENT status on failure.
func (v *Validator) ValidateSubmit(req *gatewayv1.SubmitTransactionRequest) error {
	var errs []string
	add := func(msg string) { errs = append(errs, msg) }

	// --- Idempotency key ---
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		add("idempotency_key: required")
	}

	// --- PAN / card data ---
	pan := strings.ReplaceAll(strings.ReplaceAll(req.Pan, " ", ""), "-", "")
	if len(pan) < 13 || len(pan) > 19 {
		add("pan: must be 13–19 digits")
	} else if !allDigits(pan) {
		add("pan: must contain digits only")
	} else if !luhn(pan) {
		add("pan: failed Luhn check")
	}

	if req.ExpiryMonth < 1 || req.ExpiryMonth > 12 {
		add("expiry_month: must be 1–12")
	}
	if req.ExpiryYear < 2024 || req.ExpiryYear > 2099 {
		add("expiry_year: must be 2024–2099")
	}

	// --- Amount ---
	if req.AmountMinor < v.cfg.AmountMinMinor || req.AmountMinor > v.cfg.AmountMaxMinor {
		add(fmt.Sprintf("amount_minor: must be %d–%d", v.cfg.AmountMinMinor, v.cfg.AmountMaxMinor))
	}

	// --- Currency ---
	if _, ok := v.currencies[strings.ToUpper(req.CurrencyCode)]; !ok {
		add(fmt.Sprintf("currency_code: unsupported value %q", req.CurrencyCode))
	}

	// --- Transaction type ---
	if _, ok := v.txnTypes[strings.ToUpper(req.TransactionType)]; !ok {
		add(fmt.Sprintf("transaction_type: unsupported value %q", req.TransactionType))
	}

	// --- Channel ---
	if _, ok := v.channels[strings.ToUpper(req.Channel)]; !ok {
		add(fmt.Sprintf("channel: unsupported value %q", req.Channel))
	}

	// --- MCC ---
	if req.MerchantCategoryCode != "" && !v.mccRE.MatchString(req.MerchantCategoryCode) {
		add(fmt.Sprintf("merchant_category_code: does not match pattern %s", v.cfg.MccPattern))
	}

	// --- Terminal / Merchant IDs ---
	if utf8.RuneCountInString(req.TerminalId) > v.cfg.TerminalIDMaxLength {
		add(fmt.Sprintf("terminal_id: exceeds max length %d", v.cfg.TerminalIDMaxLength))
	}
	if utf8.RuneCountInString(req.MerchantId) > v.cfg.MerchantIDMaxLength {
		add(fmt.Sprintf("merchant_id: exceeds max length %d", v.cfg.MerchantIDMaxLength))
	}

	// --- RRN (optional) ---
	if req.RetrievalReferenceNumber != "" && !v.rrnRE.MatchString(req.RetrievalReferenceNumber) {
		add(fmt.Sprintf("retrieval_reference_number: does not match pattern %s", v.cfg.RRNPattern))
	}

	if len(errs) > 0 {
		return status.Errorf(codes.InvalidArgument, "validation failed: %s", strings.Join(errs, "; "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// luhn implements the Luhn algorithm.
func luhn(pan string) bool {
	sum := 0
	alt := false
	for i := len(pan) - 1; i >= 0; i-- {
		n := int(pan[i] - '0')
		if alt {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		alt = !alt
	}
	return sum%10 == 0
}
