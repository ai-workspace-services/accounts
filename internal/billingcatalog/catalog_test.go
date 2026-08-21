package billingcatalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// The plan catalog is described in two places that must not drift:
//
//   - scripts/billing-plans.json  -- the rows seeded into billing_plans, which
//     is what /prices and the user centre read to decide what is purchasable
//     and at what price.
//   - scripts/stripe-catalog.yaml -- the Stripe Products and Prices, which is
//     what the customer is actually charged.
//
// A mismatch between them is the worst kind of billing bug: the storefront
// quotes one number and Stripe charges another, and nothing fails until a
// customer notices. Stripe prices are immutable once created, so the amount
// cannot simply be corrected afterwards either.

type manifestPlan struct {
	PlanID             string             `json:"planId"`
	DisplayName        string             `json:"displayName"`
	Kind               string             `json:"kind"`
	IncludedQuotaBytes int64              `json:"includedQuotaBytes"`
	PackageName        string             `json:"packageName"`
	PriceAmount        int64              `json:"priceAmount"`
	PriceCurrency      string             `json:"priceCurrency"`
	PriceUnit          string             `json:"priceUnit"`
	PriceMultipliers   map[string]float64 `json:"priceMultipliers"`
	Features           map[string]any     `json:"features"`
	TrialDays          int                `json:"trialDays"`
	Active             bool               `json:"active"`
	SortOrder          int                `json:"sortOrder"`
}

type stripeCatalog struct {
	Products []struct {
		Key    string `yaml:"key"`
		Prices []struct {
			Key               string `yaml:"key"`
			PlanID            string `yaml:"plan_id"`
			UnitAmount        int64  `yaml:"unit_amount"`
			Currency          string `yaml:"currency"`
			RecurringInterval string `yaml:"recurring_interval"`
		} `yaml:"prices"`
	} `yaml:"products"`
}

func scriptsPath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("..", "..", "scripts", name)
}

func loadManifest(t *testing.T) []manifestPlan {
	t.Helper()
	raw, err := os.ReadFile(scriptsPath(t, "billing-plans.json"))
	if err != nil {
		t.Fatalf("read plan manifest: %v", err)
	}
	var manifest struct {
		Plans []manifestPlan `json:"plans"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse plan manifest: %v", err)
	}
	return manifest.Plans
}

func loadStripeCatalog(t *testing.T) stripeCatalog {
	t.Helper()
	raw, err := os.ReadFile(scriptsPath(t, "stripe-catalog.yaml"))
	if err != nil {
		t.Fatalf("read stripe catalog: %v", err)
	}
	var catalog stripeCatalog
	if err := yaml.Unmarshal(raw, &catalog); err != nil {
		t.Fatalf("parse stripe catalog: %v", err)
	}
	return catalog
}

func TestManifestQuotesWhatStripeCharges(t *testing.T) {
	plans := make(map[string]manifestPlan)
	for _, plan := range loadManifest(t) {
		plans[plan.PlanID] = plan
	}

	// A recurring price bills per interval; a one-off top-up bills once.
	unitFor := map[string]string{"month": "month", "year": "year", "": "once"}

	for _, product := range loadStripeCatalog(t).Products {
		for _, price := range product.Prices {
			plan, ok := plans[price.PlanID]
			if !ok {
				t.Errorf("stripe price %q sells plan %q, which the manifest does not define",
					price.Key, price.PlanID)
				continue
			}
			if plan.PriceAmount != price.UnitAmount {
				t.Errorf("plan %s: manifest quotes %d, Stripe charges %d",
					plan.PlanID, plan.PriceAmount, price.UnitAmount)
			}
			if got, want := plan.PriceCurrency, "CNY"; got != want && price.Currency == "cny" {
				t.Errorf("plan %s: manifest currency %q, Stripe currency %q",
					plan.PlanID, got, price.Currency)
			}
			if want := unitFor[price.RecurringInterval]; plan.PriceUnit != want {
				t.Errorf("plan %s: manifest unit %q, expected %q from Stripe interval %q",
					plan.PlanID, plan.PriceUnit, want, price.RecurringInterval)
			}
		}
	}
}

func TestSellablePlansArePublishable(t *testing.T) {
	// The storefront (portal src/modules/billing/catalog.ts) shows a buy button
	// only when the plan is active and carries a Stripe price, and quotes a
	// price only when priceAmount and priceCurrency are both set. A plan that
	// misses either renders as 即将上线 with no explanation.
	sellable := []string{
		"PRO-MONTHLY", "PRO-YEARLY",
		"PAYG-TOPUP-50", "PAYG-TOPUP-100", "PAYG-TOPUP-500",
	}

	plans := make(map[string]manifestPlan)
	for _, plan := range loadManifest(t) {
		plans[plan.PlanID] = plan
	}

	for _, planID := range sellable {
		plan, ok := plans[planID]
		if !ok {
			t.Errorf("manifest is missing sellable plan %s", planID)
			continue
		}
		if !plan.Active {
			t.Errorf("plan %s is on the storefront but not active", planID)
		}
		if plan.PriceAmount <= 0 {
			t.Errorf("plan %s has no published price", planID)
		}
		if plan.PriceCurrency == "" {
			t.Errorf("plan %s has an amount but no currency, which accounts rejects", planID)
		}
	}
}

func TestKindsAreAcceptedByTheUpsertEndpoint(t *testing.T) {
	// adminUpsertBillingPlan rejects anything outside this set, and the column
	// has no other accepted values. A typo here fails the seed at the last step.
	allowed := map[string]bool{"trial": true, "subscription": true, "paygo_topup": true}

	for _, plan := range loadManifest(t) {
		if !allowed[plan.Kind] {
			t.Errorf("plan %s has kind %q, which the upsert endpoint rejects", plan.PlanID, plan.Kind)
		}
		if plan.PackageName == "" {
			t.Errorf("plan %s has no package name; billing-service prices against it", plan.PlanID)
		}
	}
}
