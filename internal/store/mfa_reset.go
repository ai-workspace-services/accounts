package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

var ErrMFAResetConflict = errors.New("MFA recovery request is invalid, stale or already decided")

type MFAResetRequest struct {
	ID             string    `json:"id"`
	UserID         string    `json:"userId"`
	Email          string    `json:"email"`
	Fingerprint    string    `json:"-"`
	CodeHash       string    `json:"-"`
	Status         string    `json:"status"`
	Reason         string    `json:"reason"`
	ReviewReason   string    `json:"reviewReason"`
	FailedAttempts int       `json:"-"`
	CreatedAt      time.Time `json:"createdAt"`
	ExpiresAt      time.Time `json:"expiresAt"`
}

func MFAFingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func validMFAResetUser(u *User, r *MFAResetRequest) bool {
	return u != nil && u.Active && u.EmailVerified && u.MFAEnabled && strings.TrimSpace(u.MFATOTPSecret) != "" &&
		strings.EqualFold(u.Email, r.Email) && MFAFingerprint(u.MFATOTPSecret) == r.Fingerprint
}

// The user lock also serializes in-flight MFA logins against recovery approval.
// A factor validated before recovery must not mint a new session afterwards.
func (s *memoryStore) CreateMFASession(ctx context.Context, token, userID, fingerprint string, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.byID[userID]
	if u == nil || !u.Active || !u.MFAEnabled || MFAFingerprint(u.MFATOTPSecret) != fingerprint {
		return ErrMFAResetConflict
	}
	s.sessions[token] = &sessionRecord{UserID: userID, ExpiresAt: expiresAt}
	return nil
}
func (s *postgresStore) CreateMFASession(ctx context.Context, token, userID, fingerprint string, expiresAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var secret string
	var active, enabled bool
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(mfa_totp_secret,''),active,mfa_enabled FROM public.users WHERE uuid=$1 FOR UPDATE`, userID).Scan(&secret, &active, &enabled); err != nil {
		return err
	}
	if !active || !enabled || MFAFingerprint(secret) != fingerprint {
		return ErrMFAResetConflict
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO public.sessions(token,user_uuid,expires_at) VALUES($1,$2,$3)`, token, userID, expiresAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *memoryStore) CreateMFAResetRequest(ctx context.Context, r *MFAResetRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validMFAResetUser(s.byID[r.UserID], r) {
		return ErrMFAResetConflict
	}
	for _, old := range s.mfaResetRequests {
		if old.UserID != r.UserID {
			continue
		}
		if old.Status == "pending_review" && old.ExpiresAt.After(r.CreatedAt) {
			return ErrMFAResetConflict
		}
		if r.CreatedAt.Sub(old.CreatedAt) < time.Minute {
			return ErrMFAResetConflict
		}
		if old.Status == "pending_email" || old.Status == "pending_review" {
			old.Status = "expired"
		}
	}
	copy := *r
	s.mfaResetRequests[r.ID] = &copy
	return nil
}

func (s *memoryStore) GetMFAResetRequestByEmail(ctx context.Context, email string) (*MFAResetRequest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var latest *MFAResetRequest
	for _, r := range s.mfaResetRequests {
		if strings.EqualFold(r.Email, email) && (latest == nil || r.CreatedAt.After(latest.CreatedAt)) {
			copy := *r
			latest = &copy
		}
	}
	if latest == nil {
		return nil, ErrMFAResetConflict
	}
	return latest, nil
}

func (s *memoryStore) GetMFAResetRequestByID(ctx context.Context, id string) (*MFAResetRequest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := s.mfaResetRequests[id]
	if r == nil {
		return nil, ErrMFAResetConflict
	}
	copy := *r
	return &copy, nil
}

func (s *memoryStore) ConfirmMFAResetRequest(ctx context.Context, id, hash, reason string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.mfaResetRequests[id]
	if r == nil || r.Status != "pending_email" || r.CodeHash != hash || r.FailedAttempts >= 5 || !r.ExpiresAt.After(now) || !validMFAResetUser(s.byID[r.UserID], r) {
		return ErrMFAResetConflict
	}
	r.Status = "pending_review"
	r.Reason = reason
	r.ExpiresAt = now.Add(7 * 24 * time.Hour)
	r.CodeHash = ""
	return nil
}

func (s *memoryStore) FailMFAResetCode(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.mfaResetRequests[id]; r != nil && r.Status == "pending_email" {
		r.FailedAttempts++
	}
	return nil
}

