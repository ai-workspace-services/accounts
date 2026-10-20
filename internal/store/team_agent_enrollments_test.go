package store

import (
	"context"
	"database/sql"
	"errors"
	_ "github.com/jackc/pgx/v5/stdlib"
	"net/url"
	"os"
	"testing"
	"time"
)

func TestTeamAgentDecisionPersistenceAndCAS(t *testing.T) {
	testTeamAgentStore(t, NewMemoryStore().(TeamAgentEnrollmentStore))
}
func TestTeamAgentPostgresDecisionPersistenceAndCAS(t *testing.T) {
	dsn := os.Getenv("TEAM_AGENT_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL fixture not provided")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Path != "/accounts_native_render" {
		t.Fatal("only isolated local fixture accepted")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	testTeamAgentStore(t, &postgresStore{db: db})
}
func testTeamAgentStore(t *testing.T, st TeamAgentEnrollmentStore) {
	t.Helper()
	ctx := context.Background()
	id := "test-" + time.Now().Format("150405.000000000")
	e, err := st.RegisterTeamAgent(ctx, id, "suggested")
	if err != nil {
		t.Fatal(err)
	}
	old := e.UpdatedAt
	e.Status = "approved"
	e.AssignedTeamID = "assigned"
	e.DecidedBy = "00000000-0000-0000-0000-000000000001"
	audit := &AuditLog{Action: "network.agent.assignment", ActorUUID: e.DecidedBy, Details: map[string]any{"test": true}}
	if err := st.DecideTeamAgent(ctx, e, old, audit); err != nil {
		t.Fatal(err)
	}
	again, err := st.RegisterTeamAgent(ctx, id, "attacker-suggestion")
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != "approved" || again.AssignedTeamID != "assigned" || again.SuggestedTeamID != "suggested" {
		t.Fatalf("telemetry changed authority: %+v", again)
	}
	e.Status = "revoked"
	if err := st.DecideTeamAgent(ctx, e, old, audit); !errors.Is(err, ErrTeamAgentConflict) {
		t.Fatalf("stale decision accepted: %v", err)
	}
	// Failed audit insertion must roll back the authorization update in PostgreSQL.
	if _, ok := st.(*postgresStore); ok {
		e.UpdatedAt = again.UpdatedAt
		bad := *audit
		bad.ActorUUID = "invalid-uuid"
		if err := st.DecideTeamAgent(ctx, e, again.UpdatedAt, &bad); err == nil {
			t.Fatal("invalid audit unexpectedly committed")
		}
		unchanged, err := st.RegisterTeamAgent(ctx, id, "")
		if err != nil || unchanged.Status != "approved" {
			t.Fatal("audit rollback changed authorization", err)
		}
	}
}
