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
	exec(target, string(billing)+`; UPDATE public.schema_migrations SET version=2026100701`)
	// Qualify the old 44-table source -> latest 53-table target explicitly. New
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
	if e != nil || receipt.Result != "copied" || !receipt.FullBusinessEqual || !receipt.TargetWrites || receipt.DatabaseCutoverApproved || len(receipt.Tables) != 53 || receipt.Tables["billing_ledger"].Rows != 1003 || receipt.Tables["traffic_minute_buckets"].Rows != 1003 {
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
		coreOptions := CoreUsersOptions{Environment: "prod", SchemaSHA256: manifest.SchemaSHA256,
			BillingSHA256: FullBusinessBillingSHA256, WritersPaused: true}
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
		r, e := CopyCoreUsers(context.Background(), sourceDSN, targetDSN, coreOptions)
		check(r, false, e)
		var versionBefore, versionAfter int64
		if e = target.QueryRow(`SELECT version FROM public.users`).Scan(&versionBefore); e != nil {
			t.Fatal("target version fixture unavailable")
		}
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
	})
	t.Run("native53_finance_and_late_trigger", func(t *testing.T) {
		// Explicit synthetic fixture reset, behind the loopback-only test DSN.
		// No runtime migration function contains DROP or disables a constraint.
		exec(target, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
		if _, e = InitializeNative(context.Background(), targetDSN, NativeInitOptions{Environment: "prod", SchemaSHA256: manifest.SchemaSHA256, WritersPaused: true, LockTimeout: time.Second, StatementTimeout: time.Minute}); e != nil {
			t.Fatal(e)
		}
		exec(target, string(billing)+`; UPDATE public.schema_migrations SET version=2026100701`)
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
		exec(native, `INSERT INTO public.finance_invoices(id,idempotency_key,account_uuid,amount_minor,currency) VALUES('00000000-0000-0000-0000-000000000501','invoice','`+sourceID+`',9007199254740993,'USD');
        INSERT INTO public.finance_payments(id,idempotency_key,invoice_id,account_uuid,amount_minor,currency) VALUES('00000000-0000-0000-0000-000000000502','payment','00000000-0000-0000-0000-000000000501','`+sourceID+`',9007199254740993,'USD');
        INSERT INTO public.finance_refunds(id,idempotency_key,payment_id,amount_minor,currency) VALUES('00000000-0000-0000-0000-000000000503','refund','00000000-0000-0000-0000-000000000502',123,'USD');
        INSERT INTO public.finance_operations(id,idempotency_key,operation_type,status) VALUES('00000000-0000-0000-0000-000000000504','operation','payment','succeeded');
        INSERT INTO public.finance_operation_events(id,operation_id,attempt,event_type,status) OVERRIDING SYSTEM VALUE VALUES(44,'00000000-0000-0000-0000-000000000504',1,'qualification','succeeded');
        INSERT INTO public.account_lifecycle_events(transition_id,user_uuid,from_state,to_state,actor_type,actor_ref) VALUES('00000000-0000-0000-0000-000000000505','`+sourceID+`','active','archived','user','`+sourceID+`');
        INSERT INTO public.password_recovery_challenges(id,user_uuid,email_snapshot,challenge_kind,secret_hash,expires_at) VALUES('00000000-0000-0000-0000-000000000506','`+sourceID+`','Native@example.invalid','token','isolated-hash','2026-11-01T00:00:00Z');
        INSERT INTO public.cloud_vendor_costs(id,provider,account_id,service_name,usage_start_time,usage_end_time,cost_amount) VALUES('00000000-0000-0000-0000-000000000507','gcp','fixture','compute','2026-10-01T00:00:00Z','2026-10-02T00:00:00Z',12.75)`)
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
		if e != nil || copied.SourceTables != 53 || len(copied.Tables) != 53 || copied.Tables["finance_payments"].Rows != 1 || copied.Tables["finance_operation_events"].Rows != 1 {
			t.Fatalf("native finance copy failed: %v", e)
		}
		if e = target.QueryRow(`SELECT amount_minor FROM public.finance_payments`).Scan(&minimum); e != nil || minimum != 9007199254740993 {
			t.Fatal("finance minor integer precision lost")
		}
		if e = target.QueryRow(`SELECT nextval('public.finance_operation_events_id_seq')`).Scan(&minimum); e != nil || minimum != 45 {
			t.Fatal("finance identity sequence not advanced")
		}
		copyOptions.CompareOnly = true
		if _, e = CopyFullBusiness(context.Background(), readonlyNative, targetDSN, copyOptions); e != nil {
			t.Fatalf("native53 compare failed: %v", e)
		}
	})

}
func fmtPort(port uint16) string { return strconv.FormatUint(uint64(port), 10) }
