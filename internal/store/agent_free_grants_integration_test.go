package store

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgresUserUpdatePreservesConcurrentFreeGrantRevocation(t *testing.T) {
	dsn := os.Getenv("FREE_GRANT_TEST_DSN")
	if dsn == "" {
		t.Skip("requires a disposable PostgreSQL database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	other, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	// CREATE TABLE fails if the disposable fixture is not empty.
	_, err = db.ExecContext(ctx, `CREATE TABLE public.users (uuid UUID PRIMARY KEY, username TEXT, email TEXT, password TEXT, email_verified_at TIMESTAMPTZ, groups JSONB);
		CREATE TABLE public.audit_logs (uuid UUID PRIMARY KEY, action TEXT, actor_uuid UUID, details JSONB, created_at TIMESTAMPTZ)`)
	if err != nil {
		t.Fatal(err)
	}
	user := User{ID: uuid.NewString(), Name: "fixture", Email: "fixture@example.invalid"}
	if _, err := db.ExecContext(ctx, `INSERT INTO public.users (uuid, groups, email) VALUES ($1, '[]', $2)`, user.ID, user.Email); err != nil {
		t.Fatal(err)
	}
	grants := &postgresStore{db: db}
	profiles := &postgresStore{db: other}
	now := time.Now().UTC()
	actor := uuid.NewString()
	grant, err := grants.PutAgentFreeGrant(ctx, AgentFreeGrant{NodeID: "custom-fixture", AccountID: user.ID, ExpiresAt: now.Add(time.Hour), UpdatedAt: now}, &AuditLog{ActorUUID: actor, Action: AuditActionAgentFreeGrantCreate})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := EncodeAgentFreeGrantGroup(grant)
	if err != nil {
		t.Fatal(err)
	}
	user.Groups = []string{"member", stale}
	result := make(chan error, 1)
	go func() { result <- profiles.UpdateUser(ctx, &user) }()
	if _, err := grants.RevokeAgentFreeGrant(ctx, grant.NodeID, grant.AccountID, actor, "test revocation", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	saved, found, err := grants.GetAgentFreeGrant(ctx, grant.NodeID, grant.AccountID)
	if err != nil || !found || saved.Status != AgentFreeGrantStatusRevoked {
		t.Fatalf("revocation lost: found=%v status=%q err=%v", found, saved.Status, err)
	}
	// Replay a profile update definitely loaded before the revocation as well.
	if err := profiles.UpdateUser(ctx, &user); err != nil {
		t.Fatal(err)
	}
	saved, found, err = grants.GetAgentFreeGrant(ctx, grant.NodeID, grant.AccountID)
	if err != nil || !found || saved.Status != AgentFreeGrantStatusRevoked {
		t.Fatalf("stale update restored revoked grant: found=%v status=%q err=%v", found, saved.Status, err)
	}
}
