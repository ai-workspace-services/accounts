package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

func (s *memoryStore) CreatePasswordRecoveryChallenge(ctx context.Context, challenge *PasswordRecoveryChallenge) error {
	_ = ctx
	if challenge == nil || strings.TrimSpace(challenge.ID) == "" || strings.TrimSpace(challenge.UserID) == "" {
		return ErrPasswordRecoveryInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[challenge.UserID]; !ok {
		return ErrUserNotFound
	}
	for _, existing := range s.passwordRecovery {
		if existing.UserID == challenge.UserID && existing.Kind == challenge.Kind && existing.ConsumedAt.IsZero() && existing.InvalidatedAt.IsZero() {
			existing.InvalidatedAt = challenge.CreatedAt
		}
	}
	copy := *challenge
	copy.Email = strings.ToLower(strings.TrimSpace(copy.Email))
	s.passwordRecovery[copy.ID] = &copy
	return nil
}

func (s *memoryStore) GetPasswordRecoveryChallengeByTokenHash(ctx context.Context, tokenHash string) (*PasswordRecoveryChallenge, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, challenge := range s.passwordRecovery {
		if challenge.Kind == "token" && challenge.SecretHash == tokenHash {
			copy := *challenge
			return &copy, nil
		}
	}
	return nil, ErrPasswordRecoveryInvalid
}

func (s *memoryStore) GetLatestPasswordRecoveryCode(ctx context.Context, email string) (*PasswordRecoveryChallenge, error) {
	_ = ctx
	email = strings.ToLower(strings.TrimSpace(email))
	s.mu.RLock()
	defer s.mu.RUnlock()
	var latest *PasswordRecoveryChallenge
	for _, challenge := range s.passwordRecovery {
		if challenge.Kind != "code" || !strings.EqualFold(challenge.Email, email) || !challenge.ConsumedAt.IsZero() || !challenge.InvalidatedAt.IsZero() {
			continue
		}
		if latest == nil || challenge.CreatedAt.After(latest.CreatedAt) {
			copy := *challenge
			latest = &copy
		}
	}
	if latest == nil {
		return nil, ErrPasswordRecoveryInvalid
	}
	return latest, nil
}

func (s *memoryStore) RecordPasswordRecoveryFailure(ctx context.Context, challengeID string, now time.Time, maxAttempts int, lockout time.Duration) (time.Time, error) {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	challenge, ok := s.passwordRecovery[challengeID]
	if !ok || !challenge.ConsumedAt.IsZero() || !challenge.InvalidatedAt.IsZero() || !now.Before(challenge.ExpiresAt) {
		return time.Time{}, ErrPasswordRecoveryInvalid
	}
	challenge.FailedAttempts++
	if challenge.FailedAttempts >= maxAttempts {
		challenge.LockedUntil = now.Add(lockout)
	}
	return challenge.LockedUntil, nil
}

func (s *memoryStore) InvalidatePasswordRecoveryChallenge(ctx context.Context, challengeID string, now time.Time) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	if challenge, ok := s.passwordRecovery[challengeID]; ok && challenge.InvalidatedAt.IsZero() && challenge.ConsumedAt.IsZero() {
		challenge.InvalidatedAt = now
	}
	return nil
}

func (s *memoryStore) CompletePasswordRecovery(ctx context.Context, challengeID, passwordHash string, now time.Time) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	challenge, ok := s.passwordRecovery[challengeID]
	if !ok || !challenge.ConsumedAt.IsZero() || !challenge.InvalidatedAt.IsZero() || !now.Before(challenge.ExpiresAt) || now.Before(challenge.LockedUntil) {
		return ErrPasswordRecoveryInvalid
	}
	user, ok := s.byID[challenge.UserID]
	if !ok {
		return ErrPasswordRecoveryInvalid
	}
	user.PasswordHash = passwordHash
	challenge.ConsumedAt = now
	for token, session := range s.sessions {
		if session.UserID == challenge.UserID {
			delete(s.sessions, token)
		}
	}
	return nil
}

