package migrate

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	schema "account/sql"
	"github.com/jackc/pgx/v5"
)

func TestFullBusinessUsageMergePostgres17(t *testing.T) {
	dsn := os.Getenv("MIGRATECTL_USAGE_MERGE_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated usage-merge PostgreSQL17 fixture not configured")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Database != "account" || (cfg.Host != "127.0.0.1" && cfg.Host != "localhost") {
		t.Fatal("requires an isolated loopback account fixture")
	}
	target, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	sourceDSN := "host=" + cfg.Host + " port=" + fmtPort(cfg.Port) + " user=" + cfg.User + " dbname=usage_merge_source sslmode=disable"
	if cfg.Password != "" {
		sourceDSN += " password=" + cfg.Password
	}
	source, err := sql.Open("pgx", sourceDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	exec := func(db *sql.DB, query string) {
		t.Helper()
		if _, e := db.Exec(query); e != nil {
			t.Fatal(e)
		}
	}
	body, manifest, err := schema.NativeArtifact()
	if err != nil {
		t.Fatal(err)
	}
	exec(source, string(body))
	_, err = InitializeNative(context.Background(), dsn, NativeInitOptions{Environment: "prod", SchemaSHA256: manifest.SchemaSHA256, WritersPaused: true, LockTimeout: time.Second, StatementTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	billing, err := os.ReadFile("testdata/cloud_vendor_costs_native.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(target, string(billing)+`; UPDATE public.schema_migrations SET version=`+strconv.Itoa(FullBusinessVersion)+`;
CREATE TABLE public.accounts_controlled_migration_history(from_version bigint NOT NULL,to_version bigint PRIMARY KEY,sql_sha256 text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now());
INSERT INTO public.accounts_controlled_migration_history(from_version,to_version,sql_sha256) VALUES
(2026101703,2026101901,'b9fee298512e7d888fe619917c8238120f0e227f4ea348e59397d074c41f878d'),
(2026101901,2026101902,'53f9aaf7c56a14fd7c132c6649b79582cd2063f8903b94efb4e23cbf4895437c');`)
	const srcID = "00000000-0000-0000-0000-000000000101"
	const dstID = "00000000-0000-0000-0000-000000000102"
	const proxy = "00000000-0000-0000-0000-000000000201"
	for _, fixture := range []struct {
		db *sql.DB
		id string
	}{{source, srcID}, {target, dstID}} {
		exec(fixture.db, `INSERT INTO public.users(uuid,username,email,password,proxy_uuid,email_verified_at) VALUES('`+fixture.id+`','fixture','usage@example.invalid','fixture-only','`+proxy+`','2026-10-01T00:00:00Z');
INSERT INTO public.account_billing_profiles(account_uuid,package_name,included_quota_bytes,created_at,updated_at) VALUES('`+fixture.id+`','free',5368709120,'2026-10-01T00:00:00Z','2026-10-01T00:00:00Z');
INSERT INTO public.account_quota_states(account_uuid,remaining_included_quota,current_balance,effective_at,updated_at) VALUES('`+fixture.id+`',100,0,'2026-10-01T00:00:00Z','2026-10-01T00:00:00Z');`)
	}
	exec(source, `UPDATE public.account_quota_states SET remaining_included_quota=50,updated_at='2026-10-02T00:00:00Z';
INSERT INTO public.billing_ledger(id,account_uuid,bucket_start,bucket_end,entry_type,rated_bytes,amount_delta,balance_after,created_at)
SELECT md5('usage-'||g::text)::uuid,'`+srcID+`','2026-10-01T00:00:00Z'::timestamptz+g*interval '1 minute','2026-10-01T00:01:00Z'::timestamptz+g*interval '1 minute','usage',9007199254740993,0.125,0,'2026-10-01T00:00:00Z' FROM generate_series(1,1003)g;
INSERT INTO public.traffic_minute_buckets(account_uuid,node_id,bucket_start,total_bytes) SELECT '`+srcID+`','fixture-node','2026-10-01T00:00:00Z'::timestamptz+g*interval '1 minute',9007199254740993 FROM generate_series(1,1003)g;`)
	options := UsageMergeOptions{SourceGeneration: "prod-v3", TargetGeneration: "prod-v4", SchemaSHA256: manifest.SchemaSHA256, BillingSHA256: FullBusinessBillingSHA256, ExpectedTargetVersion: FullBusinessVersion}
	ctx := context.Background()
	preview, err := ReconcileUsage(ctx, sourceDSN, dsn, options)
	if err != nil {
		t.Fatal(err)
	}
	if preview.TargetWrites || preview.Result != "preview" || preview.Tables["billing_ledger"].Append != 1003 || preview.Tables["account_quota_states"].Update != 1 || preview.MappedUsers != 1 || preview.UsersAfter != preview.UsersBefore {
		t.Fatalf("bad preview: %+v", preview)
	}
	checkTarget := func(wantQuota, wantLedger int64) {
		t.Helper()
		var quota, ledger int64
		if target.QueryRow(`SELECT remaining_included_quota FROM public.account_quota_states`).Scan(&quota) != nil || target.QueryRow(`SELECT count(*) FROM public.billing_ledger`).Scan(&ledger) != nil || quota != wantQuota || ledger != wantLedger {
			t.Fatalf("unexpected target state: quota=%d ledger=%d", quota, ledger)
		}
	}
	checkTarget(100, 0)
	options.Apply = true
	options.WritersPaused = true
	options.ExpectedPlanSHA256 = preview.PlanSHA256
	// A changed source requires a new preview even if the change is harmless.
	exec(source, `UPDATE public.account_quota_states SET remaining_included_quota=49`)
	if _, err = ReconcileUsage(ctx, sourceDSN, dsn, options); err == nil || !strings.Contains(err.Error(), "repeat preview") {
		t.Fatalf("stale plan accepted: %v", err)
	}
	checkTarget(100, 0)
	exec(source, `UPDATE public.account_quota_states SET remaining_included_quota=50`)
	// A late ledger constraint must roll back an earlier quota update and all
	// inserted traffic pages. The preview remains read-only and can succeed.
	exec(target, `ALTER TABLE public.billing_ledger ADD CONSTRAINT fixture_reject CHECK(rated_bytes<0)`)
	options.Apply = false
	preview, err = ReconcileUsage(ctx, sourceDSN, dsn, options)
	if err != nil {
		t.Fatal(err)
	}
	options.Apply = true
	options.ExpectedPlanSHA256 = preview.PlanSHA256
	if _, err = ReconcileUsage(ctx, sourceDSN, dsn, options); err == nil {
		t.Fatal("late constraint failure committed")
	}
	checkTarget(100, 0)
	var traffic int64
	if target.QueryRow(`SELECT count(*) FROM public.traffic_minute_buckets`).Scan(&traffic) != nil || traffic != 0 {
		t.Fatal("traffic escaped rollback")
	}
	exec(target, `ALTER TABLE public.billing_ledger DROP CONSTRAINT fixture_reject`)
	options.Apply = false
	preview, err = ReconcileUsage(ctx, sourceDSN, dsn, options)
	if err != nil {
		t.Fatal(err)
	}
	options.Apply = true
	options.ExpectedPlanSHA256 = preview.PlanSHA256
	merged, err := ReconcileUsage(ctx, sourceDSN, dsn, options)
	if err != nil {
		t.Fatal(err)
	}
	if !merged.TargetWrites || merged.Result != "merged" || merged.SchemaBeforeSHA256 != merged.SchemaAfterSHA256 || merged.UsersBefore != merged.UsersAfter {
		t.Fatal("merge acceptance failed")
	}
	checkTarget(50, 1003)
	var exact int64
	if target.QueryRow(`SELECT min(rated_bytes) FROM public.billing_ledger`).Scan(&exact) != nil || exact != 9007199254740993 {
		t.Fatal("lost integer precision")
	}
	options.Apply = false
	replay, err := ReconcileUsage(ctx, sourceDSN, dsn, options)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Tables["billing_ledger"].Append != 0 || replay.Tables["account_quota_states"].Update != 0 {
		t.Fatal("replay was not idempotent")
	}
	options.Apply = true
	options.ExpectedPlanSHA256 = replay.PlanSHA256
	if _, err = ReconcileUsage(ctx, sourceDSN, dsn, options); err != nil {
		t.Fatal(err)
	}
	checkTarget(50, 1003)
	// Same accounting key with different money is never overwritten.
	exec(source, `UPDATE public.billing_ledger SET amount_delta=0.5 WHERE id=md5('usage-1')::uuid`)
	options.Apply = false
	conflict, err := ReconcileUsage(ctx, sourceDSN, dsn, options)
	if err == nil || conflict.Result != "conflict" || conflict.Tables["billing_ledger"].Conflicts != 1 {
		t.Fatalf("financial conflict ignored: %v", err)
	}
	checkTarget(50, 1003)
	exec(source, `UPDATE public.billing_ledger SET amount_delta=0.125 WHERE id=md5('usage-1')::uuid`)
	// Protect V3-only accounting from a replacement quota snapshot.
	exec(target, `INSERT INTO public.billing_ledger(id,account_uuid,bucket_start,bucket_end,entry_type,rated_bytes,amount_delta,balance_after) VALUES('00000000-0000-0000-0000-000000000301','`+dstID+`','2026-10-02','2026-10-03','fixture',1,0,0)`)
	exec(source, `UPDATE public.account_quota_states SET remaining_included_quota=40`)
	conflict, err = ReconcileUsage(ctx, sourceDSN, dsn, options)
	if err == nil || conflict.Conflicts == 0 || conflict.Tables["billing_ledger"].TargetOnly != 1 {
		t.Fatal("V3 accounting independence lost")
	}
	checkTarget(50, 1004)
	var sourceUsers int
	if source.QueryRow(`SELECT count(*) FROM public.users WHERE uuid='`+srcID+`'`).Scan(&sourceUsers) != nil || sourceUsers != 1 {
		t.Fatal("source identity mutated")
	}
}
