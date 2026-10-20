package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"account/internal/store"
)

func TestReferralDelayedInvoiceUsesImmutablePriceSnapshot(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	user := &store.User{ID: "delayed-invoice-user", Name: "Delayed Invoice User", Email: "delayed-invoice@example.test", Active: true, Role: store.RoleUser}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := st.UpsertBillingPlan(ctx, &store.BillingPlan{
		PlanID: "PRO-YEARLY", StripePriceID: "price_xconnect_yearly_v1", Kind: "subscription", PriceUnit: "year", Active: true,
	}); err != nil {
		t.Fatalf("seed historical annual plan: %v", err)
	}
	if err := st.UpsertBillingPlan(ctx, &store.BillingPlan{
		PlanID: "PRO-MONTHLY", StripePriceID: "price_xconnect_monthly_v2", Kind: "subscription", PriceUnit: "month", Active: true,
	}); err != nil {
		t.Fatalf("seed current monthly plan: %v", err)
	}
	if err := st.UpsertSubscription(ctx, &store.Subscription{
		UserID: user.ID, Provider: "stripe", Kind: "subscription", PlanID: "PRO-MONTHLY", ExternalID: "sub-current-monthly",
		Status: "active", Meta: map[string]any{"product_slug": "xconnect", "plan_id": "PRO-MONTHLY"},
	}); err != nil {
		t.Fatalf("change current subscription to monthly: %v", err)
	}

	var invoice stripeInvoice
	if err := json.Unmarshal([]byte(`{"id":"in-delayed-yearly","lines":{"data":[{"price":"price_xconnect_yearly_v1"}]}}`), &invoice); err != nil {
		t.Fatalf("decode delayed invoice fixture: %v", err)
	}
	planID, err := (&handler{store: st}).referralPlanFromInvoice(ctx, &invoice)
	if err != nil || planID != "PRO-YEARLY" {
		t.Fatalf("delayed invoice plan = %q, err=%v; want immutable annual price despite current monthly plan", planID, err)
	}
}

