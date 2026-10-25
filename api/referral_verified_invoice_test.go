package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"account/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This test deliberately starts from a generic settled inviter invoice whose
// issued_at precedes paid_at, then exercises the signed webhook with actual
// PostgreSQL transactions and a deterministic provider adapter.
func TestReferralSettledInvoicePostgresWebhook(t *testing.T) {
	dsn := os.Getenv("ACCOUNTS_REFERRAL_WEBHOOK_TEST_DSN")
	if dsn == "" {
		t.Skip("set isolated ACCOUNTS_REFERRAL_WEBHOOK_TEST_DSN; provider is mocked, PostgreSQL is real")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || (cfg.Host != "localhost" && cfg.Host != "127.0.0.1") || (cfg.Database != "referral_sandbox_test" && cfg.Database != "referral_webhook_test") || cfg.User != "referral_app" {
		t.Fatal("only local isolated referral webhook database and business role allowed")
	}
	ctx := context.Background()
	st, closeStore, err := store.New(ctx, store.Config{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var privileged bool
	if err := db.QueryRow(`SELECT rolsuper OR rolcreaterole OR rolcreatedb OR rolreplication OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&privileged); err != nil || privileged {
		t.Fatal("business test identity must not be privileged")
	}
	if _, err := st.(store.ReferralCampaignStore).ConfigureReferralCampaign(ctx, &store.ReferralCampaign{Enabled: true, Reason: "isolated deterministic PostgreSQL webhook regression"}); err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	owner, friend := &store.User{Name: "pg-owner-" + suffix, Email: "pg-owner-" + suffix + "@example.invalid", Active: true, EmailVerified: true}, &store.User{Name: "pg-friend-" + suffix, Email: "pg-friend-" + suffix + "@example.invalid", Active: true, EmailVerified: true}
	if err := st.CreateUser(ctx, owner); err != nil {
		t.Fatal(err)
	}
	code := "PG" + strings.ToUpper(suffix[:10])
	if _, err := st.EnsureReferralCode(ctx, owner.ID, code); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateUserWithReferral(ctx, friend, code); err != nil {
		t.Fatal(err)
	}
	monthlyPrice, annualPrice, otherPrice := "price_pg_month_"+suffix, "price_pg_year_"+suffix, "price_pg_other_"+suffix
	for _, plan := range []*store.BillingPlan{{PlanID: "PRO-MONTHLY", StripePriceID: monthlyPrice, Kind: "subscription", PriceUnit: "month", PackageName: "xconnect-pro", IncludedQuotaBytes: 20000, Active: true}, {PlanID: "PRO-YEARLY", StripePriceID: annualPrice, Kind: "subscription", PriceUnit: "year", PackageName: "xconnect-pro", IncludedQuotaBytes: 20000, Active: true}, {PlanID: "KNOWLEDGE-MONTHLY", StripePriceID: otherPrice, Kind: "subscription", PriceUnit: "month", Active: true}} {
		if err := st.UpsertBillingPlan(ctx, plan); err != nil {
			t.Fatal(err)
		}
	}
	paid := time.Now().UTC().Truncate(time.Second)
	ownerStart, ownerEnd := paid.Add(-24*time.Hour), paid.Add(20*24*time.Hour)
	ownerPaid, ownerIssued := paid.Add(-2*time.Hour), paid.Add(-3*time.Hour)
	ownerExternal, friendExternal := "sub_pg_owner_"+suffix, "sub_pg_friend_"+suffix
	ownerInvoiceID, friendInvoiceID := "in_pg_owner_"+suffix, "in_pg_friend_"+suffix
	ownerCustomer, friendCustomer := "cus_pg_owner_"+suffix, "cus_pg_friend_"+suffix
	ownerPI, friendPI := "pi_pg_owner_"+suffix, "pi_pg_friend_"+suffix
	if err := st.UpsertSubscription(ctx, &store.Subscription{UserID: owner.ID, Provider: "stripe", Kind: "subscription", PlanID: "PRO-MONTHLY", ExternalID: ownerExternal, Status: "active", Meta: map[string]any{"customer_id": ownerCustomer, "product_slug": "xconnect", "expiresAt": ownerEnd.Format(time.RFC3339)}}); err != nil {
		t.Fatal(err)
	}
	ownerSubs, _ := st.ListSubscriptionsByUser(ctx, owner.ID)
	ownerSubID := ownerSubs[0].ID
	original := &store.FinanceInvoice{IdempotencyKey: "stripe-invoice:" + ownerInvoiceID, AccountUUID: owner.ID, SubscriptionUUID: ownerSubID, Provider: "stripe", ProviderInvoiceID: ownerInvoiceID, AmountMinor: 1000, Currency: "USD", Description: "Settled Stripe subscription invoice", IssuedAt: ownerIssued, ServicePeriodStart: &ownerStart, ServicePeriodEnd: &ownerEnd}
	if _, err := st.CreateFinanceInvoice(ctx, original); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordFinancePayment(ctx, &store.FinancePayment{IdempotencyKey: "stripe-payment:" + ownerInvoiceID, InvoiceID: original.ID, AccountUUID: owner.ID, Provider: "stripe", ProviderPaymentID: ownerPI, AmountMinor: 1000, Currency: "USD", PaidAt: ownerPaid}); err != nil {
		t.Fatal(err)
	}
	// A local subscription's current metadata must not qualify a generic invoice.
	if before, err := st.ReferralExtensionEnd(ctx, ownerSubID); err != nil || !before.IsZero() {
		t.Fatalf("unverified metadata admitted paid product: %v %v", before, err)
	}
	invoiceJSON := func(id, customer, sub, pi, price string, amount int64, issued, paidAt, start, end time.Time) string {
		return fmt.Sprintf(`{"id":%q,"customer":%q,"subscription":%q,"payment_intent":%q,"status":"paid","amount_paid":%d,"currency":"usd","created":%d,"billing_reason":"subscription_create","status_transitions":{"paid_at":%d},"lines":{"data":[{"price":%q,"period":{"start":%d,"end":%d}}]}}`, id, customer, sub, pi, amount, issued.Unix(), paidAt.Unix(), price, start.Unix(), end.Unix())
	}
	ownerJSON := invoiceJSON(ownerInvoiceID, ownerCustomer, ownerExternal, ownerPI, monthlyPrice, 1000, ownerIssued, ownerPaid, ownerStart, ownerEnd)
	friendJSON := invoiceJSON(friendInvoiceID, friendCustomer, friendExternal, friendPI, annualPrice, 12345, paid.Add(-time.Hour), paid, paid, paid.AddDate(1, 0, 0))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/subscriptions/" + friendExternal:
			fmt.Fprintf(w, `{"id":%q,"status":"active","customer":%q,"latest_invoice":%q,"metadata":{"user_id":%q,"plan_id":"PRO-YEARLY","kind":"subscription","product_slug":"xconnect"},"current_period_start":%d,"current_period_end":%d,"items":{"data":[{"price":{"id":%q}}]}}`, friendExternal, friendCustomer, friendInvoiceID, friend.ID, paid.Unix(), paid.AddDate(1, 0, 0).Unix(), annualPrice)
		case "/v1/invoices/" + friendInvoiceID:
			fmt.Fprint(w, friendJSON)
		case "/v1/invoices/" + ownerInvoiceID:
			fmt.Fprint(w, ownerJSON)
		case "/v1/invoices":
			if r.URL.Query().Get("customer") == ownerCustomer {
				fmt.Fprintf(w, `{"data":[%s],"has_more":false}`, ownerJSON)
			} else {
				fmt.Fprintf(w, `{"data":[%s],"has_more":false}`, friendJSON)
			}
		case "/v1/payment_intents/" + ownerPI:
			fmt.Fprintf(w, `{"id":%q,"latest_charge":{"id":%q,"status":"succeeded","amount":1000,"amount_refunded":0,"refunded":false,"disputed":false}}`, ownerPI, "ch_"+suffix)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newStripeClient(StripeConfig{SecretKey: "sk_test_isolated_pg", WebhookSecret: testStripeWebhookSecret})
	client.apiBaseURL = server.URL + "/v1"
	client.httpClient = server.Client()
	h := &handler{store: st, stripe: client}
	router := gin.New()
	router.POST("/webhook", h.stripeWebhook)
	raw := []byte(fmt.Sprintf(`{"id":%q,"type":"invoice.paid","livemode":false,"data":{"object":%s}}`, "evt_pg_"+suffix, friendJSON))
	for i := 0; i < 2; i++ {
		request := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(raw))
		request.Header.Set("Stripe-Signature", signStripePayload(t, raw))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("signed PG webhook=%d %s", response.Code, response.Body.String())
		}
	}
	rewards, err := st.ListReferralRewards(ctx, owner.ID, 10)
	if err != nil || len(rewards) != 1 || rewards[0].RewardMinor != 1234 || rewards[0].AnnualDays != 30 {
		t.Fatalf("reward=%+v err=%v", rewards, err)
	}
	actual, err := st.GetFinanceInvoice(ctx, original.ID)
	if err != nil || actual.Description != original.Description || !actual.IssuedAt.Equal(ownerIssued) {
		t.Fatal("historical issued_at/description was rewritten")
	}
	ownerInvoices, err := st.ListFinanceInvoices(ctx, owner.ID, "", 10)
	if err != nil || len(ownerInvoices) != 1 {
		t.Fatal("proof created duplicate inviter financial invoice")
	}
	friendInvoices, err := st.ListFinanceInvoices(ctx, friend.ID, "", 10)
	if err != nil || len(friendInvoices) != 1 || friendInvoices[0].Description != "Settled Stripe subscription invoice" || !friendInvoices[0].IssuedAt.Equal(paid.Add(-time.Hour)) {
		t.Fatal("generic paid invoice was rewritten or duplicated")
	}
	extended, err := st.ReferralExtensionEnd(ctx, ownerSubID)
	if err != nil || !extended.Equal(ownerEnd.Add(30*24*time.Hour)) {
		t.Fatalf("verified generic paid tail=%v %v", extended, err)
	}
	var proofs int
	if err := db.QueryRow(`SELECT count(*) FROM public.referral_verified_invoices WHERE invoice_id IN ($1,$2)`, original.ID, friendInvoices[0].ID).Scan(&proofs); err != nil || proofs != 2 {
		t.Fatalf("immutable price proof count=%d err=%v", proofs, err)
	}
	var ownerSource stripeInvoice
	if err := json.Unmarshal([]byte(ownerJSON), &ownerSource); err != nil {
		t.Fatal(err)
	}
	if _, err := h.verifiedReferralFinanceInvoice(ctx, &ownerSource, owner.ID, ownerSubID, "PRO-MONTHLY", 1001, ownerStart, ownerEnd); err == nil {
		t.Fatal("mismatched immutable local amount was accepted")
	}
	ownerSource.Lines.Data[0].Price = otherPrice
	if plan, err := h.referralPlanFromInvoice(ctx, &ownerSource); err != nil || plan != "" {
		t.Fatal("non-XConnect invoice line admitted")
	}
	if err := st.VerifyReferralInvoice(ctx, &store.ReferralInvoiceVerification{InvoiceID: original.ID, ProviderPriceID: monthlyPrice, PlanID: "KNOWLEDGE-MONTHLY", ProductSlug: "knowledge"}); err == nil {
		t.Fatal("non-XConnect proof was accepted")
	}
	if err := st.VerifyReferralInvoice(ctx, &store.ReferralInvoiceVerification{InvoiceID: original.ID, ProviderPriceID: "different-price", PlanID: "PRO-MONTHLY", ProductSlug: "xconnect"}); err == nil {
		t.Fatal("immutable proof was overwritten")
	}
	t.Log("PASS: mocked Stripe signed webhook + real PostgreSQL; generic settled facts preserved, exact 10%/30d, non-XConnect and conflicting facts rejected")
}
