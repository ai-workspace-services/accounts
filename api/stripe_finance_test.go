package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"account/internal/store"
)

func TestSandboxRejectsSignedLiveModeEvent(t *testing.T) {
	router, _, _ := newBillingWebhookHarness(t)
	rec := postStripeEvent(t, router, []byte(`{"id":"evt_live_rejected","type":"invoice.paid","livemode":true,"data":{"object":{}}}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("sandbox accepted live event: %d", rec.Code)
	}
}

func TestCloverInvoiceAndSubscriptionSnapshots(t *testing.T) {
	var invoice stripeInvoice
	err := json.Unmarshal([]byte(`{"id":"in_clover","status":"paid","amount_paid":1200,"currency":"usd","parent":{"subscription_details":{"subscription":"sub_clover"}},"payments":{"data":[{"status":"paid","amount_paid":1200,"currency":"usd","payment":{"type":"payment_intent","payment_intent":"pi_clover"}}],"has_more":false}}`), &invoice)
	if err != nil || customerIDFromAny(invoice.Subscription) != "sub_clover" || customerIDFromAny(invoice.PaymentIntent) != "pi_clover" {
		t.Fatalf("Clover invoice lost provider references: %#v, %v", invoice, err)
	}
	var sub stripeSubscription
	err = json.Unmarshal([]byte(`{"id":"sub_clover","items":{"data":[{"current_period_start":1770000000,"current_period_end":1772600000,"price":{"id":"price_clover"}}]}}`), &sub)
	if err != nil || sub.CurrentPeriodStart != 1770000000 || sub.CurrentPeriodEnd != 1772600000 {
		t.Fatalf("Clover item period lost: %#v, %v", sub, err)
	}
	for _, body := range []string{
		`{"amount_paid":1200,"currency":"usd","payments":{"data":[{"status":"open","amount_paid":1200,"currency":"usd","payment":{"type":"payment_intent","payment_intent":"pi_not_paid"}}]}}`,
		`{"amount_paid":1200,"currency":"usd","payments":{"has_more":true,"data":[{"status":"paid","amount_paid":1200,"currency":"usd","payment":{"type":"payment_intent","payment_intent":"pi_incomplete"}}]}}`,
		`{"amount_paid":1200,"currency":"usd","payments":{"data":[{"status":"paid","amount_paid":1000,"currency":"usd","payment":{"type":"payment_intent","payment_intent":"pi_partial"}}]}}`,
	} {
		if err := json.Unmarshal([]byte(body), &invoice); err != nil || customerIDFromAny(invoice.PaymentIntent) != "" {
			t.Fatalf("must not invent a settled payment from incomplete facts: %#v, %v", invoice, err)
		}
	}
}

func TestStripeRefundFactsAreIdempotentAndBoundedByOriginalPayment(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	user := &store.User{Name: "Sandbox", Email: "p0f-refund@example.invalid"}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	invoice := &store.FinanceInvoice{IdempotencyKey: "refund-test-invoice", AccountUUID: user.ID, Provider: "stripe", ProviderInvoiceID: "in_refund", AmountMinor: 1200, Currency: "USD"}
	if _, err := st.CreateFinanceInvoice(ctx, invoice); err != nil {
		t.Fatal(err)
	}
	payment := &store.FinancePayment{IdempotencyKey: "refund-test-payment", InvoiceID: invoice.ID, AccountUUID: user.ID, Provider: "stripe", ProviderPaymentID: "pi_refund", AmountMinor: 1200, Currency: "USD"}
	if _, err := st.RecordFinancePayment(ctx, payment); err != nil {
		t.Fatal(err)
	}
	h := &handler{store: st}
	refund := stripeRefund{ID: "re_partial", Status: "pending", PaymentIntent: "pi_refund", Amount: 400, Currency: "usd", Created: 1770000000}
	if err := h.recordStripeRefund(ctx, &refund); err != nil {
		t.Fatal(err)
	}
	facts, _ := st.ListFinanceRefunds(ctx, user.ID, 0)
	if len(facts) != 0 {
		t.Fatal("pending refund must not post")
	}
	refund.Status = "succeeded"
	for _, eventType := range []string{"refund.created", "refund.updated", "refund.updated"} {
		body, _ := json.Marshal(refund)
		event := stripeEvent{Type: eventType}
		event.Data.Object = body
		if err := h.handleStripeEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	facts, _ = st.ListFinanceRefunds(ctx, user.ID, 0)
	if len(facts) != 1 || facts[0].AmountMinor != 400 || facts[0].PaymentID != payment.ID {
		t.Fatalf("refund facts: %#v", facts)
	}
	refund.ID, refund.Amount = "re_over", 801
	if err := h.recordStripeRefund(ctx, &refund); !errors.Is(err, store.ErrFinanceRefundExceedsPayment) {
		t.Fatalf("over refund must fail: %v", err)
	}
	refund.ID, refund.Amount, refund.Currency = "re_wrong_currency", 100, "EUR"
	if err := h.recordStripeRefund(ctx, &refund); err == nil {
		t.Fatal("currency mismatch must fail")
	}
	refund.ID, refund.PaymentIntent, refund.Currency = "re_out_of_order", "pi_not_yet_seen", "USD"
	if err := h.recordStripeRefund(ctx, &refund); !errors.Is(err, store.ErrFinanceRecordNotFound) {
		t.Fatalf("out-of-order refund must remain retryable: %v", err)
	}
}

func TestCloverFullRefundRevokesOnlyAfterAllPaymentsAreRefunded(t *testing.T) {
	_, st, user := newBillingWebhookHarness(t)
	ctx := context.Background()
	// A stale validity window can survive a later explicit Free assignment.
	// It must not keep a just-refunded Stripe purchase entitled to Plus.
	validUntil := time.Now().UTC().AddDate(0, 6, 0)
	user.SubscriptionValidUntil = &validUntil
	user.Groups = []string{"User", "segment:free", store.MonthlyPlusQuotaLimitGroup, "operator:keep"}
	if err := st.UpdateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	sub := &store.Subscription{UserID: user.ID, Provider: "stripe", Kind: "subscription", ExternalID: "sub_full_refund", Status: "active", PlanID: "PRO-M"}
	if err := st.UpsertSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	invoice := &store.FinanceInvoice{IdempotencyKey: "full-refund-invoice", AccountUUID: user.ID, SubscriptionUUID: sub.ID, Provider: "stripe", ProviderInvoiceID: "in_full", AmountMinor: 1200, Currency: "USD"}
	if _, err := st.CreateFinanceInvoice(ctx, invoice); err != nil {
		t.Fatal(err)
	}
	payment := &store.FinancePayment{IdempotencyKey: "full-refund-payment", InvoiceID: invoice.ID, AccountUUID: user.ID, Provider: "stripe", ProviderPaymentID: "pi_full", AmountMinor: 1200, Currency: "USD"}
	if _, err := st.RecordFinancePayment(ctx, payment); err != nil {
		t.Fatal(err)
	}
	reviewBody, _ := json.Marshal(refundReviewRequest{AccountID: user.ID, SubscriptionID: sub.ID, ExternalID: sub.ExternalID, RequestedAt: time.Now().UTC()})
	review := &store.FinanceOperation{IdempotencyKey: refundReviewKey(user.ID), OperationType: "refund_review", TargetType: "account", TargetID: user.ID, Provider: "stripe", Request: reviewBody}
	lifecycle, err := st.GetSubscriptionLifecycle(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSubscriptionRefundReview(ctx, review, lifecycle.Version); err != nil {
		t.Fatal(err)
	}
	h := &handler{store: st}
	refund := stripeRefund{ID: "re_first", Status: "succeeded", PaymentIntent: "pi_full", Amount: 400, Currency: "usd", Created: 1770000000}
	if err := h.recordStripeRefund(ctx, &refund); err != nil {
		t.Fatal(err)
	}
	subs, _ := st.ListSubscriptionsByUser(ctx, user.ID)
	if len(subs) != 1 || subs[0].Status != "active" {
		t.Fatalf("partial refund revoked access: %#v", subs)
	}
	pending, _ := st.GetSubscriptionLifecycle(ctx, user.ID)
	if !pending.RefundReviewPending {
		t.Fatal("partial refund cleared review")
	}
	refund.ID, refund.Amount = "re_second", 800
	for range 2 {
		if err := h.recordStripeRefund(ctx, &refund); err != nil {
			t.Fatal(err)
		}
	}
	subs, _ = st.ListSubscriptionsByUser(ctx, user.ID)
	if len(subs) != 1 || subs[0].Status != "refunded" {
		t.Fatalf("full refund did not revoke access: %#v", subs)
	}
	storedUser, _ := st.GetUserByID(ctx, user.ID)
	if store.MonthlyQuotaGroup(storedUser) != store.MonthlyFreeQuotaLimitGroup || !slices.Contains(storedUser.Groups, "operator:keep") {
		t.Fatalf("full refund did not restore Free quota group or removed unrelated group: %#v", storedUser.Groups)
	}
	profile, err := st.GetAccountBillingProfile(ctx, user.ID)
	if err != nil || profile.PricingRuleVersion != "plan:FREE" {
		t.Fatalf("full refund did not restore Free billing profile: %#v %v", profile, err)
	}

	source := &stripeSubscription{ID: sub.ExternalID, Status: "active", LatestInvoice: "in_full", Metadata: map[string]string{"user_id": user.ID}}
	if err := h.upsertStripeSubscription(ctx, source, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.syncSubscriptionEntitlements(ctx, source, false); err != nil {
		t.Fatal(err)
	}
	subs, _ = st.ListSubscriptionsByUser(ctx, user.ID)
	if subs[0].Status != "refunded" {
		t.Fatal("late active event restored refunded access")
	}
	review, err = st.GetFinanceOperation(ctx, refundReviewKey(user.ID))
	if err != nil || review.Status != store.FinanceOperationSucceeded {
		t.Fatalf("review not completed: %#v %v", review, err)
	}
	lifecycle, err = st.GetSubscriptionLifecycle(ctx, user.ID)
	if err != nil || lifecycle.RefundReviewPending {
		t.Fatalf("review gate remains blocked: %#v %v", lifecycle, err)
	}
}

func TestRefundPreservesActiveListedLocalSubscription(t *testing.T) {
	_, st, user := newBillingWebhookHarness(t)
	ctx := context.Background()
	validUntil := time.Now().UTC().AddDate(0, 6, 0)
	user.SubscriptionValidUntil = &validUntil
	user.Groups = []string{"User", store.MonthlyPlusQuotaLimitGroup, store.SubscriptionListedGroup, "segment:yearly"}
	if err := st.UpdateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	sub := &store.Subscription{UserID: user.ID, Provider: "stripe", Kind: "subscription", ExternalID: "sub_local_grant", Status: "active", PlanID: "PLUS"}
	if err := st.UpsertSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	h := &handler{store: st}
	if err := h.revokeRefundedSubscription(ctx, user.ID, sub.ExternalID); err != nil {
		t.Fatal(err)
	}
	storedUser, _ := st.GetUserByID(ctx, user.ID)
	if store.MonthlyQuotaGroup(storedUser) != store.MonthlyPlusQuotaLimitGroup {
		t.Fatalf("active listed local subscription was removed: %#v", storedUser.Groups)
	}
}

func TestCloverPartialRefundReversesReferralRewardWithoutDeletingHistory(t *testing.T) {
	ctx := context.Background()
	harness := newReferralContractHarness(t)
	owner, _ := harness.user(t, "campaign-owner", "campaign owner", "", store.RoleUser)
	invitee, _ := harness.user(t, "campaign-invitee", "campaign invitee", "", store.RoleUser)
	reward := seedReferralReward(t, harness.store, owner, invitee, "campaign@example.invalid")
	invoice := &store.FinanceInvoice{IdempotencyKey: "campaign-invoice", AccountUUID: invitee.ID, Provider: "stripe", ProviderInvoiceID: reward.ProviderInvoiceID, AmountMinor: 1000, Currency: "USD"}
	if _, err := harness.store.CreateFinanceInvoice(ctx, invoice); err != nil {
		t.Fatal(err)
	}
	payment := &store.FinancePayment{IdempotencyKey: "campaign-payment", InvoiceID: invoice.ID, AccountUUID: invitee.ID, Provider: "stripe", ProviderPaymentID: "pi_campaign", AmountMinor: 1000, Currency: "USD"}
	if _, err := harness.store.RecordFinancePayment(ctx, payment); err != nil {
		t.Fatal(err)
	}
	h := &handler{store: harness.store}
	refund := stripeRefund{ID: "re_campaign", Status: "pending", PaymentIntent: "pi_campaign", Amount: 100, Currency: "USD", Created: 1770000000}
	if err := h.recordStripeRefund(ctx, &refund); err != nil {
		t.Fatal(err)
	}
	rewards, _ := harness.store.ListReferralRewards(ctx, owner.ID, 0)
	if len(rewards) != 1 || rewards[0].Status == "reversed" {
		t.Fatal("pending refund changed reward")
	}
	refund.Status = "succeeded"
	for range 2 {
		if err := h.recordStripeRefund(ctx, &refund); err != nil {
			t.Fatal(err)
		}
	}
	rewards, _ = harness.store.ListReferralRewards(ctx, owner.ID, 0)
	if len(rewards) != 1 || rewards[0].ID != reward.ID || rewards[0].Status != "reversed" {
		t.Fatalf("reward history not retained/reversed: %#v", rewards)
	}
	refunds, _ := harness.store.ListFinanceRefunds(ctx, invitee.ID, 0)
	if len(refunds) != 1 {
		t.Fatalf("campaign replay duplicated financial refund: %d", len(refunds))
	}
}

func TestRecordStripePaidInvoicePersistsInvoiceAndPaymentIdempotently(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	user := &store.User{Name: "Stripe Test", Email: "stripe-finance@example.com", EmailVerified: true, Role: store.RoleUser, Level: store.LevelUser, Active: true}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	subscription := &store.Subscription{ID: "local-subscription", UserID: user.ID, Provider: "stripe", Kind: "subscription", ExternalID: "sub_test_123", Status: "active"}
	if err := st.UpsertSubscription(ctx, subscription); err != nil {
		t.Fatal(err)
	}
	paidAt := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	invoice := &stripeInvoice{
		ID: "in_test_paid_123", Subscription: "sub_test_123", Status: "paid",
		PaymentIntent: "pi_test_paid_123", AmountPaid: 1200, Currency: "usd", Created: paidAt.Add(-time.Hour).Unix(),
	}
	invoice.StatusTransitions.PaidAt = paidAt.Unix()
	h := &handler{store: st}
	for range 2 {
		if err := h.recordStripePaidInvoice(ctx, invoice, &stripeSubscription{ID: "sub_test_123"}, user.ID); err != nil {
			t.Fatalf("record invoice: %v", err)
		}
	}
	invoices, err := st.ListFinanceInvoices(ctx, user.ID, subscription.ID, 10)
	if err != nil || len(invoices) != 1 {
		t.Fatalf("want one local invoice, got %d (err=%v)", len(invoices), err)
	}
	payments, err := st.ListFinancePayments(ctx, user.ID, 10)
	if err != nil || len(payments) != 1 {
		t.Fatalf("want one local payment, got %d (err=%v)", len(payments), err)
	}
	if invoices[0].ProviderInvoiceID != invoice.ID || invoices[0].AmountMinor != 1200 || invoices[0].Currency != "USD" {
		t.Fatalf("unexpected invoice facts: %+v", invoices[0])
	}
	if payments[0].ProviderPaymentID != "pi_test_paid_123" || payments[0].AmountMinor != 1200 || payments[0].Currency != "USD" {
		t.Fatalf("unexpected payment facts: %+v", payments[0])
	}
}

func TestRecordStripePaidInvoiceRejectsMismatchedSubscriptionAndInvalidCurrency(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	user := &store.User{Name: "Stripe Test", Email: "stripe-finance-reject@example.com", EmailVerified: true, Role: store.RoleUser, Level: store.LevelUser, Active: true}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	subscription := &store.Subscription{ID: "local-subscription", UserID: user.ID, Provider: "stripe", Kind: "subscription", ExternalID: "sub_expected", Status: "active"}
	if err := st.UpsertSubscription(ctx, subscription); err != nil {
		t.Fatal(err)
	}
	paidAt := time.Now().UTC().Unix()
	h := &handler{store: st}
	base := &stripeInvoice{ID: "in_rejected", Subscription: "sub_other", Status: "paid", PaymentIntent: "pi_rejected", AmountPaid: 500, Currency: "USD"}
	base.StatusTransitions.PaidAt = paidAt
	if err := h.recordStripePaidInvoice(ctx, base, &stripeSubscription{ID: "sub_expected"}, user.ID); err == nil {
		t.Fatal("expected mismatched subscription to be rejected")
	}
	base.Subscription = "sub_expected"
	base.Currency = "U$D"
	if err := h.recordStripePaidInvoice(ctx, base, &stripeSubscription{ID: "sub_expected"}, user.ID); err == nil {
		t.Fatal("expected invalid currency to be rejected")
	}
	invoices, err := st.ListFinanceInvoices(ctx, user.ID, "", 10)
	if err != nil || len(invoices) != 0 {
		t.Fatalf("invalid invoices must have no local side effects: %d (err=%v)", len(invoices), err)
	}
}

func TestRecordStripePaidInvoiceDoesNotInventPaymentIntent(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	user := &store.User{Name: "Stripe Test", Email: "stripe-finance-no-pi@example.com", EmailVerified: true, Role: store.RoleUser, Level: store.LevelUser, Active: true}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	subscription := &store.Subscription{ID: "local-subscription", UserID: user.ID, Provider: "stripe", Kind: "subscription", ExternalID: "sub_no_pi", Status: "active"}
	if err := st.UpsertSubscription(ctx, subscription); err != nil {
		t.Fatal(err)
	}
	invoice := &stripeInvoice{ID: "in_no_pi", Subscription: "sub_no_pi", Status: "paid", AmountPaid: 100, Currency: "USD"}
	invoice.StatusTransitions.PaidAt = time.Now().UTC().Unix()
	if err := (&handler{store: st}).recordStripePaidInvoice(ctx, invoice, &stripeSubscription{ID: "sub_no_pi"}, user.ID); err == nil {
		t.Fatal("expected paid invoice without payment intent to be left for repair")
	}
	invoices, err := st.ListFinanceInvoices(ctx, user.ID, "", 10)
	if err != nil || len(invoices) != 1 {
		t.Fatalf("invoice fact should be durable for retry: %d (err=%v)", len(invoices), err)
	}
	payments, err := st.ListFinancePayments(ctx, user.ID, 10)
	if err != nil || len(payments) != 0 {
		t.Fatalf("must not invent a payment: %+v (err=%v)", payments, err)
	}
}
