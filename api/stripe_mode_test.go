package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"account/internal/store"

	"github.com/gin-gonic/gin"
)

func TestStripeRuntimeModeConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, key, mode string
		live, enabled   bool
	}{
		{"secret sandbox", "sk_test_fixture", "", false, true},
		{"secret live", "sk_live_fixture", "", true, true},
		{"restricted sandbox", "rk_test_fixture", "sandbox", false, true},
		{"restricted live", "rk_live_fixture", "live", true, true},
		{"test alias", "sk_test_fixture", "test", false, true},
		{"normalized mode", "sk_test_fixture", " SANDBOX ", false, true},
		{"test key live mode", "sk_test_fixture", "live", false, false},
		{"live key test mode", "sk_live_fixture", "test", false, false},
		{"restricted conflict", "rk_live_fixture", "sandbox", false, false},
		{"unknown mode", "sk_test_fixture", "prod-v4", false, false},
		{"publishable key", "pk_test_fixture", "test", false, false},
		{"organization key", "sk_org_fixture", "test", false, false},
		{"unknown key", "unrecognized_fixture", "test", false, false},
		{"no key", "", "test", false, false},
		{"empty key suffix", "sk_test_", "test", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newStripeClient(StripeConfig{SecretKey: tc.key, Mode: tc.mode})
			if client.enabled() != tc.enabled {
				t.Fatalf("enabled=%v, want %v", client.enabled(), tc.enabled)
			}
			if tc.enabled && client.liveMode != tc.live {
				t.Fatalf("live=%v, want %v", client.liveMode, tc.live)
			}
		})
	}
}

type stripeEnvelopeStore struct {
	store.Store
	received int
}

func (s *stripeEnvelopeStore) BeginStripeWebhookEvent(ctx context.Context, event *store.StripeWebhookEvent) (bool, error) {
	s.received++
	return s.Store.BeginStripeWebhookEvent(ctx, event)
}

// All live keys and events below are fixtures. These requests run solely in the
// local Gin handler and must not issue any Stripe API request.
func TestStripeWebhookRejectsWrongModeBeforePersistence(t *testing.T) {
	for _, tc := range []struct {
		name, key, payload, code string
		status                   int
	}{
		{"test receives live", "sk_test_fixture", `{"id":"evt_mode","type":"invoice.paid","livemode":true}`, "stripe_mode_mismatch", 400},
		{"live receives test", "sk_live_fixture", `{"id":"evt_mode","type":"invoice.paid","livemode":false}`, "stripe_mode_mismatch", 400},
		{"restricted test receives live", "rk_test_fixture", `{"id":"evt_mode","type":"invoice.paid","livemode":true}`, "stripe_mode_mismatch", 400},
		{"restricted live receives test", "rk_live_fixture", `{"id":"evt_mode","type":"invoice.paid","livemode":false}`, "stripe_mode_mismatch", 400},
		{"missing mode", "sk_test_fixture", `{"id":"evt_mode","type":"invoice.paid"}`, "invalid_stripe_event", 400},
		{"null mode", "sk_test_fixture", `{"id":"evt_mode","type":"invoice.paid","livemode":null}`, "invalid_stripe_event", 400},
		{"missing ID", "sk_test_fixture", `{"type":"invoice.paid","livemode":false}`, "invalid_stripe_event", 400},
		{"missing type", "sk_test_fixture", `{"id":"evt_mode","livemode":false}`, "invalid_stripe_event", 400},
		{"connected account", "sk_test_fixture", `{"id":"evt_mode","type":"invoice.paid","livemode":false,"account":"acct_other"}`, "stripe_account_mismatch", 400},
		{"unknown API key", "unknown_fixture", `{"id":"evt_mode","type":"invoice.paid","livemode":false}`, "stripe_not_configured", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &stripeEnvelopeStore{Store: store.NewMemoryStore()}
			router := gin.New()
			RegisterRoutes(router, WithStore(st), WithStripeConfig(StripeConfig{SecretKey: tc.key, WebhookSecret: testStripeWebhookSecret}), func(h *handler) {
				h.stripe.httpClient = &http.Client{Transport: stripeForbiddenTransport{t}}
			})
			rec := postStripeEvent(t, router, []byte(tc.payload))
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if rec.Code != tc.status || body.Error != tc.code || st.received != 0 {
				t.Fatalf("status=%d error=%s persisted=%d", rec.Code, body.Error, st.received)
			}
		})
	}
}

type stripeForbiddenTransport struct{ t *testing.T }

func (s stripeForbiddenTransport) RoundTrip(*http.Request) (*http.Response, error) {
	s.t.Fatal("unexpected Stripe request from a rejected event")
	return nil, nil
}

func TestStripeWebhookMatchingModesRemainProcessable(t *testing.T) {
	for _, key := range []string{"sk_test_fixture", "rk_test_fixture", "sk_live_fixture", "rk_live_fixture"} {
		t.Run(key, func(t *testing.T) {
			live, _ := stripeRuntimeMode(key, "")
			payload, _ := json.Marshal(map[string]any{"id": "evt_matching", "type": "fixture.noop", "livemode": live})
			st := &stripeEnvelopeStore{Store: store.NewMemoryStore()}
			router := gin.New()
			RegisterRoutes(router, WithStore(st), WithStripeConfig(StripeConfig{SecretKey: key, WebhookSecret: testStripeWebhookSecret}))
			if rec := postStripeEvent(t, router, payload); rec.Code != http.StatusOK || st.received != 1 {
				t.Fatalf("status=%d persisted=%d", rec.Code, st.received)
			}
		})
	}
}

var _ http.RoundTripper = stripeForbiddenTransport{}

func TestInvalidStripeConfigurationCannotIssueRequests(t *testing.T) {
	client := newStripeClient(StripeConfig{SecretKey: "sk_live_fixture", Mode: "sandbox"})
	client.httpClient = &http.Client{Transport: stripeForbiddenTransport{t}}
	if err := client.doForm(context.Background(), http.MethodPost, "/checkout/sessions", nil, nil); err == nil {
		t.Fatal("invalid configuration submitted a checkout")
	}
	if err := client.doJSON(context.Background(), http.MethodGet, "/subscriptions/sub_fixture", nil); err == nil {
		t.Fatal("invalid configuration read a subscription")
	}
	if _, err := client.refundPaymentIntent(context.Background(), "pi_fixture", "fixture-key", 100); err == nil {
		t.Fatal("invalid configuration submitted a refund")
	}
}
