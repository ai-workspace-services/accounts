package migrate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func syntheticSnapshot() *ThreeTableSnapshot {
	s := &ThreeTableSnapshot{Columns: map[string][]ColumnDefinition{}, Rows: map[string][]string{}}
	for _, table := range accountTables {
		for _, name := range strings.Fields(transportColumns[table]) {
			s.Columns[table] = append(s.Columns[table], ColumnDefinition{Name: name})
		}
		s.Rows[table] = []string{}
	}
	row := map[string]any{}
	for _, c := range s.Columns["users"] {
		row[c.Name] = nil
	}
	row["uuid"] = "11111111-1111-4111-8111-111111111111"
	row["proxy_uuid"] = row["uuid"]
	row["username"] = "synthetic"
	row["email"] = "fixture@example.invalid"
	row["email_verified"] = false
	b, _ := json.Marshal(row)
	s.Rows["users"] = []string{string(b)}
	return s
}

func TestAccountsOnlySnapshotRejects(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*ThreeTableSnapshot)
	}{
		{"duplicate UUID", func(s *ThreeTableSnapshot) { s.Rows["users"] = append(s.Rows["users"], s.Rows["users"][0]) }},
		{"missing table", func(s *ThreeTableSnapshot) { delete(s.Rows, "sessions") }},
		{"unknown entitlement", func(s *ThreeTableSnapshot) {
			s.Columns["users"] = append(s.Columns["users"], ColumnDefinition{Name: "plan"})
		}},
		{"duplicate email", func(s *ThreeTableSnapshot) {
			s.Rows["users"] = append(s.Rows["users"], strings.ReplaceAll(s.Rows["users"][0], "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"))
		}},
		{"orphan", func(s *ThreeTableSnapshot) {
			row := map[string]any{}
			for _, c := range s.Columns["sessions"] {
				row[c.Name] = nil
			}
			row["uuid"] = "33333333-3333-4333-8333-333333333333"
			row["user_uuid"] = "22222222-2222-4222-8222-222222222222"
			row["token"] = "synthetic"
			b, _ := json.Marshal(row)
			s.Rows["sessions"] = []string{string(b)}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := syntheticSnapshot()
			test.mutate(s)
			if _, err := validateThreeTableRows(s); err == nil {
				t.Fatal("expected fail closed")
			}
		})
	}
}

func TestAccountsOnlyRequiresExplicitIndependentDatabaseBeforeConnection(t *testing.T) {
	_, err := NewImporter().Import(context.Background(), "not-a-dsn", nil, ImportOptions{AccountsOnly: true})
	if err == nil || !strings.Contains(err.Error(), "independent") {
		t.Fatal("target guard must precede connection")
	}
}
