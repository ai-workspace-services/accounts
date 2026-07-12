package store

import (
	"encoding/json"
	"time"
)

// BillingEvent is the compact lifecycle notification accounts publishes to the
// shared-PG PGMQ queue (extension pgmq v1.8.0, shipped in the
// postgresql.svc.plus runtime image). Consumers — billing-service reconcile,
// dunning, notifications — pop from pgmq without accounts knowing them.
type BillingEvent struct {
	Type       string    `json:"type"` // subscription_activated | subscription_updated | invoice_paid | payment_failed | subscription_deleted | trial_provisioned | arrears_cleared
	UserID     string    `json:"userId"`
	PlanID     string    `json:"planId,omitempty"`
	PriceID    string    `json:"priceId,omitempty"`
	ExternalID string    `json:"externalId,omitempty"`
	OccurredAt time.Time `json:"occurredAt"`
}

// BillingEventQueueName is the PGMQ queue accounts publishes to.
const BillingEventQueueName = "billing_events"

func (e *BillingEvent) payload() (json.RawMessage, error) {
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	return json.Marshal(e)
}
