package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"account/internal/store"

	"github.com/gin-gonic/gin"
)

type stripeFinalizeFaultStore struct {
	store.Store
	attempts int
}

func (s *stripeFinalizeFaultStore) FinishStripeWebhookEvent(ctx context.Context, id string, procErr error) error {
	s.attempts++
	if s.attempts == 1 {
		return errors.New("injected database failure")
	}
	return s.Store.FinishStripeWebhookEvent(ctx, id, procErr)
}

func TestStripeWebhookFinalizationFailureRequestsRetry(t *testing.T) {
	st := &stripeFinalizeFaultStore{Store: store.NewMemoryStore()}
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithStripeConfig(StripeConfig{
		SecretKey: "sk_test_fixture", WebhookSecret: testStripeWebhookSecret,
	}))
	payload := []byte(`{"id":"evt_finalize_retry","type":"fixture.noop","livemode":false,"data":{"object":{}}}`)
	first := postStripeEvent(t, router, payload)
	if first.Code != http.StatusServiceUnavailable || !strings.Contains(first.Body.String(), "stripe_event_finalize_failed") {
		t.Fatalf("expected retryable failure after lost finalization: %d %s", first.Code, first.Body.String())
	}
	if st.attempts != 1 {
		t.Fatalf("expected one finalization attempt, got %d", st.attempts)
	}
	// A real Stripe redelivery reaches processing again because the first
	// receipt was never marked processed. No operator edits are needed.
	second := postStripeEvent(t, router, payload)
	if second.Code != http.StatusOK || st.attempts != 2 || strings.Contains(second.Body.String(), "duplicate") {
		t.Fatalf("redelivery did not finish processing: %d %s attempts=%d", second.Code, second.Body.String(), st.attempts)
	}
	third := postStripeEvent(t, router, payload)
	if third.Code != http.StatusOK || !strings.Contains(third.Body.String(), `"duplicate":true`) || st.attempts != 2 {
		t.Fatalf("settled event was not deduplicated: %d %s attempts=%d", third.Code, third.Body.String(), st.attempts)
	}
}
