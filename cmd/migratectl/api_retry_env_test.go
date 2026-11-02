package main

import "testing"

func TestMigrationRetryIDsFromEnvironment(t *testing.T) {
	t.Setenv("MIGRATION_PLAN_ID", "11111111-1111-4111-8111-111111111111")
	t.Setenv("MIGRATION_SNAPSHOT_ID", "22222222-2222-4222-8222-222222222222")
	cmd := newMigrationAPICmd()
	for name, want := range map[string]string{"plan-id": "11111111-1111-4111-8111-111111111111", "snapshot-id": "22222222-2222-4222-8222-222222222222"} {
		got, err := cmd.Flags().GetString(name)
		if err != nil || got != want {
			t.Fatalf("%s = %q, err=%v", name, got, err)
		}
	}
	if err := cmd.Flags().Set("plan-id", "33333333-3333-4333-8333-333333333333"); err != nil {
		t.Fatal(err)
	}
	if got, _ := cmd.Flags().GetString("plan-id"); got != "33333333-3333-4333-8333-333333333333" {
		t.Fatal("explicit flag did not override environment")
	}
}
