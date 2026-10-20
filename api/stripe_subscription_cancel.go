package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"account/internal/store"
)

// Cancellation is separate from change cooldown. An ambiguous provider result
// stays reserved; retries query before replay and never submit an expired key.
func (h *handler) cancelStripeSubscription(ctx context.Context, sub *store.Subscription) error {
	key := "subscription-cancel:" + sub.ID
	raw, _ := json.Marshal(map[string]string{"subscriptionId": sub.ID, "externalId": sub.ExternalID})
	op, err := h.store.ReserveSubscriptionAction(ctx, sub.UserID, key, "cancel", raw)
	if err != nil {
		return err
	}
	if op.Status == store.FinanceOperationSucceeded {
		return nil
	}
	if op.Status == store.FinanceOperationFailed {
		return errors.New("cancellation requires audited investigation")
	}
	remote, err := h.stripe.fetchSubscription(ctx, sub.ExternalID)
	if err != nil {
		return err
	}
	if remote.ID != sub.ExternalID || remote.Metadata["user_id"] != sub.UserID {
		return errors.New("cancellation ownership mismatch")
	}
	if customerIDFromAny(remote.Schedule) != "" {
		return errors.New("schedule ownership requires investigation before cancellation")
	}
	if !remote.CancelAtPeriodEnd {
		if time.Since(op.CreatedAt) < 0 || time.Since(op.CreatedAt) > 20*time.Minute {
			return errors.New("original cancellation requires provider reconciliation")
		}
		if err = h.stripe.doForm(ctx, http.MethodPost, "/subscriptions/"+url.PathEscape(sub.ExternalID), url.Values{"cancel_at_period_end": {"true"}}, &remote, key); err != nil {
			return err
		}
	}
	_, end := subscriptionPeriod(remote)
	if remote.ID != sub.ExternalID || !remote.CancelAtPeriodEnd || !end.After(time.Now().UTC()) {
		return errors.New("cancellation has not been confirmed for a valid paid term")
	}
	sub.Status = "canceling"
	if sub.Meta == nil {
		sub.Meta = make(map[string]any)
	}
	sub.Meta["cancelAtPeriodEnd"] = "true"
	sub.Meta["expiresAt"] = end.UTC().Format(time.RFC3339)
	if err = h.store.UpsertSubscription(ctx, sub); err != nil {
		return err
	}
	effective := end.UTC()
	receipt, _ := json.Marshal(map[string]any{"outcome": "cancel_scheduled", "effectiveAt": effective})
	return h.store.FinishSubscriptionAction(ctx, sub.UserID, op.ID, sub.ExternalID, receipt, true, "FREE", &effective)
}
