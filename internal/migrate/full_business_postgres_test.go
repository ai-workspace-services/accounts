package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"account/internal/dbruntime"
	schema "account/sql"
	"github.com/jackc/pgx/v5"
)

// Only a dedicated loopback disposable fixture may execute this test. It never
// reads Vault or any application DSN, and its source/target names are fixed.
func TestFullBusinessPostgres17(t *testing.T) {
	targetDSN := os.Getenv("MIGRATECTL_FULL_BUSINESS_TEST_DSN")
	if targetDSN == "" {
		t.Skip("isolated full-business PostgreSQL17 fixture not configured")
	}
	cfg, e := pgx.ParseConfig(targetDSN)
	if e != nil || cfg.Database != "account" || (cfg.Host != "localhost" && cfg.Host != "127.0.0.1") {
		t.Fatal("test requires isolated loopback account fixture")
	}

	sourceAdmin, e := sql.Open("pgx", "postgres://"+cfg.User+":"+cfg.Password+"@"+cfg.Host+":"+fmtPort(cfg.Port)+"/full_business_source?sslmode=disable")
	if e != nil {
		t.Fatal("fixture source unavailable")
	}
	defer sourceAdmin.Close()
	target, e := sql.Open("pgx", targetDSN)
	if e != nil {
		t.Fatal("fixture target unavailable")
	}
	defer target.Close()
	var version int
	if e = target.QueryRow(`SELECT current_setting('server_version_num')::int`).Scan(&version); e != nil || version < 170000 || version >= 180000 {
		t.Fatal("test requires PostgreSQL17")
	}
	exec := func(db *sql.DB, body string) {
		t.Helper()
		if _, err := db.Exec(body); err != nil {
			t.Fatalf("isolated fixture statement failed: %v", err)
		}
	}
	body, manifest, e := schema.NativeArtifact()
	if e != nil {
		t.Fatal(e)
	}
	exec(sourceAdmin, string(body))
	options := FullBusinessOptions{Environment: "prod", SchemaSHA256: manifest.SchemaSHA256, BillingSHA256: FullBusinessBillingSHA256, WritersPaused: true, DryRun: true}
	if _, e = InitializeNative(context.Background(), targetDSN, NativeInitOptions{Environment: "prod", SchemaSHA256: manifest.SchemaSHA256, WritersPaused: true, LockTimeout: time.Second, StatementTimeout: time.Minute}); e != nil {
		t.Fatal(e)
	}
	billing, e := os.ReadFile("testdata/cloud_vendor_costs_native.sql")
	if e != nil {
		t.Fatal(e)
	}
	exec(target, string(billing)+`; UPDATE public.schema_migrations SET version=`+strconv.FormatUint(uint64(FullBusinessVersion), 10))
	// A controlled upgrade keeps checksum receipts outside the business scope.
	// Runtime must accept that exact metadata table and still reject unknown
	// relations below. No receipt is renamed, deleted, or copied from source.
	exec(target, `CREATE TABLE public.accounts_controlled_migration_history
 (from_version bigint NOT NULL, to_version bigint PRIMARY KEY,
 sql_sha256 text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`)
	if e = VerifyNativeRuntime(context.Background(), target); e != nil {
		t.Fatalf("native runtime rejected controlled migration metadata: %v", e)
	}
	// Data-only catalog checkpoints retain the old native schema, but require
	// the exact reviewed migration chain rather than accepting arbitrary versions.
	for _, version := range []uint{2026101901, 2026101902} {
		exec(target, `UPDATE public.schema_migrations SET version=`+strconv.Itoa(int(version)))
		if e = VerifyNativeRuntime(context.Background(), target); e == nil {
			t.Fatal("missing catalog receipt admitted")
		}
		if version == 2026101901 {
			exec(target, `INSERT INTO public.accounts_controlled_migration_history(from_version,to_version,sql_sha256) VALUES(2026101703,2026101901,'b9fee298512e7d888fe619917c8238120f0e227f4ea348e59397d074c41f878d')`)
		} else {
			exec(target, `INSERT INTO public.accounts_controlled_migration_history(from_version,to_version,sql_sha256) VALUES(2026101901,2026101902,'53f9aaf7c56a14fd7c132c6649b79582cd2063f8903b94efb4e23cbf4895437c')`)
		}
		got, err := VerifiedNativeRuntimeVersion(context.Background(), target)
		if err != nil || got != version {
			t.Fatalf("reviewed catalog checkpoint: %d %v", got, err)
		}
	}
	exec(target, `DELETE FROM public.accounts_controlled_migration_history; UPDATE public.schema_migrations SET version=`+strconv.Itoa(FullBusinessVersion))
	// Qualify the old 44-table source -> latest 63-table target explicitly. New
	// tables may be absent in source; they must remain empty in the target.
	tables, _, e := fullBusinessContract()
	if e != nil {
		t.Fatal(e)
	}
	for name := range optionalBusinessTables {
		if name != "cloud_vendor_costs" {
			exec(sourceAdmin, "DROP TABLE public."+quoteBusiness(name)+" CASCADE")
		}
	}
	for _, c := range tables["users"].Columns {
		if strings.HasPrefix(c.Name, "account_lifecycle_") || absentNativeUserMetadata(c) {
			exec(sourceAdmin, "ALTER TABLE public.users DROP COLUMN "+quoteBusiness(c.Name)+" CASCADE")
		}
	}
	exec(sourceAdmin, `ALTER TABLE public.email_blacklist DROP COLUMN uuid CASCADE;
 ALTER TABLE public.email_blacklist ADD PRIMARY KEY(email);
 INSERT INTO public.email_blacklist(email,created_at) VALUES('Blocked@example.invalid','2026-01-01T00:00:00Z')`)
	exec(sourceAdmin, `CREATE ROLE readonly_release LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD 'isolated-readonly-fixture'`)
	exec(sourceAdmin, `GRANT USAGE ON SCHEMA public TO readonly_release; GRANT SELECT ON ALL TABLES IN SCHEMA public TO readonly_release`)
	for name := range tables {
		if optionalBusinessTables[name] {
			continue
		}
		exec(sourceAdmin, "ALTER TABLE public."+quoteBusiness(name)+" ENABLE ROW LEVEL SECURITY")
		exec(sourceAdmin, "CREATE POLICY release_initialization_readonly ON public."+quoteBusiness(name)+" FOR SELECT TO readonly_release USING (true)")
	}
	const sourceID = "00000000-0000-0000-0000-000000000101"
	const proxyID = "00000000-0000-0000-0000-000000000201"
	exec(sourceAdmin, `INSERT INTO public.users(uuid,username,password,email,proxy_uuid,email_verified_at) VALUES('`+sourceID+`','full-business-fixture','isolated-password',' User@example.invalid ','`+proxyID+`','2026-10-01T00:00:00Z')`)
	exec(sourceAdmin, `INSERT INTO public.billing_ledger(id,account_uuid,bucket_start,bucket_end,entry_type,rated_bytes,amount_delta,balance_after,created_at)
 SELECT md5('ledger-'||g::text)::uuid,'`+sourceID+`','2026-10-01T00:00:00Z'::timestamptz+g*interval '1 minute','2026-10-01T00:01:00Z'::timestamptz+g*interval '1 minute','usage',9007199254740993,0.125,12.75,'2026-10-01T00:00:00Z' FROM generate_series(1,1003)g`)
	// A composite PK and user UUID in a non-FK field must also survive batches.
	exec(sourceAdmin, `INSERT INTO public.traffic_minute_buckets(account_uuid,node_id,bucket_start,total_bytes) SELECT '`+sourceID+`','node-fixture','2026-10-01T00:00:00Z'::timestamptz+g*interval '1 minute',9007199254740993 FROM generate_series(1,1003)g`)
	exec(sourceAdmin, `INSERT INTO public.audit_logs(uuid,action,actor_uuid,details,created_at) VALUES('00000000-0000-0000-0000-000000000401','qualification','`+sourceID+`','{"exact":9007199254740993,"decimal":1.20}','2026-10-01T00:00:00Z')`)
	exec(sourceAdmin, `INSERT INTO public.sandbox_bindings(id,agent_id,created_at,updated_at) VALUES(42,'fixture-agent','2026-10-01T00:00:00Z','2026-10-01T00:00:00Z')`)
	// Build DSNs explicitly: Config.Copy retains the parsed original connString.
	sourceDSN := "postgres://readonly_release:isolated-readonly-fixture@" + cfg.Host + ":" + fmtPort(cfg.Port) + "/full_business_source?sslmode=disable"
	serverlessDSN := "postgres://" + cfg.User + ":" + cfg.Password + "@" + cfg.Host + ":" + fmtPort(cfg.Port) + "/full_business_source?sslmode=disable"
	var before string
	if e = sourceAdmin.QueryRow(`SELECT md5(string_agg(to_jsonb(u)::text,'' ORDER BY uuid)) FROM public.users u`).Scan(&before); e != nil {
		t.Fatal(e)
	}
	preview, e := CopyFullBusiness(context.Background(), sourceDSN, targetDSN, options)
	if e != nil || preview.Result != "eligible" || preview.SourceTables != 44 || preview.FullBusinessEqual || preview.TargetWrites || len(preview.Tables) != 0 {
		t.Fatalf("preview: %#v %v", preview, e)
	}
	serverlessPreview, e := CopyFullBusiness(context.Background(), serverlessDSN, targetDSN, options)
	if e != nil || serverlessPreview.Result != "eligible" || !serverlessPreview.SourceReadOnly || serverlessPreview.TargetWrites {
		t.Fatalf("existing Serverless DSN was not constrained to readonly preview: %#v %v", serverlessPreview, e)
	}
	readonlyTx, e := sourceAdmin.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if e != nil {
		t.Fatal("could not open read-only source transaction")
	}
	if e = businessSourceRole(context.Background(), readonlyTx); e == nil {
		t.Fatal("unverified session readonly default accepted")
	}
	if _, e = readonlyTx.ExecContext(context.Background(), `SET LOCAL default_transaction_read_only=on`); e != nil {
		t.Fatal("transaction-local source connection default could not be enforced")
	}
	if e = businessSourceRole(context.Background(), readonlyTx); e != nil {
		t.Fatal("both verified readonly settings were refused")
	}
	if _, e = readonlyTx.ExecContext(context.Background(), `UPDATE public.users SET email='mutated@example.invalid' WHERE uuid='`+sourceID+`'`); e == nil {
		t.Fatal("source write succeeded inside the migration readonly transaction")
	}
	_ = readonlyTx.Rollback()
	var restored string
	if e = sourceAdmin.QueryRow(`SHOW default_transaction_read_only`).Scan(&restored); e != nil || restored != "off" {
		t.Fatal("source connection default leaked past migration transaction")
	}
	// Prove restrictive RLS rejection before any data can be written.
	exec(sourceAdmin, `CREATE POLICY hidden_release_rows ON public.users AS RESTRICTIVE FOR SELECT TO readonly_release USING(false)`)
	if _, e = CopyFullBusiness(context.Background(), sourceDSN, targetDSN, options); e == nil {
		t.Fatal("partial RLS accepted")
	}
	exec(sourceAdmin, `DROP POLICY hidden_release_rows ON public.users`)
	// Late insert failure must roll back earlier copied tables, including users.
	exec(target, `ALTER TABLE public.billing_ledger ADD CONSTRAINT fixture_reject CHECK(rated_bytes<0)`)
	options.DryRun = false
	if _, e = CopyFullBusiness(context.Background(), sourceDSN, targetDSN, options); e == nil {
		t.Fatal("failed batch committed")
	}
	var count int
	if e = target.QueryRow(`SELECT count(*) FROM public.users`).Scan(&count); e != nil || count != 0 {
		t.Fatal("failed import retained users")
	}
	exec(target, `ALTER TABLE public.billing_ledger DROP CONSTRAINT fixture_reject`)
	receipt, e := CopyFullBusiness(context.Background(), sourceDSN, targetDSN, options)
	if e != nil || receipt.Result != "copied" || !receipt.FullBusinessEqual || !receipt.TargetWrites || receipt.DatabaseCutoverApproved || len(receipt.Tables) != 79 || receipt.Tables["billing_ledger"].Rows != 1003 || receipt.Tables["traffic_minute_buckets"].Rows != 1003 {
		t.Fatalf("full copy: %#v %v", receipt, e)
	}
	var after string
	if e = sourceAdmin.QueryRow(`SELECT md5(string_agg(to_jsonb(u)::text,'' ORDER BY uuid)) FROM public.users u`).Scan(&after); e != nil || before != after {
		t.Fatal("source users mutated")
	}
	var minimum int64
	if e = target.QueryRow(`SELECT min(rated_bytes) FROM public.billing_ledger`).Scan(&minimum); e != nil || minimum != 9007199254740993 {
		t.Fatal("ledger integer precision lost")
	}
	if e = target.QueryRow(`SELECT nextval('public.sandbox_bindings_id_seq')`).Scan(&minimum); e != nil || minimum != 43 {
		t.Fatal("target sequence not advanced above copied maximum")
	}
	var email, proxy string
	if e = target.QueryRow(`SELECT email,proxy_uuid::text FROM public.users`).Scan(&email, &proxy); e != nil || email != " User@example.invalid " || proxy != proxyID {
		t.Fatal("authoritative source user facts changed")
	}
	var blacklistUUID string
	if e = target.QueryRow(`SELECT uuid::text FROM public.email_blacklist WHERE email='Blocked@example.invalid'`).Scan(&blacklistUUID); e != nil || blacklistUUID != "ee5affdd-9684-56a0-84a3-3df3be74caa6" || receipt.Tables["email_blacklist"].Rows != 1 {
		t.Fatal("email-keyed blacklist did not retain stable native identity")
	}
	if _, e = CopyFullBusiness(context.Background(), sourceDSN, targetDSN, options); e == nil {
		t.Fatal("populated target replay accepted")
	}

	options.CompareOnly = true
	equal, e := CopyFullBusiness(context.Background(), sourceDSN, targetDSN, options)
	if e != nil || !equal.FullBusinessEqual || equal.TargetWrites || equal.Result != "equal" {
		t.Fatalf("full comparison: %v", e)
	}
	// Compare the same email with a different source UUID. These are fixture-only
	// admin writes after proving transfer source immutability. Runtime transfer
	// never disables a source trigger or holds an admin connection.
	const otherID = "00000000-0000-0000-0000-000000000111"
	exec(sourceAdmin, `SET session_replication_role=replica; UPDATE public.users SET uuid='`+otherID+`' WHERE uuid='`+sourceID+`'; UPDATE public.billing_ledger SET account_uuid='`+otherID+`'; UPDATE public.traffic_minute_buckets SET account_uuid='`+otherID+`'; UPDATE public.audit_logs SET actor_uuid='`+otherID+`'; SET session_replication_role=origin`)
	if _, e = CopyFullBusiness(context.Background(), sourceDSN, targetDSN, options); e != nil {
		t.Fatalf("email-based UUID comparison: %v", e)
	}
	exec(sourceAdmin, `SET session_replication_role=replica; UPDATE public.users SET proxy_uuid='00000000-0000-0000-0000-000000000222'; SET session_replication_role=origin`)
	if _, e = CopyFullBusiness(context.Background(), sourceDSN, targetDSN, options); e == nil {
		t.Fatal("different Proxy UUID accepted")
	}
	exec(sourceAdmin, `SET session_replication_role=replica; UPDATE public.users SET proxy_uuid='`+proxyID+`'; SET session_replication_role=origin`)
	// A row field mismatch is insufficient for equality despite equal counts.
	exec(target, `UPDATE public.billing_ledger SET amount_delta=0.5 WHERE id=md5('ledger-1')::uuid`)
	if _, e = CopyFullBusiness(context.Background(), sourceDSN, targetDSN, options); e == nil {
		t.Fatal("equal-count unequal-ledger accepted")
	}
	exec(sourceAdmin, `CREATE TABLE public.unreviewed_business(id integer PRIMARY KEY)`)
	if _, e = CopyFullBusiness(context.Background(), sourceDSN, targetDSN, options); e == nil {
		t.Fatal("unreviewed source table accepted")
	}
	raw, _ := json.Marshal(receipt)
	if strings.Contains(string(raw), "fixture-password") || strings.Contains(string(raw), "@example.invalid") || strings.Contains(string(raw), proxyID) {
		t.Fatal("receipt contains private business records")
	}
	t.Run("populated_core_user_receipt", func(t *testing.T) {
		// Core reconciliation changes only the three source-authoritative fields;
		// the already-populated target UUID is retained; dynamic business rows
		// are outside the core equality contract. User triggers may bump metadata.
		var originalUUID string
		if e = target.QueryRow(`SELECT uuid::text FROM public.users`).Scan(&originalUUID); e != nil {
			t.Fatal("target core fixture unavailable")
		}
		exec(target, `UPDATE public.users SET email='user@example.invalid',password='stale-core-fixture',proxy_uuid='00000000-0000-0000-0000-000000000333'`)
		coreOptions := CoreUsersOptions{Environment: "prod", SchemaSHA256: manifest.SchemaSHA256, WritersPaused: true,
			Preview: true}
		expectedIdentity, e := dbruntime.ConnectionIdentity(sourceDSN)
		if e != nil {
			t.Fatal("fixture source identity unavailable")
		}
		check := func(r FullBusinessReceipt, compare bool, e error) {
			t.Helper()
			if e != nil || r.Scope != "core_users" || r.SourceIdentitySHA256 != expectedIdentity ||
				r.SourceIdentitySHA256 == "" || r.SourceTables != 1 || r.UserCount != 1 || r.BatchSize != 1000 ||
				!r.SourceReadOnly || r.TargetWrites == compare || !r.FullBusinessEqual ||
				r.DatabaseCutoverApproved || len(r.Tables) != 0 || r.CoreUsers.Source != r.CoreUsers.Target ||
				r.SourceSnapshotSHA256 != "" || r.SourceCatalogSHA256 != "" ||
				r.SnapshotStartedAt.IsZero() || r.CompletedAt.Before(r.SnapshotStartedAt) {
				t.Fatalf("core-user receipt does not satisfy the execution owner contract: %v", e)
			}
		}
		preview, e := CopyCoreUsers(context.Background(), sourceDSN, targetDSN, coreOptions)
		if e != nil || preview.TargetWrites || len(preview.CoreUsersPlanSHA256) != 64 || preview.BillingSHA256 != "" {
			t.Fatal("users preview contract invalid", e)
		}
		coreOptions.Preview = false
		coreOptions.ExpectedPlanSHA256 = strings.Repeat("0", 64)
		if _, e = CopyCoreUsers(context.Background(), sourceDSN, targetDSN, coreOptions); e == nil {
			t.Fatal("wrong plan accepted")
		}
		coreOptions.ExpectedPlanSHA256 = preview.CoreUsersPlanSHA256
		exec(target, `UPDATE public.users SET mfa_enabled=NOT mfa_enabled`)
		if _, e = CopyCoreUsers(context.Background(), sourceDSN, targetDSN, coreOptions); e == nil {
			t.Fatal("target MFA drift accepted")
		}
		exec(target, `UPDATE public.users SET mfa_enabled=NOT mfa_enabled`)
		coreOptions.Preview = true
		preview, e = CopyCoreUsers(context.Background(), sourceDSN, targetDSN, coreOptions)
		if e != nil {
			t.Fatal(e)
		}
		// Billing schema is outside this users-only operation.
		exec(target, `ALTER TABLE public.finance_invoices ADD COLUMN users_migration_unrelated_fixture TEXT`)
		coreOptions.Preview = false
		coreOptions.ExpectedPlanSHA256 = preview.CoreUsersPlanSHA256
		exec(sourceAdmin, `UPDATE public.users SET mfa_enabled=NOT mfa_enabled`)
		if _, e = CopyCoreUsers(context.Background(), sourceDSN, targetDSN, coreOptions); e == nil {
			t.Fatal("source MFA drift accepted")
		}
		exec(sourceAdmin, `UPDATE public.users SET mfa_enabled=NOT mfa_enabled`)
		coreOptions.Preview = true
		preview, e = CopyCoreUsers(context.Background(), sourceDSN, targetDSN, coreOptions)
		if e != nil {
			t.Fatal(e)
		}
		coreOptions.Preview = false
		coreOptions.ExpectedPlanSHA256 = preview.CoreUsersPlanSHA256
		r, e := CopyCoreUsers(context.Background(), sourceDSN, targetDSN, coreOptions)
		check(r, false, e)
		var versionBefore, versionAfter int64
		if e = target.QueryRow(`SELECT version FROM public.users`).Scan(&versionBefore); e != nil {
			t.Fatal("target version fixture unavailable")
		}
		coreOptions.Preview = true
		preview, e = CopyCoreUsers(context.Background(), sourceDSN, targetDSN, coreOptions)
		if e != nil {
			t.Fatal(e)
		}
		coreOptions.Preview = false
		coreOptions.ExpectedPlanSHA256 = preview.CoreUsersPlanSHA256
		r, e = CopyCoreUsers(context.Background(), sourceDSN, targetDSN, coreOptions)
		check(r, false, e)
		if e = target.QueryRow(`SELECT version FROM public.users`).Scan(&versionAfter); e != nil || versionAfter != versionBefore {
			t.Fatal("already-aligned core replay fired user metadata triggers")
		}
		coreOptions.CompareOnly = true
		r, e = CopyCoreUsers(context.Background(), sourceDSN, targetDSN, coreOptions)
		check(r, true, e)
		var retainedUUID string
		if e = target.QueryRow(`SELECT uuid::text FROM public.users`).Scan(&retainedUUID); e != nil || retainedUUID != originalUUID {
			t.Fatal("existing target user UUID changed")
		}
		raw, _ := json.Marshal(r)
		if strings.Contains(string(raw), "isolated-password") || strings.Contains(string(raw), "@example.invalid") || strings.Contains(string(raw), proxyID) {
			t.Fatal("core-user receipt contains private user fields")
		}
		// A differing preview must return a usable safe report while leaving
		// every source and target user field (including trigger metadata) intact.
		exec(target, `UPDATE public.users SET proxy_uuid='00000000-0000-0000-0000-000000000334'`)
		fingerprint := func(db *sql.DB) string {
			t.Helper()
			var hash string
			if err := db.QueryRow(`SELECT md5(string_agg(to_jsonb(u)::text,'' ORDER BY uuid)) FROM public.users u`).Scan(&hash); err != nil {
				t.Fatal("cannot fingerprint isolated user fixture")
			}
			return hash
		}
		sourceBefore, targetBefore := fingerprint(sourceAdmin), fingerprint(target)
		r, e = CopyCoreUsers(context.Background(), sourceDSN, targetDSN, coreOptions)
		if e == nil || r.Result != "different" || r.CoreUsersDiff == nil || r.CoreUsersDiff.ProxyUUIDMismatches != 1 || r.TargetWrites || !r.SourceReadOnly {
			t.Fatal("differing comparison did not fail with a readonly diff receipt")
		}
		if fingerprint(sourceAdmin) != sourceBefore || fingerprint(target) != targetBefore {
			t.Fatal("readonly comparison changed user data or trigger metadata")
		}
	})
	t.Run("empty_target_core_users_preview_apply", func(t *testing.T) {
		exec(target, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
		if _, err := InitializeNative(context.Background(), targetDSN, NativeInitOptions{Environment: "prod", SchemaSHA256: manifest.SchemaSHA256, WritersPaused: true, LockTimeout: time.Second, StatementTimeout: time.Minute}); err != nil {
			t.Fatal(err)
		}
		core := CoreUsersOptions{Environment: "prod", SchemaSHA256: manifest.SchemaSHA256, Preview: true, WritersPaused: true}
		preview, err := CopyCoreUsers(context.Background(), sourceDSN, targetDSN, core)
		if err != nil || preview.CoreUsers.Source.Count != 1 || preview.CoreUsers.Target.Count != 0 || preview.TargetWrites || preview.FullBusinessEqual || len(preview.CoreUsersPlanSHA256) != 64 {
			t.Fatalf("fresh target preview failed: %v", err)
		}
		var count int
		if err = target.QueryRow(`SELECT count(*) FROM users`).Scan(&count); err != nil || count != 0 {
			t.Fatal("preview wrote target users")
		}
		core.Preview = false
		core.ExpectedPlanSHA256 = preview.CoreUsersPlanSHA256
		// Simulate a running service writer committing while migration waits
		// for its users lock. The old preview must never overwrite this user.
		writer, err := target.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Rollback()
		if _, err = writer.Exec(`INSERT INTO users(uuid,username,password,email,proxy_uuid) VALUES('00000000-0000-0000-0000-000000000999','concurrent-writer','concurrent-password',' User@example.invalid ','00000000-0000-0000-0000-000000000998')`); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, err := CopyCoreUsers(context.Background(), sourceDSN, targetDSN, core)
			result <- err
		}()
		waiting := false
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if err = target.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_locks WHERE relation='public.users'::regclass AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND mode='AccessExclusiveLock' AND NOT granted)`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !waiting {
			t.Fatal("migration did not wait for the running writer")
		}
		if err = writer.Commit(); err != nil {
			t.Fatal(err)
		}
		if err = <-result; err == nil || !strings.Contains(err.Error(), "plan") {
			t.Fatalf("concurrent commit was not rejected by the locked plan check: %v", err)
		}
		var retainedPassword string
		if err = target.QueryRow(`SELECT password FROM users`).Scan(&retainedPassword); err != nil || retainedPassword != "concurrent-password" {
			t.Fatal("concurrent writer was overwritten")
		}
		// Reset only this disposable loopback fixture, as above. Runtime user
		// retention triggers stay enabled and are never bypassed by migration.
		exec(target, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
		if _, err = InitializeNative(context.Background(), targetDSN, NativeInitOptions{Environment: "prod", SchemaSHA256: manifest.SchemaSHA256, WritersPaused: true, LockTimeout: time.Second, StatementTimeout: time.Minute}); err != nil {
			t.Fatal(err)
		}
		core.Preview = true
		preview, err = CopyCoreUsers(context.Background(), sourceDSN, targetDSN, core)
		if err != nil {
			t.Fatal(err)
		}
		core.Preview = false
		core.ExpectedPlanSHA256 = preview.CoreUsersPlanSHA256
		applied, err := CopyCoreUsers(context.Background(), sourceDSN, targetDSN, core)
		if err != nil || !applied.FullBusinessEqual || applied.CoreUsers.Target.Count != 1 || !applied.TargetWrites {
			t.Fatalf("fresh target apply failed: %v", err)
		}
		core.CompareOnly = true
		compared, err := CopyCoreUsers(context.Background(), sourceDSN, targetDSN, core)
		if err != nil || !compared.FullBusinessEqual || compared.TargetWrites {
			t.Fatalf("fresh target comparison failed: %v", err)
		}
	})
	t.Run("native63_finance_and_late_trigger", func(t *testing.T) {
		// Explicit synthetic fixture reset, behind the loopback-only test DSN.
		// No runtime migration function contains DROP or disables a constraint.
		exec(target, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
		if _, e = InitializeNative(context.Background(), targetDSN, NativeInitOptions{Environment: "prod", SchemaSHA256: manifest.SchemaSHA256, WritersPaused: true, LockTimeout: time.Second, StatementTimeout: time.Minute}); e != nil {
			t.Fatal(e)
		}
		exec(target, string(billing)+`; UPDATE public.schema_migrations SET version=`+strconv.FormatUint(uint64(FullBusinessVersion), 10))
		nativeDSN := "postgres://" + cfg.User + ":" + cfg.Password + "@" + cfg.Host + ":" + fmtPort(cfg.Port) + "/full_business_native_source?sslmode=disable"
		native, e := sql.Open("pgx", nativeDSN)
		if e != nil {
			t.Fatal(e)
		}
		defer native.Close()
		exec(native, string(body))
		exec(native, string(billing))
		exec(native, `GRANT USAGE ON SCHEMA public TO readonly_release; GRANT SELECT ON ALL TABLES IN SCHEMA public TO readonly_release`)
		for name := range tables {
			exec(native, "ALTER TABLE public."+quoteBusiness(name)+" ENABLE ROW LEVEL SECURITY; CREATE POLICY release_initialization_readonly ON public."+quoteBusiness(name)+" FOR SELECT TO readonly_release USING(true)")
		}
		exec(native, `INSERT INTO public.users(uuid,username,password,email,proxy_uuid,email_verified_at) VALUES('`+sourceID+`','native-full-business','isolated-password','Native@example.invalid','`+proxyID+`','2026-10-01T00:00:00Z')`)
		exec(native, `INSERT INTO public.finance_invoices(id,idempotency_key,account_uuid,amount_minor,currency,service_period_start,service_period_end) VALUES('00000000-0000-0000-0000-000000000501','invoice','`+sourceID+`',9007199254740993,'USD','2026-10-01T00:00:00Z','2026-11-01T00:00:00Z');
        INSERT INTO public.finance_payments(id,idempotency_key,invoice_id,account_uuid,amount_minor,currency) VALUES('00000000-0000-0000-0000-000000000502','payment','00000000-0000-0000-0000-000000000501','`+sourceID+`',9007199254740993,'USD');
        INSERT INTO public.finance_refunds(id,idempotency_key,payment_id,amount_minor,currency) VALUES('00000000-0000-0000-0000-000000000503','refund','00000000-0000-0000-0000-000000000502',123,'USD');
        INSERT INTO public.finance_operations(id,idempotency_key,operation_type,status) VALUES('00000000-0000-0000-0000-000000000504','operation','payment','succeeded');
        INSERT INTO public.finance_operation_events(id,operation_id,attempt,event_type,status) OVERRIDING SYSTEM VALUE VALUES(44,'00000000-0000-0000-0000-000000000504',1,'qualification','succeeded');
        INSERT INTO public.account_lifecycle_events(transition_id,user_uuid,from_state,to_state,actor_type,actor_ref) VALUES('00000000-0000-0000-0000-000000000505','`+sourceID+`','active','archived','user','`+sourceID+`');
        INSERT INTO public.password_recovery_challenges(id,user_uuid,email_snapshot,challenge_kind,secret_hash,expires_at) VALUES('00000000-0000-0000-0000-000000000506','`+sourceID+`','Native@example.invalid','token','isolated-hash','2026-11-01T00:00:00Z');
        INSERT INTO public.cloud_vendor_costs(id,provider,account_id,service_name,usage_start_time,usage_end_time,cost_amount) VALUES('00000000-0000-0000-0000-000000000507','gcp','fixture','compute','2026-10-01T00:00:00Z','2026-10-02T00:00:00Z',12.75)`)

		exec(native, `INSERT INTO public.referral_verified_invoices(invoice_id,provider_price_id,plan_id) VALUES('00000000-0000-0000-0000-000000000501','price_native_verified','PRO-YEARLY')`)
		// Preserve the referral tables as real business facts, not just
		// empty schema declarations. Values are fictional local fixture data.
		exec(native, `INSERT INTO public.users(uuid,username,password,email,proxy_uuid,email_verified_at)
 VALUES('00000000-0000-0000-0000-000000000801','referral-invitee','isolated-password','invitee@example.invalid','00000000-0000-0000-0000-000000000802','2026-10-13T00:00:00Z');
 INSERT INTO public.subscriptions(uuid,user_uuid,provider,payment_method,kind,plan_id,external_id,status,meta)
 VALUES('00000000-0000-0000-0000-000000000803','`+sourceID+`','stripe','stripe','subscription','PRO-YEARLY','native-referral-sub','active','{"product_slug":"xconnect","startsAt":"2026-10-01T00:00:00Z","expiresAt":"2026-11-01T00:00:00Z"}');
 INSERT INTO public.referral_campaign_revisions(enabled,effective_at,actor_uuid,reason) VALUES(true,'1970-01-01Z','`+sourceID+`','explicit native source fixture');
 INSERT INTO public.referral_codes(user_uuid,code) VALUES('`+sourceID+`','NATIVE123');
 INSERT INTO public.referrals(id,referrer_uuid,invitee_uuid,referral_code)
 VALUES('00000000-0000-0000-0000-000000000804','`+sourceID+`','00000000-0000-0000-0000-000000000801','NATIVE123');
 INSERT INTO public.referral_rewards(id,referral_id,provider_invoice_id,referrer_uuid,invitee_uuid,plan_id,product_slug,paid_minor,reward_minor,currency,paid_at,eligible_at,annual_days,status)
 VALUES('00000000-0000-0000-0000-000000000805','00000000-0000-0000-0000-000000000804','native-referral-invoice','`+sourceID+`','00000000-0000-0000-0000-000000000801','PRO-YEARLY','xconnect',10000,1000,'USD','2026-10-13T00:00:00Z','2026-10-20T00:00:00Z',30,'settled');
 INSERT INTO public.referral_reward_events(reward_id,source_key,event_type,amount_delta_minor,reason)
 VALUES('00000000-0000-0000-0000-000000000805','award:native-referral-invoice','award',1000,'qualification fixture');
 INSERT INTO public.referral_payment_risks(source_key,provider_invoice_id,risk_type,reason)
 VALUES('native-referral-pre-invoice-risk','native-unrewarded-invoice','refund','qualification tombstone');
 INSERT INTO public.referral_settlements(id,referrer_uuid,currency,amount_minor,status,payout_reference,completed_at,completed_by)
 VALUES('00000000-0000-0000-0000-000000000806','`+sourceID+`','USD',1000,'paid','native-fixture-offline-transfer','2026-10-20T00:00:00Z','`+sourceID+`');
 INSERT INTO public.referral_settlement_items(settlement_id,reward_id,amount_minor)
 VALUES('00000000-0000-0000-0000-000000000806','00000000-0000-0000-0000-000000000805',1000);
 INSERT INTO public.subscription_extension_ledger(reward_id,user_uuid,subscription_uuid,days,effective_from,effective_until)
 VALUES('00000000-0000-0000-0000-000000000805','`+sourceID+`','00000000-0000-0000-0000-000000000803',30,'2026-11-01T00:00:00Z','2026-12-01T00:00:00Z')`)
		exec(native, `INSERT INTO public.email_preferences(user_id,product_updates,tutorials,offers,frequency,version,updated_at)
 VALUES('`+sourceID+`',true,false,false,'monthly',7,'2026-10-01T00:00:00Z');
 INSERT INTO public.email_preference_events(id,user_id,source,policy_version,preferences,created_at)
 VALUES('consent-fixture','`+sourceID+`','account_settings','email-preferences-v1','{"productUpdates":true,"offers":false}','2026-10-01T00:00:00Z');
 INSERT INTO public.email_preference_tokens(hash,user_id,email,purpose,expires_at)
 VALUES('isolated-hashed-token-fixture','`+sourceID+`','native@example.invalid','unsubscribe','2027-01-01T00:00:00Z');
 INSERT INTO public.email_management_settings(id,version,company_name,postal_address,contact_email,console_url,updated_by,updated_at)
 VALUES('default',1,'Example LLC','Example Street','support@example.invalid','https://console.example','`+sourceID+`','2026-10-01T00:00:00Z')`)
		exec(native, `INSERT INTO public.email_campaigns(id,version,kind,subject,body,action_label,action_url,audience,delivery_mode,status,created_by,created_at,updated_at)
 VALUES('campaign-fixture',1,'feature','Feature fixture','Details','','','all','important','draft','`+sourceID+`','2026-10-01T00:00:00Z','2026-10-01T00:00:00Z');
 INSERT INTO public.email_onboarding_steps(id,version,enabled,subject,body,action_label,action_url,delay_hours,activated_at,updated_by,updated_at)
 VALUES('hello',1,false,'Hello','Account guide','','',0,'2026-10-01T00:00:00Z','`+sourceID+`','2026-10-01T00:00:00Z');
 INSERT INTO public.email_deliveries(id,dedupe_key,campaign_id,step_id,user_id,email,subject,body,action_label,action_url,category,delivery_mode,audience,status,attempts,due_at,lease_token,last_error,created_at,updated_at)
 VALUES('delivery-fixture','onboarding:hello:`+sourceID+`','','hello','`+sourceID+`','native@example.invalid','Hello','Account guide','','','','important','all','queued',0,'2026-10-01T00:00:00Z','','','2026-10-01T00:00:00Z','2026-10-01T00:00:00Z');
 INSERT INTO public.email_recipient_leases(user_id,token,expires_at)
 VALUES('`+sourceID+`','lease-fixture','2026-10-01T00:00:00Z');
 INSERT INTO public.email_suppressions(email,reason,updated_by,created_at)
 VALUES('bounce@example.invalid','bounce','`+sourceID+`','2026-10-01T00:00:00Z')`)
		readonlyNative := "postgres://readonly_release:isolated-readonly-fixture@" + cfg.Host + ":" + fmtPort(cfg.Port) + "/full_business_native_source?sslmode=disable"
		copyOptions := options
		copyOptions.CompareOnly = false
		copyOptions.DryRun = false
		// A later insert mutates an earlier table. End-of-transaction whole
		// scope verification must catch it, even though row counts match.
		exec(target, `CREATE FUNCTION public.fixture_late_mutation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN UPDATE public.cloud_vendor_costs SET cost_amount=99; RETURN NEW; END $$;
        CREATE TRIGGER fixture_late_mutation AFTER INSERT ON public.users FOR EACH ROW EXECUTE FUNCTION public.fixture_late_mutation()`)
		if _, e = CopyFullBusiness(context.Background(), readonlyNative, targetDSN, copyOptions); e == nil {
			t.Fatal("late insert trigger mutation accepted")
		}
		if e = target.QueryRow(`SELECT count(*) FROM public.cloud_vendor_costs`).Scan(&count); e != nil || count != 0 {
			t.Fatal("late equality failure retained target facts")
		}
		exec(target, `DROP TRIGGER fixture_late_mutation ON public.users; DROP FUNCTION public.fixture_late_mutation()`)
		copied, e := CopyFullBusiness(context.Background(), readonlyNative, targetDSN, copyOptions)
		if e != nil || copied.SourceTables != 79 || len(copied.Tables) != 79 || copied.Tables["finance_payments"].Rows != 1 || copied.Tables["finance_operation_events"].Rows != 1 {
			t.Fatalf("native finance copy failed: %v", e)
		}

		for _, table := range []string{"email_campaigns", "email_onboarding_steps", "email_deliveries", "email_recipient_leases", "email_suppressions", "email_preferences", "email_preference_tokens", "email_preference_events", "email_management_settings", "referral_verified_invoices", "referral_campaign_revisions", "referral_reward_campaign_snapshots", "referral_codes", "referrals", "referral_rewards", "referral_reward_events", "referral_payment_risks", "referral_settlements", "referral_settlement_items", "subscription_extension_ledger"} {
			if copied.Tables[table].Rows != 1 {
				t.Fatalf("referral business table was not copied exactly: %s", table)
			}
		}
		if e = target.QueryRow(`SELECT reward_minor FROM public.referral_rewards WHERE provider_invoice_id='native-referral-invoice'`).Scan(&minimum); e != nil || minimum != 1000 {
			t.Fatal("referral financial minor units changed")
		}
		// Reward insert guards can create the exact snapshot before its copy
		// page arrives; conflicting history must still reject the entire copy.
		snapshotTx, err := target.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = insertBusiness(context.Background(), snapshotTx, "referral_reward_campaign_snapshots", tables["referral_reward_campaign_snapshots"], []rawRow{{"reward_id": json.RawMessage(`"00000000-0000-0000-0000-000000000805"`), "campaign_revision": json.RawMessage(`999`)}}); err == nil {
			_ = snapshotTx.Rollback()
			t.Fatal("different pre-existing campaign snapshot accepted")
		}
		_ = snapshotTx.Rollback()
		var days int
		if e = target.QueryRow(`SELECT days FROM public.subscription_extension_ledger`).Scan(&days); e != nil || days != 30 {
			t.Fatal("referral entitlement days changed")
		}
		if e = target.QueryRow(`SELECT amount_minor FROM public.finance_payments`).Scan(&minimum); e != nil || minimum != 9007199254740993 {
			t.Fatal("finance minor integer precision lost")
		}
		var invoicePeriodOK bool
		if e = target.QueryRow(`SELECT service_period_start='2026-10-01T00:00:00Z'::timestamptz AND service_period_end='2026-11-01T00:00:00Z'::timestamptz FROM public.finance_invoices`).Scan(&invoicePeriodOK); e != nil || !invoicePeriodOK {
			t.Fatal("verified immutable invoice service period changed")
		}
		if e = target.QueryRow(`SELECT nextval('public.finance_operation_events_id_seq')`).Scan(&minimum); e != nil || minimum != 45 {
			t.Fatal("finance identity sequence not advanced")
		}
		copyOptions.CompareOnly = true
		if _, e = CopyFullBusiness(context.Background(), readonlyNative, targetDSN, copyOptions); e != nil {
			t.Fatalf("native63 compare failed: %v", e)
		}

		t.Run("older_finance_source_retains_null_period", func(t *testing.T) {
			// Synthetic older-source shape: these columns did not exist in
			// the prior release. Native target defaults remain unknown (NULL).
			exec(native, `ALTER TABLE public.finance_invoices DROP COLUMN service_period_start, DROP COLUMN service_period_end`)
			exec(target, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
			if _, e = InitializeNative(context.Background(), targetDSN, NativeInitOptions{Environment: "prod", SchemaSHA256: manifest.SchemaSHA256, WritersPaused: true, LockTimeout: time.Second, StatementTimeout: time.Minute}); e != nil {
				t.Fatal(e)
			}
			exec(target, string(billing)+`; UPDATE public.schema_migrations SET version=`+strconv.FormatUint(uint64(FullBusinessVersion), 10))
			copyOptions.CompareOnly = false
			if _, e = CopyFullBusiness(context.Background(), readonlyNative, targetDSN, copyOptions); e != nil {
				t.Fatal(e)
			}
			if e = target.QueryRow(`SELECT service_period_start IS NULL AND service_period_end IS NULL AND amount_minor=9007199254740993 FROM public.finance_invoices`).Scan(&invoicePeriodOK); e != nil || !invoicePeriodOK {
				t.Fatal("older invoice facts or NULL period defaults changed")
			}
			copyOptions.CompareOnly = true
			if _, e = CopyFullBusiness(context.Background(), readonlyNative, targetDSN, copyOptions); e != nil {
				t.Fatal(e)
			}
		})
	})

}
func fmtPort(port uint16) string { return strconv.FormatUint(uint64(port), 10) }
