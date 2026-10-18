package api

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"account/internal/store"
)

// Pay-As-You-Go top-up crediting.
//
// A one-time Stripe checkout previously recorded only a subscription row and
// never touched current_balance, so a PAYG customer could pay and end up with
// a balance of zero — the core path of that tier was broken.
//
// Idempotency is the hard requirement here, because Stripe retries. The
// existing event guard (BeginStripeWebhookEvent) deliberately replays events
// whose previous attempt did not reach `processed`, which is right for
// reliability but means a handler that credits and then fails downstream
// would credit twice on the retry. Money must not be created by a retry.
//
// The ledger's own primary key is used as the lock: the entry id is derived
// deterministically from the payment intent, so a second attempt to credit
// the same payment collides on the PK instead of inserting. No new table, no
// migration, and the guarantee lives in the database rather than in a check
// that a future refactor could reorder away.

// topUpNamespace is a fixed UUID namespace for deriving ledger ids from Stripe
// payment identifiers. It must never change: doing so would make previously
// credited payments look uncredited and allow a double credit.
var topUpNamespace = uuid.MustParse("6f1b3f0e-9c1a-4f6e-8f4a-2c9a5b7d1e30")

const topUpEntryType = "topup"

// topUpLedgerID maps a Stripe payment reference onto a stable ledger id.
func topUpLedgerID(paymentRef string) string {
	return uuid.NewSHA1(topUpNamespace, []byte(strings.TrimSpace(paymentRef))).String()
}

// creditTopUpBalance adds a completed one-time payment to the account balance.
//
// Returns nil for sessions that are not top-ups (a paid session with no
// amount, or one whose payment did not actually succeed) — those are not
// errors, they simply have nothing to credit.
func (h *handler) creditTopUpBalance(ctx context.Context, userID string, session *stripeCheckoutSession) error {
	if session == nil || strings.TrimSpace(userID) == "" {
		return nil
	}
	// Every supported Checkout Session carries server-controlled catalog
	// metadata. Do not credit hand-created sessions that lack it, and do not
	// treat a recurring session as a PAYG top-up.
	if kind := strings.TrimSpace(session.Metadata["kind"]); !strings.EqualFold(kind, "paygo") {
		slog.Warn("skipping balance credit for non-PAYG checkout session",
			"userID", userID, "sessionID", session.ID)
		return nil
	}

	// Stripe reports `paid` for a completed one-time payment. Anything else
	// (unpaid, no_payment_required) must not move money.
	if status := strings.TrimSpace(strings.ToLower(session.PaymentStatus)); status != "" && status != "paid" {
		slog.Info("skipping top-up credit for non-paid session",
			"userID", userID, "sessionID", session.ID, "paymentStatus", status)
		return nil
	}
	if session.AmountTotal <= 0 {
		return nil
	}
	planID := strings.TrimSpace(session.Metadata["plan_id"])
	currency := strings.ToUpper(strings.TrimSpace(session.Currency))
	amountMinor := session.AmountTotal
	custom := isCustomPaygoTopUp(planID, amountMinor, currency)
	var plan *store.BillingPlan
	if custom {
		metadataAmount, parseErr := strconv.ParseInt(strings.TrimSpace(session.Metadata["amount_minor"]), 10, 64)
		if parseErr != nil || metadataAmount != amountMinor || !strings.EqualFold(session.Metadata["currency"], currency) {
			return errors.New("custom top-up amount does not match checkout metadata")
		}
	} else {
		var err error
		plan, err = h.store.GetBillingPlan(ctx, planID)
		if err != nil || plan == nil || !strings.EqualFold(strings.TrimSpace(plan.Kind), "paygo_topup") || !isFixedPaygoTopUp(planID, plan.PriceAmount, plan.PriceCurrency, plan.PriceUnit) {
			return errors.New("top-up session does not match an approved denomination")
		}
		if currency != strings.ToUpper(strings.TrimSpace(plan.PriceCurrency)) || amountMinor != plan.PriceAmount {
			return errors.New("top-up amount or currency does not match the billing catalog")
		}
	}
	if currency != "CNY" && currency != "USD" {
		return errors.New("unsupported top-up currency")
	}

	// Prefer the payment intent: it is the identifier Stripe keeps stable
	// across retries of the same payment. The session id is the fallback for
	// the rare case where the intent is not expanded on the event.
	paymentRef := firstNonEmpty(session.PaymentIntent, session.ID)
	if strings.TrimSpace(paymentRef) == "" {
		return errors.New("top-up session has neither a payment intent nor an id")
	}

	// Stripe amounts are in the smallest currency unit.
	amount := float64(session.AmountTotal) / 100.0
	entryID := topUpLedgerID(paymentRef)

	now := time.Now().UTC()

	// Ledger first, balance second. Balance has to equal the sum of its
	// ledger; writing the balance first would leave an unexplainable figure if
	// the ledger insert then failed. If this insert collides on the PK, the
	// payment was already credited by a concurrent delivery and we stop.
	entry := &store.BillingLedgerEntry{
		ID:                 entryID,
		AccountUUID:        userID,
		BucketStart:        now,
		BucketEnd:          now,
		EntryType:          topUpEntryType,
		Currency:           currency,
		RatedBytes:         0,
		AmountDelta:        amount,
		PricingRuleVersion: "stripe:" + paymentRef,
	}
	// A PK collision means this payment was already credited — by an earlier
	// delivery or a concurrent one. Either way the correct outcome is to do
	// nothing and report success, so Stripe stops redelivering. This is the
	// idempotency guarantee itself, not an optimisation, so it is not preceded
	// by a "does it already exist" read: a read would still leave a window
	// between check and insert.
	duplicate, err := h.store.CreditTopUp(ctx, entry)
	if err != nil {
		return err
	}
	if duplicate {
		slog.Info("top-up already credited, skipping", "userID", userID, "paymentRef", paymentRef, "entryID", entryID)
	} else {
		h.publishBillingEvent(ctx, &store.BillingEvent{Type: "balance_topped_up", UserID: userID})
	}
	if !h.hasActivePaidSubscription(ctx, userID) {
		paygPlan, paygErr := h.store.GetBillingPlan(ctx, "PAYG")
		if paygErr == nil && paygPlan != nil {
			if err := h.applyPlanEntitlements(ctx, userID, paygPlan); err != nil {
				return err
			}
		}
		if state, stateErr := h.store.GetAccountQuotaState(ctx, userID); stateErr == nil && state != nil && state.RemainingIncludedQuota != 0 {
			state.RemainingIncludedQuota = 0
			state.EffectiveAt = now
			if err := h.store.UpsertAccountQuotaState(ctx, state); err != nil {
				return err
			}
		} else if stateErr != nil && !errors.Is(stateErr, store.ErrUserNotFound) {
			return stateErr
		}
	}
	slog.Info("credited top-up to currency wallet", "userID", userID, "amount", amount, "currency", currency)
	return nil
}

