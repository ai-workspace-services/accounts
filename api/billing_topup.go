package api

import (
	"context"
	"errors"
	"log/slog"
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

	state, err := h.store.GetAccountQuotaState(ctx, userID)
	if err != nil || state == nil {
		// A PAYG customer may be topping up before any entitlement sync has
		// created their quota row; that is a normal first-purchase path.
		state = &store.AccountQuotaState{AccountUUID: userID}
	}

	newBalance := state.CurrentBalance + amount
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
		RatedBytes:         0,
		AmountDelta:        amount,
		BalanceAfter:       newBalance,
		PricingRuleVersion: "stripe:" + paymentRef,
	}
	// A PK collision means this payment was already credited — by an earlier
	// delivery or a concurrent one. Either way the correct outcome is to do
	// nothing and report success, so Stripe stops redelivering. This is the
	// idempotency guarantee itself, not an optimisation, so it is not preceded
	// by a "does it already exist" read: a read would still leave a window
	// between check and insert.
	if err := h.store.InsertBillingLedgerEntry(ctx, entry); err != nil {
		if isDuplicateKeyError(err) {
			slog.Info("top-up already credited, skipping",
				"userID", userID, "paymentRef", paymentRef, "entryID", entryID)
			return nil
		}
		return err
	}

	state.CurrentBalance = newBalance
	state.EffectiveAt = now
	// A successful top-up settles the arrears episode: the customer has paid.
	// Suspension is lifted by billing-service on its next pass, driven by the
	// cleared arrears flag.
	if state.Arrears {
		state.Arrears = false
		state.ArrearsSince = nil
	}
	if err := h.store.UpsertAccountQuotaState(ctx, state); err != nil {
		return err
	}

	h.publishBillingEvent(ctx, &store.BillingEvent{
		Type: "balance_topped_up", UserID: userID,
	})

	slog.Info("credited top-up to balance",
		"userID", userID, "amount", amount, "currency", session.Currency, "balance", newBalance)
	return nil
}

// isDuplicateKeyError reports whether an insert failed because the row already
// exists. The memory store returns a sentinel; postgres surfaces a driver
// error, matched on text because the store layer returns it unwrapped and this
// package does not depend on the pq/pgx error types.
func isDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, store.ErrDuplicateLedgerEntry) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "already exists")
}
