package store

import (
	"encoding/json"
	"testing"
)

// 这四个结构体是 /api/account/{usage/buckets,billing/summary} 的响应体本身,
// 不是内部模型。字段名一旦回到 Go 默认的大驼峰, 前端读到的每个字段都是
// undefined —— 而 ledger 为空时没有任何症状, 直到真有账目才在
// entry.ratedBytes.toLocaleString() 上整页崩掉。
// 这里逐个字段断言, 就是不让那次静默漂移再发生一遍。
func TestAccountingResponsesUseCamelCaseFieldNames(t *testing.T) {
	cases := []struct {
		name  string
		value any
		keys  []string
	}{
		{
			name:  "TrafficMinuteBucket",
			value: TrafficMinuteBucket{},
			keys: []string{
				"bucketStart", "nodeId", "accountUuid", "region", "lineCode",
				"uplinkBytes", "downlinkBytes", "totalBytes", "multiplier",
				"ratingStatus", "sourceRevision", "createdAt", "updatedAt",
			},
		},
		{
			name:  "BillingLedgerEntry",
			value: BillingLedgerEntry{},
			keys: []string{
				"id", "accountUuid", "bucketStart", "bucketEnd", "entryType",
				"ratedBytes", "amountDelta", "balanceAfter",
				"pricingRuleVersion", "createdAt",
			},
		},
		{
			name:  "AccountQuotaState",
			value: AccountQuotaState{},
			keys: []string{
				"accountUuid", "remainingIncludedQuota", "currentBalance",
				"arrears", "arrearsSince", "throttleState", "suspendState",
				"lastRatedBucketAt", "periodStart", "periodEnd", "effectiveAt",
				"updatedAt",
			},
		},
		{
			name:  "AccountBillingProfile",
			value: AccountBillingProfile{},
			keys: []string{
				"accountUuid", "packageName", "includedQuotaBytes",
				"basePricePerByte", "regionMultiplier", "lineMultiplier",
				"peakMultiplier", "offPeakMultiplier", "pricingRuleVersion",
				"createdAt", "updatedAt",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatalf("marshal %s: %v", tc.name, err)
			}
			var decoded map[string]json.RawMessage
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.name, err)
			}
			for _, key := range tc.keys {
				if _, ok := decoded[key]; !ok {
					t.Errorf("%s is missing JSON field %q; got %v", tc.name, key, keysOf(decoded))
				}
			}
			if len(decoded) != len(tc.keys) {
				t.Errorf("%s serializes %d fields, expected %d: %v",
					tc.name, len(decoded), len(tc.keys), keysOf(decoded))
			}
		})
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
