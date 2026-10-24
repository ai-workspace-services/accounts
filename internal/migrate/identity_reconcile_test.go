package migrate

import (
	"encoding/json"
	"strings"
	"testing"
)

func identityFixture(password, groups string) map[string]identityRow {
	return map[string]identityRow{"stable-id": {"uuid": json.RawMessage(`"stable-id"`), "password": json.RawMessage(`"` + password + `"`), "groups": json.RawMessage(groups)}}
}
func TestIdentityPlanProtectsUserSetAndBindsAllChanges(t *testing.T) {
	source := identityFixture("original", `["source"]`)
	target := identityFixture("changed", `["target"]`)
	p, err := identityPlan(source, target, "prod:pair:schema")
	if err != nil {
		t.Fatal(err)
	}
	if p.ChangedUsers != 1 || p.ChangedFields["password"] != 1 || p.ChangedFields["groups"] != 1 {
		t.Fatal("identity fields not covered")
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "original") || strings.Contains(string(raw), "stable-id") {
		t.Fatal("receipt leaked identity values")
	}
	q, _ := identityPlan(source, target, "prod:other-pair:schema")
	if q.PlanSHA256 == p.PlanSHA256 {
		t.Fatal("plan not bound to database identity")
	}
	target["stable-id"]["mfa_enabled"] = json.RawMessage(`true`)
	q, _ = identityPlan(source, target, "prod:pair:schema")
	if q.PlanSHA256 == p.PlanSHA256 {
		t.Fatal("plan not bound to MFA")
	}
	delete(target, "stable-id")
	if _, err = identityPlan(source, target, "binding"); err == nil {
		t.Fatal("missing target user accepted")
	}
}
func TestIdentityPlanAlignedReplaysAreNoOps(t *testing.T) {
	for i := 0; i < 3; i++ {
		p, err := identityPlan(identityFixture("original", `["source"]`), identityFixture("original", `["source"]`), "prod:pair:schema")
		if err != nil || p.ChangedUsers != 0 {
			t.Fatal("aligned identity generated changes")
		}
	}
}
