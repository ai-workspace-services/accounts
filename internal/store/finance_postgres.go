package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const financeInvoiceColumns = `id::text, idempotency_key, account_uuid::text, COALESCE(subscription_uuid::text, ''), provider, COALESCE(provider_invoice_id, ''), amount_minor, currency, description, issued_at, due_at, created_at`

func scanFinanceInvoice(scanner interface{ Scan(...any) error }) (*FinanceInvoice, error) {
	var invoice FinanceInvoice
	var dueAt sql.NullTime
	err := scanner.Scan(&invoice.ID, &invoice.IdempotencyKey, &invoice.AccountUUID, &invoice.SubscriptionUUID, &invoice.Provider, &invoice.ProviderInvoiceID, &invoice.AmountMinor, &invoice.Currency, &invoice.Description, &invoice.IssuedAt, &dueAt, &invoice.CreatedAt)
	if err != nil {
		return nil, err
	}
	if dueAt.Valid {
		invoice.DueAt = &dueAt.Time
	}
	return &invoice, nil
}

func (s *postgresStore) CreateFinanceInvoice(ctx context.Context, invoice *FinanceInvoice) (bool, error) {
	if invoice == nil || strings.TrimSpace(invoice.IdempotencyKey) == "" || strings.TrimSpace(invoice.AccountUUID) == "" || invoice.AmountMinor < 0 {
		return false, ErrFinanceIdempotencyConflict
	}
	copy := *invoice
	copy.ID = financeID(copy.ID)
	copy.IdempotencyKey = strings.TrimSpace(copy.IdempotencyKey)
	copy.AccountUUID = strings.TrimSpace(copy.AccountUUID)
	copy.SubscriptionUUID = strings.TrimSpace(copy.SubscriptionUUID)
	copy.Provider = financeProvider(copy.Provider)
	copy.ProviderInvoiceID = strings.TrimSpace(copy.ProviderInvoiceID)
	copy.Currency = financeCurrency(copy.Currency)
	if !validFinanceCurrency(copy.Currency) {
		return false, ErrFinanceIdempotencyConflict
	}
	issuedAt := copy.IssuedAt
	if issuedAt.IsZero() {
		issuedAt = time.Now().UTC()
	}
	copy.IssuedAt = issuedAt.UTC()
	const insert = `INSERT INTO public.finance_invoices (id, idempotency_key, account_uuid, subscription_uuid, provider, provider_invoice_id, amount_minor, currency, description, issued_at, due_at)
VALUES ($1::uuid, $2, $3::uuid, NULLIF($4, '')::uuid, $5, NULLIF($6, ''), $7, $8, $9, $10, $11)
ON CONFLICT (idempotency_key) DO NOTHING RETURNING id::text, created_at`
	err := s.db.QueryRowContext(ctx, insert, copy.ID, copy.IdempotencyKey, copy.AccountUUID, strings.TrimSpace(copy.SubscriptionUUID), copy.Provider, copy.ProviderInvoiceID, copy.AmountMinor, copy.Currency, strings.TrimSpace(copy.Description), issuedAt.UTC(), copy.DueAt).Scan(&copy.ID, &copy.CreatedAt)
	if err == nil {
		*invoice = copy
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	existing, err := scanFinanceInvoice(s.db.QueryRowContext(ctx, `SELECT `+financeInvoiceColumns+` FROM public.finance_invoices WHERE idempotency_key = $1`, copy.IdempotencyKey))
	if err != nil {
		return false, err
	}
	if existing.AccountUUID != copy.AccountUUID || existing.SubscriptionUUID != copy.SubscriptionUUID || existing.Provider != copy.Provider || existing.ProviderInvoiceID != copy.ProviderInvoiceID || existing.AmountMinor != copy.AmountMinor || existing.Currency != copy.Currency || existing.Description != strings.TrimSpace(copy.Description) || !sameOptionalTime(copy.DueAt, existing.DueAt) || !copy.IssuedAt.IsZero() && !copy.IssuedAt.Equal(existing.IssuedAt) {
		return false, ErrFinanceIdempotencyConflict
	}
	*invoice = *existing
	return false, nil
}

func (s *postgresStore) ListFinanceInvoices(ctx context.Context, accountUUID, subscriptionUUID string, limit int) ([]FinanceInvoice, error) {
	query := `SELECT ` + financeInvoiceColumns + ` FROM public.finance_invoices WHERE ($1 = '' OR account_uuid::text = $1) AND ($2 = '' OR subscription_uuid::text = $2) ORDER BY created_at DESC`
	args := []any{strings.TrimSpace(accountUUID), strings.TrimSpace(subscriptionUUID)}
	if limit > 0 {
		query += ` LIMIT $3`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var invoices []FinanceInvoice
	for rows.Next() {
		invoice, err := scanFinanceInvoice(rows)
		if err != nil {
			return nil, err
		}
		invoices = append(invoices, *invoice)
	}
	return invoices, rows.Err()
}

func (s *postgresStore) GetFinanceInvoice(ctx context.Context, id string) (*FinanceInvoice, error) {
	invoice, err := scanFinanceInvoice(s.db.QueryRowContext(ctx, `SELECT `+financeInvoiceColumns+` FROM public.finance_invoices WHERE id = $1::uuid`, strings.TrimSpace(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrFinanceRecordNotFound
	}
	return invoice, err
}

func (s *postgresStore) RecordFinancePayment(ctx context.Context, payment *FinancePayment) (bool, error) {
	if payment == nil || strings.TrimSpace(payment.IdempotencyKey) == "" || payment.AmountMinor <= 0 {
		return false, ErrFinanceIdempotencyConflict
	}
	copy := *payment
	copy.ID = financeID(copy.ID)
	copy.IdempotencyKey = strings.TrimSpace(copy.IdempotencyKey)
	copy.InvoiceID = strings.TrimSpace(copy.InvoiceID)
	copy.Provider = financeProvider(copy.Provider)
	copy.ProviderPaymentID = strings.TrimSpace(copy.ProviderPaymentID)
	copy.Currency = financeCurrency(copy.Currency)
	paidAtProvided := !copy.PaidAt.IsZero()
	if !validFinanceCurrency(copy.Currency) {
		return false, ErrFinanceIdempotencyConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var accountUUID, invoiceCurrency string
	var invoiceAmount int64
	if err := tx.QueryRowContext(ctx, `SELECT account_uuid::text, currency, amount_minor FROM public.finance_invoices WHERE id = $1::uuid FOR KEY SHARE`, copy.InvoiceID).Scan(&accountUUID, &invoiceCurrency, &invoiceAmount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrFinanceRecordNotFound
		}
		return false, err
	}
	if copy.AccountUUID == "" {
		copy.AccountUUID = accountUUID
	}
	if accountUUID != copy.AccountUUID || invoiceCurrency != copy.Currency || invoiceAmount != copy.AmountMinor {
		return false, ErrFinanceIdempotencyConflict
	}
	if copy.PaidAt.IsZero() {
		copy.PaidAt = time.Now().UTC()
	}
	const insert = `INSERT INTO public.finance_payments (id, idempotency_key, invoice_id, account_uuid, provider, provider_payment_id, amount_minor, currency, paid_at)
VALUES ($1::uuid, $2, $3::uuid, $4::uuid, $5, NULLIF($6, ''), $7, $8, $9)
ON CONFLICT (idempotency_key) DO NOTHING RETURNING created_at`
	err = tx.QueryRowContext(ctx, insert, copy.ID, copy.IdempotencyKey, copy.InvoiceID, copy.AccountUUID, copy.Provider, copy.ProviderPaymentID, copy.AmountMinor, copy.Currency, copy.PaidAt.UTC()).Scan(&copy.CreatedAt)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		*payment = copy
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "finance_payments_invoice_uk" {
			return false, ErrFinanceInvoiceAlreadyPaid
		}
		return false, err
	}
	var old FinancePayment
	err = tx.QueryRowContext(ctx, `SELECT id::text, invoice_id::text, account_uuid::text, provider, COALESCE(provider_payment_id, ''), amount_minor, currency, paid_at, created_at FROM public.finance_payments WHERE idempotency_key = $1`, copy.IdempotencyKey).Scan(&old.ID, &old.InvoiceID, &old.AccountUUID, &old.Provider, &old.ProviderPaymentID, &old.AmountMinor, &old.Currency, &old.PaidAt, &old.CreatedAt)
	if err != nil {
		return false, err
	}
	if old.InvoiceID != copy.InvoiceID || old.AccountUUID != copy.AccountUUID || old.Provider != copy.Provider || old.ProviderPaymentID != copy.ProviderPaymentID || old.AmountMinor != copy.AmountMinor || old.Currency != copy.Currency || paidAtProvided && !copy.PaidAt.Equal(old.PaidAt) {
		return false, ErrFinanceIdempotencyConflict
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	old.IdempotencyKey = copy.IdempotencyKey
	*payment = old
	return false, nil
}

func (s *postgresStore) ListFinancePayments(ctx context.Context, accountUUID string, limit int) ([]FinancePayment, error) {
	query := `SELECT id::text, idempotency_key, invoice_id::text, account_uuid::text, provider, COALESCE(provider_payment_id, ''), amount_minor, currency, paid_at, created_at FROM public.finance_payments WHERE ($1 = '' OR account_uuid::text = $1) ORDER BY created_at DESC`
	args := []any{strings.TrimSpace(accountUUID)}
	if limit > 0 {
		query += ` LIMIT $2`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var payments []FinancePayment
	for rows.Next() {
		var payment FinancePayment
		if err := rows.Scan(&payment.ID, &payment.IdempotencyKey, &payment.InvoiceID, &payment.AccountUUID, &payment.Provider, &payment.ProviderPaymentID, &payment.AmountMinor, &payment.Currency, &payment.PaidAt, &payment.CreatedAt); err != nil {
			return nil, err
		}
		payments = append(payments, payment)
	}
	return payments, rows.Err()
}

func (s *postgresStore) RecordFinanceRefund(ctx context.Context, refund *FinanceRefund) (bool, error) {
	if refund == nil || strings.TrimSpace(refund.IdempotencyKey) == "" || refund.AmountMinor <= 0 {
		return false, ErrFinanceIdempotencyConflict
	}
	copy := *refund
	copy.ID = financeID(copy.ID)
	copy.IdempotencyKey = strings.TrimSpace(copy.IdempotencyKey)
	copy.PaymentID = strings.TrimSpace(copy.PaymentID)
	copy.Provider = financeProvider(copy.Provider)
	copy.ProviderRefundID = strings.TrimSpace(copy.ProviderRefundID)
	copy.Currency = financeCurrency(copy.Currency)
	if !validFinanceCurrency(copy.Currency) {
		return false, ErrFinanceIdempotencyConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, copy.IdempotencyKey); err != nil {
		return false, err
	}
	var paidAmount int64
	var currency string
	if err := tx.QueryRowContext(ctx, `SELECT amount_minor, currency FROM public.finance_payments WHERE id = $1::uuid FOR UPDATE`, copy.PaymentID).Scan(&paidAmount, &currency); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrFinanceRecordNotFound
		}
		return false, err
	}
	var old FinanceRefund
	err = tx.QueryRowContext(ctx, `SELECT id::text, payment_id::text, provider, COALESCE(provider_refund_id, ''), amount_minor, currency, reason, refunded_at, created_at FROM public.finance_refunds WHERE idempotency_key = $1`, copy.IdempotencyKey).Scan(&old.ID, &old.PaymentID, &old.Provider, &old.ProviderRefundID, &old.AmountMinor, &old.Currency, &old.Reason, &old.RefundedAt, &old.CreatedAt)
	if err == nil {
		if old.PaymentID != copy.PaymentID || old.Provider != copy.Provider || old.ProviderRefundID != copy.ProviderRefundID || old.AmountMinor != copy.AmountMinor || old.Currency != copy.Currency || old.Reason != strings.TrimSpace(copy.Reason) || !copy.RefundedAt.IsZero() && !copy.RefundedAt.Equal(old.RefundedAt) {
			return false, ErrFinanceIdempotencyConflict
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		old.IdempotencyKey = copy.IdempotencyKey
		*refund = old
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	refundOperation, operationErr := scanFinanceOperation(tx.QueryRowContext(ctx, `SELECT `+financeOperationColumns+` FROM public.finance_operations WHERE idempotency_key = $1`, copy.IdempotencyKey))
	if operationErr == nil {
		if !financeRefundMatchesOperation(refundOperation, &copy) {
			return false, ErrFinanceIdempotencyConflict
		}
	} else if !errors.Is(operationErr, sql.ErrNoRows) {
		return false, operationErr
	}
	if currency != copy.Currency {
		return false, ErrFinanceIdempotencyConflict
	}
	var refunded int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(sum(amount_minor), 0) FROM public.finance_refunds WHERE payment_id = $1::uuid`, copy.PaymentID).Scan(&refunded); err != nil {
		return false, err
	}
	var reserved int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(sum((o.request->>'amount_minor')::bigint), 0)
FROM public.finance_operations o
WHERE o.operation_type = 'refund' AND o.target_type = 'payment' AND o.target_id = $1
  AND o.status IN ('pending', 'in_progress', 'succeeded', 'reconcile_needed')
  AND o.idempotency_key <> $2
  AND o.request->>'amount_minor' ~ '^[0-9]+$'
  AND NOT EXISTS (SELECT 1 FROM public.finance_refunds r WHERE r.idempotency_key = o.idempotency_key)`, copy.PaymentID, copy.IdempotencyKey).Scan(&reserved); err != nil {
		return false, err
	}
	refunded += reserved
	if copy.AmountMinor > paidAmount-refunded {
		return false, ErrFinanceRefundExceedsPayment
	}
	if copy.RefundedAt.IsZero() {
		copy.RefundedAt = time.Now().UTC()
	}
	const insert = `INSERT INTO public.finance_refunds (id, idempotency_key, payment_id, provider, provider_refund_id, amount_minor, currency, reason, refunded_at)
VALUES ($1::uuid, $2, $3::uuid, $4, NULLIF($5, ''), $6, $7, $8, $9) RETURNING created_at`
	if err := tx.QueryRowContext(ctx, insert, copy.ID, copy.IdempotencyKey, copy.PaymentID, copy.Provider, copy.ProviderRefundID, copy.AmountMinor, copy.Currency, strings.TrimSpace(copy.Reason), copy.RefundedAt.UTC()).Scan(&copy.CreatedAt); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	*refund = copy
	return true, nil
}

func (s *postgresStore) ListFinanceRefunds(ctx context.Context, accountUUID string, limit int) ([]FinanceRefund, error) {
	query := `SELECT r.id::text, r.idempotency_key, r.payment_id::text, r.provider, COALESCE(r.provider_refund_id, ''), r.amount_minor, r.currency, r.reason, r.refunded_at, r.created_at FROM public.finance_refunds r JOIN public.finance_payments p ON p.id = r.payment_id WHERE ($1 = '' OR p.account_uuid::text = $1) ORDER BY r.created_at DESC`
	args := []any{strings.TrimSpace(accountUUID)}
	if limit > 0 {
		query += ` LIMIT $2`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refunds []FinanceRefund
	for rows.Next() {
		var refund FinanceRefund
		if err := rows.Scan(&refund.ID, &refund.IdempotencyKey, &refund.PaymentID, &refund.Provider, &refund.ProviderRefundID, &refund.AmountMinor, &refund.Currency, &refund.Reason, &refund.RefundedAt, &refund.CreatedAt); err != nil {
			return nil, err
		}
		refunds = append(refunds, refund)
	}
	return refunds, rows.Err()
}

const financeOperationColumns = `id::text, idempotency_key, operation_type, target_type, target_id, provider, COALESCE(provider_operation_id, ''), status, attempt_count, next_attempt_at, last_error, request, response, created_at, updated_at`

func scanFinanceOperation(scanner interface{ Scan(...any) error }) (*FinanceOperation, error) {
	var operation FinanceOperation
	var nextAttempt sql.NullTime
	var request, response []byte
	err := scanner.Scan(&operation.ID, &operation.IdempotencyKey, &operation.OperationType, &operation.TargetType, &operation.TargetID, &operation.Provider, &operation.ProviderOperationID, &operation.Status, &operation.AttemptCount, &nextAttempt, &operation.LastError, &request, &response, &operation.CreatedAt, &operation.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if nextAttempt.Valid {
		operation.NextAttemptAt = &nextAttempt.Time
	}
	operation.Request = financeJSON(request)
	operation.Response = financeJSON(response)
	return &operation, nil
}

func (s *postgresStore) BeginFinanceOperation(ctx context.Context, operation *FinanceOperation) (bool, error) {
	if operation == nil || strings.TrimSpace(operation.IdempotencyKey) == "" || strings.TrimSpace(operation.OperationType) == "" {
		return false, ErrFinanceIdempotencyConflict
	}
	copy := *operation
	copy.IdempotencyKey = strings.TrimSpace(copy.IdempotencyKey)
	copy.OperationType = strings.TrimSpace(copy.OperationType)
	copy.TargetType = strings.TrimSpace(copy.TargetType)
	copy.TargetID = strings.TrimSpace(copy.TargetID)
	copy.Provider = financeProvider(copy.Provider)
	copy.Request = financeJSON(copy.Request)
	var refundAmount int64
	var refundCurrency string
	var err error
	if copy.OperationType == "refund" {
		if copy.TargetType != "payment" || copy.TargetID == "" {
			return false, ErrFinanceIdempotencyConflict
		}
		refundAmount, refundCurrency, err = financeRefundRequest(&copy)
		if err != nil {
			return false, ErrFinanceIdempotencyConflict
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	copy.ID = financeID(copy.ID)
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, copy.IdempotencyKey); err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO public.finance_operations (id, idempotency_key, operation_type, target_type, target_id, provider, status, request)
VALUES ($1::uuid, $2, $3, $4, $5, $6, 'pending', $7::jsonb) ON CONFLICT (idempotency_key) DO NOTHING`, copy.ID, copy.IdempotencyKey, copy.OperationType, copy.TargetType, copy.TargetID, copy.Provider, []byte(copy.Request))
	if err != nil {
		return false, err
	}
	stored, err := scanFinanceOperation(tx.QueryRowContext(ctx, `SELECT `+financeOperationColumns+` FROM public.finance_operations WHERE idempotency_key = $1 FOR UPDATE`, copy.IdempotencyKey))
	if err != nil {
		return false, err
	}
	if stored.OperationType != copy.OperationType || stored.TargetType != copy.TargetType || stored.TargetID != copy.TargetID || stored.Provider != copy.Provider || !jsonEqual(stored.Request, copy.Request) {
		return false, ErrFinanceIdempotencyConflict
	}
	if stored.Status == FinanceOperationInProgress {
		return false, ErrFinanceOperationInProgress
	}
	if stored.Status == FinanceOperationSucceeded {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		*operation = *stored
		return false, nil
	}
	if copy.OperationType == "refund" {
		var paymentAmount int64
		var paymentCurrency string
		if err := tx.QueryRowContext(ctx, `SELECT amount_minor, currency FROM public.finance_payments WHERE id = $1::uuid FOR UPDATE`, copy.TargetID).Scan(&paymentAmount, &paymentCurrency); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, ErrFinanceRecordNotFound
			}
			return false, err
		}
		if paymentCurrency != refundCurrency {
			return false, ErrFinanceIdempotencyConflict
		}
		var refunded, reserved int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(sum(amount_minor), 0) FROM public.finance_refunds WHERE payment_id = $1::uuid`, copy.TargetID).Scan(&refunded); err != nil {
			return false, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(sum((o.request->>'amount_minor')::bigint), 0)
FROM public.finance_operations o
WHERE o.operation_type = 'refund' AND o.target_type = 'payment' AND o.target_id = $1
  AND o.status IN ('pending', 'in_progress', 'succeeded', 'reconcile_needed')
  AND o.idempotency_key <> $2
  AND o.request->>'amount_minor' ~ '^[0-9]+$'
  AND NOT EXISTS (SELECT 1 FROM public.finance_refunds r WHERE r.idempotency_key = o.idempotency_key)`, copy.TargetID, copy.IdempotencyKey).Scan(&reserved); err != nil {
			return false, err
		}
		if refundAmount > paymentAmount-refunded-reserved {
			return false, ErrFinanceRefundExceedsPayment
		}
	}
	if stored.Status != FinanceOperationPending && stored.Status != FinanceOperationFailed && stored.Status != FinanceOperationReconciliationRequired {
		return false, ErrFinanceOperationNotRetryable
	}
	stored.AttemptCount++
	stored.Status = FinanceOperationInProgress
	stored.UpdatedAt = time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE public.finance_operations SET status = $2, attempt_count = $3, provider = $4, updated_at = $5, next_attempt_at = NULL WHERE id = $1::uuid`, stored.ID, stored.Status, stored.AttemptCount, stored.Provider, stored.UpdatedAt); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO public.finance_operation_events (operation_id, attempt, event_type, status, payload) VALUES ($1::uuid, $2, 'attempt_started', $3, $4::jsonb)`, stored.ID, stored.AttemptCount, stored.Status, []byte(stored.Request)); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	*operation = *stored
	return true, nil
}

func jsonEqual(a, b json.RawMessage) bool {
	var left, right any
	return json.Unmarshal(a, &left) == nil && json.Unmarshal(b, &right) == nil && reflect.DeepEqual(left, right)
}

func (s *postgresStore) FinishFinanceOperation(ctx context.Context, operationID, status, providerOperationID string, response json.RawMessage, operationErr error, nextAttemptAt *time.Time) error {
	if status != FinanceOperationSucceeded && status != FinanceOperationFailed && status != FinanceOperationReconciliationRequired {
		return ErrFinanceOperationNotRetryable
	}
	if operationErr != nil {
		status = FinanceOperationFailed
	}
	lastError := ""
	if operationErr != nil {
		lastError = operationErr.Error()
	}
	response = financeJSON(response)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var attempt int
	var updatedAt = time.Now().UTC()
	err = tx.QueryRowContext(ctx, `UPDATE public.finance_operations SET status = $2, provider_operation_id = NULLIF($3, ''), response = $4::jsonb, last_error = $5, next_attempt_at = $6, updated_at = $7 WHERE id = $1::uuid AND status = 'in_progress' RETURNING attempt_count`, strings.TrimSpace(operationID), status, strings.TrimSpace(providerOperationID), []byte(response), lastError, nextAttemptAt, updatedAt).Scan(&attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrFinanceOperationNotRetryable
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO public.finance_operation_events (operation_id, attempt, event_type, status, provider_operation_id, payload, error) VALUES ($1::uuid, $2, $3, $4, NULLIF($5, ''), $6::jsonb, $7)`, strings.TrimSpace(operationID), attempt, "attempt_"+status, status, strings.TrimSpace(providerOperationID), []byte(response), lastError); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *postgresStore) GetFinanceOperation(ctx context.Context, idempotencyKey string) (*FinanceOperation, error) {
	operation, err := scanFinanceOperation(s.db.QueryRowContext(ctx, `SELECT `+financeOperationColumns+` FROM public.finance_operations WHERE idempotency_key = $1`, strings.TrimSpace(idempotencyKey)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrFinanceRecordNotFound
	}
	return operation, err
}

func (s *postgresStore) ListFinanceOperationsForReconciliation(ctx context.Context, limit int) ([]FinanceOperation, error) {
	query := `SELECT ` + financeOperationColumns + ` FROM public.finance_operations o WHERE status IN ('pending', 'in_progress', 'failed', 'reconcile_needed') OR (operation_type = 'refund' AND status = 'succeeded' AND NOT EXISTS (SELECT 1 FROM public.finance_refunds r WHERE r.idempotency_key = o.idempotency_key)) ORDER BY updated_at ASC`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT $1`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var operations []FinanceOperation
	for rows.Next() {
		operation, err := scanFinanceOperation(rows)
		if err != nil {
			return nil, err
		}
		operations = append(operations, *operation)
	}
	return operations, rows.Err()
}

func (s *postgresStore) ListFinanceOperationEvents(ctx context.Context, operationID string) ([]FinanceOperationEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id::text, operation_id::text, attempt, event_type, status, COALESCE(provider_operation_id, ''), payload, error, occurred_at FROM public.finance_operation_events WHERE operation_id = $1::uuid ORDER BY id`, strings.TrimSpace(operationID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []FinanceOperationEvent
	for rows.Next() {
		var event FinanceOperationEvent
		var payload []byte
		if err := rows.Scan(&event.ID, &event.OperationID, &event.Attempt, &event.EventType, &event.Status, &event.ProviderOperationID, &payload, &event.Error, &event.OccurredAt); err != nil {
			return nil, err
		}
		event.Payload = financeJSON(payload)
		events = append(events, event)
	}
	return events, rows.Err()
}
