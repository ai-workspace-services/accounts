package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestReferralAnnualBonusRequiresUnrefundedInviterPaymentAtInviteePaidAt uses
// a dedicated disposable database. It intentionally covers payment facts and
// extension-ledger writes that the memory Store cannot represent.
func TestReferralAnnualBonusRequiresUnrefundedInviterPaymentAtInviteePaidAt(t *testing.T) {
	dsn := os.Getenv("ACCOUNTS_REFERRAL_ELIGIBILITY_TEST_DSN")
	if dsn == "" {
		if os.Getenv("ACCOUNTS_REFERRAL_ELIGIBILITY_TEST_DSN_REQUIRED") == "1" {
			t.Fatal("required isolated annual eligibility PostgreSQL test DSN is missing")
		}
		t.Skip("set ACCOUNTS_REFERRAL_ELIGIBILITY_TEST_DSN to a fresh local referral_annual_test database")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || (cfg.Host != "127.0.0.1" && cfg.Host != "localhost") || cfg.Database != "referral_annual_test" {
		t.Fatal("integration test accepts only the local referral_annual_test database")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("cannot open isolated referral annual test database")
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal("cannot connect to isolated referral annual test database")
	}
	var users int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM public.users`).Scan(&users); err != nil || users != 0 {
		t.Fatal("annual eligibility test database must be fresh and empty")
	}
	st := &postgresStore{db: db}

	now := time.Now().UTC().Truncate(time.Second)
	eventAt := now.Add(-time.Hour)
	baseEnd := now.Add(45 * 24 * time.Hour)
	cases := []struct {
		name                       string
		ownerID                    string
		inviteeID                  string
		ownerSubID                 string
		code                       string
		invoiceID                  string
		paymentAt                  time.Time
		refundedAt                 time.Time
		seedPayment                bool
		providerRisk               bool
		renewedAfterEvent          bool
		seedPriorExtension         bool
		seedReversedPriorExtension bool
		futureUnpaidRenewal        bool
		wantAnnualDay              int
	}{
		{name: "unrefunded payment before invitee payment", ownerID: "00000000-0000-4000-8000-000000000101", inviteeID: "00000000-0000-4000-8000-000000000102", ownerSubID: "00000000-0000-4000-8000-000000000111", code: "ANNUALOK101", invoiceID: "owner-invoice-valid", paymentAt: eventAt.Add(-24 * time.Hour), seedPayment: true, wantAnnualDay: 30},
		{name: "fully refunded inviter payment", ownerID: "00000000-0000-4000-8000-000000000201", inviteeID: "00000000-0000-4000-8000-000000000202", ownerSubID: "00000000-0000-4000-8000-000000000211", code: "ANNUALRF201", invoiceID: "owner-invoice-refunded", paymentAt: eventAt.Add(-48 * time.Hour), refundedAt: eventAt.Add(-24 * time.Hour), seedPayment: true, wantAnnualDay: 0},
		{name: "provider dispute blocks extension without local refund row", ownerID: "00000000-0000-4000-8000-000000000251", inviteeID: "00000000-0000-4000-8000-000000000252", ownerSubID: "00000000-0000-4000-8000-000000000261", code: "ANNUALDP251", invoiceID: "owner-invoice-provider-dispute", paymentAt: eventAt.Add(-48 * time.Hour), seedPayment: true, providerRisk: true, wantAnnualDay: 0},
		{name: "inviter payment occurs after invitee payment", ownerID: "00000000-0000-4000-8000-000000000301", inviteeID: "00000000-0000-4000-8000-000000000302", ownerSubID: "00000000-0000-4000-8000-000000000311", code: "ANNUALLT301", invoiceID: "owner-invoice-late", paymentAt: eventAt.Add(time.Hour), seedPayment: true, wantAnnualDay: 0},
		{name: "subscription has no verified payment", ownerID: "00000000-0000-4000-8000-000000000401", inviteeID: "00000000-0000-4000-8000-000000000402", ownerSubID: "00000000-0000-4000-8000-000000000411", code: "ANNUALNP401", invoiceID: "owner-invoice-unpaid", seedPayment: false, wantAnnualDay: 0},
		{name: "delayed invoice remains eligible from historical paid period after renewal", ownerID: "00000000-0000-4000-8000-000000000501", inviteeID: "00000000-0000-4000-8000-000000000502", ownerSubID: "00000000-0000-4000-8000-000000000511", code: "ANNUALHS501", invoiceID: "owner-invoice-historical-period", paymentAt: eventAt.Add(-10 * 24 * time.Hour), seedPayment: true, renewedAfterEvent: true, wantAnnualDay: 30},
		{name: "existing applied extension preserves validity after paid base period ends", ownerID: "00000000-0000-4000-8000-000000000601", inviteeID: "00000000-0000-4000-8000-000000000602", ownerSubID: "00000000-0000-4000-8000-000000000611", code: "ANNUALEX601", invoiceID: "owner-invoice-extension-covered", paymentAt: eventAt.Add(-90 * 24 * time.Hour), seedPayment: true, seedPriorExtension: true, wantAnnualDay: 30},
		{name: "future unpaid renewal metadata cannot shift earned extension from verified paid period", ownerID: "00000000-0000-4000-8000-000000000701", inviteeID: "00000000-0000-4000-8000-000000000702", ownerSubID: "00000000-0000-4000-8000-000000000711", code: "ANNUALUN701", invoiceID: "owner-invoice-unpaid-renewal", paymentAt: eventAt.Add(-90 * 24 * time.Hour), seedPayment: true, seedPriorExtension: true, futureUnpaidRenewal: true, wantAnnualDay: 30},
		{name: "applied extension B preserves validity after extension A is reversed", ownerID: "00000000-0000-4000-8000-000000000801", inviteeID: "00000000-0000-4000-8000-000000000802", ownerSubID: "00000000-0000-4000-8000-000000000811", code: "ANNUALAB801", invoiceID: "owner-invoice-reversed-extension-a", paymentAt: eventAt.Add(-90 * 24 * time.Hour), seedPayment: true, seedReversedPriorExtension: true, wantAnnualDay: 30},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, `INSERT INTO public.users(uuid,username,password,email,proxy_uuid) VALUES($1,$2,'fixture-password',$3,gen_random_uuid()),($4,$5,'fixture-password',$6,gen_random_uuid())`, tc.ownerID, "owner"+tc.code, "owner"+tc.code+"@example.invalid", tc.inviteeID, "invitee"+tc.code, "invitee"+tc.code+"@example.invalid"); err != nil {
				t.Fatalf("cannot seed synthetic users: %v", err)
			}
			ownerPeriodSubID := tc.ownerSubID
			ownerPeriodStart := now.Add(-90 * 24 * time.Hour)
			ownerPeriodEnd := baseEnd
			ownerPlanID := "PRO-YEARLY"
			if tc.renewedAfterEvent {
				// The currently-upserted subscription is a monthly renewal that
				// starts after the invitee's paidAt; retain the old paid annual
				// period as a separate historical subscription snapshot.
				ownerPeriodSubID = "00000000-0000-4000-8000-000000000512"
				ownerPeriodStart = eventAt.Add(-90 * 24 * time.Hour)
				ownerPeriodEnd = eventAt.Add(30 * 24 * time.Hour)
				if _, err := db.ExecContext(ctx, `INSERT INTO public.subscriptions(uuid,user_uuid,provider,payment_method,kind,status,plan_id,external_id,meta) VALUES($1,$2,'stripe','card','subscription','active','PRO-MONTHLY',$3,jsonb_build_object('product_slug','xconnect','plan_id','PRO-MONTHLY','startsAt',$4::text,'expiresAt',$5::text))`, tc.ownerSubID, tc.ownerID, "sub_current_monthly_"+tc.code, now.Add(time.Hour).Format(time.RFC3339), now.Add(31*24*time.Hour).Format(time.RFC3339)); err != nil {
					t.Fatalf("cannot seed current monthly renewal: %v", err)
				}
			}
			if tc.seedPriorExtension {
				ownerPeriodStart = eventAt.Add(-365 * 24 * time.Hour)
				ownerPeriodEnd = eventAt.Add(-10 * 24 * time.Hour)
			}
			if tc.seedReversedPriorExtension {
				ownerPeriodStart = eventAt.Add(-365 * 24 * time.Hour)
				ownerPeriodEnd = eventAt.Add(-15 * 24 * time.Hour)
			}
			localExpiry := ownerPeriodEnd
			if tc.futureUnpaidRenewal {
				// Stripe's current subscription snapshot may include a future renewal
				// period that has not been paid. Keep it later than the verified invoice.
				localExpiry = eventAt.Add(180 * 24 * time.Hour)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO public.subscriptions(uuid,user_uuid,provider,payment_method,kind,status,plan_id,external_id,meta) VALUES($1,$2,'stripe','card','subscription','canceled',$3,$4,jsonb_build_object('product_slug','xconnect','plan_id',$3::text,'startsAt',$5::text,'expiresAt',$6::text))`, ownerPeriodSubID, tc.ownerID, ownerPlanID, "sub_"+tc.code, ownerPeriodStart.Format(time.RFC3339), localExpiry.Format(time.RFC3339)); err != nil {
				t.Fatalf("cannot seed inviter subscription: %v", err)
			}
			if _, err := st.EnsureReferralCode(ctx, tc.ownerID, tc.code); err != nil {
				t.Fatal("cannot seed referral code")
			}
			if err := st.CreateReferralAttribution(ctx, tc.code, tc.inviteeID, eventAt.Add(-time.Hour)); err != nil {
				t.Fatal("cannot seed referral attribution")
			}
			if tc.seedPriorExtension || tc.seedReversedPriorExtension {
				priorCount := 1
				if tc.seedReversedPriorExtension {
					priorCount = 2
				}
				for priorIndex := 0; priorIndex < priorCount; priorIndex++ {
					priorInviteeID := "00000000-0000-4000-8000-000000000" + fmt.Sprintf("%03d", 799+i)
					if tc.seedReversedPriorExtension {
						priorInviteeID = "00000000-0000-4000-8000-000000000" + fmt.Sprintf("%03d", 899+i*2+priorIndex)
					}
					priorIdentity := fmt.Sprintf("%s-%d", tc.code, priorIndex)
					if _, err := db.ExecContext(ctx, `INSERT INTO public.users(uuid,username,password,email,proxy_uuid) VALUES($1,$2,'fixture-password',$3,gen_random_uuid())`, priorInviteeID, "prior-extension-invitee-"+priorIdentity, "prior-extension-invitee-"+priorIdentity+"@example.invalid"); err != nil {
						t.Fatalf("cannot seed prior extension invitee: %v", err)
					}
					if err := st.CreateReferralAttribution(ctx, tc.code, priorInviteeID, eventAt.Add(-60*24*time.Hour)); err != nil {
						t.Fatalf("cannot seed prior extension attribution: %v", err)
					}
					var priorReferralID string
					if err := db.QueryRowContext(ctx, `SELECT id::text FROM public.referrals WHERE invitee_uuid=$1`, priorInviteeID).Scan(&priorReferralID); err != nil {
						t.Fatalf("cannot read prior extension referral: %v", err)
					}
					var priorRewardID string
					priorStatus := "applied"
					priorStart := ownerPeriodEnd
					if tc.seedReversedPriorExtension {
						priorStart = ownerPeriodEnd.Add(time.Duration(priorIndex) * 30 * 24 * time.Hour)
					}
					priorUntil := priorStart.Add(30 * 24 * time.Hour)
					if tc.seedReversedPriorExtension && priorIndex == 0 {
						priorStatus = "reversed"
					}
					if err := db.QueryRowContext(ctx, `INSERT INTO public.referral_rewards(referral_id,provider_invoice_id,referrer_uuid,invitee_uuid,plan_id,product_slug,paid_minor,reward_minor,currency,paid_at,eligible_at,annual_days,status) VALUES($1,$2,$3,$4,'PRO-YEARLY','xconnect',12000,1200,'USD',$5,$5,30,'eligible') RETURNING id::text`, priorReferralID, fmt.Sprintf("prior-extension-reward-%s-%d", tc.code, priorIndex), tc.ownerID, priorInviteeID, eventAt.Add(-30*24*time.Hour)).Scan(&priorRewardID); err != nil {
						t.Fatalf("cannot seed prior extension reward: %v", err)
					}
					reversalSource := any(nil)
					if priorStatus == "reversed" {
						reversalSource = "manual:prior-extension-a-" + tc.code
					}
					if _, err := db.ExecContext(ctx, `INSERT INTO public.subscription_extension_ledger(reward_id,user_uuid,subscription_uuid,days,effective_from,effective_until,status,reversal_source_key) VALUES($1,$2,$3,30,$4,$5,$6,$7)`, priorRewardID, tc.ownerID, ownerPeriodSubID, priorStart, priorUntil, priorStatus, reversalSource); err != nil {
						t.Fatalf("cannot seed prior applied extension: %v", err)
					}
				}
			}

			if tc.seedPayment {
				paymentID := seedAnnualEligibilityPayment(t, ctx, db, tc.ownerID, ownerPeriodSubID, tc.invoiceID, tc.paymentAt, 12000, "owner", ownerPeriodStart, ownerPeriodEnd)
				if !tc.refundedAt.IsZero() {
					if _, err := st.RecordFinanceRefund(ctx, &FinanceRefund{IdempotencyKey: "refund:" + tc.invoiceID, PaymentID: paymentID, Provider: "stripe", ProviderRefundID: "re_" + tc.invoiceID, AmountMinor: 12000, Currency: "USD", Reason: "full refund before invitee payment", RefundedAt: tc.refundedAt}); err != nil {
						t.Fatal("cannot record full inviter refund")
					}
					if err := st.RecordReferralRisk(ctx, ReferralPaymentRisk{SourceKey: "charge.refunded:" + tc.invoiceID, ProviderInvoiceID: tc.invoiceID, Type: "refund", Reason: "full refund", OccurredAt: tc.refundedAt}); err != nil {
						t.Fatal("cannot record inviter refund risk")
					}
				}
			}
			if tc.providerRisk {
				if err := st.RecordReferralRisk(ctx, ReferralPaymentRisk{
					SourceKey: "provider-history:dispute:" + tc.invoiceID, ProviderInvoiceID: tc.invoiceID,
					Type: "dispute", Reason: "provider reports disputed inviter payment", OccurredAt: eventAt.Add(-24 * time.Hour),
				}); err != nil {
					t.Fatal("cannot record provider dispute risk")
				}
			}
			if tc.seedReversedPriorExtension {
				validBefore, err := st.ReferralExtensionEnd(ctx, ownerPeriodSubID)
				wantBefore := ownerPeriodEnd.Add(30 * 24 * time.Hour)
				if err != nil || !validBefore.Equal(wantBefore) {
					t.Fatalf("active B extension did not preserve event-time validity after A reversal: got=%s want=%s err=%v", validBefore, wantBefore, err)
				}
			}

			inviteeSub := "00000000-0000-4000-8000-000000000" + fmt.Sprintf("%03d", i+21)
			if _, err := db.ExecContext(ctx, `INSERT INTO public.subscriptions(uuid,user_uuid,provider,payment_method,kind,status,plan_id,external_id,meta) VALUES($1,$2,'stripe','card','subscription','active','PRO-YEARLY',$3,jsonb_build_object('product_slug','xconnect','plan_id','PRO-YEARLY','startsAt',$4::text,'expiresAt',$5::text))`, inviteeSub, tc.inviteeID, "sub_invitee_"+tc.code, now.Add(-30*24*time.Hour).Format(time.RFC3339), now.Add(335*24*time.Hour).Format(time.RFC3339)); err != nil {
				t.Fatalf("cannot seed invitee subscription: %v", err)
			}
			inviteeInvoice := "invitee-invoice-" + tc.code
			seedAnnualEligibilityPayment(t, ctx, db, tc.inviteeID, inviteeSub, inviteeInvoice, eventAt, 90000, "invitee", now.Add(-30*24*time.Hour), now.Add(335*24*time.Hour))

			reward := &ReferralReward{ProviderInvoiceID: inviteeInvoice, InviteeUUID: tc.inviteeID, PlanID: "PRO-YEARLY", ProductSlug: "xconnect", PaidMinor: 90000, RewardMinor: 9000, Currency: "USD", PaidAt: eventAt, EligibleAt: eventAt.Add(7 * 24 * time.Hour)}
			inserted, err := st.CreateReferralReward(ctx, reward)
			if err != nil || !inserted {
				t.Fatalf("create annual referral reward: inserted=%v err=%v", inserted, err)
			}
			if reward.AnnualDays != tc.wantAnnualDay {
				t.Errorf("annual days = %d, want %d", reward.AnnualDays, tc.wantAnnualDay)
			}
			var extensions int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM public.subscription_extension_ledger WHERE reward_id=$1`, reward.ID).Scan(&extensions); err != nil {
				t.Fatal("cannot verify extension ledger")
			}
			wantExtensions := 0
			if tc.wantAnnualDay > 0 {
				wantExtensions = 1
			}
			if extensions != wantExtensions {
				t.Errorf("extension ledger rows = %d, want %d", extensions, wantExtensions)
			}
			if tc.seedPriorExtension {
				var applied int
				var earliestEnd, latestEnd time.Time
				if err := db.QueryRowContext(ctx, `SELECT count(*), min(effective_until), max(effective_until) FROM public.subscription_extension_ledger WHERE user_uuid=$1 AND subscription_uuid=$2 AND status='applied'`, tc.ownerID, ownerPeriodSubID).Scan(&applied, &earliestEnd, &latestEnd); err != nil {
					t.Fatal("cannot verify stacked extension ledger")
				}
				if applied != 2 || !earliestEnd.After(eventAt) || !latestEnd.After(earliestEnd) {
					t.Errorf("prior extension did not preserve event-time eligibility and stack the new award: count=%d earliest=%s latest=%s eventAt=%s", applied, earliestEnd, latestEnd, eventAt)
				}
			}
			if tc.seedReversedPriorExtension {
				var applied, reversed int
				if err := db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE status='applied'), count(*) FILTER (WHERE status='reversed') FROM public.subscription_extension_ledger WHERE user_uuid=$1 AND subscription_uuid=$2`, tc.ownerID, ownerPeriodSubID).Scan(&applied, &reversed); err != nil {
					t.Fatal("cannot verify A/B extension reversal state")
				}
				if applied != 2 || reversed != 1 {
					t.Errorf("A/B ledger state = applied:%d reversed:%d, want applied:2 reversed:1", applied, reversed)
				}
				var appliedFrom, appliedUntil time.Time
				if err := db.QueryRowContext(ctx, `SELECT effective_from,effective_until FROM public.subscription_extension_ledger WHERE reward_id=$1`, reward.ID).Scan(&appliedFrom, &appliedUntil); err != nil {
					t.Fatal("cannot inspect newly stacked extension")
				}
				wantFrom := ownerPeriodEnd.Add(30 * 24 * time.Hour)
				wantUntil := ownerPeriodEnd.Add(60 * 24 * time.Hour)
				if !appliedFrom.Equal(wantFrom) || !appliedUntil.Equal(wantUntil) {
					t.Errorf("new extension did not stack after applied B aggregate: from=%s until=%s want_from=%s want_until=%s", appliedFrom, appliedUntil, wantFrom, wantUntil)
				}
				validAfter, err := st.ReferralExtensionEnd(ctx, ownerPeriodSubID)
				if err != nil || !validAfter.Equal(wantUntil) {
					t.Errorf("A/B effective validity end = %s, want %s; err=%v", validAfter, wantUntil, err)
				}
			}
			if tc.futureUnpaidRenewal {
				var appliedFrom, appliedUntil time.Time
				if err := db.QueryRowContext(ctx, `SELECT effective_from,effective_until FROM public.subscription_extension_ledger WHERE reward_id=$1`, reward.ID).Scan(&appliedFrom, &appliedUntil); err != nil {
					t.Fatal("cannot verify extension anchored to paid invoice")
				}
				wantFrom := ownerPeriodEnd.Add(30 * 24 * time.Hour)
				wantUntil := ownerPeriodEnd.Add(60 * 24 * time.Hour)
				if !appliedFrom.Equal(wantFrom) || !appliedUntil.Equal(wantUntil) {
					t.Errorf("new extension used local unpaid renewal metadata instead of verified paid period: from=%s until=%s want_from=%s want_until=%s", appliedFrom, appliedUntil, wantFrom, wantUntil)
				}
				validUntil, err := st.ReferralExtensionEnd(ctx, ownerPeriodSubID)
				if err != nil || !validUntil.Equal(wantUntil) {
					t.Errorf("effective entitlement end differs from paid-period ledger: got=%s want=%s err=%v", validUntil, wantUntil, err)
				}
				if !localExpiry.After(validUntil) {
					t.Errorf("fixture should distinguish unpaid Stripe metadata expiry %s from verified entitlement end %s", localExpiry, validUntil)
				}
			}
		})
	}
}

