package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestReferralLedgerPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("ACCOUNTS_REFERRAL_TEST_DSN")
	if dsn == "" {
		if os.Getenv("ACCOUNTS_REFERRAL_TEST_DSN_REQUIRED") == "1" {
			t.Fatal("required isolated referral PostgreSQL test DSN is missing")
		}
		t.Skip("set ACCOUNTS_REFERRAL_TEST_DSN to fresh disposable referral_store_test database")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || (cfg.Host != "127.0.0.1" && cfg.Host != "localhost") || cfg.Database != "referral_store_test" {
		t.Fatal("integration test accepts only the local referral_store_test database")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("cannot open isolated referral test database")
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		t.Fatal("cannot connect to isolated referral test database")
	}
	var userCount int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM public.users`).Scan(&userCount); err != nil || userCount != 0 {
		t.Fatal("referral test database must be fresh and empty")
	}
	st := &postgresStore{db: db}
	ids := map[string]string{"owner": "00000000-0000-4000-8000-000000000001", "a": "00000000-0000-4000-8000-000000000002", "b": "00000000-0000-4000-8000-000000000003", "risk": "00000000-0000-4000-8000-000000000004", "c": "00000000-0000-4000-8000-000000000005", "d": "00000000-0000-4000-8000-000000000006", "e": "00000000-0000-4000-8000-000000000007"}
	for name, id := range ids {
		if _, err = db.ExecContext(ctx, `INSERT INTO public.users(uuid,email) VALUES($1,$2)`, id, name+"@example.invalid"); err != nil {
			t.Fatal("cannot seed synthetic user")
		}
	}
	if _, err = st.EnsureReferralCode(ctx, ids["owner"], "REFOWNER1"); err != nil {
		t.Fatal("cannot seed referral code")
	}
	for _, key := range []string{"a", "b", "risk", "c", "d", "e"} {
		if err = st.CreateReferralAttribution(ctx, "REFOWNER1", ids[key], time.Now().UTC()); err != nil {
			t.Fatal("cannot seed referral attribution")
		}
	}
	now := time.Now().UTC()
	baseEnd := now.Add(390 * 24 * time.Hour)
	ownerSub := "00000000-0000-4000-8000-000000000011"
	if _, err = db.ExecContext(ctx, `INSERT INTO public.subscriptions(uuid,user_uuid,provider,kind,status,plan_id,meta) VALUES($1,$2,'stripe','subscription','canceled','PRO-YEARLY',jsonb_build_object('product_slug','xconnect','plan_id','PRO-YEARLY','startsAt',$3::text,'expiresAt',$4::text))`, ownerSub, ids["owner"], now.Add(-100*24*time.Hour).Format(time.RFC3339), baseEnd.Format(time.RFC3339)); err != nil {
		t.Fatal("cannot seed paid inviter subscription")
	}
	if err = seedReferralInvoice(ctx, db, ids["owner"], ownerSub, "owner-paid-invoice", now.Add(-10*24*time.Hour), 10000); err != nil {
		t.Fatalf("cannot seed inviter payment: %v", err)
	}
	periodStart, periodEnd := now.Add(-10*24*time.Hour), now.Add(355*24*time.Hour)
	periodInvoice := &FinanceInvoice{IdempotencyKey: "stripe-invoice:period-enrichment", AccountUUID: ids["owner"], SubscriptionUUID: ownerSub, Provider: "stripe", ProviderInvoiceID: "period-enrichment", AmountMinor: 12000, Currency: "USD", Description: "Verified XConnect subscription invoice", IssuedAt: now.Add(-10 * 24 * time.Hour)}
	if inserted, e := st.CreateFinanceInvoice(ctx, periodInvoice); e != nil || !inserted {
		t.Fatalf("create invoice before period evidence: inserted=%v err=%v", inserted, e)
	}
	periodInvoice.ServicePeriodStart, periodInvoice.ServicePeriodEnd = &periodStart, &periodEnd
	if inserted, e := st.CreateFinanceInvoice(ctx, periodInvoice); e != nil || inserted {
		t.Fatalf("same invoice should enrich only missing verified period: inserted=%v err=%v", inserted, e)
	}
	storedPeriodInvoice, e := st.GetFinanceInvoice(ctx, periodInvoice.ID)
	if e != nil || storedPeriodInvoice.ServicePeriodStart == nil || !storedPeriodInvoice.ServicePeriodStart.Equal(periodStart) || storedPeriodInvoice.ServicePeriodEnd == nil || !storedPeriodInvoice.ServicePeriodEnd.Equal(periodEnd) {
		t.Fatalf("verified period enrichment did not persist: invoice=%+v err=%v", storedPeriodInvoice, e)
	}
	conflictingPeriod := *periodInvoice
	differentPeriodEnd := periodEnd.Add(time.Hour)
	conflictingPeriod.ServicePeriodEnd = &differentPeriodEnd
	if _, e = st.CreateFinanceInvoice(ctx, &conflictingPeriod); !errors.Is(e, ErrFinanceIdempotencyConflict) {
		t.Fatalf("already verified invoice period must be immutable: %v", e)
	}
	seedInvitee := func(key, invoice string, amount int64) {
		t.Helper()
		subIDs := map[string]string{"a": "00000000-0000-4000-8000-000000000021", "b": "00000000-0000-4000-8000-000000000022", "risk": "00000000-0000-4000-8000-000000000023"}
		sub := subIDs[key]
		if _, e := db.ExecContext(ctx, `INSERT INTO public.subscriptions(uuid,user_uuid,provider,kind,status,plan_id,meta) VALUES($1,$2,'stripe','subscription','active','PRO-YEARLY',jsonb_build_object('product_slug','xconnect','plan_id','PRO-YEARLY','startsAt',$3::text,'expiresAt',$4::text))`, sub, ids[key], now.Add(-30*24*time.Hour).Format(time.RFC3339), now.Add(335*24*time.Hour).Format(time.RFC3339)); e != nil {
			t.Fatal("cannot seed invitee subscription")
		}
		if e := seedReferralInvoice(ctx, db, ids[key], sub, invoice, now.Add(-10*24*time.Hour), amount); e != nil {
			t.Fatal("cannot seed invitee payment")
		}
	}
	seedInvitee("a", "invoice-a", 30000)
	seedInvitee("b", "invoice-b", 50000)
	seedInvitee("risk", "invoice-risk", 20000)
	createReward := func(key, invoice string, amount int64) *ReferralReward {
		t.Helper()
		r := &ReferralReward{ProviderInvoiceID: invoice, InviteeUUID: ids[key], PlanID: "PRO-YEARLY", ProductSlug: "xconnect", PaidMinor: amount, RewardMinor: amount / 10, Currency: "USD", PaidAt: now.Add(-8 * 24 * time.Hour), EligibleAt: now.Add(-time.Hour)}
		inserted, e := st.CreateReferralReward(ctx, r)
		if e != nil || !inserted {
			t.Fatalf("reward insert failed: inserted=%v err=%v", inserted, e)
		}
		return r
	}
	a := createReward("a", "invoice-a", 30000)
	settled, err := st.RequestReferralSettlement(ctx, ids["owner"], "USD", now)
	if err != nil || settled.AmountMinor != 3000 {
		t.Fatalf("initial settlement=%v err=%v", settled, err)
	}
	if _, err = st.CompleteReferralSettlement(ctx, settled.ID, ids["owner"], "payout-a", now); err != nil {
		t.Fatalf("complete initial settlement: %v", err)
	}
	if err = st.ReverseReferralReward(ctx, a.ID, ids["owner"], "refund-a", "test reversal", now); err != nil {
		t.Fatalf("reverse paid reward: %v", err)
	}
	if err = st.ReverseReferralReward(ctx, a.ID, ids["owner"], "refund-a-replay", "duplicate reversal", now); err != nil {
		t.Fatalf("duplicate reversal idempotency: %v", err)
	}
	summary, err := st.GetReferralSummary(ctx, ids["owner"], now)
	if err != nil || referralSummaryUSD(summary).EligibleMinor != 0 {
		t.Fatalf("paid reversal should create an unoffsettable debt until new rewards arrive: summary=%+v err=%v", summary, err)
	}
	b := createReward("b", "invoice-b", 50000)
	summary, err = st.GetReferralSummary(ctx, ids["owner"], now)
	if err != nil || referralSummaryUSD(summary).EligibleMinor != 2000 {
		t.Fatalf("eligible balance should net paid reversal debt once: summary=%+v err=%v", summary, err)
	}
	actualEnd, err := st.ReferralExtensionEnd(ctx, ownerSub)
	if err != nil || !actualEnd.Equal(baseEnd.Add(30*24*time.Hour)) {
		t.Fatalf("reversed extension effective end=%s err=%v", actualEnd, err)
	}
	var effectiveFrom, effectiveUntil time.Time
	if err = db.QueryRowContext(ctx, `SELECT effective_from,effective_until FROM public.subscription_extension_ledger WHERE reward_id=$1`, b.ID).Scan(&effectiveFrom, &effectiveUntil); err != nil || !effectiveFrom.Equal(baseEnd) || !effectiveUntil.Equal(baseEnd.Add(30*24*time.Hour)) {
		t.Fatalf("new extension timestamps %s..%s err=%v", effectiveFrom, effectiveUntil, err)
	}
	net, err := st.RequestReferralSettlement(ctx, ids["owner"], "USD", now)
	if err != nil || net.AmountMinor != 2000 {
		t.Fatalf("settlement should net 5000 reward against 3000 debt: %v err=%v", net, err)
	}
	if _, err = st.RequestReferralSettlement(ctx, ids["owner"], "USD", now); !errors.Is(err, ErrReferralSettlementUnavailable) {
		t.Fatalf("pending debt must not be offset twice: %v", err)
	}
	summary, err = st.GetReferralSummary(ctx, ids["owner"], now)
	if err != nil || referralSummaryUSD(summary).EligibleMinor != 0 {
		t.Fatalf("requested net settlement should reserve rewards and debt: summary=%+v err=%v", summary, err)
	}
	if _, err = st.CompleteReferralSettlement(ctx, net.ID, ids["owner"], "payout-net", now); err != nil {
		t.Fatalf("complete net settlement: %v", err)
	}
	if _, err = st.CompleteReferralSettlement(ctx, net.ID, ids["owner"], "payout-net", now); err != nil {
		t.Fatalf("payout replay: %v", err)
	}
	summary, err = st.GetReferralSummary(ctx, ids["owner"], now)
	if err != nil || referralSummaryUSD(summary).SettledMinor != 5000 || referralSummaryUSD(summary).ReversedMinor != 3000 || referralSummaryUSD(summary).EligibleMinor != 0 {
		t.Fatalf("gross paid, reversal debt, and available balance mismatch: summary=%+v err=%v", summary, err)
	}

	// A risk event blocks completion but keeps every item reserved until staff
	// confirms payment or explicitly rejects the unpaid request.
	c := createReward("c", "invoice-c", 30000)
	rewardE := createReward("d", "invoice-e", 30000)
	prepaid, err := st.RequestReferralSettlement(ctx, ids["owner"], "USD", now)
	if err != nil || prepaid.AmountMinor != 6000 {
		t.Fatalf("prepaid settlement=%v err=%v", prepaid, err)
	}
	if err = st.RecordReferralRisk(ctx, ReferralPaymentRisk{SourceKey: "risk-c", ProviderInvoiceID: c.ProviderInvoiceID, Type: "refund", Reason: "late refund", OccurredAt: now}); err != nil {
		t.Fatalf("record settlement risk: %v", err)
	}
	if _, err = st.CompleteReferralSettlement(ctx, prepaid.ID, ids["owner"], "late-payout", now); !errors.Is(err, ErrReferralSettlementRisk) {
		t.Fatalf("ordinary completion must reject risky settlement: %v", err)
	}
	if _, err = st.RequestReferralSettlement(ctx, ids["owner"], "USD", now); !errors.Is(err, ErrReferralSettlementUnavailable) {
		t.Fatalf("risk hold must keep unaffected items reserved against a second settlement: %v", err)
	}
	reconciled, err := st.ReconcileReferralSettlement(ctx, prepaid.ID, ids["owner"], "late-payout", "bank transfer completed before refund alert", now)
	if err != nil || reconciled.Status != "paid" || reconciled.PayoutReference != "late-payout" {
		t.Fatalf("record actual payout after risk rejection: result=%+v err=%v", reconciled, err)
	}
	if _, err = st.ReconcileReferralSettlement(ctx, prepaid.ID, ids["owner"], "late-payout", "idempotent replay", now); err != nil {
		t.Fatalf("reconciliation replay: %v", err)
	}
	var cStatus, eStatus string
	if err = db.QueryRowContext(ctx, `SELECT status FROM public.referral_rewards WHERE id=$1`, c.ID).Scan(&cStatus); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, `SELECT status FROM public.referral_rewards WHERE id=$1`, rewardE.ID).Scan(&eStatus); err != nil || cStatus != "reversed" || eStatus != "settled" {
		t.Fatalf("reconciled mixed payout statuses: risky=%s unaffected=%s err=%v", cStatus, eStatus, err)
	}
	if err = st.ReverseReferralReward(ctx, c.ID, ids["owner"], "refund-c-replay", "repeated risk", now); err != nil {
		t.Fatalf("reversed reward replay: %v", err)
	}
	createReward("e", "invoice-d", 80000)
	postRisk, err := st.RequestReferralSettlement(ctx, ids["owner"], "USD", now)
	if err != nil || postRisk.AmountMinor != 5000 {
		t.Fatalf("future settlement must offset reconciled payout reversal debt: result=%+v err=%v", postRisk, err)
	}

	// Hold the invoice lock, start the award, commit a risk tombstone, then
	// release. The award transaction must observe the committed tombstone.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("cannot start risk race fixture")
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('referral-invoice:' || $1,0))`, "invoice-risk"); err != nil {
		t.Fatal("cannot hold invoice lock")
	}
	result := make(chan error, 1)
	go func() {
		r := &ReferralReward{ProviderInvoiceID: "invoice-risk", InviteeUUID: ids["risk"], PlanID: "PRO-YEARLY", ProductSlug: "xconnect", PaidMinor: 20000, RewardMinor: 2000, Currency: "USD", PaidAt: now.Add(-8 * 24 * time.Hour), EligibleAt: now.Add(-time.Hour)}
		_, e := st.CreateReferralReward(ctx, r)
		result <- e
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err = tx.ExecContext(ctx, `INSERT INTO public.referral_payment_risks(source_key,provider_invoice_id,risk_type,reason) VALUES('risk-before-award','invoice-risk','refund','race fixture')`); err != nil {
		t.Fatal("cannot insert risk tombstone")
	}
	if err = tx.Commit(); err != nil {
		t.Fatal("cannot commit risk tombstone")
	}
	if err = <-result; err != nil {
		t.Fatalf("risk race operation: %v", err)
	}
	var count int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM public.referral_rewards WHERE provider_invoice_id='invoice-risk'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("risk-before-award reward count=%d err=%v", count, err)
	}
	if _, err = st.GetReferralSummary(ctx, ids["owner"], now); err != nil {
		t.Fatalf("debt summary query: %v", err)
	}
}

