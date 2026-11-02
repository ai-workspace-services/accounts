package migrate

import "testing"

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
