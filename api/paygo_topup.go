package api

import "strings"

type fixedTopUp struct {
	currency string
	amount   int64
}

var fixedTopUps = map[string]fixedTopUp{
	"PAYG-TOPUP-50":      {currency: "CNY", amount: 5000},
	"PAYG-TOPUP-100":     {currency: "CNY", amount: 10000},
	"PAYG-TOPUP-200":     {currency: "CNY", amount: 20000},
	"PAYG-TOPUP-USD-20":  {currency: "USD", amount: 2000},
	"PAYG-TOPUP-USD-50":  {currency: "USD", amount: 5000},
	"PAYG-TOPUP-USD-100": {currency: "USD", amount: 10000},
}

const (
	customCNYTopUpPlanID = "PAYG-CUSTOM-CNY"
	customUSDTopUpPlanID = "PAYG-CUSTOM-USD"
	customCNYMinimum     = int64(1000)  // ¥10.00
	customCNYMaximum     = int64(20000) // ¥200.00
	customUSDMinimum     = int64(500)   // $5.00
	customUSDMaximum     = int64(10000) // $100.00
)

func isFixedPaygoTopUp(planID string, amount int64, currency, unit string) bool {
	expected, ok := fixedTopUps[strings.ToUpper(strings.TrimSpace(planID))]
	return ok && amount == expected.amount && strings.EqualFold(strings.TrimSpace(currency), expected.currency) && strings.EqualFold(strings.TrimSpace(unit), "once")
}

func isCustomPaygoTopUp(planID string, amount int64, currency string) bool {
	switch strings.ToUpper(strings.TrimSpace(planID)) {
	case customCNYTopUpPlanID:
		return strings.EqualFold(strings.TrimSpace(currency), "CNY") && amount >= customCNYMinimum && amount <= customCNYMaximum
	case customUSDTopUpPlanID:
		return strings.EqualFold(strings.TrimSpace(currency), "USD") && amount >= customUSDMinimum && amount <= customUSDMaximum
	default:
		return false
	}
}
