package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"account/internal/store"
)

func TestCustomTopUpCreatesOneTimeDynamicStripePrice(t *testing.T) {
	var submitted url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/checkout/sessions" {
			t.Fatalf("unexpected Stripe request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk_test_unit" {
			t.Fatalf("unexpected auth header: %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		submitted, err = url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cs_test_custom","url":"https://checkout.stripe.test/session"}`)
	}))
	defer server.Close()

	client := newStripeClient(StripeConfig{SecretKey: "sk_test_unit", FrontendURL: "https://console.example.test"})
	client.apiBaseURL = server.URL + "/v1"
	client.httpClient = server.Client()
	user := &store.User{ID: "user-1", Email: "payer@example.test"}
	session, err := client.createCheckoutSession(context.Background(), user, stripeCheckoutRequest{
		PlanID:      customUSDTopUpPlanID,
		AmountMinor: 7250,
		Currency:    "USD",
		Mode:        "payment",
		ProductSlug: "xconnect",
		SourcePath:  "/panel/subscription",
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != "cs_test_custom" {
		t.Fatalf("session ID = %q", session.ID)
	}
	for key, want := range map[string]string{
		"mode":                                   "payment",
		"line_items[0][quantity]":                "1",
		"line_items[0][price_data][currency]":    "usd",
		"line_items[0][price_data][unit_amount]": "7250",
		"metadata[plan_id]":                      customUSDTopUpPlanID,
		"metadata[amount_minor]":                 "7250",
		"metadata[currency]":                     "USD",
		"payment_intent_data[metadata][plan_id]": customUSDTopUpPlanID,
		"payment_intent_data[metadata][kind]":    "paygo",
	} {
		if got := submitted.Get(key); got != want {
			t.Errorf("Stripe form %s = %q, want %q", key, got, want)
		}
	}
	if strings.Contains(submitted.Encode(), "line_items%5B0%5D%5Bprice%5D=") {
		t.Fatal("custom amount must not reference a reusable Stripe Price ID")
	}
}
