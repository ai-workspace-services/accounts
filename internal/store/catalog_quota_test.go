package store

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	schema "account/sql"
)

// Run against both the memory store and a disposable native PostgreSQL schema.
func checkCatalogQuotaPropagation(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	end := start.AddDate(0, 1, 0)
	for _, name := range []string{"free", "exhausted", "other", "contract", "override"} {
		u := &User{Name: "catalog-" + name, Email: "catalog-" + name + "@example.invalid", PasswordHash: "unused", Active: true, Role: RoleUser, Groups: []string{MonthlyFreeQuotaLimitGroup}}
		if name == "override" {
			u.Groups = append(u.Groups, "segment:subscription:unlimited")
		}
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		planID := "FREE"
		if name == "other" {
			planID = "CUSTOM"
		}
		if name == "contract" {
			planID = "PLUS"
		}
		if err := st.UpsertAccountBillingProfile(ctx, &AccountBillingProfile{AccountUUID: u.ID, IncludedQuotaBytes: 3 * gib, PackageName: "free", PricingRuleVersion: "plan:" + planID, BasePricePerByte: 0.25}); err != nil {
			t.Fatal(err)
		}
		remaining := 2 * gib
		if name == "exhausted" {
			remaining = 0
		}
		if err := st.UpsertAccountQuotaState(ctx, &AccountQuotaState{AccountUUID: u.ID, RemainingIncludedQuota: remaining, CurrentBalance: 42, SuspendState: "suspended", ProxyAccessState: "paused", PeriodStart: &start, PeriodEnd: &end}); err != nil {
			t.Fatal(err)
		}
		if name == "contract" {
			switch target := st.(type) {
			case *memoryStore:
				target.subscriptionRecords[u.ID] = SubscriptionRecord{UserID: u.ID, MonthlyQuotaBytes: 3 * gib}
			case *postgresStore:
				if _, err := target.db.ExecContext(ctx, `INSERT INTO subscription_records(user_uuid,monthly_quota_bytes) VALUES ($1,$2)`, u.ID, 3*gib); err != nil {
					t.Fatal(err)
				}
			}
		}
		if name == "exhausted" {
			if err := st.UpsertTrafficMinuteBucket(ctx, &TrafficMinuteBucket{AccountUUID: u.ID, NodeID: "catalog-node", BucketStart: start.Add(time.Minute), TotalBytes: 4 * gib}); err != nil {
				t.Fatal(err)
			}
		}
	}
	users, err := st.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, quota := range []int64{5 * gib, 5 * gib, 2 * gib, 5 * gib} {
		for _, id := range []string{"FREE", "PLUS"} {
			if err := st.UpsertBillingPlan(ctx, &BillingPlan{PlanID: id, Kind: "subscription", Active: true, IncludedQuotaBytes: quota}); err != nil {
				t.Fatal(err)
			}
		}
		for _, u := range users {
			p, err := st.GetAccountBillingProfile(ctx, u.ID)
			if err != nil {
				t.Fatal(err)
			}
			q, err := st.GetAccountQuotaState(ctx, u.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantIncluded, wantRemaining := quota, max(int64(0), quota-gib)
			switch u.Name {
			case "catalog-other", "catalog-contract":
				wantIncluded, wantRemaining = 3*gib, 2*gib
			case "catalog-exhausted":
				wantRemaining = max(int64(0), quota-4*gib)
			}
			if p.IncludedQuotaBytes != wantIncluded || q.RemainingIncludedQuota != wantRemaining {
				t.Fatalf("%s at quota %d: got %d/%d want %d/%d", u.Name, quota, p.IncludedQuotaBytes, q.RemainingIncludedQuota, wantIncluded, wantRemaining)
			}
			if p.BasePricePerByte != 0.25 || q.CurrentBalance != 42 || q.SuspendState != "suspended" || q.ProxyAccessState != "paused" || !q.PeriodStart.Equal(start) || !q.PeriodEnd.Equal(end) {
				t.Fatalf("protected account state changed: %s", u.Name)
			}
			current, err := st.GetUserByID(ctx, u.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !equalStoreStrings(current.Groups, u.Groups) {
				t.Fatal("groups changed")
			}
		}
	}
}

func TestCatalogQuotaPropagation(t *testing.T) { checkCatalogQuotaPropagation(t, NewMemoryStore()) }

func TestCatalogQuotaPropagationPostgres(t *testing.T) {
	dsn := os.Getenv("CATALOG_QUOTA_TEST_DSN")
	if dsn == "" {
		t.Skip("requires an empty disposable PostgreSQL database")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var existing bool
	if err := db.QueryRow("SELECT to_regclass('public.users') IS NOT NULL").Scan(&existing); err != nil {
		t.Fatal(err)
	}
	if existing {
		t.Fatal("refusing to initialize an existing users database")
	}
	artifact, _, err := schema.NativeArtifact()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(artifact)); err != nil {
		t.Fatal(err)
	}
	checkCatalogQuotaPropagation(t, &postgresStore{db: db})
	// A failed quota write must roll back the catalog and every profile update.
	if _, err := db.Exec(`CREATE FUNCTION reject_catalog_quota() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN RAISE EXCEPTION 'injected quota write failure'; END $$;
	CREATE TRIGGER reject_catalog_quota BEFORE UPDATE ON account_quota_states
	FOR EACH ROW EXECUTE FUNCTION reject_catalog_quota()`); err != nil {
		t.Fatal(err)
	}
	st := &postgresStore{db: db}
	if err := st.UpsertBillingPlan(context.Background(), &BillingPlan{PlanID: "FREE", Kind: "subscription", Active: true, IncludedQuotaBytes: 7 * gib}); err == nil {
		t.Fatal("expected injected transaction failure")
	}
	plan, err := st.GetBillingPlan(context.Background(), "FREE")
	if err != nil {
		t.Fatal(err)
	}
	if plan.IncludedQuotaBytes != 5*gib {
		t.Fatal("failed save changed catalog")
	}
	var changed int
	if err := db.QueryRow(`SELECT count(*) FROM account_billing_profiles WHERE pricing_rule_version='plan:FREE' AND included_quota_bytes <> $1`, 5*gib).Scan(&changed); err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Fatal("failed save changed account profiles")
	}
}