func (s *memoryStore) ListMFAResetRequests(ctx context.Context) ([]MFAResetRequest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []MFAResetRequest{}
	for _, r := range s.mfaResetRequests {
		if r.Status != "pending_email" {
			out = append(out, *r)
		}
	}
	// Match the bounded production queue.
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Status == "pending_review") != (out[j].Status == "pending_review") {
			return out[i].Status == "pending_review"
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if len(out) > 100 {
		out = out[:100]
	}
	return out, nil
}

func (s *memoryStore) DecideMFAResetRequest(ctx context.Context, id, actorID, decision, reason string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.mfaResetRequests[id]
	actor := s.byID[actorID]
	if actor == nil || !actor.Active || !IsRootRole(actor.Role) || !actor.MFAEnabled || r == nil || r.UserID == actorID || r.Status != "pending_review" || !r.ExpiresAt.After(now) || (decision != "approved" && decision != "rejected") || strings.TrimSpace(reason) == "" {
		return ErrMFAResetConflict
	}
	u := s.byID[r.UserID]
	if !validMFAResetUser(u, r) {
		return ErrMFAResetConflict
	}
	if decision == "approved" {
		u.MFAEnabled = false
		u.MFATOTPSecret = ""
		u.MFAConfirmedAt = time.Time{}
		u.MFASecretIssuedAt = time.Time{}
		for token, sess := range s.sessions {
			if sess.UserID == u.ID {
				delete(s.sessions, token)
			}
		}
		for _, code := range s.mfaRecoveryCodes {
			if code.UserID == u.ID && code.ConsumedAt.IsZero() {
				code.RevokedAt = now
			}
		}
	}
	r.Status = decision
	r.ReviewReason = reason
	// Audit is committed under the same lock as the account change.
	s.auditLogs = append(s.auditLogs, &AuditLog{UUID: uuid.NewString(), ActorUUID: actorID, Action: "identity.mfa_recovery." + decision, Details: map[string]any{"target_uuid": r.UserID, "requestId": id, "reason": reason}, CreatedAt: now})
	return nil
}

const mfaResetColumns = `id::text, user_uuid::text, email_snapshot, mfa_fingerprint, code_hash, status, reason, review_reason, failed_attempts, created_at, expires_at`

type mfaResetScanner interface{ Scan(...any) error }

func scanMFAReset(row mfaResetScanner) (*MFAResetRequest, error) {
	r := &MFAResetRequest{}
	err := row.Scan(&r.ID, &r.UserID, &r.Email, &r.Fingerprint, &r.CodeHash, &r.Status, &r.Reason, &r.ReviewReason, &r.FailedAttempts, &r.CreatedAt, &r.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMFAResetConflict
	}
	return r, err
}

func (s *postgresStore) CreateMFAResetRequest(ctx context.Context, r *MFAResetRequest) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// User lock serializes requests, confirmation and approval across instances.
	var secret, email string
	var active, verified, enabled bool
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(mfa_totp_secret,''),email,active,email_verified,mfa_enabled FROM public.users WHERE uuid=$1 FOR UPDATE`, r.UserID).Scan(&secret, &email, &active, &verified, &enabled)
	if err != nil {
		return err
	}
	if !validMFAResetUser(&User{MFATOTPSecret: secret, Email: email, Active: active, EmailVerified: verified, MFAEnabled: enabled}, r) {
		return ErrMFAResetConflict
	}
	var blocked bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.mfa_reset_requests WHERE user_uuid=$1 AND (created_at > $2::timestamptz - interval '1 minute' OR (status='pending_review' AND expires_at>$2::timestamptz)))`, r.UserID, r.CreatedAt).Scan(&blocked)
	if err != nil {
		return err
	}
	if blocked {
		return ErrMFAResetConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE public.mfa_reset_requests SET status='expired' WHERE user_uuid=$1 AND status IN ('pending_email','pending_review')`, r.UserID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO public.mfa_reset_requests(id,user_uuid,email_snapshot,mfa_fingerprint,code_hash,status,created_at,expires_at) VALUES($1,$2,$3,$4,$5,'pending_email',$6,$7)`, r.ID, r.UserID, r.Email, r.Fingerprint, r.CodeHash, r.CreatedAt, r.ExpiresAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *postgresStore) GetMFAResetRequestByEmail(ctx context.Context, email string) (*MFAResetRequest, error) {
	return scanMFAReset(s.db.QueryRowContext(ctx, `SELECT `+mfaResetColumns+` FROM public.mfa_reset_requests WHERE lower(email_snapshot)=lower($1) ORDER BY created_at DESC LIMIT 1`, email))
}

