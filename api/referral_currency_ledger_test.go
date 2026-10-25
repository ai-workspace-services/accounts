package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"account/internal/store"
)

func TestReferralSignedWebhookKeepsProviderFinanceAndISORewards(t *testing.T) {
	for _, tc := range []struct {
		currency string
		raw, iso int64
	}{{"USD", 12345, 12345}, {"ISK", 10000, 100}, {"UGX", 10000, 100}, {"MGA", 100, 10000}} {
		t.Run(tc.currency, func(t *testing.T) {
			f := newReferralCompletionFixture(t, false, true)
			f.invoice = strings.ReplaceAll(f.invoice, "pi-completion", "pi_currency")
			f.invoice = strings.ReplaceAll(f.invoice, `"amount_paid":12345`, fmt.Sprintf(`"amount_paid":%d`, tc.raw))
			f.invoice = strings.ReplaceAll(f.invoice, `"currency":"usd"`, fmt.Sprintf(`"currency":%q`, strings.ToLower(tc.currency)))
			for _, id := range []string{"evt-currency", "evt-currency", "evt-currency-second-envelope"} {
				if response := f.send(t, id); response.Code != 200 {
					t.Fatalf("payment handler=%d %s", response.Code, response.Body.String())
				}
			}
			ctx := context.Background()
			invoices, err := f.h.store.ListFinanceInvoices(ctx, f.invitee.ID, "", 10)
			if err != nil || len(invoices) != 1 || invoices[0].AmountMinor != tc.raw {
				t.Fatalf("provider invoice=%+v %v", invoices, err)
			}
			payments, err := f.h.store.ListFinancePayments(ctx, f.invitee.ID, 10)
			if err != nil || len(payments) != 1 || payments[0].AmountMinor != tc.raw {
				t.Fatalf("provider payment=%+v %v", payments, err)
			}
			rewards, err := f.h.store.ListReferralRewards(ctx, f.referrer.ID, 10)
			if err != nil || len(rewards) != 1 || rewards[0].PaidMinor != tc.iso || rewards[0].RewardMinor != tc.iso/10 {
				t.Fatalf("ISO reward=%+v %v", rewards, err)
			}
			// Successful partial refunds use provider units as do payment/refund
			// reservations. Distinct refund envelopes must retain one raw refund fact.
			for _, eventID := range []string{"evt-currency-refund", "evt-currency-refund", "evt-currency-refund-updated"} {
				raw := []byte(fmt.Sprintf(`{"id":%q,"type":"refund.created","livemode":false,"data":{"object":{"id":"re_currency","status":"succeeded","payment_intent":"pi_currency","amount":%d,"currency":%q,"created":%d}}}`, eventID, tc.raw/2, strings.ToLower(tc.currency), time.Now().Unix()))
				request := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(raw))
				request.Header.Set("Stripe-Signature", signStripePayload(t, raw))
				response := httptest.NewRecorder()
				f.router.ServeHTTP(response, request)
				if response.Code != 200 {
					t.Fatalf("refund handler=%d %s", response.Code, response.Body.String())
				}
			}
			refunds, err := f.h.store.ListFinanceRefunds(ctx, f.invitee.ID, 10)
			if err != nil || len(refunds) != 1 || refunds[0].AmountMinor != tc.raw/2 {
				t.Fatalf("raw refund=%+v %v", refunds, err)
			}
			rewards, err = f.h.store.ListReferralRewards(ctx, f.referrer.ID, 10)
			if err != nil || len(rewards) != 1 || rewards[0].Status != "reversed" {
				t.Fatal("refund must reverse original ISO reward once")
			}
			// Complete the refund in the original provider units and replay it.
			for i := 0; i < 2; i++ {
				raw := []byte(fmt.Sprintf(`{"id":"evt-currency-refund-final","type":"refund.created","livemode":false,"data":{"object":{"id":"re_currency_final","status":"succeeded","payment_intent":"pi_currency","amount":%d,"currency":%q,"created":%d}}}`, tc.raw-tc.raw/2, strings.ToLower(tc.currency), time.Now().Unix()))
				request := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(raw))
				request.Header.Set("Stripe-Signature", signStripePayload(t, raw))
				response := httptest.NewRecorder()
				f.router.ServeHTTP(response, request)
				if response.Code != 200 {
					t.Fatalf("full refund handler=%d %s", response.Code, response.Body.String())
				}
			}
			refunds, err = f.h.store.ListFinanceRefunds(ctx, f.invitee.ID, 10)
			var total int64
			for _, refund := range refunds {
				total += refund.AmountMinor
			}
			if err != nil || len(refunds) != 2 || total != tc.raw {
				t.Fatalf("full provider refund total=%d count=%d err=%v", total, len(refunds), err)
			}
		})
	}
}

