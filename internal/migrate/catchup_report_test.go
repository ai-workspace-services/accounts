package migrate

import (
	"strings"
	"testing"
	"time"
)

func TestCatchupReportHashBindsCountsAndTarget(t *testing.T) {
	r := CatchupPreview{RequestID: "11111111-1111-4111-8111-111111111111", TargetIdentitySHA256: "original", Tables: map[string]CatchupTableSummary{"users": {Matches: 1}}}
	r.ReportSHA256 = catchupPreviewHash(r)
	if r.ReportSHA256 != catchupPreviewHash(r) {
		t.Fatal("unstable report hash")
	}
	changed := r
	changed.TargetIdentitySHA256 = "different"
	if catchupPreviewHash(changed) == r.ReportSHA256 {
		t.Fatal("target identity not bound")
	}
	changed = r
	changed.Tables = map[string]CatchupTableSummary{"users": {Matches: 2}}
	if catchupPreviewHash(changed) == r.ReportSHA256 {
		t.Fatal("counts not bound")
	}
}

func TestCatchupRevalidationRefusesDriftAndConflicts(t *testing.T) {
	_, order, err := migrationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	r := CatchupPreview{RequestID: "11111111-1111-4111-8111-111111111111", ToolSHA256: migrationExecutableSHA256(), CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), BaselinePlanID: "22222222-2222-4222-8222-222222222222", BaselinePlanSHA256: strings.Repeat("a", 64), SourceSnapshotID: "33333333-3333-4333-8333-333333333333", SourceSnapshotSHA256: strings.Repeat("b", 64), TargetIdentitySHA256: strings.Repeat("c", 64), TargetSchemaSHA256: strings.Repeat("d", 64), TargetBaselineSHA256: strings.Repeat("e", 64), Tables: map[string]CatchupTableSummary{}}
	for _, name := range order {
		r.Tables[name] = CatchupTableSummary{}
	}
	r.ReportSHA256 = catchupPreviewHash(r)
	if err = verifyCatchupRevalidation(r, r, now); err != nil {
		t.Fatal(err)
	}
	changed := r
	changed.TargetBaselineSHA256 = "different"
	if verifyCatchupRevalidation(r, changed, now) == nil {
		t.Fatal("target drift accepted")
	}
	changed = r
	changed.SourceSnapshotSHA256 = "different"
	if verifyCatchupRevalidation(r, changed, now) == nil {
		t.Fatal("source drift accepted")
	}
	if verifyCatchupRevalidation(r, r, now.Add(2*time.Hour)) == nil {
		t.Fatal("expired preview accepted")
	}
	changed = r
	changed.Tables = map[string]CatchupTableSummary{}
	for k, v := range r.Tables {
		changed.Tables[k] = v
	}
	changed.Tables["users"] = CatchupTableSummary{Conflicts: 1}
	changed.ReportSHA256 = catchupPreviewHash(changed)
	if verifyCatchupRevalidation(changed, changed, now) == nil {
		t.Fatal("conflicts accepted")
	}
}
