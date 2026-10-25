package migrate

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	schema "account/sql"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pquerna/otp/totp"
)

// This suite creates NEW randomly named databases on an explicitly supplied
// loopback PostgreSQL17 fixture. No application DSN, existing DB or port is used.
func TestMigrationAPIPostgres17(t *testing.T) {
	dsn := os.Getenv("MIGRATECTL_API_TEST_DSN")
	if dsn == "" {
		if os.Getenv("MIGRATECTL_API_TEST_REQUIRED") == "true" {
			t.Fatal("MIGRATECTL_API_TEST_DSN is required for migration API PostgreSQL17 coverage")
		}
		t.Skip("dedicated loopback PostgreSQL17 test cluster not configured")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Database != "postgres" || (cfg.Host != "127.0.0.1" && cfg.Host != "localhost") {
		t.Fatal("test requires explicit loopback postgres fixture")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	var version int
	if admin.QueryRow(`SELECT current_setting('server_version_num')::int`).Scan(&version) != nil || version < 170000 || version >= 180000 {
		t.Fatal("PostgreSQL17 required")
	}
	ctx := context.Background()
	binding := MigrationAuthorization{SourceAudience: "source-fixture", TargetAudience: "target-fixture", JobID: uuid.NewString(), SubjectSHA256: migrationDigest("operator")}
	execSQL := func(db *sql.DB, query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("isolated fixture SQL: %v", err)
		}
	}
	execSQL(admin, `DO $$BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='readonly_release') THEN CREATE ROLE readonly_release LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS; END IF; END$$`)
	// Fixture roles need passwords on the CI service's SCRAM-authenticated TCP
	// listener. Do not rely on the local developer cluster's trust configuration.
	const fixturePassword = "isolated-readonly-fixture"
	execSQL(admin, `ALTER ROLE readonly_release PASSWORD 'isolated-readonly-fixture'`)
	body, _, err := schema.NativeArtifact()
	if err != nil {
		t.Fatal(err)
	}
	newDB := func() (*sql.DB, string) {
		t.Helper()
		name := "migration_api_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		execSQL(admin, `CREATE DATABASE `+quoteBusiness(name))
		testConfig := cfg.Copy()
		testConfig.Database = name
		db := stdlib.OpenDB(*testConfig)
		t.Cleanup(func() {
			db.Close()
			if _, e := admin.Exec(`DROP DATABASE ` + quoteBusiness(name) + ` WITH (FORCE)`); e != nil {
				t.Errorf("test database cleanup: %v", e)
			}
		})
		execSQL(db, string(body))
		execSQL(db, `CREATE TABLE public.schema_migrations(version bigint PRIMARY KEY,dirty boolean NOT NULL); INSERT INTO public.schema_migrations VALUES(`+strconv.Itoa(FullBusinessVersion)+`,false)`)
		return db, name
	}
	source, sourceName := newDB()
	target, _ := newDB()
	const userA = "00000000-0000-0000-0000-000000000101"
	const userB = "00000000-0000-0000-0000-000000000102"
	const proxyA = "00000000-0000-0000-0000-000000000201"
	const proxyB = "00000000-0000-0000-0000-000000000202"
	const sub = "00000000-0000-0000-0000-000000000301"
	const invoice = "00000000-0000-0000-0000-000000000401"
	const payment = "00000000-0000-0000-0000-000000000501"
	const operation = "00000000-0000-0000-0000-000000000601"
	const referral = "00000000-0000-0000-0000-000000000701"
	const reward = "00000000-0000-0000-0000-000000000801"
	const settlement = "00000000-0000-0000-0000-000000000901"
	execSQL(source, `INSERT INTO public.users(uuid,username,password,email,proxy_uuid,mfa_totp_secret,mfa_enabled,email_verified_at) VALUES
 ('`+userA+`','alice','fixture-hash-A','alice@example.invalid','`+proxyA+`','JBSWY3DPEHPK3PXP',true,'2026-01-01Z'),
 ('`+userB+`','bob','fixture-hash-B','bob@example.invalid','`+proxyB+`',NULL,false,NULL);
 INSERT INTO public.identities(provider,external_id,user_uuid) VALUES('github','fixture-oauth','`+userA+`');
 INSERT INTO public.mfa_recovery_codes(id,user_uuid,batch_uuid,code_hash,expires_at) VALUES(gen_random_uuid(),'`+userA+`',gen_random_uuid(),'fixture-recovery-hash','2030-01-01Z');
 INSERT INTO public.account_lifecycle_events(user_uuid,to_state,actor_type) VALUES('`+userA+`','active','system');
 INSERT INTO public.billing_plans(plan_id,display_name) VALUES('fixture-plan','fixture');
 INSERT INTO public.subscriptions(uuid,user_uuid,provider,external_id,plan_id) VALUES('`+sub+`','`+userA+`','local','fixture-sub','fixture-plan');
 INSERT INTO public.subscription_records(user_uuid,monthly_quota_bytes) VALUES('`+userA+`',9007199254740993);
 INSERT INTO public.account_billing_profiles(account_uuid) VALUES('`+userA+`');
 INSERT INTO public.account_quota_states(account_uuid,remaining_included_quota,current_balance) VALUES('`+userA+`',9007199254740993,12.75);
 INSERT INTO public.account_policy_snapshots(account_uuid,policy_version,expires_at) VALUES('`+userA+`','fixture','2030-01-01Z');
 INSERT INTO public.billing_ledger(id,account_uuid,bucket_start,bucket_end,entry_type,rated_bytes,amount_delta,balance_after)
 SELECT md5('api-ledger-'||g)::uuid,'`+userA+`','2026-01-01Z'::timestamptz+g*interval '1 minute','2026-01-01Z'::timestamptz+(g+1)*interval '1 minute','usage',9007199254740993,0.125,12.75 FROM generate_series(1,1003)g;
 INSERT INTO public.billing_source_sync_state(source_id) VALUES('fixture-source');
 INSERT INTO public.traffic_minute_buckets(account_uuid,node_id,bucket_start,total_bytes) VALUES('`+userA+`','fixture-node','2026-01-01Z',9007199254740993);
 INSERT INTO public.traffic_stat_checkpoints(account_uuid,node_id,last_seen_at) VALUES('`+userA+`','fixture-node','2026-01-01Z');
 INSERT INTO public.stripe_webhook_events(event_id,event_type,payload,status) VALUES('fixture-event','test','{"n":9007199254740993,"d":1.20}','processed');
 INSERT INTO public.finance_invoices(id,idempotency_key,account_uuid,subscription_uuid,amount_minor,currency) VALUES('`+invoice+`','fixture-invoice','`+userA+`','`+sub+`',1000,'CNY');
 INSERT INTO public.finance_payments(id,idempotency_key,invoice_id,account_uuid,amount_minor,currency) VALUES('`+payment+`','fixture-payment','`+invoice+`','`+userA+`',1000,'CNY');
 INSERT INTO public.finance_refunds(idempotency_key,payment_id,amount_minor,currency) VALUES('fixture-refund','`+payment+`',100,'CNY');
 INSERT INTO public.finance_operations(id,idempotency_key,operation_type,target_type,target_id,status) VALUES('`+operation+`','fixture-operation','refund','payment','`+payment+`','succeeded');
 INSERT INTO public.finance_operation_events(id,operation_id,attempt,event_type,status) OVERRIDING SYSTEM VALUE VALUES(99,'`+operation+`',1,'refund','succeeded');
 INSERT INTO public.subscription_lifecycle_states(account_uuid,operation_id) VALUES('`+userA+`','`+operation+`');
 INSERT INTO public.network_teams(id,name,member_limit,valid_from,valid_until) VALUES('fixture-team','fixture',5,'2026-01-01Z','2030-01-01Z');
 INSERT INTO public.network_team_members(user_uuid,team_id) VALUES('`+userA+`','fixture-team');
 INSERT INTO public.team_agent_enrollments(agent_id,assigned_team_id,status) VALUES('fixture-agent','fixture-team','approved');
 INSERT INTO public.audit_logs(action,actor_uuid,details) VALUES('fixture-lifecycle','`+userA+`','{"n":9007199254740993}');
 INSERT INTO public.tenants(id,name,edition,created_at,updated_at) VALUES('fixture-tenant','fixture','team','2026-01-01Z','2026-01-01Z');
 INSERT INTO public.tenant_domains(id,tenant_id,domain,kind,status,created_at,updated_at) VALUES('fixture-domain','fixture-tenant','tenant.example.invalid','custom','active','2026-01-01Z','2026-01-01Z');
 INSERT INTO public.tenant_memberships(tenant_id,user_id,role,created_at,updated_at) VALUES('fixture-tenant','`+userA+`','owner','2026-01-01Z','2026-01-01Z');
 INSERT INTO public.rbac_roles(role_key) VALUES('fixture-role');
 INSERT INTO public.rbac_permissions(permission_key) VALUES('fixture-permission');
 INSERT INTO public.rbac_role_permissions(role_key,permission_key) VALUES('fixture-role','fixture-permission');
 INSERT INTO public.referral_verified_invoices(invoice_id,provider_price_id,plan_id) VALUES('`+invoice+`','price_api_verified','PRO-MONTHLY');
 INSERT INTO public.referral_campaign_revisions(enabled,effective_at,reason) VALUES(true,'1970-01-01Z','explicit API source fixture');
 INSERT INTO public.referral_codes(user_uuid,code) VALUES('`+userB+`','FIXTURE1');
 INSERT INTO public.referrals(id,referrer_uuid,invitee_uuid,referral_code) VALUES('`+referral+`','`+userB+`','`+userA+`','FIXTURE1');
 INSERT INTO public.referral_rewards(id,referral_id,provider_invoice_id,referrer_uuid,invitee_uuid,plan_id,product_slug,paid_minor,reward_minor,currency,paid_at,eligible_at) VALUES('`+reward+`','`+referral+`','fixture-invoice','`+userB+`','`+userA+`','fixture-plan','xconnect',1000,100,'CNY','2026-01-01Z','2026-02-01Z');
 INSERT INTO public.referral_reward_events(reward_id,source_key,event_type,amount_delta_minor,actor_uuid) VALUES('`+reward+`','fixture-award','award',100,'`+userB+`');
 INSERT INTO public.referral_payment_risks(source_key,provider_invoice_id,risk_type) VALUES('fixture-risk','fixture-invoice','refund');
 INSERT INTO public.referral_settlements(id,referrer_uuid,currency,amount_minor) VALUES('`+settlement+`','`+userB+`','CNY',100);
 INSERT INTO public.referral_settlement_items(settlement_id,reward_id,amount_minor) VALUES('`+settlement+`','`+reward+`',100);
 INSERT INTO public.subscription_extension_ledger(reward_id,user_uuid,subscription_uuid,days,effective_from,effective_until) VALUES('`+reward+`','`+userA+`','`+sub+`',30,'2026-01-01Z','2026-01-31Z');
 INSERT INTO public.sessions(token,expires_at,user_uuid) VALUES('fixture-session-secret','2030-01-01Z','`+userA+`');
 INSERT INTO public.oauth_exchange_codes(code,session_token,session_expires_at,expires_at) VALUES('fixture-code','fixture-token','2030-01-01Z','2030-01-01Z')`)
	execSQL(source, `GRANT USAGE ON SCHEMA public TO readonly_release; GRANT SELECT ON ALL TABLES IN SCHEMA public TO readonly_release; GRANT EXECUTE ON FUNCTION pg_catalog.pg_control_system() TO readonly_release`)
	for _, name := range migrationAPITables {
		var rls bool
		if source.QueryRow(`SELECT relrowsecurity FROM pg_class WHERE oid=to_regclass($1)`, "public."+name).Scan(&rls) != nil {
			t.Fatal("fixture RLS unavailable")
		}
		if rls {
			execSQL(source, `CREATE POLICY release_initialization_readonly ON public.`+quoteBusiness(name)+` FOR SELECT TO readonly_release USING(true)`)
		}
	}
	readonlyConfig := cfg.Copy()
	readonlyConfig.User, readonlyConfig.Password, readonlyConfig.Database = "readonly_release", fixturePassword, sourceName
	readonly := stdlib.OpenDB(*readonlyConfig)
	defer readonly.Close()
	if _, err = CaptureMigrationSnapshot(ctx, source, uuid.NewString(), time.Hour, binding); err == nil {
		t.Fatal("business/superuser source accepted")
	}
	snapshot, err := CaptureMigrationSnapshot(ctx, readonly, uuid.NewString(), time.Hour, binding)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("restrictive-source-visibility", func(t *testing.T) {
		execSQL(source, `CREATE POLICY fixture_hidden_rows ON public.finance_operations AS RESTRICTIVE FOR SELECT TO readonly_release USING(false)`)
		defer execSQL(source, `DROP POLICY fixture_hidden_rows ON public.finance_operations`)
		if _, e := CaptureMigrationSnapshot(ctx, readonly, uuid.NewString(), time.Hour, binding); e == nil {
			t.Fatal("partial source RLS visibility accepted")
		}
	})
	if len(snapshot.Manifest.Tables) != len(migrationAPITables) {
		t.Fatal("scope missing")
	}
	for _, name := range migrationAPITables {
		if snapshot.Manifest.Tables[name].Rows == 0 {
			t.Fatalf("unpopulated dependency fixture %s", name)
		}
	}
	for _, name := range []string{"sessions", "oauth_exchange_codes", "password_recovery_challenges"} {
		if _, ok := snapshot.Rows[name]; ok {
			t.Fatalf("excluded data exported: %s", name)
		}
	}
	if _, err = PreviewMigration(ctx, source, snapshot, uuid.NewString(), time.Hour); err == nil {
		t.Fatal("same physical DB accepted")
	}
	// Keep one unrelated target account; all its facts must survive migration.
	execSQL(target, `INSERT INTO public.users(uuid,username,password,email,proxy_uuid) VALUES('00000000-0000-0000-0000-000000000103','target-only','target-hash','target@example.invalid','00000000-0000-0000-0000-000000000203')`)
	if err = InitializeMigrationReceipts(ctx, target); err != nil {
		t.Fatal(err)
	}
	plan, err := PreviewMigration(ctx, target, snapshot, uuid.NewString(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if plan.State != "preview" || plan.InsertRows < 1003 || plan.ConflictRows != 0 {
		t.Fatalf("bad preview: %+v", plan)
	}
	var count int
	if target.QueryRow(`SELECT count(*) FROM public.users`).Scan(&count) != nil || count != 1 {
		t.Fatal("preview wrote business data")
	}
	t.Run("hash-and-content-tamper", func(t *testing.T) {
		if _, e := ApplyMigration(ctx, target, snapshot, plan, strings.Repeat("0", 64), "operator"); e == nil {
			t.Fatal("wrong plan hash accepted")
		}
		var copySnapshot MigrationSnapshot
		encoded, _ := json.Marshal(snapshot)
		json.Unmarshal(encoded, &copySnapshot)
		copySnapshot.Rows["users"][0]["password"] = json.RawMessage(`"changed"`)
		if _, e := ApplyMigration(ctx, target, copySnapshot, plan, plan.PlanSHA256, "operator"); e == nil {
			t.Fatal("content tamper accepted")
		}
	})
	t.Run("baseline-drift", func(t *testing.T) {
		execSQL(target, `INSERT INTO public.billing_source_sync_state(source_id) VALUES('target-new-activity')`)
		if _, e := ApplyMigration(ctx, target, snapshot, plan, plan.PlanSHA256, "operator"); e == nil {
			t.Fatal("baseline drift accepted")
		}
		execSQL(target, `DELETE FROM public.billing_source_sync_state WHERE source_id='target-new-activity'`)
	})
	t.Run("schema-drift", func(t *testing.T) {
		execSQL(target, `CREATE INDEX fixture_changed_schema ON public.users(username)`)
		if _, e := ApplyMigration(ctx, target, snapshot, plan, plan.PlanSHA256, "operator"); e == nil {
			t.Fatal("schema drift accepted")
		}
		execSQL(target, `DROP INDEX public.fixture_changed_schema`)
	})
	t.Run("external-function-drift", func(t *testing.T) {
		execSQL(target, `CREATE SCHEMA fixture_ops; CREATE FUNCTION fixture_ops.helper() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RETURN NEW; END$$; CREATE TRIGGER fixture_helper BEFORE INSERT ON public.users FOR EACH ROW EXECUTE FUNCTION fixture_ops.helper()`)
		externalPlan, e := PreviewMigration(ctx, target, snapshot, uuid.NewString(), time.Hour)
		if e != nil {
			t.Fatal(e)
		}
		execSQL(target, `CREATE OR REPLACE FUNCTION fixture_ops.helper() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN INSERT INTO public.oauth_exchange_codes(code,session_token,session_expires_at,expires_at) VALUES('side-effect','secret',now(),now()); RETURN NEW; END$$`)
		if _, e = ApplyMigration(ctx, target, snapshot, externalPlan, externalPlan.PlanSHA256, "operator"); e == nil {
			t.Fatal("external-schema function drift accepted")
		}
		execSQL(target, `DROP TRIGGER fixture_helper ON public.users; DROP FUNCTION fixture_ops.helper(); DROP SCHEMA fixture_ops`)
	})
	t.Run("expiry", func(t *testing.T) {
		expired := plan
		expired.ExpiresAt = time.Now().Add(-time.Minute)
		expired.PlanSHA256 = migrationPlanHash(expired)
		if _, e := ApplyMigration(ctx, target, snapshot, expired, expired.PlanSHA256, "operator"); e == nil {
			t.Fatal("expired plan accepted")
		}
	})
	t.Run("atomic-late-failure", func(t *testing.T) {
		execSQL(target, `CREATE FUNCTION public.fixture_late_failure() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'fixture rejection'; END$$; CREATE TRIGGER fixture_late_failure BEFORE INSERT ON public.subscription_extension_ledger FOR EACH ROW EXECUTE FUNCTION public.fixture_late_failure()`)
		failPlan, e := PreviewMigration(ctx, target, snapshot, uuid.NewString(), time.Hour)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = ApplyMigration(ctx, target, snapshot, failPlan, failPlan.PlanSHA256, "operator"); e == nil {
			t.Fatal("late failure accepted")
		}
		if target.QueryRow(`SELECT count(*) FROM public.users`).Scan(&count) != nil || count != 1 {
			t.Fatal("late failure left partial users")
		}
		if target.QueryRow(`SELECT count(*) FROM accounts_migration_api.receipts`).Scan(&count) != nil || count != 0 {
			t.Fatal("late failure left receipt")
		}
		execSQL(target, `DROP TRIGGER fixture_late_failure ON public.subscription_extension_ledger; DROP FUNCTION public.fixture_late_failure()`)
	})
	plan, err = PreviewMigration(ctx, target, snapshot, uuid.NewString(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("concurrent-commit-and-retry", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				done, e := ApplyMigration(ctx, target, snapshot, plan, plan.PlanSHA256, "operator")
				if e == nil && done.State != "applied" {
					e = io.ErrUnexpectedEOF
				}
				errs <- e
			}()
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			if e != nil {
				t.Fatal(e)
			}
		}
		if target.QueryRow(`SELECT count(*) FROM public.users`).Scan(&count) != nil || count != 3 {
			t.Fatal("retry duplicated users")
		}
		if target.QueryRow(`SELECT count(*) FROM accounts_migration_api.receipts`).Scan(&count) != nil || count != 1 {
			t.Fatal("retry duplicated receipt")
		}
	})
	var secret string
	if target.QueryRow(`SELECT mfa_totp_secret FROM public.users WHERE uuid=$1`, userA).Scan(&secret) != nil {
		t.Fatal("MFA missing")
	}
	code, e := totp.GenerateCode(secret, time.Now())
	if e != nil || !totp.Validate(code, "JBSWY3DPEHPK3PXP") {
		t.Fatal("copied TOTP unusable")
	}
	var exact int64
	if target.QueryRow(`SELECT remaining_included_quota FROM public.account_quota_states WHERE account_uuid=$1`, userA).Scan(&exact) != nil || exact != 9007199254740993 {
		t.Fatal("quota integer precision lost")
	}
	for _, name := range []string{"sessions", "oauth_exchange_codes"} {
		if target.QueryRow(`SELECT count(*) FROM public.`+quoteBusiness(name)).Scan(&count) != nil || count != 0 {
			t.Fatal("excluded auth data imported")
		}
	}
	noop, err := PreviewMigration(ctx, target, snapshot, uuid.NewString(), time.Hour)
	if err != nil || noop.InsertRows != 0 || noop.MatchingRows == 0 {
		t.Fatalf("exact-content no-op: %+v %v", noop, err)
	}
	t.Run("queued-committed-retry-after-expiry", func(t *testing.T) {
		queued := noop
		queued.PlanID = uuid.NewString()
		queued.ExpiresAt = time.Now().Add(2 * time.Second)
		queued.PlanSHA256 = migrationPlanHash(queued)
		blocker, e := target.BeginTx(ctx, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer blocker.Rollback()
		if _, e = blocker.Exec(`SELECT pg_advisory_xact_lock(674302318055)`); e != nil {
			t.Fatal(e)
		}
		done := make(chan error, 1)
		go func() {
			_, e := ApplyMigration(ctx, target, snapshot, queued, queued.PlanSHA256, "operator", func(ctx context.Context, p MigrationPlan) error {
				if !time.Now().Before(p.ExpiresAt) {
					return io.ErrUnexpectedEOF
				}
				return nil
			})
			done <- e
		}()
		deadline := time.Now().Add(time.Second)
		waiting := false
		for time.Now().Before(deadline) {
			if target.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted)`).Scan(&waiting) != nil {
				t.Fatal("waiter state unavailable")
			}
			if waiting {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !waiting {
			t.Fatal("retry never waited")
		}
		committed := queued
		committed.State = "applied"
		body, _ := json.Marshal(committed)
		if _, e = blocker.Exec(`INSERT INTO accounts_migration_api.receipts(plan_id,plan_sha256,subject,receipt) VALUES($1,$2,$3,$4::jsonb)`, queued.PlanID, queued.PlanSHA256, "operator", string(body)); e != nil {
			t.Fatal(e)
		}
		time.Sleep(time.Until(queued.ExpiresAt) + 10*time.Millisecond)
		if e = blocker.Commit(); e != nil {
			t.Fatal(e)
		}
		if e = <-done; e != nil {
			t.Fatal("committed retry incorrectly revalidated expired new-write gate", e)
		}
	})
	t.Run("cloned-receipt-identity", func(t *testing.T) {
		clone, _ := newDB()
		if e := InitializeMigrationReceipts(ctx, clone); e != nil {
			t.Fatal(e)
		}
		done, ok, e := ReadMigrationReceipt(ctx, target, plan.PlanID, plan.PlanSHA256, "operator")
		if e != nil || !ok {
			t.Fatal("original receipt unavailable", e)
		}
		body, _ := json.Marshal(done)
		execSQL(clone, `INSERT INTO accounts_migration_api.receipts(plan_id,plan_sha256,subject,receipt) VALUES($1,$2,$3,$4::jsonb)`, plan.PlanID, plan.PlanSHA256, "operator", string(body))
		if _, _, e = ReadMigrationReceipt(ctx, clone, plan.PlanID, plan.PlanSHA256, "operator"); e == nil {
			t.Fatal("restored receipt accepted on different physical DB")
		}
	})
	execSQL(source, `UPDATE public.users SET password='late-source-password' WHERE uuid='`+userA+`'`)
	changed, err := CaptureMigrationSnapshot(ctx, readonly, uuid.NewString(), time.Hour, binding)
	if err != nil {
		t.Fatal(err)
	}
	conflict, err := PreviewMigration(ctx, target, changed, uuid.NewString(), time.Hour)
	if err != nil || conflict.State != "conflict" || conflict.ConflictRows == 0 {
		t.Fatal("identity conflict not reported", err)
	}
	if _, err = ApplyMigration(ctx, target, changed, conflict, conflict.PlanSHA256, "operator"); err == nil {
		t.Fatal("conflicting password overwritten")
	}
	t.Run("secondary-identity-collision", func(t *testing.T) {
		collisionTarget, _ := newDB()
		execSQL(collisionTarget, `INSERT INTO public.users(uuid,username,password,email,proxy_uuid) VALUES(gen_random_uuid(),'collision','different','alice@example.invalid',gen_random_uuid())`)
		if _, e := PreviewMigration(ctx, collisionTarget, snapshot, uuid.NewString(), time.Hour); e == nil {
			t.Fatal("email collision accepted")
		}
	})
	t.Run("oauth-collision", func(t *testing.T) {
		collisionTarget, _ := newDB()
		execSQL(collisionTarget, `INSERT INTO public.users(uuid,username,password,email,proxy_uuid) VALUES('00000000-0000-0000-0000-000000000104','collision','different','other@example.invalid',gen_random_uuid()); INSERT INTO public.identities(provider,external_id,user_uuid) VALUES('github','fixture-oauth','00000000-0000-0000-0000-000000000104')`)
		if _, e := PreviewMigration(ctx, collisionTarget, snapshot, uuid.NewString(), time.Hour); e == nil {
			t.Fatal("OAuth collision accepted")
		}
	})
	// Run real authenticated target-driven HTTP, encrypted restart, lost response.
	parentTest := t
	t.Run("dedicated-target-executor", func(t *testing.T) {
		role := "migration_executor_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		execSQL(admin, `CREATE ROLE `+quoteBusiness(role)+` LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD 'isolated-readonly-fixture'`)
		parentTest.Cleanup(func() {
			if _, e := admin.Exec(`DROP ROLE ` + quoteBusiness(role)); e != nil {
				parentTest.Error(e)
			}
		})
		db, name := newDB()
		execSQL(db, `GRANT USAGE,CREATE ON SCHEMA public TO `+quoteBusiness(role))
		for _, table := range migrationAPITables {
			execSQL(db, `ALTER TABLE public.`+quoteBusiness(table)+` OWNER TO `+quoteBusiness(role))
		}
		execSQL(db, `REVOKE CREATE ON SCHEMA public FROM `+quoteBusiness(role)+`; GRANT SELECT ON public.schema_migrations TO `+quoteBusiness(role)+`; GRANT EXECUTE ON FUNCTION pg_catalog.pg_control_system() TO `+quoteBusiness(role))
		if e := InitializeMigrationReceipts(ctx, db); e != nil {
			t.Fatal(e)
		}
		execSQL(db, `GRANT USAGE ON SCHEMA accounts_migration_api TO `+quoteBusiness(role)+`; GRANT SELECT,INSERT ON accounts_migration_api.receipts TO `+quoteBusiness(role))
		executorConfig := cfg.Copy()
		executorConfig.User, executorConfig.Password, executorConfig.Database = role, fixturePassword, name
		executor := stdlib.OpenDB(*executorConfig)
		defer executor.Close()
		var privileged bool
		if executor.QueryRow(`SELECT rolsuper OR rolbypassrls OR rolcreatedb OR rolcreaterole FROM pg_roles WHERE rolname=current_user`).Scan(&privileged) != nil || privileged {
			t.Fatal("executor unexpectedly privileged")
		}
		if _, e := executor.Exec(`CREATE TABLE public.executor_must_not_create(id int)`); e == nil {
			t.Fatal("executor unexpectedly has public CREATE")
		}
		if _, e = executor.Exec(`CREATE SCHEMA executor_must_not_create`); e == nil {
			t.Fatal("executor unexpectedly has database CREATE")
		}
		p, e := PreviewMigration(ctx, executor, snapshot, uuid.NewString(), time.Hour)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = ApplyMigration(ctx, executor, snapshot, p, p.PlanSHA256, "operator"); e != nil {
			t.Fatal(e)
		}
		if _, e = ApplyMigration(ctx, executor, snapshot, p, p.PlanSHA256, "operator"); e != nil {
			t.Fatal("executor retry", e)
		}
	})

	t.Run("http-persisted-recovery", func(t *testing.T) {
		httpTarget, _ := newDB()
		sourceDir := t.TempDir()
		targetDir := t.TempDir()
		os.Chmod(sourceDir, 0700)
		os.Chmod(targetDir, 0700)
		sourceKey := bytes.Repeat([]byte{1}, 32)
		targetKey := bytes.Repeat([]byte{2}, 32)
		sourceOptions := MigrationServerOptions{DB: readonly, Role: "source", Audience: "source-fixture", AuthKey: sourceKey, StagingKey: bytes.Repeat([]byte{3}, 32), StagingDirectory: sourceDir, Lifetime: time.Hour}
		sourceOptions.SourceAudience = binding.SourceAudience
		sourceOptions.TargetAudience = binding.TargetAudience
		sourceOptions.JobID = binding.JobID
		h, e := NewMigrationHandler(sourceOptions)
		if e != nil {
			t.Fatal(e)
		}
		sourceHTTP := httptest.NewServer(h)
		defer sourceHTTP.Close()
		targetOptions := MigrationServerOptions{DB: httpTarget, Role: "target", Audience: "target-fixture", AuthKey: targetKey, StagingKey: bytes.Repeat([]byte{4}, 32), StagingDirectory: targetDir, Lifetime: time.Hour, Peers: MigrationPeerPolicy{URLs: []string{sourceHTTP.URL}, Networks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, AllowLoopbackHTTP: true}}
		targetOptions.SourceAudience = binding.SourceAudience
		targetOptions.TargetAudience = binding.TargetAudience
		targetOptions.JobID = binding.JobID
		h, e = NewMigrationHandler(targetOptions)
		if e != nil {
			t.Fatal(e)
		}
		targetHTTP := httptest.NewServer(h)
		defer func() { targetHTTP.Close() }()
		srcToken, _ := MintMigrationToken(sourceKey, "source-fixture", "operator", []string{"source.read"}, time.Minute, binding)
		dstToken, _ := MintMigrationToken(targetKey, "target-fixture", "operator", []string{"target.plan", "target.apply"}, time.Minute, binding)
		options := MigrationClientOptions{SourceURL: sourceHTTP.URL, TargetURL: targetHTTP.URL, SourceToken: srcToken, TargetToken: dstToken, SnapshotID: uuid.NewString(), PlanID: uuid.NewString(), AllowLoopbackHTTP: true}
		otherSource, _ := MintMigrationToken(sourceKey, "source-fixture", "other-operator", []string{"source.read"}, time.Minute, binding)
		mixed := options
		mixed.SourceToken = otherSource
		mixed.SnapshotID = uuid.NewString()
		mixed.PlanID = uuid.NewString()
		if _, e = RunMigrationClient(ctx, mixed); e == nil {
			t.Fatal("source credential from another operator paired with target job")
		}
		preview, e := RunMigrationClient(ctx, options)
		if e != nil {
			t.Fatal(e)
		}
		options.PlanSHA256 = preview.PlanSHA256
		var receiptSchema bool
		if httpTarget.QueryRow(`SELECT to_regnamespace('accounts_migration_api') IS NOT NULL`).Scan(&receiptSchema) != nil || receiptSchema {
			t.Fatal("preview listener initialized receipt schema")
		}
		options.Apply = true
		if _, e = RunMigrationClient(ctx, options); e == nil {
			t.Fatal("preview-only listener allowed apply")
		}
		options.Apply = false
		// The same request is immutable even after source password changes again.
		execSQL(source, `UPDATE public.users SET password='changed-after-http-snapshot' WHERE uuid='`+userA+`'`)
		again, e := RunMigrationClient(ctx, options)
		if e != nil || again.PlanSHA256 != preview.PlanSHA256 {
			t.Fatal("preview replay changed", e)
		}
		for _, dir := range []string{sourceDir, targetDir} {
			entries, _ := os.ReadDir(dir)
			for _, entry := range entries {
				b, _ := os.ReadFile(filepath.Join(dir, entry.Name()))
				for _, secret := range []string{"alice@example.invalid", "late-source-password", "JBSWY3DPEHPK3PXP", srcToken, dstToken} {
					if bytes.Contains(b, []byte(secret)) {
						t.Fatal("plaintext persisted")
					}
				}
			}
		}
		// Restart both handlers with the same keys and persisted objects.
		h, e = NewMigrationHandler(sourceOptions)
		if e != nil {
			t.Fatal(e)
		}
		sourceHTTP.Config.Handler = h
		targetHTTP.Close()
		// Migration owner explicitly enables receipt DDL and injects a verified
		// isolated-restore gate bound to the exact preview baseline.
		targetOptions.InitializeReceipts = true
		targetOptions.ReceiptOwnerRole = cfg.User
		targetOptions.BackupGate = func(ctx context.Context, p MigrationPlan) error {
			if p.TargetBaselineSHA256 != preview.TargetBaselineSHA256 {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		h, e = NewMigrationHandler(targetOptions)
		if e != nil {
			t.Fatal(e)
		}
		targetHTTP = httptest.NewServer(h)
		options.TargetURL = targetHTTP.URL
		options.TargetToken = dstToken
		options.Apply = true
		done, e := RunMigrationClient(ctx, options)
		if e != nil || done.State != "applied" {
			t.Fatal("apply after restart", e)
		}
		if _, e = RunMigrationClient(ctx, options); e != nil {
			t.Fatal("lost-response retry", e)
		}
		options.Apply = false
		options.Status = true
		if done, e = RunMigrationClient(ctx, options); e != nil || done.State != "applied" {
			t.Fatal("status recovery", e)
		}
		if httpTarget.QueryRow(`SELECT count(*) FROM accounts_migration_api.receipts`).Scan(&count) != nil || count != 1 {
			t.Fatal("HTTP retries duplicated receipt")
		}
		// Refuse changed owner, scope, audience, key, lifetime or shared token.
		for name, token := range map[string]string{"missing": "", "service-token": "shared-service-token", "source-token": srcToken} {
			r, _ := http.NewRequest("GET", targetHTTP.URL+MigrationAPIBase+"/plans/"+preview.PlanID, nil)
			r.Header.Set("Authorization", "Bearer "+token)
			res, e := http.DefaultClient.Do(r)
			if e != nil {
				t.Fatal(e)
			}
			res.Body.Close()
			if res.StatusCode != 401 {
				t.Fatal(name, res.StatusCode)
			}
		}
		wrongScope, _ := MintMigrationToken(targetKey, "target-fixture", "operator", []string{"target.apply"}, time.Minute, binding)
		otherOwner, _ := MintMigrationToken(targetKey, "target-fixture", "other", []string{"target.plan"}, time.Minute, binding)
		for _, test := range []struct {
			token  string
			status int
		}{{wrongScope, 403}, {otherOwner, 409}} {
			r, _ := http.NewRequest("GET", targetHTTP.URL+MigrationAPIBase+"/plans/"+preview.PlanID, nil)
			r.Header.Set("Authorization", "Bearer "+test.token)
			res, e := http.DefaultClient.Do(r)
			if e != nil {
				t.Fatal(e)
			}
			res.Body.Close()
			if res.StatusCode != test.status {
				t.Fatal("auth scope/owner", res.StatusCode)
			}
		}
		t.Run("large-chunk-transfer-interrupt-restart", func(t *testing.T) {
			previousOptions := targetOptions
			defer func() {
				targetHTTP.Close()
				targetOptions = previousOptions
				handler, e := NewMigrationHandler(targetOptions)
				if e != nil {
					t.Error(e)
					return
				}
				targetHTTP = httptest.NewServer(handler)
			}()
			largeTarget, _ := newDB()
			execSQL(largeTarget, `INSERT INTO public.users(uuid,username,password,email,proxy_uuid) VALUES('00000000-0000-0000-0000-000000000199','large-target-only','protected-target-hash','large-target@example.invalid','00000000-0000-0000-0000-000000000299')`)
			targetOptions.DB = largeTarget
			targetOptions.InitializeReceipts = true
			execSQL(source, `INSERT INTO public.audit_logs(action,details) SELECT 'large-chunk-fixture',jsonb_build_object('payload',repeat('private-chunk-payload-',30000)) FROM generate_series(1,70)`)
			t.Run("unpublished-capture-interruption", func(t *testing.T) {
				captureOptions := sourceOptions
				captureOptions.StagingDirectory = t.TempDir()
				os.Chmod(captureOptions.StagingDirectory, 0700)
				handler, e := NewMigrationHandler(captureOptions)
				if e != nil {
					t.Fatal(e)
				}
				finished := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r); close(finished) }))
				defer server.Close()
				captureCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				id := uuid.NewString()
				go func() {
					ticker := time.NewTicker(time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-captureCtx.Done():
							return
						case <-ticker.C:
							entries, _ := os.ReadDir(captureOptions.StagingDirectory)
							for _, entry := range entries {
								if strings.HasPrefix(entry.Name(), "source-chunk-") {
									cancel()
									return
								}
							}
						}
					}
				}()
				var m MigrationManifest
				if e = migrationHTTP(captureCtx, http.DefaultClient, "POST", server.URL+MigrationAPIBase+"/snapshots", srcToken, map[string]string{"request_id": id}, &m); e == nil {
					t.Fatal("interrupted capture unexpectedly published")
				}
				select {
				case <-finished:
				case <-time.After(10 * time.Second):
					t.Fatal("source failed to cancel capture")
				}
				if _, e = os.Stat(filepath.Join(captureOptions.StagingDirectory, "snapshot-"+id+".gcm")); !os.IsNotExist(e) {
					t.Fatal("incomplete source manifest was exposed")
				}
			})
			chunkHandler, e := NewMigrationHandler(sourceOptions)
			if e != nil {
				t.Fatal(e)
			}
			var mu sync.Mutex
			approvedLargeBaseline := ""
			targetOptions.BackupGate = func(ctx context.Context, p MigrationPlan) error {
				mu.Lock()
				defer mu.Unlock()
				if approvedLargeBaseline == "" || p.TargetBaselineSHA256 != approvedLargeBaseline {
					return io.ErrUnexpectedEOF
				}
				return nil
			}
			fail := true
			seen := map[string]int{}
			attempts := 0
			chunkSource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				handler := chunkHandler
				refuse := false
				if strings.Contains(r.URL.Path, "/chunks/") {
					id := filepath.Base(r.URL.Path)
					seen[id]++
					attempts++
					refuse = fail && attempts >= 3
				}
				mu.Unlock()
				if refuse {
					w.WriteHeader(503)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer chunkSource.Close()
			targetHTTP.Close()
			targetOptions.Peers.URLs = []string{chunkSource.URL}
			nextTarget, e := NewMigrationHandler(targetOptions)
			if e != nil {
				t.Fatal(e)
			}
			targetHTTP = httptest.NewServer(nextTarget)
			srcToken, e = MintMigrationToken(sourceKey, "source-fixture", "operator", []string{"source.read"}, 10*time.Minute, binding)
			if e != nil {
				t.Fatal(e)
			}
			dstToken, e = MintMigrationToken(targetKey, "target-fixture", "operator", []string{"target.plan", "target.apply"}, 10*time.Minute, binding)
			if e != nil {
				t.Fatal(e)
			}
			large := MigrationClientOptions{SourceURL: chunkSource.URL, TargetURL: targetHTTP.URL, SourceToken: srcToken, TargetToken: dstToken, SnapshotID: uuid.NewString(), PlanID: uuid.NewString(), TransferOnly: true, AllowLoopbackHTTP: true}
			if _, e = RunMigrationClient(ctx, large); e == nil {
				t.Fatal("injected transfer interruption ignored")
			}
			entries, _ := os.ReadDir(targetDir)
			var cached []string
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "target-chunk-") {
					cached = append(cached, strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "target-chunk-"), ".gcm"))
				}
			}
			if len(cached) == 0 {
				t.Fatal("no durable pages after interruption")
			}
			nextSource, e := NewMigrationHandler(sourceOptions)
			if e != nil {
				t.Fatal(e)
			}
			mu.Lock()
			chunkHandler = nextSource
			fail = false
			prior := map[string]int{}
			for _, id := range cached {
				prior[id] = seen[id]
			}
			mu.Unlock()
			targetHTTP.Close()
			nextTarget, e = NewMigrationHandler(targetOptions)
			if e != nil {
				t.Fatal(e)
			}
			targetHTTP = httptest.NewServer(nextTarget)
			large.TargetURL = targetHTTP.URL
			receipt, e := RunMigrationClient(ctx, large)
			if e != nil || receipt.State != "transfer_staged" {
				t.Fatal("resumable large transfer", e)
			}
			var total int64
			for _, chunk := range receipt.Snapshot.Chunks {
				total += int64(chunk.Bytes)
			}
			if total <= 32<<20 {
				t.Fatal("fixture did not exceed old cap")
			}
			mu.Lock()
			for id, count := range prior {
				if seen[id] != count {
					t.Error("completed page redownloaded", id)
				}
			}
			mu.Unlock()
			if info, e := os.Stat(filepath.Join(sourceDir, "snapshot-"+large.SnapshotID+".gcm")); e != nil || info.Size() > 256<<10 {
				t.Fatal("source persisted whole snapshot instead of manifest")
			}
			large.TransferOnly = false
			largePlan, e := RunMigrationClient(ctx, large)
			if e != nil || largePlan.State != "preview" || largePlan.ConflictRows != 0 {
				t.Fatal("large native preview", largePlan.State, e)
			}
			if largeTarget.QueryRow(`SELECT count(*) FROM public.audit_logs WHERE action='large-chunk-fixture'`).Scan(&count) != nil || count != 0 {
				t.Fatal("transfer-only wrote business data")
			}
			mu.Lock()
			approvedLargeBaseline = largePlan.TargetBaselineSHA256
			mu.Unlock()
			large.Apply = true
			large.PlanSHA256 = largePlan.PlanSHA256
			applied, e := RunMigrationClient(ctx, large)
			if e != nil || applied.State != "applied" {
				t.Fatal("large native apply", e)
			}
			if _, e = RunMigrationClient(ctx, large); e != nil {
				t.Fatal("large apply retry", e)
			}
			if largeTarget.QueryRow(`SELECT count(*) FROM public.audit_logs WHERE action='large-chunk-fixture'`).Scan(&count) != nil || count != 70 {
				t.Fatal("large native rows not committed")
			}
			var protected string
			if largeTarget.QueryRow(`SELECT password FROM public.users WHERE uuid='00000000-0000-0000-0000-000000000199'`).Scan(&protected) != nil || protected != "protected-target-hash" {
				t.Fatal("large apply changed target-only fact")
			}
			check := large
			check.Apply = false
			check.PlanSHA256 = ""
			check.PlanID = uuid.NewString()
			noop, e := RunMigrationClient(ctx, check)
			if e != nil || noop.InsertRows != 0 || noop.ConflictRows != 0 {
				t.Fatal("large exact-content replay preview", e)
			}
			execSQL(largeTarget, `UPDATE public.users SET password='independent-target-password' WHERE uuid='`+userA+`'`)
			check.PlanID = uuid.NewString()
			conflicted, e := RunMigrationClient(ctx, check)
			if e != nil || conflicted.State != "conflict" || conflicted.ConflictRows == 0 {
				t.Fatal("large identity conflict not retained", e)
			}
			check.Apply = true
			check.PlanSHA256 = conflicted.PlanSHA256
			if _, e = RunMigrationClient(ctx, check); e == nil {
				t.Fatal("large conflicting identity overwritten")
			}
			if info, e := os.Stat(filepath.Join(targetDir, "plan-"+large.PlanID+".gcm")); e != nil || info.Size() > 256<<10 {
				t.Fatal("plan persisted full rows")
			}
			first := receipt.Snapshot.Chunks[0]
			path := filepath.Join(targetDir, "target-chunk-"+first.ID+".gcm")
			encrypted, e := os.ReadFile(path)
			if e != nil {
				t.Fatal(e)
			}
			encrypted[len(encrypted)-1] ^= 1
			os.WriteFile(path, encrypted, 0600)
			large.TransferOnly = true
			large.Apply = false
			large.PlanSHA256 = ""
			if _, e = RunMigrationClient(ctx, large); e == nil {
				t.Fatal("tampered cached chunk accepted")
			}
		})
		// Encrypt/authenticate files: one bit changed cannot become a usable plan.
		options.TargetURL = targetHTTP.URL
		path := filepath.Join(targetDir, "plan-"+preview.PlanID+".gcm")
		encrypted, _ := os.ReadFile(path)
		encrypted[len(encrypted)-1] ^= 1
		if os.WriteFile(path, encrypted, 0600) != nil {
			t.Fatal("fixture tamper failed")
		}
		if _, e = RunMigrationClient(ctx, options); e == nil {
			t.Fatal("tampered persisted plan accepted")
		}
	})
	// Ensure source remained untouched by capture/apply (only explicit fixture
	// updates above changed it); no API receipt schema was created on source.
	var exists bool
	if source.QueryRow(`SELECT to_regnamespace('accounts_migration_api') IS NOT NULL`).Scan(&exists) != nil || exists {
		t.Fatal("source API wrote database metadata")
	}
}

func TestMigrationAPIAuthAndPeerPolicy(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	jobID := uuid.NewString()
	s := &migrationServer{MigrationServerOptions: MigrationServerOptions{AuthKey: key, Audience: "fixture", SourceAudience: "fixture", TargetAudience: "target-fixture", JobID: jobID}}
	now := time.Now()
	valid := MigrationClaims{Use: migrationTokenUse, Scopes: []string{"source.read"}, SourceAudience: "fixture", TargetAudience: "target-fixture", JobID: jobID, RegisteredClaims: jwt.RegisteredClaims{Issuer: migrationTokenUse, Subject: "operator", ID: uuid.NewString(), Audience: jwt.ClaimStrings{"fixture"}, IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))}}
	for _, tc := range []struct {
		name   string
		change func(*MigrationClaims)
		method jwt.SigningMethod
		status int
	}{
		{"valid", func(*MigrationClaims) {}, jwt.SigningMethodHS256, 0},
		{"expired", func(c *MigrationClaims) { c.ExpiresAt = jwt.NewNumericDate(now.Add(-time.Minute)) }, jwt.SigningMethodHS256, 401},
		{"overlong", func(c *MigrationClaims) { c.ExpiresAt = jwt.NewNumericDate(now.Add(time.Hour)) }, jwt.SigningMethodHS256, 401},
		{"wrong-use", func(c *MigrationClaims) { c.Use = "accounts-service" }, jwt.SigningMethodHS256, 401},
		{"wrong-audience", func(c *MigrationClaims) { c.Audience = jwt.ClaimStrings{"other"} }, jwt.SigningMethodHS256, 401},
		{"missing-issued", func(c *MigrationClaims) { c.IssuedAt = nil }, jwt.SigningMethodHS256, 401},
		{"wrong-scope", func(c *MigrationClaims) { c.Scopes = []string{"apply"} }, jwt.SigningMethodHS256, 403},
		{"wrong-algorithm", func(*MigrationClaims) {}, jwt.SigningMethodHS384, 401},
		{"wrong-source-peer", func(c *MigrationClaims) { c.SourceAudience = "other-source" }, jwt.SigningMethodHS256, 403},
		{"wrong-target-peer", func(c *MigrationClaims) { c.TargetAudience = "other-target" }, jwt.SigningMethodHS256, 403},
		{"wrong-job", func(c *MigrationClaims) { c.JobID = uuid.NewString() }, jwt.SigningMethodHS256, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := valid
			tc.change(&claims)
			token, e := jwt.NewWithClaims(tc.method, claims).SignedString(key)
			if e != nil {
				t.Fatal(e)
			}
			r := httptest.NewRequest("GET", "/", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			_, status := s.authenticate(r, "source.read")
			if status != tc.status {
				t.Fatal(status)
			}
		})
	}
	for _, bad := range []string{"https://user:pass@example.invalid", "https://example.invalid/path", "https://example.invalid?query=1", "http://169.254.169.254", "http://example.invalid", "file:///etc/passwd", "https://example.invalid#x"} {
		if _, e := migrationURL(bad, true); e == nil {
			t.Fatal("unsafe URL accepted", bad)
		}
	}
	seen := 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen++; migrationJSON(w, 200, map[string]string{}) }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 307) }))
	defer redirect.Close()
	policy := MigrationPeerPolicy{URLs: []string{redirect.URL}, Networks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, AllowLoopbackHTTP: true}
	client, base, e := policy.client(redirect.URL)
	if e != nil {
		t.Fatal(e)
	}
	if e = migrationHTTP(context.Background(), client, "GET", base, "fixture-token", nil, nil); e == nil || seen != 0 {
		t.Fatal("redirect followed")
	}
	if _, _, e = policy.client(destination.URL); e == nil {
		t.Fatal("non-allowlisted peer accepted")
	}
	metadata := MigrationPeerPolicy{URLs: []string{"https://169.254.169.254"}, Networks: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}
	metadataClient, metadataURL, e := metadata.client("https://169.254.169.254")
	if e != nil {
		t.Fatal(e)
	}
	if e = migrationHTTP(context.Background(), metadataClient, "GET", metadataURL, "fixture-token", nil, nil); e == nil {
		t.Fatal("metadata address allowed even with broad CIDR")
	}
	policy.Networks = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	client, base, e = policy.client(redirect.URL)
	if e != nil {
		t.Fatal(e)
	}
	if e = migrationHTTP(context.Background(), client, "GET", base, "fixture-token", nil, nil); e == nil {
		t.Fatal("destination outside network accepted")
	}
	for _, status := range []int{404, 405, 501} {
		endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		e = migrationHTTP(context.Background(), http.DefaultClient, "GET", endpoint.URL, "fixture-token", nil, nil)
		endpoint.Close()
		if e == nil || !strings.Contains(e.Error(), "unsupported migration endpoint; no fallback") {
			t.Fatal("unsupported fallback", e)
		}
	}
}
