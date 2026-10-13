package api

import (
	"context"
	"testing"

	"account/internal/store"
)

func TestStripeAmountToCurrencyMinorNormalizesProviderRepresentations(t *testing.T) {
	for _, tc := range []struct {
		currency     string
		amount, want int64
	}{
		{"ISK", 100, 1}, {"UGX", 2500, 25}, {"MGA", 100, 10000},
		{"JPY", 100, 100}, {"CNY", 1000, 1000}, {"USD", 1000, 1000},
	} {
		got, err := stripeAmountToCurrencyMinor(tc.currency, tc.amount)
		if err != nil || got != tc.want {
			t.Errorf("%s amount %d => %d, %v; want %d", tc.currency, tc.amount, got, err, tc.want)
		}
	}
	if _, err := stripeAmountToCurrencyMinor("ISK", 101); err == nil {
		t.Fatal("fractional ISK amount should fail closed")
	}
	if _, err := stripeAmountToCurrencyMinor("MGA", int64(^uint64(0)>>1)); err == nil {
		t.Fatal("MGA overflow should fail closed")
	}
}

func TestReferralPlanUsesImmutableInvoicePriceInsteadOfCurrentSubscription(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	for _, plan := range []store.BillingPlan{
		{PlanID: "PRO-MONTHLY", StripePriceID: "price_month", Kind: "subscription", PriceUnit: "month"},
		{PlanID: "PRO-YEARLY", StripePriceID: "price_year", Kind: "subscription", PriceUnit: "year"},
	} {
		if err := st.UpsertBillingPlan(ctx, &plan); err != nil {
			t.Fatal(err)
		}
	}
	h := &handler{store: st}
	monthly := &stripeInvoice{}
	monthly.Lines.Data = append(monthly.Lines.Data, struct {
		Price  any `json:"price"`
		Period struct {
			Start int64 `json:"start"`
			End   int64 `json:"end"`
		} `json:"period"`
		Pricing struct {
			PriceDetails struct {
				Price any `json:"price"`
			} `json:"price_details"`
		} `json:"pricing"`
	}{Price: "price_month"})
	planID, err := h.referralPlanFromInvoice(ctx, monthly)
	if err != nil || planID != "PRO-MONTHLY" {
		t.Fatalf("monthly invoice should stay monthly after a current subscription upgrade: plan=%q err=%v", planID, err)
	}
	missing := &stripeInvoice{}
	if _, err = h.referralPlanFromInvoice(ctx, missing); err == nil {
		t.Fatal("invoice without immutable price lines must fail closed")
	}
}
