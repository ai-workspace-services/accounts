package store

import (
	"context"
	"database/sql"
	"github.com/google/uuid"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMFAResetStoreConformance(t *testing.T) {
	t.Run("memory", func(t *testing.T) { testMFAResetStore(t, NewMemoryStore()) })
	t.Run("postgres17", func(t *testing.T) {
		dsn := os.Getenv("MFA_RESET_TEST_DSN")
		if dsn == "" {
			t.Skip("isolated PostgreSQL 17 fixture not configured")
		}
		u, err := url.Parse(dsn)
		if err != nil || u.Hostname() != "127.0.0.1" || u.Path != "/accounts_native_render" {
			t.Fatal("requires isolated local accounts_native_render")
		}
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var version string
		if err = db.QueryRow("SHOW server_version").Scan(&version); err != nil || !strings.HasPrefix(version, "17.") {
			t.Fatal("requires PostgreSQL 17")
		}
		testMFAResetStore(t, &postgresStore{db: db})
		testMFAResetAuditRollback(t, db)
	})
}

func testMFAResetAuditRollback(t *testing.T, db *sql.DB) {
	ctx := context.Background()
	st := &postgresStore{db: db}
	now := time.Now().UTC()
	suffix := uuid.NewString()
	u := &User{Name: "rollback-" + suffix, Email: suffix + "@example.invalid", Active: true, EmailVerified: true, MFAEnabled: true, MFATOTPSecret: "factor"}
	root := &User{Name: "reviewer-" + suffix, Email: "reviewer-" + suffix + "@example.invalid", Active: true, EmailVerified: true, Role: RoleRoot, MFAEnabled: true, MFATOTPSecret: "root-factor"}
	for _, user := range []*User{u, root} {
		if err := st.CreateUser(ctx, user); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	r := &MFAResetRequest{ID: uuid.NewString(), UserID: u.ID, Email: u.Email, CodeHash: "hash", Fingerprint: MFAFingerprint("factor"), Status: "pending_email", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := st.CreateMFAResetRequest(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := st.ConfirmMFAResetRequest(ctx, r.ID, r.CodeHash, "Lost device", now); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, suffix, u.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, err := db.Exec(`CREATE FUNCTION public.mfa_reset_test_fail_audit() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.action='identity.mfa_recovery.approved' THEN RAISE EXCEPTION 'simulated audit failure'; END IF; RETURN NEW; END;$$; CREATE TRIGGER mfa_reset_test_fail_audit BEFORE INSERT ON public.audit_logs FOR EACH ROW EXECUTE FUNCTION public.mfa_reset_test_fail_audit()`)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Exec(`DROP TRIGGER IF EXISTS mfa_reset_test_fail_audit ON public.audit_logs; DROP FUNCTION IF EXISTS public.mfa_reset_test_fail_audit()`)
	if err = st.DecideMFAResetRequest(ctx, r.ID, root.ID, "approved", "identity verified", now); err == nil {
		t.Fatal("audit failure ignored")
	}
	after, _ := st.GetUserByID(ctx, u.ID)
	if !after.MFAEnabled {
		t.Fatal("MFA disable survived transaction rollback")
	}
	if _, _, err = st.GetSession(ctx, suffix); err != nil {
		t.Fatal("session revocation survived transaction rollback")
	}
	request, _ := st.GetMFAResetRequestByEmail(ctx, u.Email)
	if request.Status != "pending_review" {
		t.Fatal("request was consumed despite rollback")
	}
}

func testMFAResetStore(t *testing.T, st Store) {
	ctx := context.Background()
	now := time.Now().UTC()
	suffix := uuid.NewString()
	u := &User{Name: "recovery-" + suffix, Email: suffix + "@example.invalid", Active: true, EmailVerified: true, MFAEnabled: true, MFATOTPSecret: "old-factor"}
	root := &User{Name: "root-" + suffix, Email: "root-" + suffix + "@example.invalid", Role: RoleRoot, Active: true, EmailVerified: true, MFAEnabled: true, MFATOTPSecret: "root-factor"}
	for _, v := range []*User{u, root} {
		if err := st.CreateUser(ctx, v); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateUser(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	req := &MFAResetRequest{ID: uuid.NewString(), UserID: u.ID, Email: u.Email, CodeHash: "hashed-code", Fingerprint: MFAFingerprint(u.MFATOTPSecret), Status: "pending_email", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := st.CreateMFAResetRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := st.DecideMFAResetRequest(ctx, req.ID, root.ID, "approved", "identity checked", now); err == nil {
		t.Fatal("unconfirmed user authorization accepted")
	}
	if err := st.ConfirmMFAResetRequest(ctx, req.ID, req.CodeHash, "Lost device", now); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, "session-"+suffix, u.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var approved atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = st.CreateMFASession(ctx, "racing-login-"+suffix, u.ID, req.Fingerprint, now.Add(time.Hour))
	}()
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if st.DecideMFAResetRequest(ctx, req.ID, root.ID, "approved", "identity verified", now) == nil {
				approved.Add(1)
			}
		}()
	}
	wg.Wait()
	if approved.Load() != 1 {
		t.Fatalf("concurrent approval count %d", approved.Load())
	}
	user, err := st.GetUserByID(ctx, u.ID)
	if err != nil || user.MFAEnabled || user.MFATOTPSecret != "" {
		t.Fatalf("factor survived: %v", err)
	}
	if _, _, err := st.GetSession(ctx, "session-"+suffix); err == nil {
		t.Fatal("session survived approval")
	}
	if _, _, err := st.GetSession(ctx, "racing-login-"+suffix); err == nil {
		t.Fatal("in-flight login survived MFA recovery")
	}
	if err := st.CreateMFASession(ctx, "late-login-"+suffix, u.ID, req.Fingerprint, now.Add(time.Hour)); err == nil {
		t.Fatal("revoked MFA created a new session")
	}
	logs, err := st.ListAuditLogs(ctx, AuditLogFilter{ActionPrefix: "identity.mfa_recovery.", TargetUUID: u.ID})
	if err != nil || len(logs) != 1 {
		t.Fatalf("audit count %d err %v", len(logs), err)
	}

	// A reviewed authorization cannot remove a newly enrolled factor.
	user.MFAEnabled = true
	user.MFATOTPSecret = "new-factor"
	if err := st.UpdateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	stale := *req
	stale.ID = uuid.NewString()
	stale.CreatedAt = now.Add(2 * time.Minute)
	stale.ExpiresAt = now.Add(time.Hour)
	stale.Fingerprint = MFAFingerprint("new-factor")
	if err := st.CreateMFAResetRequest(ctx, &stale); err != nil {
		t.Fatal(err)
	}
	if err := st.ConfirmMFAResetRequest(ctx, stale.ID, stale.CodeHash, "Lost again", stale.CreatedAt); err != nil {
		t.Fatal(err)
	}
	user.MFATOTPSecret = "another-factor"
	if err := st.UpdateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	if err := st.DecideMFAResetRequest(ctx, stale.ID, root.ID, "approved", "identity checked", stale.CreatedAt); err == nil {
		t.Fatal("stale request removed a new factor")
	}
}
