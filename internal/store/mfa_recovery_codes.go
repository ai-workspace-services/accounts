package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

func (s *memoryStore) ReplaceMFARecoveryCodes(ctx context.Context, userID string, codes []MFARecoveryCode) error {
	_ = ctx
	if strings.TrimSpace(userID) == "" || len(codes) == 0 {
		return ErrMFARecoveryCodeInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[userID]; !ok {
		return ErrUserNotFound
	}
	now := codes[0].CreatedAt
	for _, code := range codes {
		if code.ID == "" || code.BatchID == "" || code.UserID != userID || code.CodeHash == "" || !code.ExpiresAt.After(now) {
			return ErrMFARecoveryCodeInvalid
		}
	}
	for _, existing := range s.mfaRecoveryCodes {
		if existing.UserID == userID && existing.ConsumedAt.IsZero() && existing.RevokedAt.IsZero() {
			existing.RevokedAt = now
		}
	}
	for i := range codes {
		copy := codes[i]
		s.mfaRecoveryCodes[copy.ID] = &copy
	}
	return nil
}

func (s *memoryStore) ListMFARecoveryCodes(ctx context.Context, userID string, now time.Time) ([]MFARecoveryCode, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()
	var codes []MFARecoveryCode
	for _, code := range s.mfaRecoveryCodes {
		if code.UserID != userID || !code.ConsumedAt.IsZero() || !code.RevokedAt.IsZero() || !now.Before(code.ExpiresAt) {
			continue
		}
		codes = append(codes, *code)
	}
	return codes, nil
}

// A wrong recovery-code attempt is counted against every code in the active
// batch. That keeps the attempt budget durable and shared across instances,
// even when the submitted value does not match any particular code hash.
func (s *memoryStore) RecordMFARecoveryCodeFailure(ctx context.Context, userID string, now time.Time, maxAttempts int, lockout time.Duration) (time.Time, error) {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	var lockedUntil time.Time
	found := false
	for _, code := range s.mfaRecoveryCodes {
		if code.UserID != userID || !code.ConsumedAt.IsZero() || !code.RevokedAt.IsZero() || !now.Before(code.ExpiresAt) {
			continue
		}
		found = true
		if now.Before(code.LockedUntil) {
			lockedUntil = code.LockedUntil
			continue
		}
		code.FailedAttempts++
		if code.FailedAttempts >= maxAttempts {
			code.LockedUntil = now.Add(lockout)
			lockedUntil = code.LockedUntil
		}
	}
	if !found {
		return time.Time{}, ErrMFARecoveryCodeInvalid
	}
	return lockedUntil, nil
}

func (s *memoryStore) RevokeMFARecoveryCodes(ctx context.Context, userID string, now time.Time) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, code := range s.mfaRecoveryCodes {
		if code.UserID == userID && code.ConsumedAt.IsZero() && code.RevokedAt.IsZero() {
			code.RevokedAt = now
		}
	}
	return nil
}

func (s *memoryStore) CompleteMFAPasswordReset(ctx context.Context, userID, recoveryCodeID, passwordHash string, now time.Time) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	user, ok := s.byID[userID]
	if !ok || !user.Active || user.ArchivedAt != nil {
		return ErrMFARecoveryCodeInvalid
	}
	if recoveryCodeID != "" {
		code, ok := s.mfaRecoveryCodes[recoveryCodeID]
		if !ok || code.UserID != userID || !code.ConsumedAt.IsZero() || !code.RevokedAt.IsZero() || !now.Before(code.ExpiresAt) || now.Before(code.LockedUntil) {
			return ErrMFARecoveryCodeInvalid
		}
		code.ConsumedAt = now
	}
	user.PasswordHash = passwordHash
	for token, session := range s.sessions {
		if session.UserID == userID {
			delete(s.sessions, token)
		}
	}
	return nil
}