func TestEnsureReferrerPaidPeriodUsesHistoricalStripeInvoicePeriod(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	referrer := seedActiveUser(t, st, "Historical Referrer", "historical-referrer@example.test", "historical-referrer")
	invitee := seedActiveUser(t, st, "Historical Invitee", "historical-invitee@example.test", "historical-invitee")
	if _, err := st.EnsureReferralCode(ctx, referrer.ID, "HISTORY123"); err != nil {
		t.Fatalf("create referral code: %v", err)
	}
	if err := st.CreateReferralAttribution(ctx, "HISTORY123", invitee.ID, time.Now().UTC().Add(-30*24*time.Hour)); err != nil {
		t.Fatalf("create attribution: %v", err)
	}

	paidAt := time.Date(2026, time.October, 20, 12, 0, 0, 0, time.UTC)
	oldPeriodStart := paidAt.AddDate(-1, 0, 0).Unix()
	oldPeriodEnd := paidAt.Add(24 * time.Hour).Unix()
	currentPeriodStart := paidAt.Add(2 * 24 * time.Hour)
	currentPeriodEnd := currentPeriodStart.AddDate(1, 0, 0)
	if err := st.UpsertSubscription(ctx, &store.Subscription{
		UserID: referrer.ID, Provider: "stripe", Kind: "subscription", PlanID: "PRO-MONTHLY", ExternalID: "sub-renewed",
		Status: "active", Meta: map[string]any{
			"product_slug": "xconnect", "customer_id": "cus-historical", "plan_id": "PRO-MONTHLY",
			"startsAt": currentPeriodStart.Format(time.RFC3339), "expiresAt": currentPeriodEnd.Format(time.RFC3339),
		},
	}); err != nil {
		t.Fatalf("seed current renewed subscription: %v", err)
	}
	for _, plan := range []store.BillingPlan{
		{PlanID: "PRO-YEARLY", StripePriceID: "price_xconnect_yearly_historical", Kind: "subscription", PriceUnit: "year", Active: true},
		{PlanID: "PRO-MONTHLY", StripePriceID: "price_xconnect_monthly_current", Kind: "subscription", PriceUnit: "month", Active: true},
	} {
		if err := st.UpsertBillingPlan(ctx, &plan); err != nil {
			t.Fatalf("seed billing plan %s: %v", plan.PlanID, err)
		}
	}

	stripeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/invoices":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"in-historical-annual","subscription":"sub-renewed","status":"paid","amount_paid":12000}],"has_more":false}`)
		case r.URL.Path == "/v1/invoices/in-historical-annual":
			body := fmt.Sprintf(`{"id":"in-historical-annual","subscription":"sub-renewed","status":"paid","payment_intent":"pi-historical-annual","amount_paid":12000,"currency":"usd","status_transitions":{"paid_at":%d},"lines":{"data":[{"price":"price_xconnect_yearly_historical","period":{"start":%d,"end":%d}}]}}`, paidAt.Add(-24*time.Hour).Unix(), oldPeriodStart, oldPeriodEnd)
			_, _ = fmt.Fprint(w, body)
		case r.URL.Path == "/v1/payment_intents/pi-historical-annual":
			_, _ = fmt.Fprint(w, `{"id":"pi-historical-annual","latest_charge":{"id":"ch-historical-annual","status":"succeeded","amount":12000,"amount_refunded":0,"disputed":false}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer stripeServer.Close()
	client := newStripeClient(StripeConfig{SecretKey: "sk_test"})
	client.httpClient = stripeServer.Client()
	client.httpClient.Transport = redirectStripeTestTransport{target: stripeServer.URL}
	h := &handler{store: st, stripe: client}

	if err := h.ensureReferrerPaidPeriod(ctx, invitee.ID, paidAt); err != nil {
		t.Fatalf("import immutable historical annual invoice: %v", err)
	}
	invoices, err := st.ListFinanceInvoices(ctx, referrer.ID, "", 20)
	if err != nil || len(invoices) != 1 {
		t.Fatalf("historical invoice ledger records = %d, err=%v; want one verified invoice", len(invoices), err)
	}
	if invoices[0].ServicePeriodStart == nil || !invoices[0].ServicePeriodStart.Equal(time.Unix(oldPeriodStart, 0).UTC()) || invoices[0].ServicePeriodEnd == nil || !invoices[0].ServicePeriodEnd.Equal(time.Unix(oldPeriodEnd, 0).UTC()) {
		t.Fatalf("ledger lost immutable historical invoice period: %+v", invoices[0])
	}
	payments, err := st.ListFinancePayments(ctx, referrer.ID, 20)
	if err != nil || len(payments) != 1 || payments[0].PaidAt.After(paidAt) {
		t.Fatalf("historical payment proof = %+v, err=%v; want one payment paid by invitee event time", payments, err)
	}
}

func TestEnsureReferrerPaidPeriodFailsClosedWithoutHistoricalCoverage(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	referrer := seedActiveUser(t, st, "Unproven Referrer", "unproven-referrer@example.test", "unproven-referrer")
	invitee := seedActiveUser(t, st, "Unproven Invitee", "unproven-invitee@example.test", "unproven-invitee")
	if _, err := st.EnsureReferralCode(ctx, referrer.ID, "UNPROVEN123"); err != nil {
		t.Fatalf("create referral code: %v", err)
	}
	if err := st.CreateReferralAttribution(ctx, "UNPROVEN123", invitee.ID, time.Now().UTC().Add(-30*24*time.Hour)); err != nil {
		t.Fatalf("create attribution: %v", err)
	}
	paidAt := time.Date(2026, time.October, 20, 12, 0, 0, 0, time.UTC)
	start, end := paidAt.AddDate(0, -1, 0), paidAt.AddDate(0, 11, 0)
	if err := st.UpsertSubscription(ctx, &store.Subscription{
		UserID: referrer.ID, Provider: "stripe", Kind: "subscription", PlanID: "PRO-YEARLY", ExternalID: "sub-unproven",
		Status: "active", Meta: map[string]any{"product_slug": "xconnect", "customer_id": "cus-unproven", "startsAt": start.Format(time.RFC3339), "expiresAt": end.Format(time.RFC3339)},
	}); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	if err := st.UpsertBillingPlan(ctx, &store.BillingPlan{PlanID: "PRO-YEARLY", StripePriceID: "price_xconnect_yearly_unproven", Kind: "subscription", PriceUnit: "year", Active: true}); err != nil {
		t.Fatalf("seed billing plan: %v", err)
	}
	stripeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/invoices" {
			_, _ = fmt.Fprint(w, `{"data":[{"id":"in-unproven","subscription":"sub-unproven","status":"paid","amount_paid":12000}],"has_more":false}`)
			return
		}
		if r.URL.Path == "/v1/invoices/in-unproven" {
			body := fmt.Sprintf(`{"id":"in-unproven","subscription":"sub-unproven","status":"paid","payment_intent":"pi-unproven","amount_paid":12000,"currency":"usd","status_transitions":{"paid_at":%d},"lines":{"data":[{"price":"price_xconnect_yearly_unproven","period":{"start":%d,"end":%d}}]}}`, paidAt.Add(-24*time.Hour).Unix(), paidAt.Add(24*time.Hour).Unix(), paidAt.AddDate(1, 0, 0).Unix())
			_, _ = fmt.Fprint(w, body)
			return
		}
		http.NotFound(w, r)
	}))
	defer stripeServer.Close()
	client := newStripeClient(StripeConfig{SecretKey: "sk_test"})
	client.httpClient = stripeServer.Client()
	client.httpClient.Transport = redirectStripeTestTransport{target: stripeServer.URL}
	if err := (&handler{store: st, stripe: client}).ensureReferrerPaidPeriod(ctx, invitee.ID, paidAt); err != nil {
		t.Fatalf("uncovered invoice should be ignored, not break webhook: %v", err)
	}
	invoices, err := st.ListFinanceInvoices(ctx, referrer.ID, "", 20)
	if err != nil || len(invoices) != 0 {
		t.Fatalf("uncovered historical period created finance proof: invoices=%+v err=%v", invoices, err)
	}
}

func TestInvoicePaidKeepsCoreQuotaOnReferralHistoryFailureAndRetryIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	referrer := seedActiveUser(t, st, "Retry Referrer", "retry-referrer@example.test", "retry-referrer")
	invitee := seedActiveUser(t, st, "Retry Invitee", "retry-invitee@example.test", "retry-invitee")
	if _, err := st.EnsureReferralCode(ctx, referrer.ID, "RETRY1234"); err != nil {
		t.Fatalf("create referral code: %v", err)
	}
	if err := st.CreateReferralAttribution(ctx, "RETRY1234", invitee.ID, time.Now().UTC().Add(-24*time.Hour)); err != nil {
		t.Fatalf("create attribution: %v", err)
	}
	if err := st.UpsertSubscription(ctx, &store.Subscription{
		UserID: referrer.ID, Provider: "stripe", Kind: "subscription", PlanID: "PRO-MONTHLY", ExternalID: "sub-referrer-retry",
		Status: "active", Meta: map[string]any{"product_slug": "xconnect", "customer_id": "cus-referrer-retry"},
	}); err != nil {
		t.Fatalf("seed referrer subscription: %v", err)
	}
	const included int64 = 20 << 30
	if err := st.UpsertBillingPlan(ctx, &store.BillingPlan{
		PlanID: "PRO-YEARLY", StripePriceID: "price-retry-yearly", Kind: "subscription", PriceUnit: "year",
		PackageName: "xconnect-pro", IncludedQuotaBytes: included, Active: true,
	}); err != nil {
		t.Fatalf("seed yearly plan: %v", err)
	}
	paidAt := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	periodStart, periodEnd := paidAt.Add(-time.Hour), paidAt.AddDate(1, 0, 0)
	quotaStart, _ := naturalMonthPeriod(time.Now().UTC())
	oldStart, oldEnd := quotaStart.AddDate(0, -1, 0), quotaStart
	if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{
		AccountUUID: invitee.ID, RemainingIncludedQuota: 1234, ThrottleState: "normal", SuspendState: "active",
		PeriodStart: &oldStart, PeriodEnd: &oldEnd, EffectiveAt: paidAt.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seed prior quota: %v", err)
	}
	stripeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/subscriptions/sub-invitee-retry" {
			body := fmt.Sprintf(`{"id":"sub-invitee-retry","status":"active","latest_invoice":"in-invitee-retry","customer":"cus-invitee-retry","metadata":{"user_id":%q,"plan_id":"PRO-YEARLY","product_slug":"xconnect"},"current_period_start":%d,"current_period_end":%d,"items":{"data":[{"price":{"id":"price-retry-yearly"}}]}}`, invitee.ID, periodStart.Unix(), periodEnd.Unix())
			_, _ = fmt.Fprint(w, body)
			return
		}
		if r.URL.Path == "/v1/invoices" {
			http.Error(w, `{"error":"temporary Stripe history failure"}`, http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/v1/invoices/in-invitee-retry" {
			fmt.Fprint(w, `{"id":"in-invitee-retry","subscription":"sub-invitee-retry","status":"paid"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer stripeServer.Close()
	client := newStripeClient(StripeConfig{SecretKey: "sk_test"})
	client.apiBaseURL = stripeServer.URL + "/v1"
	client.httpClient = stripeServer.Client()
	h := &handler{store: st, stripe: client}
	invoiceBody := fmt.Sprintf(`{"id":"in-invitee-retry","customer":"cus-invitee-retry","subscription":"sub-invitee-retry","status":"paid","payment_intent":"pi-invitee-retry","amount_paid":90000,"currency":"usd","status_transitions":{"paid_at":%d},"lines":{"data":[{"price":"price-retry-yearly","period":{"start":%d,"end":%d}}]}}`, paidAt.Unix(), periodStart.Unix(), periodEnd.Unix())
	event := stripeEvent{ID: "evt-invitee-retry", Type: "invoice.paid", Data: struct {
		Object json.RawMessage `json:"object"`
	}{Object: json.RawMessage(invoiceBody)}}
	if err := h.handleStripeEvent(ctx, event); err == nil {
		t.Fatal("transient inviter invoice history failure should keep the webhook retryable")
	}
	profile, err := st.GetAccountBillingProfile(ctx, invitee.ID)
	if err != nil || profile.IncludedQuotaBytes != included || profile.PricingRuleVersion != "plan:PRO-YEARLY" {
		t.Fatalf("core subscription entitlement was lost after referral verification error: profile=%+v err=%v", profile, err)
	}
	quota, err := st.GetAccountQuotaState(ctx, invitee.ID)
	if err != nil || quota.RemainingIncludedQuota != included || quota.PeriodStart == nil || !quota.PeriodStart.Equal(quotaStart) {
		t.Fatalf("core quota was not reset before referral retry error: quota=%+v err=%v", quota, err)
	}
	quota.RemainingIncludedQuota -= 4096
	if err := st.UpsertAccountQuotaState(ctx, quota); err != nil {
		t.Fatalf("simulate usage after first retryable event: %v", err)
	}
	if err := h.handleStripeEvent(ctx, event); err == nil {
		t.Fatal("history error should remain retryable on replay")
	}
	quota, err = st.GetAccountQuotaState(ctx, invitee.ID)
	if err != nil || quota.RemainingIncludedQuota != included-4096 {
		t.Fatalf("same paid period was granted quota more than once: quota=%+v err=%v", quota, err)
	}
}

func TestEnsureReferrerPaidPeriodRejectsProviderRefundAndDisputeHistory(t *testing.T) {
	for _, tc := range []struct {
		name          string
		chargePayload string
	}{
		{name: "fully refunded charge", chargePayload: `{"id":"ch-referrer","status":"succeeded","amount":12000,"amount_refunded":12000,"refunded":true}`},
		{name: "disputed charge", chargePayload: `{"id":"ch-referrer","status":"succeeded","amount":12000,"amount_refunded":0,"disputed":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := store.NewMemoryStore()
			referrer := seedActiveUser(t, st, "Risky Legacy Referrer", "risky-referrer-"+tc.name+"@example.test", "risky-referrer")
			invitee := seedActiveUser(t, st, "Risky Legacy Invitee", "risky-invitee-"+tc.name+"@example.test", "risky-invitee")
			if _, err := st.EnsureReferralCode(ctx, referrer.ID, "RISKHISTORY1"); err != nil {
				t.Fatalf("create referral code: %v", err)
			}
			if err := st.CreateReferralAttribution(ctx, "RISKHISTORY1", invitee.ID, time.Now().UTC().Add(-24*time.Hour)); err != nil {
				t.Fatalf("create attribution: %v", err)
			}
			if err := st.UpsertSubscription(ctx, &store.Subscription{
				UserID: referrer.ID, Provider: "stripe", Kind: "subscription", PlanID: "PRO-YEARLY", ExternalID: "sub-risky-legacy",
				Status: "active", Meta: map[string]any{"product_slug": "xconnect", "customer_id": "cus-risky-legacy"},
			}); err != nil {
				t.Fatalf("seed legacy subscription: %v", err)
			}
			if err := st.UpsertBillingPlan(ctx, &store.BillingPlan{PlanID: "PRO-YEARLY", StripePriceID: "price-risky-legacy", Kind: "subscription", PriceUnit: "year", Active: true}); err != nil {
				t.Fatalf("seed yearly plan: %v", err)
			}
			paidAt := time.Date(2026, time.October, 20, 12, 0, 0, 0, time.UTC)
			periodStart, periodEnd := paidAt.AddDate(-1, 0, 0).Unix(), paidAt.Add(24*time.Hour).Unix()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/v1/invoices":
					_, _ = fmt.Fprint(w, `{"data":[{"id":"in-risky-legacy","subscription":"sub-risky-legacy","status":"paid","amount_paid":12000}],"has_more":false}`)
				case r.URL.Path == "/v1/invoices/in-risky-legacy":
					body := fmt.Sprintf(`{"id":"in-risky-legacy","subscription":"sub-risky-legacy","status":"paid","payment_intent":"pi-risky-legacy","amount_paid":12000,"currency":"usd","status_transitions":{"paid_at":%d},"lines":{"data":[{"price":"price-risky-legacy","period":{"start":%d,"end":%d}}]}}`, paidAt.Add(-24*time.Hour).Unix(), periodStart, periodEnd)
					_, _ = fmt.Fprint(w, body)
				case r.URL.Path == "/v1/payment_intents/pi-risky-legacy":
					_, _ = fmt.Fprint(w, `{"id":"pi-risky-legacy","latest_charge":`+tc.chargePayload+`}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := newStripeClient(StripeConfig{SecretKey: "sk_test"})
			client.apiBaseURL = server.URL + "/v1"
			client.httpClient = server.Client()
			if err := (&handler{store: st, stripe: client}).ensureReferrerPaidPeriod(ctx, invitee.ID, paidAt); err != nil {
				t.Fatalf("provider risk should disqualify history without breaking the invitee event: %v", err)
			}
			invoices, err := st.ListFinanceInvoices(ctx, referrer.ID, "", 20)
			if err != nil || len(invoices) != 0 {
				t.Fatalf("risky provider payment was imported as verified finance proof: invoices=%+v err=%v", invoices, err)
			}
			payments, err := st.ListFinancePayments(ctx, referrer.ID, 20)
			if err != nil || len(payments) != 0 {
				t.Fatalf("risky provider payment was imported as paid finance proof: payments=%+v err=%v", payments, err)
			}
		})
	}
}

func TestMonthlyReferralDoesNotQueryReferrerPaymentHistory(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	referrer := seedActiveUser(t, st, "Monthly Referrer", "monthly-referrer@example.test", "monthly-referrer")
	invitee := seedActiveUser(t, st, "Monthly Invitee", "monthly-invitee@example.test", "monthly-invitee")
	if _, err := st.EnsureReferralCode(ctx, referrer.ID, "MONTHLY1234"); err != nil {
		t.Fatalf("create referral code: %v", err)
	}
	if err := st.CreateReferralAttribution(ctx, "MONTHLY1234", invitee.ID, time.Now().UTC().Add(-24*time.Hour)); err != nil {
		t.Fatalf("create attribution: %v", err)
	}
	if err := st.UpsertBillingPlan(ctx, &store.BillingPlan{PlanID: "PRO-MONTHLY", StripePriceID: "price-monthly-referral", Kind: "subscription", PriceUnit: "month", Active: true}); err != nil {
		t.Fatalf("seed monthly plan: %v", err)
	}
	paidAt := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	periodStart, periodEnd := paidAt.Add(-time.Hour), paidAt.AddDate(0, 1, 0)
	if err := st.UpsertSubscription(ctx, &store.Subscription{UserID: invitee.ID, Provider: "stripe", Kind: "subscription", PlanID: "PRO-MONTHLY", ExternalID: "sub-monthly-invitee", Meta: map[string]any{"product_slug": "xconnect"}}); err != nil {
		t.Fatalf("seed invitee subscription: %v", err)
	}
	if err := st.UpsertSubscription(ctx, &store.Subscription{UserID: referrer.ID, Provider: "stripe", Kind: "subscription", PlanID: "PRO-YEARLY", ExternalID: "sub-monthly-referrer", Meta: map[string]any{"product_slug": "xconnect", "customer_id": "cus-referrer-history-fails"}}); err != nil {
		t.Fatalf("seed referrer subscription: %v", err)
	}
	var referrerHistoryLookups int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/invoices" && r.URL.Path != "/v1/invoices/in-monthly-current" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/v1/invoices" {
			if r.URL.Query().Get("customer") == "cus-referrer-history-fails" {
				referrerHistoryLookups++
				http.Error(w, `{"error":"referrer history unavailable"}`, http.StatusServiceUnavailable)
				return
			}
			_, _ = fmt.Fprint(w, `{"data":[{"id":"in-monthly-current","subscription":"sub-monthly-invitee","status":"paid","amount_paid":1000}],"has_more":false}`)
			return
		}
		body := fmt.Sprintf(`{"id":"in-monthly-current","customer":"cus-monthly-invitee","subscription":"sub-monthly-invitee","status":"paid","payment_intent":"pi-monthly-current","amount_paid":1000,"currency":"usd","status_transitions":{"paid_at":%d},"lines":{"data":[{"price":"price-monthly-referral","period":{"start":%d,"end":%d}}]}}`, paidAt.Unix(), periodStart.Unix(), periodEnd.Unix())
		_, _ = fmt.Fprint(w, body)
	}))
	defer server.Close()
	client := newStripeClient(StripeConfig{SecretKey: "sk_test"})
	client.apiBaseURL = server.URL + "/v1"
	client.httpClient = server.Client()
	h := &handler{store: st, stripe: client}
	invoice := &stripeInvoice{ID: "in-monthly-current", Customer: "cus-monthly-invitee", Subscription: "sub-monthly-invitee", Status: "paid", PaymentIntent: "pi-monthly-current", AmountPaid: 1000, Currency: "usd"}
	invoice.StatusTransitions.PaidAt = paidAt.Unix()
	invoice.Lines.Data = append(invoice.Lines.Data, struct {
		Price  any `json:"price"`
		Period struct {
			Start int64 `json:"start"`
			End   int64 `json:"end"`
		} `json:"period"`
		Pricing struct {
			PriceDetails struct {
				Price any `json:"price"`
			} `json:"price_details"`
		} `json:"pricing"`
	}{Price: "price-monthly-referral"})
	line := &invoice.Lines.Data[0]
	line.Period.Start, line.Period.End = periodStart.Unix(), periodEnd.Unix()
	subscription := &stripeSubscription{ID: "sub-monthly-invitee", Metadata: map[string]string{"user_id": invitee.ID, "product_slug": "xconnect", "plan_id": "PRO-MONTHLY"}}
	if err := h.recordReferralPaidInvoice(ctx, invoice, subscription, invitee.ID); err != nil {
		t.Fatalf("monthly referral should not depend on inviter history lookup: %v", err)
	}
	if referrerHistoryLookups != 0 {
		t.Fatalf("monthly invoice queried inviter payment history %d times", referrerHistoryLookups)
	}
	profile, err := st.GetReferralSummary(ctx, referrer.ID, time.Now().UTC())
	if err != nil || profile.SuccessfulInvites != 1 {
		t.Fatalf("monthly first invoice was not accepted after unrelated history failure: summary=%+v err=%v", profile, err)
	}
}
