package scoring

import (
	"fmt"
	"math"

	riskpb "github.com/sentinelswitch/proto/risk/v1"
)

// NormaliseConfig describes how a raw feature value is normalised to [0, 1].
type NormaliseConfig struct {
	Method string  // "log10", "linear_cap", "binary", "passthrough"
	Cap    float64
}

// FeatureConfig describes a single feature in the scoring model.
type FeatureConfig struct {
	Name      string
	Weight    float64
	Normalise NormaliseConfig
}

// Config holds the complete configuration for the Scorer.
type Config struct {
	Features                    []FeatureConfig
	ApproveBelowScore           int32
	DeclineAboveScore           int32
	ContributingFactorThreshold float64
}

// Result is the output of a single scoring operation.
type Result struct {
	Score          int32
	Decision       string   // "APPROVE", "REVIEW", "DECLINE"
	ContribFactors []string // feature names whose contribution exceeds threshold
}

// Scorer implements the weighted-linear fraud risk scoring model.
type Scorer struct {
	cfg Config
}

// New validates cfg and returns a ready Scorer.
// Returns an error if the sum of all feature weights exceeds 1.0.
func New(cfg Config) (*Scorer, error) {
	var totalWeight float64
	for _, f := range cfg.Features {
		totalWeight += f.Weight
	}
	if totalWeight > 1.0+1e-9 {
		return nil, fmt.Errorf(
			"scoring: sum of feature weights %.4f exceeds 1.0",
			totalWeight,
		)
	}
	if len(cfg.Features) == 0 {
		return nil, fmt.Errorf("scoring: at least one feature must be configured")
	}
	return &Scorer{cfg: cfg}, nil
}

// Score computes a risk score and decision for the given feature vector.
func (s *Scorer) Score(features *riskpb.FraudFeatures, amountMinor int64) Result {
	type contrib struct {
		name  string
		value float64
	}

	var weightedSum float64
	contribs := make([]contrib, 0, len(s.cfg.Features))

	for _, fc := range s.cfg.Features {
		raw := extractRaw(fc.Name, features, amountMinor)
		norm := normalise(raw, fc.Normalise)
		c := norm * fc.Weight
		weightedSum += c
		contribs = append(contribs, contrib{name: fc.Name, value: c})
	}

	// Scale [0, 1] → [100, 1000]
	rawScore := int32(math.Round(100 + weightedSum*900))
	if rawScore < 100 {
		rawScore = 100
	}
	if rawScore > 1000 {
		rawScore = 1000
	}

	var decision string
	switch {
	case rawScore < s.cfg.ApproveBelowScore:
		decision = "APPROVE"
	case rawScore > s.cfg.DeclineAboveScore:
		decision = "DECLINE"
	default:
		decision = "REVIEW"
	}

	var factors []string
	for i, fc := range s.cfg.Features {
		threshold := s.cfg.ContributingFactorThreshold * fc.Weight
		if contribs[i].value > threshold {
			factors = append(factors, fc.Name)
		}
	}

	return Result{
		Score:          rawScore,
		Decision:       decision,
		ContribFactors: factors,
	}
}

// extractRaw maps a feature name to its raw float64 value from the proto.
func extractRaw(name string, f *riskpb.FraudFeatures, amountMinor int64) float64 {
	switch name {
	case "amount_minor":
		return float64(amountMinor)
	case "txn_count_1min":
		return float64(f.TxnCount_1Min)
	case "txn_count_5min":
		return float64(f.TxnCount_5Min)
	case "txn_count_1hr":
		return float64(f.TxnCount_1Hr)
	case "amount_sum_1min":
		return float64(f.AmountSum_1Min)
	case "amount_sum_1hr":
		return float64(f.AmountSum_1Hr)
	case "unique_merchants_5min":
		return float64(f.UniqueMerchants_5Min)
	case "unique_terminals_5min":
		return float64(f.UniqueTerminals_5Min)
	case "merchant_risk":
		return f.MerchantRisk
	case "is_night":
		return float64(f.IsNight)
	case "is_risky_mcc":
		return float64(f.IsRiskyMcc)
	case "is_card_not_present":
		return float64(f.IsCardNotPresent)
	default:
		return 0
	}
}

// normalise maps a raw value to [0, 1] using the configured method.
func normalise(v float64, cfg NormaliseConfig) float64 {
	var result float64
	switch cfg.Method {
	case "log10":
		if cfg.Cap <= 1 {
			return 0
		}
		safe := math.Max(v, 1)
		result = math.Log10(safe) / math.Log10(cfg.Cap)
	case "linear_cap":
		if cfg.Cap == 0 {
			return 0
		}
		result = math.Min(v, cfg.Cap) / cfg.Cap
	case "binary":
		if v > 0 {
			return 1.0
		}
		return 0.0
	default: // passthrough
		result = v
	}
	if result < 0 {
		return 0
	}
	if result > 1 {
		return 1
	}
	return result
}
