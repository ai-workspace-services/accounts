package store

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	schema "account/sql"
	"github.com/google/uuid"
)

// The regression initializes only an explicitly supplied empty disposable database.
func TestAdminPlanGroupsNativeSchema(t *testing.T) {
	dsn := os.Getenv("TEAM_QUOTA_NATIVE_TEST_DSN")
	if dsn == "" {
		t.Skip("disposable PostgreSQL database required")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var present bool
	if err = db.QueryRowContext(ctx, "SELECT to_regclass('public.users') IS NOT NULL").Scan(&present); err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatal("refusing to initialize an existing users schema")
	}
	artifact, _, err := schema.NativeArtifact()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, string(artifact)+"\nSET search_path TO public;\n"); err != nil {
		t.Fatal(err)
	}
	// Selfhost's transferred native users omit the optional legacy date columns.
	if _, err = db.ExecContext(ctx, "ALTER TABLE users DROP COLUMN IF EXISTS subscription_valid_from, DROP COLUMN IF EXISTS subscription_valid_until"); err != nil {
		t.Fatal(err)
	}
	st := &postgresStore{db: db}
	id := uuid.NewString()
	_, err = db.ExecContext(ctx, `INSERT INTO users(uuid,username,email,password,proxy_uuid,role,groups,permissions,active,created_at,updated_at,version) VALUES ($1,'native-team-member','native-team@example.invalid','unused',$2,'user','["limit:monthly-free"]','[]',true,now(),now(),0)`, id, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	user, err := st.GetUserByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(-time.Hour)
	end := start.AddDate(0, 1, 0)
	if err = st.UpsertAccountBillingProfile(ctx, &AccountBillingProfile{AccountUUID: id, PackageName: "free", IncludedQuotaBytes: 5 * gib, PricingRuleVersion: "plan:FREE"}); err != nil {
		t.Fatal(err)
	}
	if err = st.UpsertAccountQuotaState(ctx, &AccountQuotaState{AccountUUID: id, RemainingIncludedQuota: gib, PeriodStart: &start, PeriodEnd: &end, EffectiveAt: start}); err != nil {
		t.Fatal(err)
	}
	change := AdminPlanGroupChange{UserID: id, PlanID: BillingPlanPlus, SetEntitlement: true, ExpectedGroups: user.Groups, Groups: []string{MonthlyPlusQuotaLimitGroup}, PackageName: "plus", IncludedQuotaBytes: 20 * gib, RegionMultiplier: 1, LineMultiplier: 1, PeakMultiplier: 1, OffPeakMultiplier: 1, PricingRuleVersion: "plan:PLUS"}
	change.SubscriptionRecord = &SubscriptionRecord{UserID: id, SubscriptionType: "year", MonthlyQuotaBytes: 20 * gib, AnnualQuotaBytes: 240 * gib, FeeMinor: 20000, PaymentStatus: "paid", Note: "persisted import"}
	preview := AdminPlanGroupPreview{ActorUUID: id, Reason: "native schema regression", RequestID: "native-quota", TokenHash: "native-token", ExpiresAt: time.Now().UTC().Add(time.Minute), Changes: []AdminPlanGroupChange{change}}
	changes, err := st.CreateAdminPlanGroupPreview(ctx, preview)
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].RemainingAfter != 16*gib {
		t.Fatalf("usage not preserved: %d", changes[0].RemainingAfter)
	}
	batch := AdminPlanGroupBatch{ActorUUID: id, Reason: preview.Reason, RequestID: preview.RequestID, PreviewTokenHash: preview.TokenHash}
	if _, _, err = st.ApplyAdminPlanGroupBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	quota, err := st.GetAccountQuotaState(ctx, id)
	if err != nil || quota.RemainingIncludedQuota != 16*gib {
		t.Fatalf("quota=%+v err=%v", quota, err)
	}

	// Exercise the reset using native UUID keys and audit JSON without adding schema.
	reset, err := st.ResetMonthlyUsage(ctx, id, id, user.Email, "native reset regression", time.Now().UTC())
	if err != nil || reset.RemainingIncludedQuota != 20*gib {
		t.Fatalf("native reset=%+v err=%v", reset, err)
	}
	var auditCount int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM audit_logs WHERE action=$1", AuditActionMonthlyUsageReset).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("reset audit: %d %v", auditCount, err)
	}
	// Restore the quota snapshot needed by the original replay/rollback checks.
	if err = st.UpsertAccountQuotaState(ctx, quota); err != nil {
		t.Fatal(err)
	}
	// The same request replays without creating another row or granting quota.
	if _, replayed, err := st.ApplyAdminPlanGroupBatch(ctx, batch); err != nil || !replayed {
		t.Fatalf("idempotent replay failed: %v %v", replayed, err)
	}
	reopened := &postgresStore{db: db}
	record, err := reopened.GetSubscriptionRecord(ctx, id)
	if err != nil || record.FeeMinor != 20000 || record.Note != "persisted import" {
		t.Fatalf("record not durable: %+v %v", record, err)
	}
	rows, err := reopened.ListSubscriptionRecords(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatal("replay duplicated subscription record")
	}
	rollback := change
	rollback.ExpectedGroups = change.Groups
	rollback.Groups = []string{MonthlyFreeQuotaLimitGroup}
	rollback.PlanID = BillingPlanFree
	rollback.IncludedQuotaBytes = 3 * gib
	rollback.SubscriptionRecord = &SubscriptionRecord{UserID: id, FeeMinor: -1}
	rollbackPreview := AdminPlanGroupPreview{ActorUUID: id, Reason: "atomic failure", RequestID: "atomic-failure", TokenHash: "atomic-failure", ExpiresAt: time.Now().Add(time.Minute), Changes: []AdminPlanGroupChange{rollback}}
	if _, err := st.CreateAdminPlanGroupPreview(ctx, rollbackPreview); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ApplyAdminPlanGroupBatch(ctx, AdminPlanGroupBatch{ActorUUID: id, Reason: "atomic failure", RequestID: "atomic-failure", PreviewTokenHash: "atomic-failure"}); err == nil {
		t.Fatal("invalid record didn't rollback transaction")
	}
	current, _ := st.GetUserByID(ctx, id)
	record, _ = st.GetSubscriptionRecord(ctx, id)
	quota, _ = st.GetAccountQuotaState(ctx, id)
	if MonthlyQuotaGroup(current) != MonthlyPlusQuotaLimitGroup || record.FeeMinor != 20000 || quota.RemainingIncludedQuota != 16*gib {
		t.Fatal("record constraint failure partially updated account")
	}
	// Date-only updates without a commercial record still require legacy columns.
	change.ExpectedGroups = change.Groups
	change.SetValidFrom = true
	preview.RequestID = "native-dates"
	preview.TokenHash = "native-dates-token"
	unsupported := change
	unsupported.SubscriptionRecord = nil
	preview.Changes = []AdminPlanGroupChange{unsupported}
	if _, err = st.CreateAdminPlanGroupPreview(ctx, preview); err == nil {
		t.Fatal("unsupported subscription date mutation accepted")
	}
	team := NetworkTeam{ID: "native-team", Name: "Custom Team", MemberLimit: 10, Active: true, ValidFrom: start, ValidUntil: end}
	if err = st.PutNetworkTeam(ctx, team, &AuditLog{Action: "network.team.upsert", ActorUUID: id}); err != nil {
		t.Fatal(err)
	}
	member := NetworkMembership{UserID: id, TeamID: team.ID, DedicatedOnly: true, Role: "admin"}
	audit := &AuditLog{Action: "network.membership.set", ActorUUID: id, Details: map[string]any{"platform_root": true}}
	if err = st.PutNetworkMembership(ctx, member, audit); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetUserByID(ctx, id)
	if err != nil || len(stored.Groups) != 2 {
		t.Fatalf("team role or quota lost: %+v %v", stored, err)
	}
	member.Role = "member"
	if err = st.PutNetworkMembership(ctx, member, nil); err == nil {
		t.Fatal("unaudited role mutation committed")
	}
	stored, err = st.GetUserByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	admin := false
	for _, g := range stored.Groups {
		if g == "team-admin:"+team.ID {
			admin = true
		}
	}
	if !admin {
		t.Fatal("failed audit did not roll back team administrator revocation")
	}
	free := &User{Name: "free-migration", Email: "free-migration@example.invalid", Role: RoleUser, Groups: []string{"segment:quota:free-5gb"}, ProxyUUID: uuid.NewString(), Active: true}
	if err := st.CreateUser(ctx, free); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccountBillingProfile(ctx, &AccountBillingProfile{AccountUUID: free.ID, PackageName: "free", IncludedQuotaBytes: 5 * gib, PricingRuleVersion: "plan:FREE"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccountQuotaState(ctx, &AccountQuotaState{AccountUUID: free.ID, RemainingIncludedQuota: 2 * gib, PeriodStart: &start, PeriodEnd: &end, EffectiveAt: start}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		for _, name := range []string{"2026101501_free_quota_3gb.up.sql", "2026101502_subscription_records.up.sql"} {
			body, err := os.ReadFile("../../sql/migrations/" + name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, string(body)); err != nil {
				t.Fatalf("migration %s: %v", name, err)
			}
		}
	}
	freeState, _ := st.GetAccountQuotaState(ctx, free.ID)
	freeProfile, _ := st.GetAccountBillingProfile(ctx, free.ID)
	freeStored, _ := st.GetUserByID(ctx, free.ID)
	if freeState.RemainingIncludedQuota != 0 || freeProfile.IncludedQuotaBytes != 3*gib || MonthlyQuotaGroup(freeStored) != MonthlyFreeQuotaLimitGroup {
		t.Fatal("free migration didn't preserve consumed quota or migrate the group")
	}
	rows, _ = st.ListSubscriptionRecords(ctx)
	if len(rows) != 1 {
		t.Fatal("reapplied migration lost subscription records")
	}

	// Imported records provide validity on native schemas without legacy columns.
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2030, 12, 31, 0, 0, 0, 0, time.UTC)
	imported := AdminPlanGroupChange{UserID: free.ID, PlanID: "PRO", ExpectedGroups: freeStored.Groups, Groups: WithSubscriptionSourcePlan([]string{MonthlyPlusQuotaLimitGroup}, "PRO"), SetEntitlement: true, PackageName: "pro", IncludedQuotaBytes: 50 * gib, PricingRuleVersion: "plan:PRO", SetValidFrom: true, SetValidUntil: true, ValidFrom: &from, ValidUntil: &until, SubscriptionRecord: &SubscriptionRecord{UserID: free.ID, PlanCode: "PRO", SubscriptionType: "month", MonthlyQuotaBytes: 50 * gib, ValidFrom: &from, ValidUntil: &until}}
	importedPreview := AdminPlanGroupPreview{ActorUUID: id, Reason: "native date fallback", RequestID: "native-date-fallback", TokenHash: "native-date-fallback", ExpiresAt: time.Now().Add(time.Minute), Changes: []AdminPlanGroupChange{imported}}
	if _, err = st.CreateAdminPlanGroupPreview(ctx, importedPreview); err != nil {
		t.Fatal(err)
	}
	// Traffic arriving after preview must be counted, including over-cap traffic.
	if err = st.UpsertTrafficMinuteBucket(ctx, &TrafficMinuteBucket{
		AccountUUID: free.ID, NodeID: "import-test", BucketStart: start.Add(time.Minute), TotalBytes: 4 * gib,
	}); err != nil {
		t.Fatal(err)
	}
	applied, _, err := st.ApplyAdminPlanGroupBatch(ctx, AdminPlanGroupBatch{ActorUUID: id, Reason: importedPreview.Reason, RequestID: importedPreview.RequestID, PreviewTokenHash: importedPreview.TokenHash})
	if err != nil {
		t.Fatal(err)
	}
	quotaAfter, err := st.GetAccountQuotaState(ctx, free.ID)
	if err != nil || quotaAfter.RemainingIncludedQuota != 46*gib || len(applied) != 1 ||
		applied[0].UsedBytesPreserved != 4*gib || applied[0].RemainingAfter != 46*gib {
		t.Fatalf("apply did not preserve newly recorded traffic: quota=%+v applied=%+v err=%v", quotaAfter, applied, err)
	}
	reloaded, err := st.GetUserByID(ctx, free.ID)
	if err != nil || reloaded.SubscriptionValidUntil == nil || !reloaded.SubscriptionValidUntil.Equal(until) {
		t.Fatalf("native imported validity: %+v %v", reloaded, err)
	}
	sourceRecord, err := st.GetSubscriptionRecord(ctx, free.ID)
	if err != nil || sourceRecord.PlanCode != "PRO" {
		t.Fatalf("native source plan: %+v %v", sourceRecord, err)
	}
	history, err := st.ListAuditLogs(ctx, AuditLogFilter{ActionPrefix: AuditActionPlanGroupUpdate, TargetUUID: free.ID, Limit: 1})
	if err != nil || len(history) != 1 {
		t.Fatalf("source receipt lookup: %v %v", history, err)
	}

}
