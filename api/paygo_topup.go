package api

import "strings"

type fixedTopUp struct {
	currency string
	amount   int64
}

var fixedTopUps = map[string]fixedTopUp{
	"PAYG-TOPUP-10":      {currency: "CNY", amount: 1000},
	"PAYG-TOPUP-50":      {currency: "CNY", amount: 5000},
	"PAYG-TOPUP-100":     {currency: "CNY", amount: 10000},
	"PAYG-TOPUP-500":     {currency: "CNY", amount: 50000},
	"PAYG-TOPUP-USD-5":   {currency: "USD", amount: 500},
	"PAYG-TOPUP-USD-10":  {currency: "USD", amount: 1000},
	"PAYG-TOPUP-USD-20":  {currency: "USD", amount: 2000},
	"PAYG-TOPUP-USD-100": {currency: "USD", amount: 10000},
}

func isFixedPaygoTopUp(planID string, amount int64, currency, unit string) bool {
	expected, ok := fixedTopUps[strings.ToUpper(strings.TrimSpace(planID))]
	return ok && amount == expected.amount && strings.EqualFold(strings.TrimSpace(currency), expected.currency) && strings.EqualFold(strings.TrimSpace(unit), "once")
}