func referralSummaryUSD(summary *ReferralSummary) ReferralCurrencySummary {
	if summary != nil {
		for _, item := range summary.CashByCurrency {
			if item.Currency == "USD" {
				return item
			}
		}
	}
	return ReferralCurrencySummary{Currency: "USD"}
}

func seedReferralInvoice(ctx context.Context, db *sql.DB, userID, subscriptionID, invoiceID string, paidAt time.Time, amount int64) error {
	var id string
	if err := db.QueryRowContext(ctx, `INSERT INTO public.finance_invoices(idempotency_key,account_uuid,subscription_uuid,provider,provider_invoice_id,amount_minor,currency,description,issued_at,service_period_start,service_period_end) VALUES('stripe-invoice:'||$3,$1,$2,'stripe',$3,$4,'USD','Verified XConnect inviter subscription invoice',$5::timestamptz,$5::timestamptz - interval '1 day',$5::timestamptz + interval '400 days') RETURNING id::text`, userID, subscriptionID, invoiceID, amount, paidAt).Scan(&id); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `INSERT INTO public.finance_payments(idempotency_key,invoice_id,account_uuid,provider,provider_payment_id,amount_minor,currency,paid_at) VALUES('stripe-payment:'||$3,$1,$2,'stripe','pi_'||$3,$4,'USD',$5)`, id, userID, invoiceID, amount, paidAt)
	return err
}
