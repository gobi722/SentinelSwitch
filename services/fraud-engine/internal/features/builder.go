package features

import (
	riskpb "github.com/sentinelswitch/proto/risk/v1"
	transactionspb "github.com/sentinelswitch/proto/transactions/v1"

	"github.com/sentinelswitch/fraud-engine/internal/rules"
	"github.com/sentinelswitch/fraud-engine/internal/velocity"
)

// Build assembles a riskpb.FraudFeatures from the transaction context, velocity
// metrics, merchant risk score, and the set of triggered rule IDs.
func Build(
	txn *transactionspb.TransactionEvent,
	vel *velocity.Result,
	merchantRisk float64,
	triggeredRules []rules.RuleID,
) *riskpb.FraudFeatures {
	return &riskpb.FraudFeatures{
		TxnCount_1Min:        vel.TxnCount1Min,
		TxnCount_5Min:        vel.TxnCount5Min,
		TxnCount_1Hr:         vel.TxnCount1Hr,
		AmountSum_1Min:       vel.AmountSum1Min,
		AmountSum_1Hr:        vel.AmountSum1Hr,
		UniqueMerchants_5Min: vel.UniqueMerchants5Min,
		UniqueTerminals_5Min: vel.UniqueTerminals5Min,
		MerchantRisk:         merchantRisk,
		IsNight:              rules.IsNight(txn.TxnTimestamp),
		IsRiskyMcc:           rules.IsRiskyMCC(txn.Mcc),
		IsCardNotPresent:     rules.IsCardNotPresent(int32(txn.Channel)),
	}
}
