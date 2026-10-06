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
		if strings.HasPrefix(c.Name, "account_lifecycle_") {
			exec(sourceAdmin, "ALTER TABLE public.users DROP COLUMN "+quoteBusiness(c.Name)+" CASCADE")
		}
	}
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
	var before string
	if e = sourceAdmin.QueryRow(`SELECT md5(string_agg(to_jsonb(u)::text,'' ORDER BY uuid)) FROM public.users u`).Scan(&before); e != nil {
		t.Fatal(e)
	}
	preview, e := CopyFullBusiness(context.Background(), sourceDSN, targetDSN, options)
	if e != nil || preview.Result != "eligible" || preview.SourceTables != 44 || preview.FullBusinessEqual || preview.TargetWrites || len(preview.Tables) != 0 {
		t.Fatalf("preview: %#v %v", preview, e)
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
}
func fmtPort(port uint16) string { return strconv.FormatUint(uint64(port), 10) }