func (s *postgresStore) CreatePasswordRecoveryChallenge(ctx context.Context, challenge *PasswordRecoveryChallenge) error {
	if challenge == nil || strings.TrimSpace(challenge.ID) == "" || strings.TrimSpace(challenge.UserID) == "" {
		return ErrPasswordRecoveryInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `UPDATE public.password_recovery_challenges
SET invalidated_at = $3
WHERE user_uuid = $1 AND challenge_kind = $2
  AND consumed_at IS NULL AND invalidated_at IS NULL`, challenge.UserID, challenge.Kind, challenge.CreatedAt.UTC())
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO public.password_recovery_challenges
(id, user_uuid, email_snapshot, challenge_kind, secret_hash, expires_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`, challenge.ID, challenge.UserID,
		strings.ToLower(strings.TrimSpace(challenge.Email)), challenge.Kind, challenge.SecretHash,
		challenge.ExpiresAt.UTC(), challenge.CreatedAt.UTC())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *postgresStore) GetPasswordRecoveryChallengeByTokenHash(ctx context.Context, tokenHash string) (*PasswordRecoveryChallenge, error) {
	return scanPasswordRecoveryChallenge(s.db.QueryRowContext(ctx, `SELECT id::text, user_uuid::text, email_snapshot, challenge_kind, secret_hash, expires_at, failed_attempts, COALESCE(locked_until, 'epoch'::timestamptz), created_at, COALESCE(consumed_at, 'epoch'::timestamptz), COALESCE(invalidated_at, 'epoch'::timestamptz)
FROM public.password_recovery_challenges WHERE challenge_kind = 'token' AND secret_hash = $1`, tokenHash))
}

func (s *postgresStore) GetLatestPasswordRecoveryCode(ctx context.Context, email string) (*PasswordRecoveryChallenge, error) {
	return scanPasswordRecoveryChallenge(s.db.QueryRowContext(ctx, `SELECT id::text, user_uuid::text, email_snapshot, challenge_kind, secret_hash, expires_at, failed_attempts, COALESCE(locked_until, 'epoch'::timestamptz), created_at, COALESCE(consumed_at, 'epoch'::timestamptz), COALESCE(invalidated_at, 'epoch'::timestamptz)
FROM public.password_recovery_challenges
WHERE challenge_kind = 'code' AND lower(email_snapshot) = lower($1)
  AND consumed_at IS NULL AND invalidated_at IS NULL
ORDER BY created_at DESC LIMIT 1`, strings.TrimSpace(email)))
}

func scanPasswordRecoveryChallenge(row *sql.Row) (*PasswordRecoveryChallenge, error) {
	var challenge PasswordRecoveryChallenge
	err := row.Scan(&challenge.ID, &challenge.UserID, &challenge.Email, &challenge.Kind,
		&challenge.SecretHash, &challenge.ExpiresAt, &challenge.FailedAttempts,
		&challenge.LockedUntil, &challenge.CreatedAt, &challenge.ConsumedAt, &challenge.InvalidatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPasswordRecoveryInvalid
	}
	if err != nil {
		return nil, err
	}
	challenge.ExpiresAt = challenge.ExpiresAt.UTC()
	challenge.CreatedAt = challenge.CreatedAt.UTC()
	return &challenge, nil
}

func (s *postgresStore) RecordPasswordRecoveryFailure(ctx context.Context, challengeID string, now time.Time, maxAttempts int, lockout time.Duration) (time.Time, error) {
	var lockedUntil sql.NullTime
	err := s.db.QueryRowContext(ctx, `UPDATE public.password_recovery_challenges
SET failed_attempts = failed_attempts + 1,
    locked_until = CASE WHEN failed_attempts + 1 >= $3 THEN $2 + ($4 * interval '1 second') ELSE locked_until END
WHERE id = $1 AND consumed_at IS NULL AND invalidated_at IS NULL AND expires_at > $2
RETURNING locked_until`, challengeID, now.UTC(), maxAttempts, lockout.Seconds()).Scan(&lockedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrPasswordRecoveryInvalid
	}
	if err != nil {
		return time.Time{}, err
	}
	if !lockedUntil.Valid {
		return time.Time{}, nil
	}
	return lockedUntil.Time.UTC(), nil
}

func (s *postgresStore) InvalidatePasswordRecoveryChallenge(ctx context.Context, challengeID string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE public.password_recovery_challenges
SET invalidated_at = $2 WHERE id = $1 AND consumed_at IS NULL AND invalidated_at IS NULL`, challengeID, now.UTC())
	return err
}

func (s *postgresStore) CompletePasswordRecovery(ctx context.Context, challengeID, passwordHash string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var userID string
	err = tx.QueryRowContext(ctx, `UPDATE public.password_recovery_challenges
SET consumed_at = $2
WHERE id = $1 AND consumed_at IS NULL AND invalidated_at IS NULL
  AND expires_at > $2 AND (locked_until IS NULL OR locked_until <= $2)
RETURNING user_uuid::text`, challengeID, now.UTC()).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrPasswordRecoveryInvalid
	}
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE public.users SET password = $2 WHERE uuid = $1`, userID, passwordHash)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return ErrPasswordRecoveryInvalid
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM public.sessions WHERE user_uuid = $1`, userID); err != nil {
		return err
	}
	return tx.Commit()
}
