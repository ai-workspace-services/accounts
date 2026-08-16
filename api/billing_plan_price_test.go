package api

import (
	"context"
	"testing"

	"account/internal/store"
)

// The catalog had no price column, so the ops console's "adjust price" only
// ever repointed stripe_price_id and the amounts shown to buyers lived in
// frontend copy. These tests pin the new fields down at the two places that
// matter: what a plan round-trips through the store, and what the payload
// hands the storefront.
func TestBillingPlanRoundTripsListPrice(t *testing.T) {
	st := store.NewMemoryStore()
	ctx := context.Background()

	plan := &store.BillingPlan{
		PlanID:        "PRO-MONTHLY",
		DisplayName:   "Pro Monthly",
		Kind:          "subscription",
		PackageName:   "pro",
		PriceAmount:   2000, // ¥20.00
		PriceCurrency: "CNY",
		PriceUnit:     "month",
		Active:        true,
	}
	if err := st.UpsertBillingPlan(ctx, plan); err != nil {
		t.Fatalf("upsert plan: %v", err)
	}

	stored, err := st.GetBillingPlan(ctx, "PRO-MONTHLY")
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if stored.PriceAmount != 2000 {
		t.Fatalf("price amount = %d, want 2000", stored.PriceAmount)
	}
	if stored.PriceCurrency != "CNY" {
		t.Fatalf("price currency = %q, want CNY", stored.PriceCurrency)
	}
	if stored.PriceUnit != "month" {
		t.Fatalf("price unit = %q, want month", stored.PriceUnit)
	}
}

func TestBillingPlanPayloadCarriesListPrice(t *testing.T) {
	payload := billingPlanToPayload(&store.BillingPlan{
		PlanID:        "PAYG-TOPUP-50",
		Kind:          "paygo_topup",
		PriceAmount:   5000,
		PriceCurrency: "CNY",
		PriceUnit:     "once",
	})

	if payload.PriceAmount != 5000 {
		t.Fatalf("payload price amount = %d, want 5000", payload.PriceAmount)
	}
	if payload.PriceCurrency != "CNY" || payload.PriceUnit != "once" {
		t.Fatalf("payload price = %s %s, want CNY once", payload.PriceCurrency, payload.PriceUnit)
	}
}

// A plan with no published price must serialize as zero rather than as a
// missing field the storefront could mistake for "free".
func TestBillingPlanPayloadKeepsUnpricedPlansExplicit(t *testing.T) {
	payload := billingPlanToPayload(&store.BillingPlan{PlanID: "FREE", Kind: "subscription"})
	if payload.PriceAmount != 0 {
		t.Fatalf("unpriced plan amount = %d, want 0", payload.PriceAmount)
	}
	if payload.PriceCurrency != "" || payload.PriceUnit != "" {
		t.Fatalf("unpriced plan should carry no currency/unit, got %q %q",
			payload.PriceCurrency, payload.PriceUnit)
	}
}