func (s *postgresStore) GetMFAResetRequestByID(ctx context.Context, id string) (*MFAResetRequest, error) {
	return scanMFAReset(s.db.QueryRowContext(ctx, `SELECT `+mfaResetColumns+` FROM public.mfa_reset_requests WHERE id=$1`, id))
}

func (s *postgresStore) ConfirmMFAResetRequest(ctx context.Context, id, hash, reason string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE public.mfa_reset_requests r SET status='pending_review', reason=$3, authorized_at=$4::timestamptz, expires_at=$4::timestamptz+interval '7 days',code_hash='' WHERE r.id=$1 AND r.code_hash=$2 AND r.status='pending_email' AND r.failed_attempts<5 AND r.expires_at>$4::timestamptz AND EXISTS (SELECT 1 FROM public.users u WHERE u.uuid=r.user_uuid AND u.active AND u.email_verified AND u.mfa_enabled AND lower(u.email)=lower(r.email_snapshot))`, id, hash, reason, now)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return ErrMFAResetConflict
	}
	return nil
}
func (s *postgresStore) FailMFAResetCode(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE public.mfa_reset_requests SET failed_attempts=failed_attempts+1 WHERE id=$1 AND status='pending_email'`, id)
	return err
}
func (s *postgresStore) ListMFAResetRequests(ctx context.Context) ([]MFAResetRequest, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+mfaResetColumns+` FROM public.mfa_reset_requests WHERE status<>'pending_email' ORDER BY (status='pending_review') DESC, created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MFAResetRequest{}
	for rows.Next() {
		r, err := scanMFAReset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}
func (s *postgresStore) DecideMFAResetRequest(ctx context.Context, id, actorID, decision, reason string, now time.Time) error {
	if (decision != "approved" && decision != "rejected") || strings.TrimSpace(reason) == "" {
		return ErrMFAResetConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Lock the user first, then the request, matching request creation.
	var userID string
	if err = tx.QueryRowContext(ctx, `SELECT user_uuid::text FROM public.mfa_reset_requests WHERE id=$1`, id).Scan(&userID); err != nil {
		return err
	}
	if userID == actorID {
		return ErrMFAResetConflict
	}
	var u User
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(mfa_totp_secret,''),email,active,email_verified,mfa_enabled FROM public.users WHERE uuid=$1 FOR UPDATE`, userID).Scan(&u.MFATOTPSecret, &u.Email, &u.Active, &u.EmailVerified, &u.MFAEnabled); err != nil {
		return err
	}
	r, err := scanMFAReset(tx.QueryRowContext(ctx, `SELECT `+mfaResetColumns+` FROM public.mfa_reset_requests WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return err
	}
	if r.Status != "pending_review" || !r.ExpiresAt.After(now) || !validMFAResetUser(&u, r) {
		return ErrMFAResetConflict
	}
	var role string
	var active, enabled bool
	if err = tx.QueryRowContext(ctx, `SELECT role,active,mfa_enabled FROM public.users WHERE uuid=$1 FOR SHARE`, actorID).Scan(&role, &active, &enabled); err != nil {
		return err
	}
	if !IsRootRole(role) || !active || !enabled {
		return ErrMFAResetConflict
	}
	if decision == "approved" {
		if _, err = tx.ExecContext(ctx, `UPDATE public.users SET mfa_totp_secret='',mfa_enabled=false,mfa_secret_issued_at=NULL,mfa_confirmed_at=NULL WHERE uuid=$1`, userID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM public.sessions WHERE user_uuid=$1`, userID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE public.mfa_recovery_codes SET revoked_at=$2 WHERE user_uuid=$1 AND consumed_at IS NULL AND revoked_at IS NULL`, userID, now); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE public.mfa_reset_requests SET status=$2,review_reason=$3,reviewer_uuid=$4,reviewed_at=$5 WHERE id=$1`, id, decision, reason, actorID, now); err != nil {
		return err
	}
	details, _ := json.Marshal(map[string]any{"target_uuid": userID, "requestId": id, "reason": reason})
	if _, err = tx.ExecContext(ctx, `INSERT INTO public.audit_logs(uuid,action,actor_uuid,details) VALUES($1,$2,$3,$4)`, uuid.NewString(), "identity.mfa_recovery."+decision, actorID, details); err != nil {
		return err
	}
	return tx.Commit()
}
