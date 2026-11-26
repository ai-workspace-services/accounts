package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"account/internal/agentproto"
	"account/internal/migrate"
	accountmigrations "account/sql/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresAgentNodeCapacityPersistsCounterHistory(t *testing.T) {
	dsn := os.Getenv("AGENT_CAPACITY_TEST_DSN")
	if dsn == "" {
		t.Skip("requires a disposable PostgreSQL database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(8)
	defer db.Close()
	migration, err := accountmigrations.Files.ReadFile("2026112601_agent_node_capacity.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE ROLE account_user NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
        CREATE TABLE public.schema_migrations (version bigint PRIMARY KEY, dirty boolean NOT NULL);
        INSERT INTO public.schema_migrations VALUES (2026112401, false);
        CREATE TABLE public.audit_logs (uuid uuid PRIMARY KEY, action text, actor_uuid uuid, details jsonb, created_at timestamptz DEFAULT now());
        GRANT SELECT, INSERT ON public.audit_logs TO account_user`); err != nil {
		t.Fatal(err)
	}
	opts := migrate.ControlledOptions{ExpectedVersion: 2026112401, TargetVersion: 2026112601, SHA256: fmt.Sprintf("%x", sha256.Sum256(migration)), LockTimeout: time.Second, StatementTimeout: 5 * time.Second}
	for range 2 {
		if err := migrate.ControlledMigrate(ctx, dsn, accountmigrations.Files, opts); err != nil {
			t.Fatal(err)
		}
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["role"] = "account_user"
	appDB := stdlib.OpenDB(*config)
	defer appDB.Close()
	appDB.SetMaxOpenConns(8)
	s := &postgresStore{db: appDB}
	if _, err := appDB.ExecContext(ctx, `DELETE FROM accounts_node_capacity.agent_node_capacity`); err == nil {
		t.Fatal("business role unexpectedly has delete access")
	}
	var receipt string
	if err := appDB.QueryRowContext(ctx, `SELECT sql_sha256 FROM public.accounts_controlled_migration_history WHERE to_version = 2026112601`).Scan(&receipt); err != nil || receipt != opts.SHA256 {
		t.Fatalf("migration receipt = %q: %v", receipt, err)
	}
	const id = "edge.test"
	begin := time.Date(2026, 11, 30, 23, 50, 0, 0, time.UTC)
	sample := func(at time.Time, boot string, rx, tx int64) agentproto.NodeTrafficSample {
		return agentproto.NodeTrafficSample{Interface: "eth0", BootID: boot, RXBytes: &rx, TXBytes: &tx, SampledAt: at}
	}
	record := func(sample agentproto.NodeTrafficSample) {
		t.Helper()
		if err := s.RecordAgentNodeTraffic(ctx, id, sample); err != nil {
			t.Error(err)
		}
	}
	limit := int64(75)
	speed := int32(100)
	if err := s.SetAgentNodeCapacity(ctx, id, AgentNodeCapacityLimits{MonthlyLimitBytes: &limit, BandwidthLimitMbps: &speed}, &AuditLog{Action: "network.agent.capacity"}); err != nil {
		t.Fatal(err)
	}
	record(sample(begin, "boot-1", 100, 200))
	baseline, err := s.GetAgentNodeCapacity(ctx, id, begin)
	if err != nil || baseline == nil || baseline.UsedBytes != nil || baseline.SampledAt == nil {
		t.Fatalf("single baseline is not a measured interval: %#v, %v", baseline, err)
	}
	latest := sample(begin.Add(time.Minute), "boot-1", 130, 220)
	var writes sync.WaitGroup
	for range 8 {
		writes.Go(func() { record(latest) })
	}
	writes.Wait()
	record(sample(begin.Add(30*time.Second), "boot-1", 120, 215))
	record(sample(begin.Add(2*time.Minute), "boot-2", 10, 15))
	record(sample(begin.Add(3*time.Minute), "boot-2", 30, 35))
	record(sample(begin.Add(4*time.Minute), "boot-2", 1, 1))
	record(sample(begin.Add(5*time.Minute), "boot-2", 2, 3))
	next := begin.Add(15 * time.Minute)
	record(sample(next, "boot-2", 1000, 2000))
	record(sample(next.Add(time.Minute), "boot-2", 1002, 2003))

	reopened := stdlib.OpenDB(*config)
	defer reopened.Close()
	s = &postgresStore{db: reopened}
	check := func(at time.Time, month string, used, remaining int64) {
		t.Helper()
		capacity, err := s.GetAgentNodeCapacity(ctx, id, at)
		if err != nil || capacity == nil {
			t.Fatalf("capacity = %v, %v", capacity, err)
		}
		if capacity.Month != month || capacity.UsedBytes == nil || *capacity.UsedBytes != used ||
			capacity.RemainingBytes == nil || *capacity.RemainingBytes != remaining || capacity.BandwidthLimitMbps == nil || *capacity.BandwidthLimitMbps != speed {
			t.Fatalf("capacity for %s = %#v", month, capacity)
		}
	}
	check(begin, "2026-11", 93, 0)
	check(next, "2026-12", 5, 70)
	if err := s.SetAgentNodeCapacity(ctx, id, AgentNodeCapacityLimits{}, &AuditLog{Action: "network.agent.capacity", ActorUUID: "invalid-uuid"}); err == nil {
		t.Fatal("expected audit failure")
	}
	check(next, "2026-12", 5, 70)
}
