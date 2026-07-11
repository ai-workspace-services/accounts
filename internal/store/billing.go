package store

import (
	"encoding/json"
	"errors"
	"time"
)

// BillingPlan is the data-driven plan catalog entry that maps a Stripe price
// to the entitlements billing-service rates against. Adjusting prices or
// quotas is a catalog edit, never a deploy.
type BillingPlan struct {
	PlanID             string
	StripePriceID      string
	DisplayName        string
	Kind               string // trial | subscription | paygo_topup
	IncludedQuotaBytes int64
	PackageName        string
	PriceMultipliers   map[string]float64 // region/line/peak/offpeak, default 1.0
	Features           map[string]any
	TrialDays          int
	Active             bool
	SortOrder          int
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// Multiplier returns a named price multiplier with a 1.0 fallback.
func (p *BillingPlan) Multiplier(name string) float64 {
	if p == nil || p.PriceMultipliers == nil {
		return 1.0
	}
	if v, ok := p.PriceMultipliers[name]; ok && v > 0 {
		return v
	}
	return 1.0
}

// StripeWebhookEvent is the audit/dedup record for inbound Stripe events.
// Events are persisted before processing so replays are idempotent and
// failures leave an inspectable trail.
type StripeWebhookEvent struct {
	EventID     string
	EventType   string
	Payload     json.RawMessage
	Status      string // received | processed | failed
	LastError   string
	ReceivedAt  time.Time
	ProcessedAt *time.Time
}

const (
	StripeWebhookEventStatusReceived  = "received"
	StripeWebhookEventStatusProcessed = "processed"
	StripeWebhookEventStatusFailed    = "failed"
)

// Well-known catalog plan ids provisioned by the seed.
const (
	BillingPlanTrial7D = "TRIAL-7D"
	BillingPlanFree    = "FREE"
)

var (
	ErrBillingPlanNotFound = errors.New("billing plan not found")
)
