package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func (s *postgresStore) EnsureReferralCode(ctx context.Context, userUUID, candidate string) (*ReferralCode, error) {
	userUUID, candidate = strings.TrimSpace(userUUID), strings.ToUpper(strings.TrimSpace(candidate))
	_, err := s.db.ExecContext(ctx, `INSERT INTO public.referral_codes(user_uuid, code) VALUES ($1, $2) ON CONFLICT (user_uuid) DO NOTHING`, userUUID, candidate)
	if err != nil {
		return nil, err
	}
	var result ReferralCode
	err = s.db.QueryRowContext(ctx, `SELECT user_uuid::text, code, created_at FROM public.referral_codes WHERE user_uuid=$1`, userUUID).Scan(&result.UserUUID, &result.Code, &result.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func (s *postgresStore) GetReferralCode(ctx context.Context, code string) (*ReferralCode, error) {
	var result ReferralCode
	err := s.db.QueryRowContext(ctx, `SELECT user_uuid::text, code, created_at FROM public.referral_codes WHERE code=$1`, strings.ToUpper(strings.TrimSpace(code))).Scan(&result.UserUUID, &result.Code, &result.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrReferralNotFound
	}
	return &result, err
}

func (s *postgresStore) CreateReferralAttribution(ctx context.Context, code, inviteeUUID string, createdAt time.Time) error {
	return createReferralAttribution(ctx, s.db, code, inviteeUUID, createdAt)
}

func (s *postgresStore) GetReferralReferrer(ctx context.Context, inviteeUUID string) (string, error) {
	var referrer string
	err := s.db.QueryRowContext(ctx, `SELECT referrer_uuid::text FROM public.referrals WHERE invitee_uuid=$1`, strings.TrimSpace(inviteeUUID)).Scan(&referrer)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrReferralNotFound
	}
	return referrer, err
}

type referralAttributionExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func createReferralAttribution(ctx context.Context, executor referralAttributionExecutor, code, inviteeUUID string, createdAt time.Time) error {
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	result, err := executor.ExecContext(ctx, `INSERT INTO public.referrals(referrer_uuid, invitee_uuid, referral_code, created_at) SELECT user_uuid, $2, code, $3 FROM public.referral_codes WHERE code=$1 AND user_uuid <> $2 ON CONFLICT (invitee_uuid) DO NOTHING`, strings.ToUpper(strings.TrimSpace(code)), strings.TrimSpace(inviteeUUID), createdAt.UTC())
	if err != nil {
		if pgUnique(err) {
			return ErrReferralAlreadyAttributed
		}
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	var owner string
	err = executor.QueryRowContext(ctx, `SELECT user_uuid::text FROM public.referral_codes WHERE code=$1`, strings.ToUpper(strings.TrimSpace(code))).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrReferralNotFound
	}
	if err != nil {
		return err
	}
	if owner == strings.TrimSpace(inviteeUUID) {
		return ErrReferralSelf
	}
	return ErrReferralAlreadyAttributed
}

func (s *postgresStore) CreateReferralReward(ctx context.Context, reward *ReferralReward) (bool, error) {
	if reward == nil || strings.TrimSpace(reward.ProviderInvoiceID) == "" || reward.PaidMinor <= 0 || reward.RewardMinor != reward.PaidMinor/10 || !validFinanceCurrency(strings.ToUpper(strings.TrimSpace(reward.Currency))) || reward.ProductSlug != "xconnect" || !isXConnectReferralPlan(reward.PlanID) {
		return false, ErrReferralNotFound
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	invoiceID := strings.TrimSpace(reward.ProviderInvoiceID)
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('referral-invoice:' || $1, 0))`, invoiceID); err != nil {
		return false, err
	}
	invitee := strings.TrimSpace(reward.InviteeUUID)
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, invitee); err != nil {
		return false, err
	}
	var referralID, referrer string
	err = tx.QueryRowContext(ctx, `SELECT id::text, referrer_uuid::text FROM public.referrals WHERE invitee_uuid=$1 FOR UPDATE`, invitee).Scan(&referralID, &referrer)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrReferralNotFound
	}
	if err != nil {
		return false, err
	}
	if reward.ReferrerUUID != "" && referrer != strings.TrimSpace(reward.ReferrerUUID) {
		return false, ErrReferralNotFound
	}
	reward.ReferrerUUID = referrer
	var prior bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.referral_rewards WHERE invitee_uuid=$1) OR EXISTS(SELECT 1 FROM public.referral_payment_risks WHERE provider_invoice_id=$2)`, invitee, strings.TrimSpace(reward.ProviderInvoiceID)).Scan(&prior); err != nil {
		return false, err
	}
	if prior {
		return false, nil
	}
	var firstPaid bool
	err = tx.QueryRowContext(ctx, `SELECT NOT EXISTS (
 SELECT 1 FROM public.finance_payments p
 JOIN public.finance_invoices i ON i.id=p.invoice_id
 WHERE i.account_uuid=$1::uuid AND i.provider='stripe' AND p.provider='stripe'
   AND i.provider_invoice_id <> $2
   AND i.description IN ('Verified XConnect subscription invoice','Verified XConnect inviter subscription invoice')
   AND i.service_period_start IS NOT NULL AND i.service_period_end > i.service_period_start
   AND (p.paid_at < $3 OR (p.paid_at=$3 AND i.provider_invoice_id < $2))
   AND NOT EXISTS(SELECT 1 FROM public.finance_refunds fr WHERE fr.payment_id=p.id)
   AND NOT EXISTS(SELECT 1 FROM public.referral_payment_risks rk WHERE rk.provider_invoice_id=i.provider_invoice_id)
)`, invitee, strings.TrimSpace(reward.ProviderInvoiceID), reward.PaidAt.UTC()).Scan(&firstPaid)
	if err != nil {
		return false, err
	}
	if !firstPaid {
		return false, nil
	}
	var risk bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.referral_payment_risks WHERE provider_invoice_id=$1)`, strings.TrimSpace(reward.ProviderInvoiceID)).Scan(&risk); err != nil {
		return false, err
	}
	if risk {
		return false, nil
	}
	if reward.PaidAt.IsZero() {
		reward.PaidAt = time.Now().UTC()
	}
	if reward.EligibleAt.IsZero() {
		reward.EligibleAt = reward.PaidAt.Add(7 * 24 * time.Hour)
	}
	reward.Currency = strings.ToUpper(strings.TrimSpace(reward.Currency))
	reward.ID = uuid.NewString()
	var annualSub string
	var annualEnd time.Time
	if isXConnectAnnualReferralPlan(reward.PlanID) {
		err = tx.QueryRowContext(ctx, `SELECT s.uuid::text,
	  (p.base_end + COALESCE(e.days,0) * interval '1 day')
 FROM public.subscriptions s
 CROSS JOIN LATERAL (
   SELECT MAX(fi.service_period_end) AS base_end
   FROM public.finance_invoices fi JOIN public.finance_payments fp ON fp.invoice_id=fi.id
   WHERE fi.subscription_uuid=s.uuid AND fi.provider='stripe' AND fp.provider='stripe'
     AND fi.description IN ('Verified XConnect subscription invoice','Verified XConnect inviter subscription invoice')
     AND fi.service_period_start IS NOT NULL AND fi.service_period_end > fi.service_period_start
     AND fp.paid_at <= $2
     AND NOT EXISTS(SELECT 1 FROM public.finance_refunds fr WHERE fr.payment_id=fp.id)
     AND NOT EXISTS(SELECT 1 FROM public.referral_payment_risks rk WHERE rk.provider_invoice_id=fi.provider_invoice_id)
 ) p
 CROSS JOIN LATERAL (
   SELECT COALESCE(SUM(ex.days),0) AS days FROM public.subscription_extension_ledger ex
   JOIN public.referral_rewards er ON er.id=ex.reward_id
   WHERE ex.subscription_uuid=s.uuid AND ex.status='applied' AND er.paid_at <= $2
 ) e
 WHERE s.user_uuid=$1::uuid AND s.provider='stripe' AND s.kind='subscription'
   AND p.base_end IS NOT NULL
   AND (EXISTS(SELECT 1 FROM public.finance_invoices fi JOIN public.finance_payments fp ON fp.invoice_id=fi.id
       WHERE fi.subscription_uuid=s.uuid AND fi.provider='stripe' AND fp.provider='stripe'
         AND fi.description IN ('Verified XConnect subscription invoice','Verified XConnect inviter subscription invoice')
         AND fi.service_period_start <= $2 AND fi.service_period_end > $2 AND fp.paid_at <= $2
         AND NOT EXISTS(SELECT 1 FROM public.finance_refunds fr WHERE fr.payment_id=fp.id)
         AND NOT EXISTS(SELECT 1 FROM public.referral_payment_risks rk WHERE rk.provider_invoice_id=fi.provider_invoice_id))
     OR (p.base_end <= $2 AND (p.base_end + COALESCE(e.days,0) * interval '1 day') > $2))
   AND (p.base_end + COALESCE(e.days,0) * interval '1 day') > $2
 ORDER BY 2 DESC LIMIT 1`, referrer, reward.PaidAt.UTC()).Scan(&annualSub, &annualEnd)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
	}
	annualDays := 0
	if annualSub != "" {
		annualDays = 30
	}
	reward.AnnualDays = annualDays
	reward.Status = "pending"
	_, err = tx.ExecContext(ctx, `INSERT INTO public.referral_rewards(id, referral_id, provider_invoice_id, referrer_uuid, invitee_uuid, plan_id, product_slug, paid_minor, reward_minor, currency, paid_at, eligible_at, annual_days, status)
