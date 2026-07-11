package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

func scanBillingPlan(scanner interface{ Scan(dest ...any) error }) (*BillingPlan, error) {
	var (
		plan            BillingPlan
		stripePriceID   sql.NullString
		multipliersJSON []byte
		featuresJSON    []byte
	)
	if err := scanner.Scan(
		&plan.PlanID,
		&stripePriceID,
		&plan.DisplayName,
		&plan.Kind,
		&plan.IncludedQuotaBytes,
		&plan.PackageName,
		&multipliersJSON,
		&featuresJSON,
		&plan.TrialDays,
		&plan.Active,
		&plan.SortOrder,
		&plan.CreatedAt,
		&plan.UpdatedAt,
	); err != nil {
		return nil, err
	}
	plan.StripePriceID = strings.TrimSpace(stripePriceID.String)
	if len(multipliersJSON) > 0 {
		_ = json.Unmarshal(multipliersJSON, &plan.PriceMultipliers)
	}
	if len(featuresJSON) > 0 {
		_ = json.Unmarshal(featuresJSON, &plan.Features)
	}
	return &plan, nil
}

const billingPlanColumns = `plan_id, stripe_price_id, display_name, kind, included_quota_bytes, package_name, price_multipliers, features, trial_days, active, sort_order, created_at, updated_at`

func (s *postgresStore) ListBillingPlans(ctx context.Context, includeInactive bool) ([]BillingPlan, error) {
	query := `SELECT ` + billingPlanColumns + ` FROM billing_plans`
	if !includeInactive {
		query += ` WHERE active`
	}
	query += ` ORDER BY sort_order ASC, plan_id ASC`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var plans []BillingPlan
	for rows.Next() {
		plan, err := scanBillingPlan(rows)
		if err != nil {
			return nil, err
		}
		plans = append(plans, *plan)
	}
	return plans, rows.Err()
}

func (s *postgresStore) GetBillingPlan(ctx context.Context, planID string) (*BillingPlan, error) {
	const query = `SELECT ` + billingPlanColumns + ` FROM billing_plans WHERE plan_id = $1`
	plan, err := scanBillingPlan(s.db.QueryRowContext(ctx, query, normalizePlanID(planID)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrBillingPlanNotFound
	}
	return plan, err
}

func (s *postgresStore) GetBillingPlanByPriceID(ctx context.Context, stripePriceID string) (*BillingPlan, error) {
	trimmed := strings.TrimSpace(stripePriceID)
	if trimmed == "" {
		return nil, ErrBillingPlanNotFound
	}
	const query = `SELECT ` + billingPlanColumns + ` FROM billing_plans WHERE stripe_price_id = $1`
	plan, err := scanBillingPlan(s.db.QueryRowContext(ctx, query, trimmed))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrBillingPlanNotFound
	}
	return plan, err
}

func (s *postgresStore) UpsertBillingPlan(ctx context.Context, plan *BillingPlan) error {
	if plan == nil || strings.TrimSpace(plan.PlanID) == "" {
		return errors.New("billing plan id is required")
	}
	multipliersJSON, err := json.Marshal(orEmptyMultipliers(plan.PriceMultipliers))
	if err != nil {
		return err
	}
	featuresJSON, err := json.Marshal(orEmptyFeatures(plan.Features))
	if err != nil {
		return err
	}
	var stripePriceID any
	if trimmed := strings.TrimSpace(plan.StripePriceID); trimmed != "" {
		stripePriceID = trimmed
	}

	const query = `
		INSERT INTO billing_plans (
			plan_id, stripe_price_id, display_name, kind, included_quota_bytes, package_name, price_multipliers, features, trial_days, active, sort_order
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (plan_id) DO UPDATE SET
			stripe_price_id = EXCLUDED.stripe_price_id,
			display_name = EXCLUDED.display_name,
			kind = EXCLUDED.kind,
			included_quota_bytes = EXCLUDED.included_quota_bytes,
			package_name = EXCLUDED.package_name,
			price_multipliers = EXCLUDED.price_multipliers,
			features = EXCLUDED.features,
			trial_days = EXCLUDED.trial_days,
			active = EXCLUDED.active,
			sort_order = EXCLUDED.sort_order,
			updated_at = now()
		RETURNING created_at, updated_at`

	return s.db.QueryRowContext(
		ctx,
		query,
		normalizePlanID(plan.PlanID),
		stripePriceID,
		strings.TrimSpace(plan.DisplayName),
		strings.TrimSpace(plan.Kind),
		plan.IncludedQuotaBytes,
		strings.TrimSpace(plan.PackageName),
		multipliersJSON,
		featuresJSON,
		plan.TrialDays,
		plan.Active,
		plan.SortOrder,
	).Scan(&plan.CreatedAt, &plan.UpdatedAt)
}

func (s *postgresStore) DeleteBillingPlan(ctx context.Context, planID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM billing_plans WHERE plan_id = $1`, normalizePlanID(planID))
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrBillingPlanNotFound
	}
	return nil
}

func (s *postgresStore) BeginStripeWebhookEvent(ctx context.Context, event *StripeWebhookEvent) (bool, error) {
	if event == nil || strings.TrimSpace(event.EventID) == "" {
		return false, nil
	}
	payload := event.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}

	const insert = `
		INSERT INTO stripe_webhook_events (event_id, event_type, payload, status)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (event_id) DO NOTHING`
	res, err := s.db.ExecContext(ctx, insert, strings.TrimSpace(event.EventID), strings.TrimSpace(event.EventType), []byte(payload), StripeWebhookEventStatusReceived)
	if err != nil {
		return false, err
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if inserted > 0 {
		return false, nil
	}

	// Event already recorded: replay only if the previous attempt failed.
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM stripe_webhook_events WHERE event_id = $1`, strings.TrimSpace(event.EventID)).Scan(&status); err != nil {
		return false, err
	}
	return status == StripeWebhookEventStatusProcessed, nil
}

func (s *postgresStore) FinishStripeWebhookEvent(ctx context.Context, eventID string, procErr error) error {
	status := StripeWebhookEventStatusProcessed
	lastError := ""
	if procErr != nil {
		status = StripeWebhookEventStatusFailed
		lastError = procErr.Error()
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE stripe_webhook_events SET status = $2, last_error = $3, processed_at = $4 WHERE event_id = $1`,
		strings.TrimSpace(eventID), status, lastError, time.Now().UTC())
	return err
}

func orEmptyMultipliers(m map[string]float64) map[string]float64 {
	if m == nil {
		return map[string]float64{}
	}
	return m
}

func orEmptyFeatures(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
