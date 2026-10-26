package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"account/internal/stripemock"
)

func TestResolveStripeAPIBaseURL(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		live      bool
		want      string
		wantErr   bool
	}{
		{"default empty", "", false, "", false},
		{"explicit default", "https://api.stripe.com/v1/", true, "", false},
		{"test override", " http://stripemock.lane.svc:12111/v1/ ", false, "http://stripemock.lane.svc:12111/v1", false},
		{"live override refused", "http://stripemock:12111/v1", true, "", true},
		{"relative refused", "stripemock/v1", false, "", true},
		{"credentials refused", "https://u:p@mock/v1", false, "", true},
		{"query refused", "https://mock/v1?x=1", false, "", true},
		{"scheme refused", "ftp://mock/v1", false, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveStripeAPIBaseURL(tc.raw, tc.live)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got %q err=%v, want %q err=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestStripeClientBaseURLFromConfig(t *testing.T) {
	if c := newStripeClient(StripeConfig{SecretKey: "sk_test_fixture"}); c.baseURL() != stripeAPIBaseURL || !c.enabled() {
		t.Fatalf("default base = %q enabled=%v", c.baseURL(), c.enabled())
	}
	if c := newStripeClient(StripeConfig{SecretKey: "sk_live_fixture", APIBaseURL: "http://mock/v1"}); c.enabled() {
		t.Fatal("live key with base override must disable Stripe")
	}
	if c := newStripeClient(StripeConfig{SecretKey: "sk_test_fixture", APIBaseURL: "not a url"}); c.enabled() {
		t.Fatal("invalid override must disable Stripe")
	}
}

func TestStripeClientUsesConfiguredMock(t *testing.T) {
	mock := stripemock.New(map[string]json.RawMessage{
		"/v1/subscriptions/sub_lane": json.RawMessage(`{"id":"sub_lane","status":"active","metadata":{"user_id":"u1"}}`),
	})
	srv := httptest.NewServer(mock)
	defer srv.Close()
	client := newStripeClient(StripeConfig{SecretKey: "sk_test_fixture", APIBaseURL: srv.URL + "/v1"})
	sub, err := client.fetchSubscription(context.Background(), "sub_lane")
	if err != nil {
		t.Fatalf("fetchSubscription: %v", err)
	}
	if sub.ID != "sub_lane" || sub.Metadata["user_id"] != "u1" {
		t.Fatalf("unexpected subscription %+v", sub)
	}
	if reqs := mock.Requests(); len(reqs) != 1 || reqs[0] != "GET /v1/subscriptions/sub_lane" {
		t.Fatalf("mock requests %v", reqs)
	}
}