func TestReferralLegacyNormalizedFinanceFailsClosed(t *testing.T) {
	f := newReferralCompletionFixture(t, false, true)
	ctx := context.Background()
	f.invoice = strings.ReplaceAll(f.invoice, `"amount_paid":12345`, `"amount_paid":10000`)
	f.invoice = strings.ReplaceAll(f.invoice, `"currency":"usd"`, `"currency":"isk"`)
	subs, _ := f.h.store.ListSubscriptionsByUser(ctx, f.invitee.ID)
	paid := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	end := paid.AddDate(0, 1, 0)
	legacy := &store.FinanceInvoice{IdempotencyKey: "stripe-invoice:in-completion-first", AccountUUID: f.invitee.ID, SubscriptionUUID: subs[0].ID, Provider: "stripe", ProviderInvoiceID: "in-completion-first", AmountMinor: 100, Currency: "ISK", Description: "Settled Stripe subscription invoice", IssuedAt: paid, ServicePeriodStart: &paid, ServicePeriodEnd: &end}
	if _, err := f.h.store.CreateFinanceInvoice(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if response := f.send(t, "evt-legacy-normalized"); response.Code != 502 {
		t.Fatalf("legacy unit conflict status=%d", response.Code)
	}
	actual, err := f.h.store.GetFinanceInvoice(ctx, legacy.ID)
	if err != nil || actual.AmountMinor != 100 {
		t.Fatal("legacy financial fact was rewritten")
	}
	rewards, err := f.h.store.ListReferralRewards(ctx, f.referrer.ID, 10)
	if err != nil || len(rewards) != 0 {
		t.Fatal("conflicting legacy unit must not award")
	}
}

func TestReferralInviterImportKeepsProviderUnits(t *testing.T) {
	for _, tc := range []struct {
		currency string
		raw      int64
	}{{"USD", 12345}, {"ISK", 10000}, {"UGX", 10000}, {"MGA", 100}} {
		t.Run(tc.currency, func(t *testing.T) {
			f := newReferralCompletionFixture(t, false, true)
			ctx := context.Background()
			paid := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
			start, end := paid.Add(-24*time.Hour), paid.Add(24*time.Hour)
			if err := f.h.store.UpsertSubscription(ctx, &store.Subscription{UserID: f.referrer.ID, Provider: "stripe", Kind: "subscription", PlanID: "PRO-MONTHLY", ExternalID: "sub_currency_owner", Status: "active", Meta: map[string]any{"customer_id": "cus_currency_owner"}}); err != nil {
				t.Fatal(err)
			}
			source := fmt.Sprintf(`{"id":"in_currency_owner","customer":"cus_currency_owner","subscription":"sub_currency_owner","status":"paid","payment_intent":"pi_currency_owner","amount_paid":%d,"currency":%q,"created":%d,"status_transitions":{"paid_at":%d},"lines":{"data":[{"price":"price-completion","period":{"start":%d,"end":%d}}]}}`, tc.raw, strings.ToLower(tc.currency), paid.Add(-2*time.Hour).Unix(), paid.Add(-time.Hour).Unix(), start.Unix(), end.Unix())
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/invoices":
					fmt.Fprintf(w, `{"data":[%s],"has_more":false}`, source)
				case "/v1/invoices/in_currency_owner":
					fmt.Fprint(w, source)
				case "/v1/payment_intents/pi_currency_owner":
					fmt.Fprintf(w, `{"id":"pi_currency_owner","latest_charge":{"id":"ch_currency_owner","status":"succeeded","amount":%d,"amount_refunded":0,"refunded":false,"disputed":false}}`, tc.raw)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			f.h.stripe.apiBaseURL = server.URL + "/v1"
			f.h.stripe.httpClient = server.Client()
			for i := 0; i < 2; i++ {
				if err := f.h.ensureReferrerPaidPeriod(ctx, f.invitee.ID, paid); err != nil {
					t.Fatal(err)
				}
			}
			invoices, err := f.h.store.ListFinanceInvoices(ctx, f.referrer.ID, "", 10)
			if err != nil || len(invoices) != 1 || invoices[0].AmountMinor != tc.raw {
				t.Fatalf("imported invoice=%+v %v", invoices, err)
			}
			payments, err := f.h.store.ListFinancePayments(ctx, f.referrer.ID, 10)
			if err != nil || len(payments) != 1 || payments[0].AmountMinor != tc.raw {
				t.Fatalf("imported payment=%+v %v", payments, err)
			}
		})
	}
}