VALUES($1,$2,$3,$4,$5,$6,'xconnect',$7,$8,$9,$10,$11,$12,'pending')`, reward.ID, referralID, reward.ProviderInvoiceID, referrer, invitee, reward.PlanID, reward.PaidMinor, reward.RewardMinor, reward.Currency, reward.PaidAt.UTC(), reward.EligibleAt.UTC(), annualDays)
	if err != nil {
		if pgUnique(err) {
			return false, nil
		}
		return false, err
	}
	if reward.RewardMinor > 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO public.referral_reward_events(reward_id,source_key,event_type,amount_delta_minor,reason) VALUES($1,$2,'award',$3,'first verified paid XConnect subscription invoice')`, reward.ID, "award:"+reward.ProviderInvoiceID, reward.RewardMinor)
		if err != nil {
			return false, err
		}
	}
	if annualDays > 0 {
		until := annualEnd.Add(30 * 24 * time.Hour)
		_, err = tx.ExecContext(ctx, `INSERT INTO public.subscription_extension_ledger(reward_id,user_uuid,subscription_uuid,days,effective_from,effective_until) VALUES($1,$2,$3,30,$4,$5)`, reward.ID, referrer, annualSub, annualEnd, until)
		if err != nil {
			return false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	reward.ReferralID = referralID
	reward.Status = "pending"
	reward.AnnualDays = annualDays
	reward.CreatedAt = time.Now().UTC()
	return true, nil
}

func (s *postgresStore) RecordReferralRisk(ctx context.Context, risk ReferralPaymentRisk) error {
	if strings.TrimSpace(risk.SourceKey) == "" || strings.TrimSpace(risk.ProviderInvoiceID) == "" {
		return errors.New("referral risk source and invoice are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('referral-invoice:' || $1, 0))`, strings.TrimSpace(risk.ProviderInvoiceID)); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO public.referral_payment_risks(source_key,provider_invoice_id,risk_type,reason,occurred_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT(source_key) DO NOTHING`, risk.SourceKey, risk.ProviderInvoiceID, risk.Type, risk.Reason, risk.OccurredAt.UTC())
	if err != nil {
		return err
	}
	var rewardID, referrer, status string
	var amount int64
	var currency string
	err = tx.QueryRowContext(ctx, `SELECT id::text,referrer_uuid::text,reward_minor,currency,status FROM public.referral_rewards WHERE provider_invoice_id=$1 FOR UPDATE`, risk.ProviderInvoiceID).Scan(&rewardID, &referrer, &amount, &currency, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	if status != "reversed" {
		if amount > 0 {
			_, err = tx.ExecContext(ctx, `INSERT INTO public.referral_reward_events(reward_id,source_key,event_type,amount_delta_minor,reason) VALUES($1,$2,'reversal',$3,$4) ON CONFLICT(reward_id,event_type) DO NOTHING`, rewardID, "risk:"+risk.SourceKey, -amount, risk.Type+": "+risk.Reason)
			if err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE public.referral_rewards SET status='reversed' WHERE id=$1`, rewardID)
		if err != nil {
			return err
		}
	}
	// Keep the whole settlement reserved while the actual off-platform payout
	// state is reconciled. Releasing unaffected items here could let a second
	// settlement pay them before staff records a first payout that already
	// happened. Complete remains blocked by the risk tombstone.
	_, err = tx.ExecContext(ctx, `UPDATE public.referral_settlements s SET reason='payment risk hold: ' || $2 WHERE s.status='pending_review' AND EXISTS(SELECT 1 FROM public.referral_settlement_items i WHERE i.settlement_id=s.id AND i.reward_id=$1 AND i.amount_minor>0)`, rewardID, risk.Type)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE public.subscription_extension_ledger SET status='reversed',reversal_source_key=$2 WHERE reward_id=$1 AND status='applied'`, rewardID, "risk:"+risk.SourceKey)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *postgresStore) ListReferralRewards(ctx context.Context, referrerUUID string, limit int) ([]ReferralReward, error) {
	query := `SELECT r.id::text,r.referral_id::text,r.provider_invoice_id,r.referrer_uuid::text,r.invitee_uuid::text,r.plan_id,r.product_slug,r.paid_minor,r.reward_minor,r.currency,r.paid_at,r.eligible_at,r.annual_days,r.status,r.created_at,COALESCE(u.email,''),f.created_at FROM public.referral_rewards r JOIN public.users u ON u.uuid=r.invitee_uuid JOIN public.referrals f ON f.id=r.referral_id WHERE r.referrer_uuid=$1 ORDER BY r.created_at DESC`
	args := []any{referrerUUID}
	if limit > 0 {
		query += ` LIMIT $2`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReferralReward
	for rows.Next() {
		var x ReferralReward
		if err = rows.Scan(&x.ID, &x.ReferralID, &x.ProviderInvoiceID, &x.ReferrerUUID, &x.InviteeUUID, &x.PlanID, &x.ProductSlug, &x.PaidMinor, &x.RewardMinor, &x.Currency, &x.PaidAt, &x.EligibleAt, &x.AnnualDays, &x.Status, &x.CreatedAt, &x.InviteeEmail, &x.ReferralCreatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *postgresStore) GetReferralSummary(ctx context.Context, referrerUUID string, now time.Time) (*ReferralSummary, error) {
	result := &ReferralSummary{}
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FILTER (WHERE status <> 'reversed'), COALESCE((SELECT SUM(days) FROM public.subscription_extension_ledger WHERE user_uuid=$1 AND status='applied'),0) FROM public.referral_rewards WHERE referrer_uuid=$1`, referrerUUID).Scan(&result.SuccessfulInvites, &result.AwardedDays)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `WITH currencies AS (
 SELECT currency FROM public.referral_rewards WHERE referrer_uuid=$1 UNION SELECT currency FROM public.referral_settlements WHERE referrer_uuid=$1
), rewards AS (
 SELECT currency,
  COALESCE(SUM(reward_minor) FILTER (WHERE status <> 'reversed'),0) AS earned,
  COALESCE(SUM(reward_minor) FILTER (WHERE status='pending' AND eligible_at>$2),0) AS pending,
  COALESCE(SUM(reward_minor) FILTER (WHERE status='reversed'),0) AS reversed
 FROM public.referral_rewards WHERE referrer_uuid=$1 GROUP BY currency
), settlements AS (
 SELECT currency, COALESCE(SUM(amount_minor) FILTER (WHERE status='paid'),0) AS settled
 FROM public.referral_settlements WHERE referrer_uuid=$1 GROUP BY currency
)
SELECT c.currency,COALESCE(r.earned,0),COALESCE(r.pending,0),
 GREATEST(0, COALESCE((SELECT SUM(rr.reward_minor) FROM public.referral_rewards rr WHERE rr.referrer_uuid=$1 AND rr.currency=c.currency AND rr.status IN ('pending','eligible') AND rr.eligible_at<=$2
  AND NOT EXISTS(SELECT 1 FROM public.referral_payment_risks k WHERE k.provider_invoice_id=rr.provider_invoice_id)
  AND NOT EXISTS(SELECT 1 FROM public.referral_settlement_items si JOIN public.referral_settlements ss ON ss.id=si.settlement_id WHERE si.reward_id=rr.id AND si.amount_minor>0 AND ss.status IN ('pending_review','paid'))),0)
 - COALESCE((SELECT SUM(-ev.amount_delta_minor) FROM public.referral_reward_events ev JOIN public.referral_rewards rr ON rr.id=ev.reward_id WHERE rr.referrer_uuid=$1 AND rr.currency=c.currency AND ev.event_type='reversal'
   AND EXISTS(SELECT 1 FROM public.referral_settlement_items pi JOIN public.referral_settlements ps ON ps.id=pi.settlement_id WHERE pi.reward_id=rr.id AND pi.amount_minor>0 AND ps.status='paid')
   AND NOT EXISTS(SELECT 1 FROM public.referral_settlement_items ni JOIN public.referral_settlements ns ON ns.id=ni.settlement_id WHERE ni.reward_id=rr.id AND ni.amount_minor<0 AND ns.status IN ('pending_review','paid'))),0)),
 COALESCE(s.settled,0),COALESCE(r.reversed,0)
FROM currencies c LEFT JOIN rewards r USING(currency) LEFT JOIN settlements s USING(currency) ORDER BY c.currency`, referrerUUID, now.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item ReferralCurrencySummary
		if err = rows.Scan(&item.Currency, &item.EarnedMinor, &item.PendingMinor, &item.EligibleMinor, &item.SettledMinor, &item.ReversedMinor); err != nil {
			return nil, err
		}
		result.CashByCurrency = append(result.CashByCurrency, item)
	}
	return result, rows.Err()
}

func (s *postgresStore) ListReferralSettlements(ctx context.Context, referrerUUID string, limit int) ([]ReferralSettlement, error) {
	query := `SELECT id::text,referrer_uuid::text,currency,amount_minor,status,payout_reference,reason,requested_at,completed_at FROM public.referral_settlements WHERE referrer_uuid=$1 ORDER BY requested_at DESC`
	args := []any{referrerUUID}
	if limit > 0 {
		query += ` LIMIT $2`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReferralSettlement
	for rows.Next() {
		var x ReferralSettlement
		var completed sql.NullTime
		if err = rows.Scan(&x.ID, &x.ReferrerUUID, &x.Currency, &x.AmountMinor, &x.Status, &x.PayoutReference, &x.Reason, &x.RequestedAt, &completed); err != nil {
			return nil, err
		}
		if completed.Valid {
			x.CompletedAt = &completed.Time
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *postgresStore) RequestReferralSettlement(ctx context.Context, referrerUUID, currency string, now time.Time) (*ReferralSettlement, error) {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1 || ':' || $2, 0))`, referrerUUID, currency); err != nil {
		return nil, err
	}
	var id string
	err = tx.QueryRowContext(ctx, `INSERT INTO public.referral_settlements(referrer_uuid,currency,amount_minor)
	 SELECT $1,$2,COALESCE(SUM(r.reward_minor),0) - COALESCE((SELECT SUM(-e.amount_delta_minor) FROM public.referral_reward_events e JOIN public.referral_rewards rr ON rr.id=e.reward_id WHERE rr.referrer_uuid=$1 AND rr.currency=$2 AND e.event_type='reversal' AND EXISTS(SELECT 1 FROM public.referral_settlement_items psi JOIN public.referral_settlements ps ON ps.id=psi.settlement_id WHERE psi.reward_id=rr.id AND psi.amount_minor > 0 AND ps.status='paid') AND NOT EXISTS(SELECT 1 FROM public.referral_settlement_items ni JOIN public.referral_settlements ns ON ns.id=ni.settlement_id WHERE ni.reward_id=rr.id AND ni.amount_minor < 0 AND ns.status IN ('pending_review','paid'))),0) FROM public.referral_rewards r
 WHERE r.referrer_uuid=$1 AND r.currency=$2 AND r.status IN ('pending','eligible') AND r.eligible_at <= $3
 AND NOT EXISTS(SELECT 1 FROM public.referral_payment_risks k WHERE k.provider_invoice_id=r.provider_invoice_id)
 AND NOT EXISTS(SELECT 1 FROM public.referral_settlement_items si JOIN public.referral_settlements s ON s.id=si.settlement_id WHERE si.reward_id=r.id AND s.status IN ('pending_review','paid'))
	 HAVING COALESCE(SUM(r.reward_minor),0) - COALESCE((SELECT SUM(-e.amount_delta_minor) FROM public.referral_reward_events e JOIN public.referral_rewards rr ON rr.id=e.reward_id WHERE rr.referrer_uuid=$1 AND rr.currency=$2 AND e.event_type='reversal' AND EXISTS(SELECT 1 FROM public.referral_settlement_items psi JOIN public.referral_settlements ps ON ps.id=psi.settlement_id WHERE psi.reward_id=rr.id AND psi.amount_minor > 0 AND ps.status='paid') AND NOT EXISTS(SELECT 1 FROM public.referral_settlement_items ni JOIN public.referral_settlements ns ON ns.id=ni.settlement_id WHERE ni.reward_id=rr.id AND ni.amount_minor < 0 AND ns.status IN ('pending_review','paid'))),0)>0 RETURNING id::text`, referrerUUID, currency, now.UTC()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrReferralSettlementUnavailable
	}
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO public.referral_settlement_items(settlement_id,reward_id,amount_minor) SELECT $1,r.id,r.reward_minor FROM public.referral_rewards r WHERE r.referrer_uuid=$2 AND r.currency=$3 AND r.status IN ('pending','eligible') AND r.eligible_at <= $4 AND NOT EXISTS(SELECT 1 FROM public.referral_payment_risks k WHERE k.provider_invoice_id=r.provider_invoice_id) AND NOT EXISTS(SELECT 1 FROM public.referral_settlement_items si JOIN public.referral_settlements s ON s.id=si.settlement_id WHERE si.reward_id=r.id AND s.status IN ('pending_review','paid'))`, id, referrerUUID, currency, now.UTC())
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO public.referral_settlement_items(settlement_id,reward_id,amount_minor) SELECT $1,r.id,e.amount_delta_minor FROM public.referral_reward_events e JOIN public.referral_rewards r ON r.id=e.reward_id WHERE r.referrer_uuid=$2 AND r.currency=$3 AND e.event_type='reversal' AND EXISTS(SELECT 1 FROM public.referral_settlement_items pi JOIN public.referral_settlements ps ON ps.id=pi.settlement_id WHERE pi.reward_id=r.id AND pi.amount_minor > 0 AND ps.status='paid') AND NOT EXISTS(SELECT 1 FROM public.referral_settlement_items ni JOIN public.referral_settlements ns ON ns.id=ni.settlement_id WHERE ni.reward_id=r.id AND ni.amount_minor < 0 AND ns.status IN ('pending_review','paid'))`, id, referrerUUID, currency)
	if err != nil {
		return nil, err
	}
	var result ReferralSettlement
	err = tx.QueryRowContext(ctx, `SELECT id::text,referrer_uuid::text,currency,amount_minor,status,payout_reference,reason,requested_at,completed_at FROM public.referral_settlements WHERE id=$1`, id).Scan(&result.ID, &result.ReferrerUUID, &result.Currency, &result.AmountMinor, &result.Status, &result.PayoutReference, &result.Reason, &result.RequestedAt, &sql.NullTime{})
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &result, nil
}

func (s *postgresStore) CompleteReferralSettlement(ctx context.Context, settlementID, actorUUID, payoutReference string, now time.Time) (*ReferralSettlement, error) {
	if strings.TrimSpace(payoutReference) == "" {
		return nil, ErrReferralSettlementUnavailable
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var owner, currency, status string
	var amount int64
	err = tx.QueryRowContext(ctx, `SELECT referrer_uuid::text,currency,status,amount_minor FROM public.referral_settlements WHERE id=$1 FOR UPDATE`, settlementID).Scan(&owner, &currency, &status, &amount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrReferralNotFound
	}
	if err != nil {
		return nil, err
	}
	if status == "paid" {
		var existingRef string
		err = tx.QueryRowContext(ctx, `SELECT payout_reference FROM public.referral_settlements WHERE id=$1`, settlementID).Scan(&existingRef)
		if err != nil {
			return nil, err
		}
		if existingRef != strings.TrimSpace(payoutReference) {
			return nil, ErrReferralSettlementUnavailable
		}
		var replay ReferralSettlement
		err = tx.QueryRowContext(ctx, `SELECT id::text,referrer_uuid::text,currency,amount_minor,status,payout_reference,reason,requested_at,completed_at FROM public.referral_settlements WHERE id=$1`, settlementID).Scan(&replay.ID, &replay.ReferrerUUID, &replay.Currency, &replay.AmountMinor, &replay.Status, &replay.PayoutReference, &replay.Reason, &replay.RequestedAt, &sql.NullTime{})
		if err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return &replay, nil
	}
	if status != "pending_review" {
		return nil, ErrReferralSettlementUnavailable
	}
	var risk bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.referral_settlement_items si JOIN public.referral_rewards r ON r.id=si.reward_id JOIN public.referral_payment_risks k ON k.provider_invoice_id=r.provider_invoice_id WHERE si.settlement_id=$1 AND si.amount_minor>0)`, settlementID).Scan(&risk)
	if err != nil {
		return nil, err
	}
	if risk {
		return nil, ErrReferralSettlementRisk
	}
	_, err = tx.ExecContext(ctx, `UPDATE public.referral_settlements SET status='paid',payout_reference=$2,completed_at=$3,completed_by=$4 WHERE id=$1`, settlementID, strings.TrimSpace(payoutReference), now.UTC(), actorUUID)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE public.referral_rewards r SET status='settled' WHERE EXISTS(SELECT 1 FROM public.referral_settlement_items i WHERE i.settlement_id=$1 AND i.reward_id=r.id AND i.amount_minor > 0) AND r.status IN ('pending','eligible')`, settlementID)
	if err != nil {
		return nil, err
	}
	var result ReferralSettlement
	err = tx.QueryRowContext(ctx, `SELECT id::text,referrer_uuid::text,currency,amount_minor,status,payout_reference,reason,requested_at,completed_at FROM public.referral_settlements WHERE id=$1`, settlementID).Scan(&result.ID, &result.ReferrerUUID, &result.Currency, &result.AmountMinor, &result.Status, &result.PayoutReference, &result.Reason, &result.RequestedAt, &sql.NullTime{})
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &result, nil
}

// ReconcileReferralSettlement records an already completed off-platform payment
// when a later risk event rejected the review row before the payment fact was
// recorded. This does not authorize or initiate a payment; the caller must
// provide the real provider/bank reference and an auditable reconciliation reason.
func (s *postgresStore) ReconcileReferralSettlement(ctx context.Context, settlementID, actorUUID, payoutReference, reason string, now time.Time) (*ReferralSettlement, error) {
	payoutReference = strings.TrimSpace(payoutReference)
	reason = strings.TrimSpace(reason)
	if payoutReference == "" || reason == "" {
		return nil, ErrReferralSettlementUnavailable
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM public.referral_settlements WHERE id=$1 FOR UPDATE`, settlementID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrReferralNotFound
	}
	if err != nil {
		return nil, err
	}
	if status == "paid" {
		var existingRef string
		if err = tx.QueryRowContext(ctx, `SELECT payout_reference FROM public.referral_settlements WHERE id=$1`, settlementID).Scan(&existingRef); err != nil {
			return nil, err
		}
		if existingRef != payoutReference {
			return nil, ErrReferralSettlementUnavailable
		}
	} else if status == "pending_review" || status == "rejected" {
		var risk bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.referral_settlement_items si JOIN public.referral_rewards r ON r.id=si.reward_id WHERE si.settlement_id=$1 AND si.amount_minor>0 AND (r.status='reversed' OR EXISTS(SELECT 1 FROM public.referral_payment_risks k WHERE k.provider_invoice_id=r.provider_invoice_id)))`, settlementID).Scan(&risk); err != nil {
			return nil, err
		}
		if !risk {
			return nil, ErrReferralSettlementUnavailable
		}
		_, err = tx.ExecContext(ctx, `UPDATE public.referral_settlements SET status='paid',payout_reference=$2,reason=left(COALESCE(reason,'') || '; reconciled actual payment: ' || $3,1000),completed_at=$4,completed_by=$5 WHERE id=$1`, settlementID, payoutReference, reason, now.UTC(), actorUUID)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE public.referral_rewards r SET status='settled' WHERE EXISTS(SELECT 1 FROM public.referral_settlement_items i WHERE i.settlement_id=$1 AND i.reward_id=r.id AND i.amount_minor>0) AND r.status IN ('pending','eligible')`, settlementID)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, ErrReferralSettlementUnavailable
	}
	var result ReferralSettlement
	var completed sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT id::text,referrer_uuid::text,currency,amount_minor,status,payout_reference,reason,requested_at,completed_at FROM public.referral_settlements WHERE id=$1`, settlementID).Scan(&result.ID, &result.ReferrerUUID, &result.Currency, &result.AmountMinor, &result.Status, &result.PayoutReference, &result.Reason, &result.RequestedAt, &completed)
	if err != nil {
		return nil, err
	}
	if completed.Valid {
		result.CompletedAt = &completed.Time
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &result, nil
}

func (s *postgresStore) RejectReferralSettlement(ctx context.Context, settlementID, actorUUID, reason string, now time.Time) (*ReferralSettlement, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, ErrReferralSettlementUnavailable
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var status, oldReason string
	var payout sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT status,COALESCE(reason,''),payout_reference FROM public.referral_settlements WHERE id=$1 FOR UPDATE`, settlementID).Scan(&status, &oldReason, &payout)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrReferralNotFound
	}
	if err != nil {
		return nil, err
	}
	if status == "rejected" && !payout.Valid && oldReason == reason {
		// An exact replay after a lost response is already applied.
	} else if status == "pending_review" && !payout.Valid {
		_, err = tx.ExecContext(ctx, `UPDATE public.referral_settlements SET status='rejected',reason=left($2,1000),completed_at=$3,completed_by=$4 WHERE id=$1`, settlementID, reason, now.UTC(), actorUUID)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, ErrReferralSettlementUnavailable
	}
	var item ReferralSettlement
	var completed sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT id::text,referrer_uuid::text,currency,amount_minor,status,payout_reference,reason,requested_at,completed_at FROM public.referral_settlements WHERE id=$1`, settlementID).Scan(&item.ID, &item.ReferrerUUID, &item.Currency, &item.AmountMinor, &item.Status, &item.PayoutReference, &item.Reason, &item.RequestedAt, &completed)
	if err != nil {
		return nil, err
	}
	if completed.Valid {
		item.CompletedAt = &completed.Time
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &item, nil
}

func (s *postgresStore) ReverseReferralReward(ctx context.Context, rewardID, actorUUID, sourceKey, reason string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var amount int64
	var status string
	err = tx.QueryRowContext(ctx, `SELECT reward_minor,status FROM public.referral_rewards WHERE id=$1 FOR UPDATE`, rewardID).Scan(&amount, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrReferralNotFound
	}
	if err != nil {
		return err
	}
	if status == "reversed" {
		return tx.Commit()
	}
	if amount > 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO public.referral_reward_events(reward_id,source_key,event_type,amount_delta_minor,reason,actor_uuid,created_at) VALUES($1,$2,'reversal',$3,$4,$5,$6) ON CONFLICT(reward_id,event_type) DO NOTHING`, rewardID, "manual:"+sourceKey, -amount, strings.TrimSpace(reason), actorUUID, now.UTC())
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE public.referral_rewards SET status='reversed' WHERE id=$1`, rewardID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE public.referral_settlements s SET status='rejected',reason='reward reversed',completed_at=$2 WHERE s.status='pending_review' AND EXISTS(SELECT 1 FROM public.referral_settlement_items i WHERE i.settlement_id=s.id AND i.reward_id=$1)`, rewardID, now.UTC())
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE public.subscription_extension_ledger SET status='reversed',reversal_source_key=$2 WHERE reward_id=$1 AND status='applied'`, rewardID, "manual:"+sourceKey)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *postgresStore) ListSubscriptionExtensions(ctx context.Context, userUUID string, limit int) ([]SubscriptionExtension, error) {
	query := `SELECT e.id::text,e.reward_id::text,r.referral_id::text,e.user_uuid::text,e.subscription_uuid::text,e.days,e.effective_from,e.effective_until,e.status,e.created_at FROM public.subscription_extension_ledger e JOIN public.referral_rewards r ON r.id=e.reward_id WHERE e.user_uuid=$1 ORDER BY e.created_at DESC`
	args := []any{userUUID}
	if limit > 0 {
		query += ` LIMIT $2`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SubscriptionExtension
	for rows.Next() {
		var x SubscriptionExtension
		if err = rows.Scan(&x.ID, &x.RewardID, &x.ReferralID, &x.UserUUID, &x.SubscriptionUUID, &x.Days, &x.EffectiveFrom, &x.EffectiveUntil, &x.Status, &x.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *postgresStore) ReferralExtensionEnd(ctx context.Context, subscriptionUUID string) (time.Time, error) {
	var end sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT (p.base_end + COALESCE(e.days,0) * interval '1 day')
FROM public.subscriptions s
CROSS JOIN LATERAL (
  SELECT MAX(fi.service_period_end) AS base_end
  FROM public.finance_invoices fi JOIN public.finance_payments fp ON fp.invoice_id=fi.id
  WHERE fi.subscription_uuid=s.uuid AND fi.provider='stripe' AND fp.provider='stripe'
    AND fi.description IN ('Verified XConnect subscription invoice','Verified XConnect inviter subscription invoice')
    AND fi.service_period_start IS NOT NULL AND fi.service_period_end > fi.service_period_start
    AND NOT EXISTS(SELECT 1 FROM public.finance_refunds fr WHERE fr.payment_id=fp.id)
    AND NOT EXISTS(SELECT 1 FROM public.referral_payment_risks rk WHERE rk.provider_invoice_id=fi.provider_invoice_id)
) p
CROSS JOIN LATERAL (
  SELECT COALESCE(SUM(days),0) AS days FROM public.subscription_extension_ledger
  WHERE subscription_uuid=s.uuid AND status='applied'
) e
WHERE s.uuid=$1`, subscriptionUUID).Scan(&end)
	if err != nil {
		return time.Time{}, err
	}
	if end.Valid {
		return end.Time.UTC(), nil
	}
	return time.Time{}, nil
}

func pgUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func referralStoreError(op string, err error) error { return fmt.Errorf("%s: %w", op, err) }
