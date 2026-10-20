package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBillingPortalRequiresExplicitNonMutatingConfiguration(t *testing.T) {
	for _, scenario := range []string{"missing", "unverifiable", "update_enabled", "cancel_enabled", "reviewed"} {
		t.Run(scenario, func(t *testing.T) {
			posts := 0
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "GET" && r.URL.Path == "/billing_portal/configurations/bpc_reviewed" {
					if scenario == "unverifiable" {
						fmt.Fprint(w, `{"id":"bpc_reviewed","active":true,"features":{}}`)
						return
					}
					fmt.Fprintf(w, `{"id":"bpc_reviewed","active":true,"features":{"subscription_update":{"enabled":%t},"subscription_cancel":{"enabled":%t},"customer_update":{"enabled":false},"invoice_history":{"enabled":true},"payment_method_update":{"enabled":true}}}`, scenario == "update_enabled", scenario == "cancel_enabled")
					return
				}
				if r.Method == "POST" && r.URL.Path == "/billing_portal/sessions" {
					posts++
					_ = r.ParseForm()
					if r.Form.Get("configuration") != "bpc_reviewed" || r.Form.Get("customer") != "cus_reviewed" {
						t.Error("configuration fallback")
					}
					fmt.Fprint(w, `{"id":"bps_fixture","url":"https://billing.stripe.com/p/session/test"}`)
					return
				}
				t.Error("unexpected provider request", r.Method, r.URL)
				http.Error(w, "unexpected", 400)
			}))
			defer provider.Close()
			cfg := StripeConfig{SecretKey: "sk_test_portal", PortalConfigurationID: "bpc_reviewed"}
			if scenario == "missing" {
				cfg.PortalConfigurationID = ""
			}
			client := newStripeClient(cfg)
			client.apiBaseURL = provider.URL
			client.httpClient = provider.Client()
			session, err := client.createPortalSession(context.Background(), "cus_reviewed", "https://console.example.invalid/panel/subscription")
			if scenario == "reviewed" {
				if err != nil || session == nil || !strings.HasPrefix(session.ID, "bps_") || posts != 1 {
					t.Fatal(session, err, posts)
				}
			} else if err == nil || posts != 0 {
				t.Fatal("unsafe portal session issued", scenario, err, posts)
			}
		})
	}
}
