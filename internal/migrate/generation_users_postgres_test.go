package migrate

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"testing"
	"time"

	schema "account/sql"
	"github.com/jackc/pgx/v5"
)

// This test only accepts two separately provisioned, disposable loopback
// PostgreSQL 17 account databases. It never reads production credentials.
func TestGenerationUsersPostgres17(t *testing.T) {
	sourceDSN := os.Getenv("MIGRATECTL_GENERATION_USERS_SOURCE_TEST_DSN")
	targetDSN := os.Getenv("MIGRATECTL_GENERATION_USERS_TARGET_TEST_DSN")
	if sourceDSN == "" || targetDSN == "" {
		t.Skip("isolated source and target PostgreSQL17 fixtures not configured")
	}
	for _, dsn := range []string{sourceDSN, targetDSN} {
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil || cfg.Database != "account" || (cfg.Host != "localhost" && cfg.Host != "127.0.0.1") {
			t.Fatal("test requires separately provisioned loopback account databases")
		}
	}
	ctx := context.Background()
	_, manifest, err := schema.NativeArtifact()
	if err != nil {
		t.Fatal(err)
	}
	initOptions := NativeInitOptions{Environment: "prod", SchemaSHA256: manifest.SchemaSHA256, WritersPaused: true,
		LockTimeout: time.Second, StatementTimeout: time.Minute}
	for _, dsn := range []string{sourceDSN, targetDSN} {
		if _, err = InitializeNative(ctx, dsn, initOptions); err != nil {
			t.Fatalf("fixture database must be empty before initialization: %v", err)
		}
		db, openErr := sql.Open("pgx", dsn)
		if openErr != nil {
			t.Fatal("fixture database unavailable")
		}
		billing, readErr := os.ReadFile("testdata/cloud_vendor_costs_native.sql")
		if readErr != nil {
			db.Close()
			t.Fatal(readErr)
		}
		_, err = db.ExecContext(ctx, string(billing)+`; UPDATE public.schema_migrations SET version=`+strconv.FormatUint(uint64(FullBusinessVersion), 10))
		db.Close()
		if err != nil {
			t.Fatalf("fixture billing schema setup failed: %v", err)
		}
	}
	options := GenerationUserOptions{SourceGeneration: "prod-v2", TargetGeneration: "prod-v3", Environment: "prod",
		SchemaSHA256: manifest.SchemaSHA256, BillingSHA256: FullBusinessBillingSHA256, ExpectedTargetVersion: FullBusinessVersion}
	source, err := sql.Open("pgx", sourceDSN)
	if err != nil {
		t.Fatal("source fixture unavailable")
	}
	defer source.Close()
	target, err := sql.Open("pgx", targetDSN)
	if err != nil {
		t.Fatal("target fixture unavailable")
	}
	defer target.Close()
	exec := func(db *sql.DB, query string, args ...any) {
		t.Helper()
		if _, execErr := db.ExecContext(ctx, query, args...); execErr != nil {
			t.Fatalf("isolated fixture operation failed: %v", execErr)
		}
	}
	const baselineID = "00000000-0000-0000-0000-000000000101"
	const baselineProxy = "00000000-0000-0000-0000-000000000201"
	const lateID = "00000000-0000-0000-0000-000000000102"
	const lateProxy = "00000000-0000-0000-0000-000000000202"
	insertUser := `INSERT INTO public.users(uuid,username,password,email,proxy_uuid,created_at) VALUES($1,$2,$3,$4,$5,'2020-01-01T00:00:00Z')`
	exec(source, insertUser, baselineID, "baseline", "source-password", "baseline@example.invalid", baselineProxy)
	exec(target, insertUser, baselineID, "baseline", "source-password", "baseline@example.invalid", baselineProxy)
	baseline, err := CaptureGenerationUserBaseline(ctx, sourceDSN, targetDSN, options)
	if err != nil {
		t.Fatalf("baseline capture failed: %v", err)
	}
	if len(baseline.SourceUserIDHashes) != 1 || baseline.SourceUserIDHashes[0] == baselineID {
		t.Fatal("baseline exposed a raw account UUID")
	}
	exec(source, insertUser, lateID, "late", "source-password", "late@example.invalid", lateProxy)
	audit, err := DetectLateGenerationUsers(ctx, sourceDSN, targetDSN, options, baseline)
	if err != nil || audit.SourceUsersAbsentFromBaseline != 1 || audit.LateSourceUsersMissingTarget != 1 || !audit.SourceWriteIsolationAnomaly {
		t.Fatalf("late source account was not reported: %+v, %v", audit, err)
	}
	syncOptions := SingleGenerationUserOptions{GenerationUserOptions: options, SourceUserID: lateID}
	preview, err := SyncSingleGenerationUser(ctx, sourceDSN, targetDSN, syncOptions)
	if err != nil || preview.Result != "preview_missing_user" || preview.PlanSHA256 == "" || preview.TargetWrites {
		t.Fatalf("missing-user preview failed: %+v, %v", preview, err)
	}
	syncOptions.Apply = true
	syncOptions.SourceWritersPaused = true
	syncOptions.ConfirmSourceUserID = lateID
	syncOptions.ExpectedPlanSHA256 = preview.PlanSHA256
	receipt, err := SyncSingleGenerationUser(ctx, sourceDSN, targetDSN, syncOptions)
	if err != nil || receipt.Result != "inserted" || receipt.UserRowsInserted != 1 || !receipt.TargetWrites {
		t.Fatalf("single-user insert failed: %+v, %v", receipt, err)
	}
	var password string
	if err = target.QueryRowContext(ctx, `SELECT password FROM public.users WHERE uuid=$1::uuid`, lateID).Scan(&password); err != nil || password != "source-password" {
		t.Fatal("source login credential was not inserted")
	}
	exec(target, `UPDATE public.users SET password='newer-target-password' WHERE uuid=$1::uuid`, lateID)
	replay, err := SyncSingleGenerationUser(ctx, sourceDSN, targetDSN, syncOptions)
	if err != nil || replay.Result != "already_present_preserved" {
		t.Fatalf("idempotent replay failed: %+v, %v", replay, err)
	}
	if err = target.QueryRowContext(ctx, `SELECT password FROM public.users WHERE uuid=$1::uuid`, lateID).Scan(&password); err != nil || password != "newer-target-password" {
		t.Fatal("replay overwrote the target's newer password")
	}

	// Business relationships are deliberately outside this core identity tool.
	const linkedID = "00000000-0000-0000-0000-000000000104"
	const linkedProxy = "00000000-0000-0000-0000-000000000204"
	exec(source, insertUser, linkedID, "linked", "source-password", "linked@example.invalid", linkedProxy)
	exec(source, `INSERT INTO public.subscriptions(user_uuid,provider,external_id) VALUES($1::uuid,'fixture','linked-subscription')`, linkedID)
	linkedReceipt, linkedErr := SyncSingleGenerationUser(ctx, sourceDSN, targetDSN, SingleGenerationUserOptions{GenerationUserOptions: options, SourceUserID: linkedID})
	if linkedErr == nil || linkedReceipt.Result != "unsupported_associations" || linkedReceipt.TargetWrites {
		t.Fatalf("linked subscription was not rejected as out of scope: %+v, %v", linkedReceipt, linkedErr)
	}

	// Email collisions are reported before any target insert.
	const collisionID = "00000000-0000-0000-0000-000000000105"
	const collisionProxy = "00000000-0000-0000-0000-000000000205"
	exec(source, insertUser, collisionID, "collision", "source-password", "collision@example.invalid", collisionProxy)
	exec(target, insertUser, "00000000-0000-0000-0000-000000000106", "target-collision", "target-password", "collision@example.invalid", "00000000-0000-0000-0000-000000000206")
	collisionReceipt, collisionErr := SyncSingleGenerationUser(ctx, sourceDSN, targetDSN, SingleGenerationUserOptions{GenerationUserOptions: options, SourceUserID: collisionID})
	if collisionErr == nil || collisionReceipt.Result != "identity_conflict" || collisionReceipt.TargetWrites {
		t.Fatalf("email identity collision was not rejected: %+v, %v", collisionReceipt, collisionErr)
	}

	// A failure after the user insert must roll that insert back with the OAuth row.
	const rollbackID = "00000000-0000-0000-0000-000000000103"
	const rollbackProxy = "00000000-0000-0000-0000-000000000203"
	exec(source, insertUser, rollbackID, "rollback", "source-password", "rollback@example.invalid", rollbackProxy)
	exec(source, `INSERT INTO public.identities(uuid,provider,external_id,user_uuid) VALUES('00000000-0000-0000-0000-000000000303','fixture-fail','external-rollback',$1::uuid)`, rollbackID)
	exec(target, `CREATE FUNCTION public.reject_fixture_identity() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.provider='fixture-fail' THEN RAISE EXCEPTION 'injected fixture failure'; END IF; RETURN NEW; END $$`)
	exec(target, `CREATE TRIGGER reject_fixture_identity BEFORE INSERT ON public.identities FOR EACH ROW EXECUTE FUNCTION public.reject_fixture_identity()`)
	rollbackPreview, err := SyncSingleGenerationUser(ctx, sourceDSN, targetDSN, SingleGenerationUserOptions{GenerationUserOptions: options, SourceUserID: rollbackID})
	if err != nil || rollbackPreview.Result != "preview_missing_user" {
		t.Fatalf("rollback preview failed: %+v, %v", rollbackPreview, err)
	}
	rollbackOptions := SingleGenerationUserOptions{GenerationUserOptions: options, SourceUserID: rollbackID,
		Apply: true, SourceWritersPaused: true, ConfirmSourceUserID: rollbackID, ExpectedPlanSHA256: rollbackPreview.PlanSHA256}
	if _, err = SyncSingleGenerationUser(ctx, sourceDSN, targetDSN, rollbackOptions); err == nil {
		t.Fatal("injected OAuth write failure unexpectedly committed")
	}
	var rollbackCount int
	if err = target.QueryRowContext(ctx, `SELECT count(*) FROM public.users WHERE uuid=$1::uuid`, rollbackID).Scan(&rollbackCount); err != nil || rollbackCount != 0 {
		t.Fatal("user insert was not rolled back after OAuth failure")
	}
}
