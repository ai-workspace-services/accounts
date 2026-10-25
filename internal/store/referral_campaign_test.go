package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func exerciseReferralCampaignHistory(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	campaigns := st.(ReferralCampaignStore)
	initial, err := campaigns.GetReferralCampaign(ctx, time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !initial.Active(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("seed campaign should preserve original window")
	}
	if initial.Active(time.Date(2026, 10, 12, 15, 59, 59, 0, time.UTC)) || !initial.Active(time.Date(2026, 10, 12, 16, 0, 0, 0, time.UTC)) || initial.Active(time.Date(2026, 12, 31, 16, 0, 0, 0, time.UTC)) {
		t.Fatal("seed Beijing boundaries changed")
	}
	if _, err := campaigns.ConfigureReferralCampaign(ctx, &ReferralCampaign{Enabled: true}); !errors.Is(err, ErrReferralCampaignInvalid) {
		t.Fatalf("missing reason=%v", err)
	}
	start, end := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
	if _, err := campaigns.ConfigureReferralCampaign(ctx, &ReferralCampaign{Enabled: true, StartsAt: &start, EndsAt: &end, Reason: "invalid"}); !errors.Is(err, ErrReferralCampaignInvalid) {
		t.Fatalf("reversed bounds=%v", err)
	}
	open, err := campaigns.ConfigureReferralCampaign(ctx, &ReferralCampaign{Revision: 999, Enabled: true, EffectiveAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), Reason: "open unbounded"})
	if err != nil {
		t.Fatal(err)
	}
	if open.Revision == 999 || open.EffectiveAt.Year() == 2000 || !open.Active(open.EffectiveAt) || open.StartsAt != nil || open.EndsAt != nil {
		t.Fatalf("server-owned configuration=%+v", open)
	}
	paidAt := open.EffectiveAt
	closed, err := campaigns.ConfigureReferralCampaign(ctx, &ReferralCampaign{Enabled: false, Reason: "close new eligibility"})
	if err != nil {
		t.Fatal(err)
	}
	old, err := campaigns.GetReferralCampaign(ctx, paidAt)
	if err != nil || old.Revision != open.Revision || !old.Active(paidAt) {
		t.Fatalf("late payment lost old policy: %+v %v", old, err)
	}
	current, err := campaigns.GetReferralCampaign(ctx, closed.EffectiveAt)
	if err != nil || current.Active(closed.EffectiveAt) {
		t.Fatalf("closure not effective: %+v %v", current, err)
	}
	// A delayed reward snapshots its paid_at revision even after closure.
	a, b := &User{Name: uuid.NewString(), Email: uuid.NewString() + "@example.invalid"}, &User{Name: uuid.NewString(), Email: uuid.NewString() + "@example.invalid"}
	for _, u := range []*User{a, b} {
		if pg, ok := st.(*postgresStore); ok {
			u.ID = uuid.NewString()
			if _, err := pg.db.ExecContext(ctx, `INSERT INTO public.users(uuid,email) VALUES($1,$2)`, u.ID, u.Email); err != nil {
				t.Fatal(err)
			}
		} else if err := st.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	code := "R" + uuid.NewString()[:8]
	if _, err := st.EnsureReferralCode(ctx, a.ID, code); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateReferralAttribution(ctx, code, b.ID, paidAt.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	reward := &ReferralReward{ProviderInvoiceID: "campaign-" + uuid.NewString(), InviteeUUID: b.ID, PlanID: "PRO-MONTHLY", ProductSlug: "xconnect", PaidMinor: 1000, RewardMinor: 100, Currency: "USD", PaidAt: paidAt}
	inserted, err := st.CreateReferralReward(ctx, reward)
	if err != nil || !inserted {
		t.Fatalf("delayed reward=%v %v", inserted, err)
	}
	snapshot, err := campaigns.GetReferralRewardCampaign(ctx, reward.ID)
	if err != nil || snapshot.Revision != open.Revision {
		t.Fatalf("reward policy=%+v %v", snapshot, err)
	}
	future := closed.EffectiveAt.Add(48 * time.Hour)
	edited, err := campaigns.ConfigureReferralCampaign(ctx, &ReferralCampaign{Enabled: true, StartsAt: &future, Reason: "edit future start"})
	if err != nil || edited.Active(edited.EffectiveAt) || !edited.Active(future) {
		t.Fatalf("edited bounds=%+v %v", edited, err)
	}
	snapshot, err = campaigns.GetReferralRewardCampaign(ctx, reward.ID)
	if err != nil || snapshot.Revision != open.Revision {
		t.Fatalf("historical snapshot mutated=%+v %v", snapshot, err)
	}
	if inserted, err := st.CreateReferralReward(ctx, reward); err != nil || inserted {
		t.Fatalf("reward replay=%v %v", inserted, err)
	}
	// Return the default window for independent suites, retaining every revision.
	if _, err := campaigns.ConfigureReferralCampaign(ctx, &ReferralCampaign{Enabled: initial.Enabled, StartsAt: initial.StartsAt, EndsAt: initial.EndsAt, Reason: "restore original window after isolated test"}); err != nil {
		t.Fatal(err)
	}
}

func TestReferralCampaignMemoryHistory(t *testing.T) {
	exerciseReferralCampaignHistory(t, NewMemoryStore())
}

func TestReferralCampaignPostgresHistory(t *testing.T) {
	dsn := os.Getenv("ACCOUNTS_REFERRAL_TEST_DSN")
	if dsn == "" {
		if os.Getenv("ACCOUNTS_REFERRAL_TEST_DSN_REQUIRED") == "1" {
			t.Fatal("isolated PostgreSQL DSN required")
		}
		t.Skip("set isolated referral PostgreSQL DSN")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || (cfg.Host != "127.0.0.1" && cfg.Host != "localhost") || (cfg.Database != "referral_store_test" && cfg.Database != "referral_campaign_test") {
		t.Fatal("only local isolated referral_store_test/referral_campaign_test allowed")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := &postgresStore{db: db}
	exerciseReferralCampaignHistory(t, st)
	if _, err := db.Exec(`UPDATE public.referral_campaign_revisions SET enabled=false WHERE revision=1`); err == nil {
		t.Fatal("historical revision UPDATE accepted")
	}
	if _, err := db.Exec(`DELETE FROM public.referral_reward_campaign_snapshots`); err == nil {
		t.Fatal("reward snapshot DELETE accepted")
	}
}
