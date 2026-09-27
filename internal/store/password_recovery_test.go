package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPasswordRecoveryChallengeIsOneTimeAndRevokesOnlyTargetSessions(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	target := &User{Name: "Recovery target", Email: "target@example.invalid", EmailVerified: true, Active: true, Groups: []string{"keep-me"}, PasswordHash: "old-hash"}
	other := &User{Name: "Other account", Email: "other@example.invalid", Active: true, PasswordHash: "other-hash"}
	if err := st.CreateUser(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateUser(ctx, other); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for token, userID := range map[string]string{"target-a": target.ID, "target-b": target.ID, "other": other.ID} {
		if err := st.CreateSession(ctx, token, userID, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}

	challenge := &PasswordRecoveryChallenge{
		ID: "challenge-1", UserID: target.ID, Email: target.Email, Kind: "token",
		SecretHash: "sha256-secret-digest", ExpiresAt: now.Add(time.Minute), CreatedAt: now,
	}
	if err := st.CreatePasswordRecoveryChallenge(ctx, challenge); err != nil {
		t.Fatal(err)
	}
	if err := st.CompletePasswordRecovery(ctx, challenge.ID, "new-password-hash", now); err != nil {
		t.Fatalf("complete recovery: %v", err)
	}
	if err := st.CompletePasswordRecovery(ctx, challenge.ID, "replay-hash", now); !errors.Is(err, ErrPasswordRecoveryInvalid) {
		t.Fatalf("expected consumed challenge rejection, got %v", err)
	}
	for _, token := range []string{"target-a", "target-b"} {
		if _, _, err := st.GetSession(ctx, token); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("target session %q was not revoked: %v", token, err)
		}
	}
	if _, _, err := st.GetSession(ctx, "other"); err != nil {
		t.Fatalf("unrelated user session was revoked: %v", err)
	}
	reloaded, err := st.GetUserByID(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.PasswordHash != "new-password-hash" || reloaded.Email != target.Email || !reloaded.EmailVerified || !reloaded.Active || len(reloaded.Groups) != 1 || reloaded.Groups[0] != "keep-me" {
		t.Fatalf("recovery changed unrelated user attributes: %#v", reloaded)
	}
}

func TestPasswordRecoveryMigrationIsAdditiveAndHasUpgradeFixture(t *testing.T) {
	migrationPath := filepath.Join("..", "..", "sql", "migrations", "2026092702_password_recovery_challenges.up.sql")
	migration, err := os.ReadFile(migrationPath)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sqlText := strings.ToUpper(string(migration))
	for _, forbidden := range []string{"DROP TABLE", "DROP SCHEMA", "TRUNCATE", "DELETE FROM", "UPDATE PUBLIC.USERS", "UPDATE PUBLIC.SUBSCRIPTIONS", "UPDATE PUBLIC.BILLING_LEDGER", "UPDATE PUBLIC.TRAFFIC_MINUTE_BUCKETS", "UPDATE PUBLIC.STRIPE_WEBHOOK_EVENTS"} {
		if strings.Contains(sqlText, forbidden) {
			t.Fatalf("additive migration contains forbidden operation %q", forbidden)
		}
	}
	for _, required := range []string{"CREATE TABLE IF NOT EXISTS PUBLIC.PASSWORD_RECOVERY_CHALLENGES", "CREATE UNIQUE INDEX IF NOT EXISTS", "CREATE INDEX IF NOT EXISTS", "ON DELETE RESTRICT", "ENABLE ROW LEVEL SECURITY", "BEGIN;", "COMMIT;"} {
		if !strings.Contains(sqlText, required) {
			t.Fatalf("migration is missing required upgrade-safe clause %q", required)
		}
	}

	fixturePath := filepath.Join("..", "..", "sql", "migrations", "tests", "password_recovery_upgrade_test.sql")
	fixture, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read upgrade fixture: %v", err)
	}
	fixtureText := strings.ToLower(string(fixture))
	for _, required := range []string{"\\ir ../2026092702_password_recovery_challenges.up.sql", "p0_2_user_snapshot", "p0_2_session_snapshot", "p0_2_subscription_snapshot", "p0_2_billing_snapshot", "p0_2_usage_snapshot", "p0_2_payment_snapshot", "multiple active challenges", "invalid challenge kind"} {
		if !strings.Contains(fixtureText, required) {
			t.Fatalf("migration upgrade fixture is missing assertion %q", required)
		}
	}
}

func TestPasswordRecoveryExpiryLockoutAndResendAreDurableStoreState(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	user := &User{Name: "Code user", Email: "code@example.invalid", Active: true}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first := &PasswordRecoveryChallenge{ID: "code-old", UserID: user.ID, Email: user.Email, Kind: "code", SecretHash: "bcrypt-digest-old", ExpiresAt: now.Add(time.Hour), CreatedAt: now}
	if err := st.CreatePasswordRecoveryChallenge(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := &PasswordRecoveryChallenge{ID: "code-new", UserID: user.ID, Email: user.Email, Kind: "code", SecretHash: "bcrypt-digest-new", ExpiresAt: now.Add(time.Hour), CreatedAt: now.Add(time.Second)}
	if err := st.CreatePasswordRecoveryChallenge(ctx, second); err != nil {
		t.Fatal(err)
	}
	latest, err := st.GetLatestPasswordRecoveryCode(ctx, user.Email)
	if err != nil || latest.ID != second.ID || latest.SecretHash != second.SecretHash {
		t.Fatalf("latest code challenge = %#v, err=%v", latest, err)
	}
	if _, err := st.RecordPasswordRecoveryFailure(ctx, first.ID, now, 5, time.Minute); !errors.Is(err, ErrPasswordRecoveryInvalid) {
		t.Fatalf("superseded challenge should not accept verification attempts: %v", err)
	}
	lockedUntil := time.Time{}
	for i := 0; i < 5; i++ {
		lockedUntil, err = st.RecordPasswordRecoveryFailure(ctx, second.ID, now, 5, 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !lockedUntil.After(now) {
		t.Fatalf("fifth failed attempt did not persist lockout: %v", lockedUntil)
	}
	if err := st.CompletePasswordRecovery(ctx, second.ID, "hash", now.Add(time.Minute)); !errors.Is(err, ErrPasswordRecoveryInvalid) {
		t.Fatalf("locked challenge should not complete, got %v", err)
	}
	if err := st.CompletePasswordRecovery(ctx, second.ID, "hash", now.Add(6*time.Minute)); err != nil {
		t.Fatalf("challenge should become usable after lockout while unexpired: %v", err)
	}

	expired := &PasswordRecoveryChallenge{ID: "expired", UserID: user.ID, Email: user.Email, Kind: "token", SecretHash: "sha256-expired", ExpiresAt: now.Add(time.Second), CreatedAt: now}
	if err := st.CreatePasswordRecoveryChallenge(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if err := st.CompletePasswordRecovery(ctx, expired.ID, "hash", now.Add(2*time.Second)); !errors.Is(err, ErrPasswordRecoveryInvalid) {
		t.Fatalf("expired challenge should not complete, got %v", err)
	}
}
