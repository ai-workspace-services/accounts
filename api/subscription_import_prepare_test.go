package api

import (
	"context"
	"strings"
	"testing"
)

func TestPrepareSubscriptionImportTSVSkipsUnknown(t *testing.T) {
	_, st, _, target := opsHarness(t)
	h := &handler{store: st}
	source := "邮箱\t用户名\t订阅开始\t订阅结束\t\t订阅类型\n " + strings.ToUpper(target.Email) + " \tignored\t2026-01-01\t2030-01-01\tplus\tyear\nmissing@example.invalid\t\t\t\tplus\tmonth"
	normalized, skipped, err := h.prepareSubscriptionImport(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0]["reason"] != "user_not_found" {
		t.Fatalf("skipped=%v", skipped)
	}
	if !strings.Contains(normalized, "套餐") || strings.Contains(normalized, "missing@example.invalid") {
		t.Fatalf("unexpected normalized import: %s", normalized)
	}
}