func seedAnnualEligibilityPayment(t *testing.T, ctx context.Context, db *sql.DB, userID, subscriptionID, invoiceID string, paidAt time.Time, amount int64, label string, periodStart, periodEnd time.Time) string {
	t.Helper()
	var invoiceUUID, paymentUUID string
	if err := db.QueryRowContext(ctx, `INSERT INTO public.finance_invoices(idempotency_key,account_uuid,subscription_uuid,provider,provider_invoice_id,amount_minor,currency,description,issued_at,service_period_start,service_period_end) VALUES($1,$2,$3,'stripe',$4,$5,'USD','Verified XConnect subscription invoice',$6,$7,$8) RETURNING id::text`, "invoice:"+label+":"+invoiceID, userID, subscriptionID, invoiceID, amount, paidAt, periodStart, periodEnd).Scan(&invoiceUUID); err != nil {
		t.Fatalf("cannot seed finance invoice: %v", err)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO public.finance_payments(idempotency_key,invoice_id,account_uuid,provider,provider_payment_id,amount_minor,currency,paid_at) VALUES($1,$2,$3,'stripe',$4,$5,'USD',$6) RETURNING id::text`, "payment:"+label+":"+invoiceID, invoiceUUID, userID, "pi_"+label+"_"+invoiceID, amount, paidAt).Scan(&paymentUUID); err != nil {
		t.Fatalf("cannot seed finance payment: %v", err)
	}
	return paymentUUID
}