func (h *handler) hasActivePaidSubscription(ctx context.Context, userID string) bool {
	subscriptions, err := h.store.ListSubscriptionsByUser(ctx, userID)
	if err != nil {
		slog.Warn("failed to check active subscription", "err", err, "userID", userID)
		return true
	}
	for i := range subscriptions {
		sub := &subscriptions[i]
		if !strings.EqualFold(strings.TrimSpace(sub.Kind), "subscription") {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(sub.Status)) {
		case "active", "trialing", "past_due", "canceling":
			return true
		}
	}
	return false
}

func (h *handler) validateBillingModeCheckout(ctx context.Context, userID string, req stripeCheckoutRequest) error {
	if req.Mode == "payment" {
		if h.hasActivePaidSubscription(ctx, userID) {
			return errors.New("按量付费与有效订阅互斥；请先取消并等待订阅到期")
		}
		return nil
	}
	// CNY is the metered usage wallet. USD is kept as a separate balance and is
	// not automatically converted or charged against CNY usage.
	cnyBalance, err := h.store.GetWalletBalance(ctx, userID, "CNY")
	if err != nil {
		return errors.New("无法确认按量付费余额，请稍后重试")
	}
	if cnyBalance != 0 {
		return errors.New("请先结清人民币按量余额，再开通订阅；两种计费方式不叠加")
	}
	return nil
}

// isDuplicateKeyError remains for older call sites which still insert ledger
// rows directly; top-up crediting uses the atomic store operation above.
func isDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, store.ErrDuplicateLedgerEntry) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate key") || strings.Contains(msg, "unique constraint") || strings.Contains(msg, "already exists")
}