func (s *postgresStore) ReplaceMFARecoveryCodes(ctx context.Context, userID string, codes []MFARecoveryCode) error {
	if strings.TrimSpace(userID) == "" || len(codes) == 0 {
		return ErrMFARecoveryCodeInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := codes[0].CreatedAt.UTC()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE public.mfa_recovery_codes
SET revoked_at = $2
WHERE user_uuid = $1 AND consumed_at IS NULL AND revoked_at IS NULL`, userID, now); err != nil {
		return err
	}
	for _, code := range codes {
		if code.ID == "" || code.BatchID == "" || code.UserID != userID || code.CodeHash == "" || !code.ExpiresAt.After(now) {
			return ErrMFARecoveryCodeInvalid
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO public.mfa_recovery_codes
(id, user_uuid, batch_uuid, code_hash, expires_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6)`, code.ID, userID, code.BatchID, code.CodeHash, code.ExpiresAt.UTC(), code.CreatedAt.UTC()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *postgresStore) ListMFARecoveryCodes(ctx context.Context, userID string, now time.Time) ([]MFARecoveryCode, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id::text, user_uuid::text, batch_uuid::text, code_hash,
expires_at, failed_attempts, COALESCE(locked_until, 'epoch'::timestamptz), created_at,
COALESCE(consumed_at, 'epoch'::timestamptz), COALESCE(revoked_at, 'epoch'::timestamptz)
FROM public.mfa_recovery_codes
WHERE user_uuid = $1 AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at > $2
ORDER BY created_at, id`, userID, now.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var codes []MFARecoveryCode
	for rows.Next() {
		var code MFARecoveryCode
		if err := rows.Scan(&code.ID, &code.UserID, &code.BatchID, &code.CodeHash, &code.ExpiresAt,
			&code.FailedAttempts, &code.LockedUntil, &code.CreatedAt, &code.ConsumedAt, &code.RevokedAt); err != nil {
			return nil, err
		}
		codes = append(codes, code)
	}
	return codes, rows.Err()
}

func (s *postgresStore) RecordMFARecoveryCodeFailure(ctx context.Context, userID string, now time.Time, maxAttempts int, lockout time.Duration) (time.Time, error) {
	var lockedUntil sql.NullTime
	err := s.db.QueryRowContext(ctx, `UPDATE public.mfa_recovery_codes
SET failed_attempts = failed_attempts + 1,
    locked_until = CASE WHEN failed_attempts + 1 >= $3
      THEN $2 + ($4 * interval '1 second') ELSE locked_until END
WHERE user_uuid = $1 AND consumed_at IS NULL AND revoked_at IS NULL
  AND expires_at > $2 AND (locked_until IS NULL OR locked_until <= $2)
RETURNING locked_until`, userID, now.UTC(), maxAttempts, lockout.Seconds()).Scan(&lockedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrMFARecoveryCodeInvalid
	}
	if err != nil {
		return time.Time{}, err
	}
	if !lockedUntil.Valid {
		return time.Time{}, nil
	}
	return lockedUntil.Time.UTC(), nil
}

func (s *postgresStore) RevokeMFARecoveryCodes(ctx context.Context, userID string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE public.mfa_recovery_codes SET revoked_at = $2
WHERE user_uuid = $1 AND consumed_at IS NULL AND revoked_at IS NULL`, userID, now.UTC())
	return err
}

func (s *postgresStore) CompleteMFAPasswordReset(ctx context.Context, userID, recoveryCodeID, passwordHash string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if recoveryCodeID != "" {
		var consumedID string
		err := tx.QueryRowContext(ctx, `UPDATE public.mfa_recovery_codes
SET consumed_at = $3
WHERE id = $1 AND user_uuid = $2 AND consumed_at IS NULL AND revoked_at IS NULL
  AND expires_at > $3 AND (locked_until IS NULL OR locked_until <= $3)
RETURNING id::text`, recoveryCodeID, userID, now.UTC()).Scan(&consumedID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMFARecoveryCodeInvalid
		}
		if err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE public.users SET password = $2
WHERE uuid = $1 AND active = TRUE AND archived_at IS NULL`, userID, passwordHash)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrMFARecoveryCodeInvalid
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM public.sessions WHERE user_uuid = $1`, userID); err != nil {
		return err
	}
	return tx.Commit()
}
