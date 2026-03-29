package rules

import (
	"fmt"
	"os"
	"time"

	transactionspb "github.com/sentinelswitch/proto/transactions/v1"
	"gopkg.in/yaml.v3"
)

// RuleID identifies a specific fraud rule.
type RuleID string

const (
	RuleHighAmount        RuleID = "HIGH_AMOUNT"
	RuleRoundAmount       RuleID = "ROUND_AMOUNT"
	RuleRiskyMCC          RuleID = "RISKY_MCC"
	RuleCryptoExchangeMCC RuleID = "CRYPTO_EXCHANGE_MCC"
	RuleNightTransaction  RuleID = "NIGHT_TRANSACTION"
	RuleWeekendHighAmount RuleID = "WEEKEND_HIGH_AMOUNT"
	RuleCardNotPresent    RuleID = "CARD_NOT_PRESENT"
	RuleForeignCurrency   RuleID = "FOREIGN_CURRENCY"
)

// rulesFile is a thin struct for parsing the rules YAML file.
type rulesFile struct {
	HighAmount struct {
		Thresholds map[string]int64 `yaml:"thresholds"`
	} `yaml:"high_amount"`
}

// Engine evaluates all fraud rules against a transaction synchronously.
type Engine struct {
	thresholds map[string]int64 // per-currency HIGH_AMOUNT thresholds
}

// riskyMCCs is the set of MCC codes flagged as risky.
var riskyMCCs = map[string]struct{}{
	"7995": {}, "7994": {}, "5999": {}, "6011": {}, "6010": {},
	"4829": {}, "6051": {}, "7273": {}, "5912": {},
}

// cryptoMCCs is the set of MCC codes flagged as crypto/exchange.
var cryptoMCCs = map[string]struct{}{
	"6051": {}, "6099": {},
}

// domesticCurrencies are currencies that are NOT considered foreign.
var domesticCurrencies = map[string]struct{}{
	"OMR": {}, "AED": {}, "SAR": {}, "BHD": {}, "KWD": {}, "QAR": {},
}

// defaultThresholds is the fallback when fraud-rules.yaml is absent or missing
// a currency entry.
var defaultThresholds = map[string]int64{
	"OMR":     500000,
	"INR":     5000000,
	"USD":     500000,
	"AED":     1837500,
	"SAR":     1875000,
	"BHD":     188500,
	"KWD":     153000,
	"DEFAULT": 500000,
}

// NewEngine loads the rules YAML at path and returns an Engine ready to use.
func NewEngine(path string) (*Engine, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("rules: read %q: %w", path, err)
	}

	var rf rulesFile
	if err := yaml.Unmarshal(data, &rf); err != nil {
		return nil, fmt.Errorf("rules: unmarshal: %w", err)
	}

	thresholds := make(map[string]int64, len(defaultThresholds))
	for k, v := range defaultThresholds {
		thresholds[k] = v
	}
	for k, v := range rf.HighAmount.Thresholds {
		thresholds[k] = v
	}

	return &Engine{thresholds: thresholds}, nil
}

// Evaluate runs all 8 fraud rules against txn and returns the list of triggered
// rule IDs in evaluation order.
func (e *Engine) Evaluate(txn *transactionspb.TransactionEvent) []RuleID {
	var triggered []RuleID

	// Parse the ISO 8601 txn_timestamp string.
	var nowUTC time.Time
	if t, err := time.Parse(time.RFC3339, txn.TxnTimestamp); err == nil {
		nowUTC = t.UTC()
	} else {
		nowUTC = time.Now().UTC()
	}

	hour := nowUTC.Hour()
	weekday := nowUTC.Weekday()

	// 1. HIGH_AMOUNT
	highAmountFired := e.checkHighAmount(txn.Currency, txn.AmountMinor)
	if highAmountFired {
		triggered = append(triggered, RuleHighAmount)
	}

	// 2. ROUND_AMOUNT — amount is a multiple of 1.00 (100000 minor units) and > 1.00
	if txn.AmountMinor > 100000 && txn.AmountMinor%100000 == 0 {
		triggered = append(triggered, RuleRoundAmount)
	}

	// 3. RISKY_MCC
	if _, ok := riskyMCCs[txn.Mcc]; ok {
		triggered = append(triggered, RuleRiskyMCC)
	}

	// 4. CRYPTO_EXCHANGE_MCC
	if _, ok := cryptoMCCs[txn.Mcc]; ok {
		triggered = append(triggered, RuleCryptoExchangeMCC)
	}

	// 5. NIGHT_TRANSACTION — UTC hour in [0, 5)
	if hour >= 0 && hour < 5 {
		triggered = append(triggered, RuleNightTransaction)
	}

	// 6. WEEKEND_HIGH_AMOUNT — Saturday or Sunday AND HIGH_AMOUNT triggered
	if highAmountFired && (weekday == time.Saturday || weekday == time.Sunday) {
		triggered = append(triggered, RuleWeekendHighAmount)
	}

	// 7. CARD_NOT_PRESENT — channel PG = proto enum value 2
	if int32(txn.Channel) == 2 {
		triggered = append(triggered, RuleCardNotPresent)
	}

	// 8. FOREIGN_CURRENCY — currency not in the domestic set
	if _, ok := domesticCurrencies[txn.Currency]; !ok {
		triggered = append(triggered, RuleForeignCurrency)
	}

	return triggered
}

func (e *Engine) checkHighAmount(currency string, amountMinor int64) bool {
	threshold, ok := e.thresholds[currency]
	if !ok {
		threshold = e.thresholds["DEFAULT"]
	}
	return amountMinor > threshold
}

// IsNight returns 1 if the ISO 8601 timestamp falls in the night window [0,5) UTC.
func IsNight(txnTimestamp string) int32 {
	t, err := time.Parse(time.RFC3339, txnTimestamp)
	if err != nil {
		return 0
	}
	h := t.UTC().Hour()
	if h >= 0 && h < 5 {
		return 1
	}
	return 0
}

// IsRiskyMCC returns 1 if the MCC code is in the risky set.
func IsRiskyMCC(mcc string) int32 {
	if _, ok := riskyMCCs[mcc]; ok {
		return 1
	}
	return 0
}

// IsCardNotPresent returns 1 if the channel proto enum value indicates PG (2).
func IsCardNotPresent(channel int32) int32 {
	if channel == 2 {
		return 1
	}
	return 0
}
