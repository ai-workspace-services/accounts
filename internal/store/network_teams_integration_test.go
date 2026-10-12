package store

import (
	"context"
	"database/sql"
	"errors"
	"github.com/google/uuid"
	"os"
	"sync"
	"testing"
	"time"
)

func TestNetworkTeamPostgresConcurrentCapAndAuditRollback(t *testing.T) {
	dsn := os.Getenv("NETWORK_TEAM_TEST_DSN")
	if dsn == "" {
		t.Skip("requires a fresh disposable PostgreSQL database")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	var table sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('public.users')::text`).Scan(&table); err != nil {
		t.Fatal(err)
	}
	if table.Valid {
		t.Fatal("refusing integration test against a database with existing users")
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE users(uuid UUID PRIMARY KEY); CREATE TABLE audit_logs(uuid UUID PRIMARY KEY,action TEXT NOT NULL CHECK(action<>''),actor_uuid UUID,details JSONB);`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../sql/migrations/2026101201_network_teams.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	st := &postgresStore{db: db}
	audit := &AuditLog{Action: "test.network", Details: map[string]any{"reason": "test fixture"}}
	if err := st.PutNetworkTeam(ctx, NetworkTeam{ID: "acme", Name: "ACME", MemberLimit: 1, Active: true, ValidFrom: time.Now(), ValidUntil: time.Now().Add(time.Hour)}, audit); err != nil {
		t.Fatal(err)
	}
	ids := []string{uuid.NewString(), uuid.NewString()}
	for _, id := range ids {
		if _, err = db.ExecContext(ctx, `INSERT INTO users VALUES($1)`, id); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range ids {
		if err := st.PutNetworkMembership(ctx, NetworkMembership{UserID: id, DedicatedOnly: true}, audit); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			errs <- st.PutNetworkMembership(ctx, NetworkMembership{UserID: id, TeamID: "acme"}, audit)
		}(id)
	}
	wg.Wait()
	close(errs)
	full := 0
	for err := range errs {
		if errors.Is(err, ErrNetworkTeamFull) {
			full++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if full != 1 {
		t.Fatalf("concurrent seat cap failed: %d", full)
	}
	members, err := st.ListNetworkMemberships(ctx)
	if err != nil || len(members) != 2 {
		t.Fatalf("members: %+v %v", members, err)
	}
	var member NetworkMembership
	bound := 0
	for _, row := range members {
		if row.TeamID == "acme" {
			member = row
			bound++
		}
	}
	if bound != 1 {
		t.Fatalf("concurrent cap admitted %d members", bound)
	}
	member.TeamID = ""
	member.DedicatedOnly = true
	if err = st.PutNetworkMembership(ctx, member, &AuditLog{}); err == nil {
		t.Fatal("missing audit did not roll back membership")
	}
	current, err := st.GetNetworkMembership(ctx, member.UserID)
	if err != nil || current.TeamID != "acme" {
		t.Fatalf("audit failure changed member: %+v %v", current, err)
	}
	if err = st.PutNetworkMembership(ctx, member, audit); err != nil {
		t.Fatal(err)
	}
	current, err = st.GetNetworkMembership(ctx, member.UserID)
	if err != nil || current.TeamID != "" || !current.DedicatedOnly {
		t.Fatalf("removal restored public: %+v %v", current, err)
	}
}
