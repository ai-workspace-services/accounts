package migrate

import "testing"

func TestReplatformRequiresExplicitSameGenerationAppend(t *testing.T) {
	cases := []struct {
		mode, source, target string
		append, valid        bool
	}{
		{"replatform", "prod-v5", "prod-v5", true, true},
		{"upgrade", "prod-v5", "prod-v5", true, false},
		{"replatform", "prod-v5", "prod-v6", true, false},
		{"replatform", "prod-v5", "prod-v5", false, false},
		{"replatform", "uat-v5", "uat-v5", true, false},
		{"upgrade", "prod-v5", "prod-v6", true, true},
		{"unknown", "prod-v5", "prod-v6", true, false},
	}
	for _, c := range cases {
		err := validateBusinessMigrationPair(UsageMergeOptions{MigrationMode: c.mode, SourceGeneration: c.source, TargetGeneration: c.target, AppendBusiness: c.append})
		if (err == nil) != c.valid {
			t.Fatalf("migration pair validation mismatch for %s %s %s", c.mode, c.source, c.target)
		}
	}
}
