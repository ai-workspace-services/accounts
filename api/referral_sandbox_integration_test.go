package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"account/internal/auth"
	"account/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// TestReferralStripeSandboxPostgres uses genuine Sandbox transactions and raw
// Stripe /events objects. Delivery is locally signed with a one-run test secret;
// it does not claim to verify public Stripe webhook endpoint delivery. Both the
// inviter's monthly payment and friend's first annual payment are real test-mode
// Stripe transactions. All local financial history is retained for inspection.
func TestReferralStripeSandboxPostgres(t *testing.T) {
	if os.Getenv("ACCOUNTS_REFERRAL_SANDBOX") != "1" {
		t.Skip("opt in with ACCOUNTS_REFERRAL_SANDBOX=1; real Stripe Sandbox and isolated PostgreSQL required")
	}
	secret := strings.TrimSpace(os.Getenv("STRIPE_SECRET_KEY"))
	if secret == "" {
		t.Fatal("explicit Sandbox run requires a test secret in the process environment")
	}
	if !strings.HasPrefix(secret, "sk_test_") {
		t.Fatal("refusing non-Sandbox Stripe key")
	}
	dsn := os.Getenv("ACCOUNTS_REFERRAL_SANDBOX_DSN")
	if dsn == "" {
		t.Fatal("explicit Sandbox run requires ACCOUNTS_REFERRAL_SANDBOX_DSN")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || (cfg.Host != "localhost" && cfg.Host != "127.0.0.1") || cfg.Database != "referral_sandbox_test" || cfg.User != "referral_app" {
		t.Fatal("only local referral_sandbox_test with referral_app is permitted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("cannot open isolated database")
	}
	defer db.Close()
	var privileged bool
	if err := db.QueryRowContext(ctx, `SELECT rolsuper OR rolcreaterole OR rolcreatedb OR rolreplication OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&privileged); err != nil || privileged {
		t.Fatal("business test role is unavailable or privileged")
	}
	st, closeStore, err := store.New(ctx, store.Config{Driver: "postgres", DSN: dsn, MaxOpenConns: 4})
	if err != nil {
		t.Fatal("cannot initialize isolated account store")
	}
	defer closeStore(context.Background())
	// Use an explicit test campaign revision rather than depending on wall-clock
	// default dates. This appends isolated configuration, never changes history.
	if _, err := st.(store.ReferralCampaignStore).ConfigureReferralCampaign(ctx, &store.ReferralCampaign{Enabled: true, Reason: "isolated real Stripe Sandbox acceptance"}); err != nil {
		t.Fatal("cannot configure isolated campaign")
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		t.Fatal("cannot generate test-only signing material")
	}
	webhookSecret := "whsec_local_acceptance_" + hex.EncodeToString(random)
	client := newStripeClient(StripeConfig{Mode: "test", SecretKey: secret, WebhookSecret: webhookSecret})
	client.httpClient.Timeout = 20 * time.Second
	runID := "referral-acceptance-" + uuid.NewString()
	runStart := time.Now().Add(-2 * time.Second).Unix()
	formCall := func(stage, path string, form url.Values, out any) {
		t.Helper()
		if err := client.doForm(ctx, http.MethodPost, path, form, out, runID+":"+stage); err != nil {
			t.Fatalf("Stripe Sandbox request failed at %s (response body deliberately omitted)", stage)
		}
	}
	var product struct {
		ID string `json:"id"`
	}
	formCall("product", "/products", url.Values{"name": {"XConnect isolated referral acceptance"}, "metadata[acceptance_run]": {runID}}, &product)
	makePrice := func(stage, interval string, amount int64) string {
		var price struct {
			ID string `json:"id"`
		}
		formCall(stage, "/prices", url.Values{"product": {product.ID}, "currency": {"usd"}, "unit_amount": {strconv.FormatInt(amount, 10)}, "recurring[interval]": {interval}, "metadata[acceptance_run]": {runID}}, &price)
		if price.ID == "" {
			t.Fatal("Stripe price identity missing")
		}
		return price.ID
	}
	monthlyPrice, annualPrice := makePrice("monthly-price", "month", 1000), makePrice("annual-price", "year", 12000)
	for _, p := range []*store.BillingPlan{
		{PlanID: "PRO-MONTHLY", StripePriceID: monthlyPrice, Kind: "subscription", PriceUnit: "month", PackageName: "xconnect-pro", IncludedQuotaBytes: 20000, Active: true},
		{PlanID: "PRO-YEARLY", StripePriceID: annualPrice, Kind: "subscription", PriceUnit: "year", PackageName: "xconnect-pro", IncludedQuotaBytes: 20000, Active: true},
	} {
		if err := st.UpsertBillingPlan(ctx, p); err != nil {
			t.Fatal("cannot seed isolated catalog")
		}
	}
	testPassword := hex.EncodeToString(random[:16])
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal("cannot prepare isolated login")
	}
	owner := &store.User{PasswordHash: string(passwordHash), Name: "Sandbox owner " + uuid.NewString()[:8], Email: uuid.NewString() + "@example.invalid", Active: true, EmailVerified: true, Role: store.RoleUser, Groups: []string{"User"}}
	if err := st.CreateUser(ctx, owner); err != nil {
		t.Fatal("cannot create isolated inviter")
	}
	code := "S" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", ""))[:11]
	if _, err := st.EnsureReferralCode(ctx, owner.ID, code); err != nil {
		t.Fatal("cannot allocate isolated invitation code")
	}
	friend := &store.User{Name: "Sandbox friend " + uuid.NewString()[:8], Email: uuid.NewString() + "@example.invalid", Active: true, EmailVerified: true, Role: store.RoleUser, Groups: []string{"User"}}
	// The same transaction used by registration creates the new user and binds
	// attribution once; no retrospective binding or client reward facts are used.
	if err := st.CreateUserWithReferral(ctx, friend, code); err != nil {
		t.Fatal("cannot atomically register attributed friend")
	}
	tokens := auth.NewTokenService(auth.TokenConfig{Store: st, PublicToken: hex.EncodeToString(random[:8]), AccessSecret: hex.EncodeToString(random), RefreshSecret: hex.EncodeToString(random), AccessExpiry: time.Hour, RefreshExpiry: time.Hour})
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithEmailVerification(false), WithTokenService(tokens), WithStripeConfig(StripeConfig{Mode: "test", SecretKey: secret, WebhookSecret: webhookSecret, AllowedPriceIDs: []string{monthlyPrice, annualPrice}}))
	sendEvent := func(raw json.RawMessage) {
		t.Helper()
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		mac := hmac.New(sha256.New, []byte(webhookSecret))
		_, _ = mac.Write([]byte(timestamp + "." + string(raw)))
		request := httptest.NewRequest(http.MethodPost, "/api/billing/stripe/webhook", bytes.NewReader(raw))
		request.Header.Set("Stripe-Signature", "t="+timestamp+",v1="+hex.EncodeToString(mac.Sum(nil)))
		result := httptest.NewRecorder()
		router.ServeHTTP(result, request)
		if result.Code != http.StatusOK {
			t.Fatalf("signed real-event handler status=%d (body omitted)", result.Code)
		}
	}
	eventFor := func(eventType, objectID string) json.RawMessage {
		t.Helper()
		deadline := time.Now().Add(45 * time.Second)
		for time.Now().Before(deadline) {
			query := url.Values{"type": {eventType}, "limit": {"100"}, "created[gte]": {strconv.FormatInt(runStart, 10)}}
			for page := 0; page < 3; page++ {
				var events struct {
					Data    []json.RawMessage `json:"data"`
					HasMore bool              `json:"has_more"`
				}
				if err := client.doJSON(ctx, http.MethodGet, "/events?"+query.Encode(), &events); err != nil {
					t.Fatal("cannot read Sandbox events (body omitted)")
				}
				last := ""
				for _, raw := range events.Data {
					var event stripeEvent
					if json.Unmarshal(raw, &event) != nil {
						t.Fatal("invalid Stripe event envelope")
					}
					last = event.ID
					var object struct {
						ID string `json:"id"`
					}
					if json.Unmarshal(event.Data.Object, &object) != nil {
						t.Fatal("invalid Stripe event object")
					}
					if event.Type == eventType && object.ID == objectID {
						if event.Livemode == nil || *event.Livemode {
							t.Fatal("refusing live or unspecified event mode")
						}
						t.Logf("Stripe event %s object %s", event.ID, object.ID)
						return raw
					}
				}
				if !events.HasMore || last == "" {
					break
				}
				query.Set("starting_after", last)
			}
			select {
			case <-ctx.Done():
				t.Fatal("Sandbox acceptance timed out")
			case <-time.After(2 * time.Second):
			}
		}
		t.Fatalf("Sandbox event %s for %s did not arrive within bounded poll", eventType, objectID)
		return nil
	}
	makePaidSubscription := func(stage string, user *store.User, priceID, planID string) (string, *stripeInvoice) {
		t.Helper()
		var customer struct {
			ID string `json:"id"`
		}
		formCall(stage+"-customer", "/customers", url.Values{"email": {user.Email}, "payment_method": {"pm_card_visa"}, "invoice_settings[default_payment_method]": {"pm_card_visa"}, "metadata[acceptance_run]": {runID}}, &customer)
		var subscription stripeSubscription
		formCall(stage+"-subscription", "/subscriptions", url.Values{"customer": {customer.ID}, "items[0][price]": {priceID}, "payment_behavior": {"error_if_incomplete"}, "metadata[user_id]": {user.ID}, "metadata[plan_id]": {planID}, "metadata[product_slug]": {"xconnect"}, "metadata[kind]": {"subscription"}, "metadata[acceptance_run]": {runID}}, &subscription)
		if subscription.ID == "" || subscription.Status != "active" {
			t.Fatal("real Sandbox subscription did not activate")
		}
		invoiceID := customerIDFromAny(subscription.LatestInvoice)
		invoice, err := client.fetchInvoice(ctx, invoiceID)
		if err != nil || invoice.Status != "paid" || invoice.AmountPaid <= 0 {
			t.Fatal("real Sandbox invoice did not settle")
		}
		t.Logf("Stripe subscription %s invoice %s customer %s", subscription.ID, invoice.ID, customer.ID)
		// Stop future test renewals while preserving all objects and payment facts.
		t.Cleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := client.doForm(cleanupCtx, http.MethodPost, "/subscriptions/"+url.PathEscape(subscription.ID), url.Values{"cancel_at_period_end": {"true"}}, nil); err != nil {
				t.Log("test subscription cancellation scheduling failed (details omitted)")
			}
		})
		sendEvent(eventFor("invoice.paid", invoice.ID))
		return subscription.ID, invoice
	}
	ownerSubscription, ownerInvoice := makePaidSubscription("inviter", owner, monthlyPrice, "PRO-MONTHLY")
	_, friendInvoice := makePaidSubscription("friend", friend, annualPrice, "PRO-YEARLY")
	rewards, err := st.ListReferralRewards(ctx, owner.ID, 10)
	if err != nil || len(rewards) != 1 || rewards[0].ProviderInvoiceID != friendInvoice.ID || rewards[0].PaidMinor != friendInvoice.AmountPaid || rewards[0].RewardMinor != friendInvoice.AmountPaid/10 || rewards[0].AnnualDays != 30 {
		t.Fatal("real annual invoice did not award exact 10 percent and 30 days")
	}
	localOwnerSubs, err := st.ListSubscriptionsByUser(ctx, owner.ID)
	if err != nil {
		t.Fatal("cannot read inviter subscription")
	}
	ownerLocalID := ""
	for _, sub := range localOwnerSubs {
		if sub.ExternalID == ownerSubscription {
			ownerLocalID = sub.ID
		}
	}
	_, ownerBaseEnd, ok := referralInvoicePeriod(ownerInvoice)
	if !ok || ownerLocalID == "" {
		t.Fatal("paid inviter service period missing")
	}
	extended, err := st.ReferralExtensionEnd(ctx, ownerLocalID)
	if err != nil || !extended.Equal(ownerBaseEnd.Add(30*24*time.Hour)) {
		t.Fatal("earned tail is not exactly 30 days after paid inviter term")
	}
	snapshot, err := st.(store.ReferralCampaignStore).GetReferralRewardCampaign(ctx, rewards[0].ID)
	if err != nil || !snapshot.Active(rewards[0].PaidAt) {
		t.Fatal("reward lacks immutable payment-time campaign snapshot")
	}
	invoices, err := st.ListFinanceInvoices(ctx, friend.ID, "", 10)
	if err != nil || len(invoices) != 1 {
		t.Fatal("expected one friend finance invoice")
	}
	payments, err := st.ListFinancePayments(ctx, friend.ID, 10)
	if err != nil || len(payments) != 1 || payments[0].AmountMinor != friendInvoice.AmountPaid {
		t.Fatal("expected one settled friend payment")
	}
	ownerSession := loginFor(t, router, owner.Email, testPassword)
	overview := referralContractRequest(t, router, http.MethodGet, "/api/auth/referrals", ownerSession, nil)
	if overview.Code != 200 {
		t.Fatalf("center API status=%d", overview.Code)
	}
	var center struct {
		Code      string `json:"code"`
		Referrals []struct {
			RewardMinor int64 `json:"rewardMinor"`
		} `json:"referrals"`
		Extensions []struct {
			Days int `json:"days"`
		} `json:"extensions"`
	}
	if json.Unmarshal(overview.Body.Bytes(), &center) != nil || center.Code != code || len(center.Referrals) != 1 || center.Referrals[0].RewardMinor != friendInvoice.AmountPaid/10 || len(center.Extensions) != 1 || center.Extensions[0].Days != 30 {
		t.Fatal("center API disagrees with real payment/reward/extension facts")
	}
	paidEvent := eventFor("invoice.paid", friendInvoice.ID)
	sendEvent(paidEvent)
	sendEvent(paidEvent)
	payments, err = st.ListFinancePayments(ctx, friend.ID, 10)
	if err != nil || len(payments) != 1 {
		t.Fatal("event replay duplicated payment")
	}
	rewards, err = st.ListReferralRewards(ctx, owner.ID, 10)
	if err != nil || len(rewards) != 1 {
		t.Fatal("event replay duplicated reward")
	}
	var refund stripeRefund
	formCall("friend-refund", "/refunds", url.Values{"payment_intent": {payments[0].ProviderPaymentID}, "metadata[acceptance_run]": {runID}}, &refund)
	if refund.ID == "" || refund.Status != "succeeded" {
		t.Fatal("real Sandbox refund did not settle")
	}
	refundEvent := eventFor("refund.created", refund.ID)
	sendEvent(refundEvent)
	sendEvent(refundEvent)
	rewards, err = st.ListReferralRewards(ctx, owner.ID, 10)
	if err != nil || len(rewards) != 1 || rewards[0].Status != "reversed" {
		t.Fatal("real refund did not reverse original reward exactly once")
	}
	extensions, err := st.ListSubscriptionExtensions(ctx, owner.ID, 10)
	if err != nil || len(extensions) != 1 || extensions[0].Status != "reversed" {
		t.Fatal("real refund did not reverse earned extension")
	}
	after, err := st.ReferralExtensionEnd(ctx, ownerLocalID)
	if err != nil || !after.Equal(ownerBaseEnd) {
		t.Fatal("refund reversal changed original paid inviter service")
	}
	var refundCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM public.finance_refunds WHERE payment_id=$1`, payments[0].ID).Scan(&refundCount); err != nil || refundCount != 1 {
		t.Fatal("refund replay did not retain one financial refund fact")
	}
	t.Logf("PASS real Stripe Sandbox: product %s reward %s refund %s; local signing only; financial history retained", product.ID, rewards[0].ID, refund.ID)
}
