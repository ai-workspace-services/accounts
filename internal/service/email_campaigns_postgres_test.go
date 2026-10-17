package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"account/internal/model"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestEmailCampaignQueuePostgres(t *testing.T) {
	dsn := os.Getenv("EMAIL_PREFERENCES_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL email fixture not configured")
	}
	if !strings.Contains(dsn, "email_preferences_test") {
		t.Fatal("requires isolated email_preferences_test database")
	}
	d, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := d.DB()
	defer sqlDB.Close()
	for _, name := range []string{"2026101702_email_preferences.up.sql", "2026101703_email_campaigns.up.sql"} {
		body, err := os.ReadFile(filepath.Join("..", "..", "sql", "migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err = d.Exec(string(body)).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	SetDB(d)
	t.Cleanup(func() { SetDB(nil) })
	ctx := context.Background()
	userID := uuid.NewString()
	now := time.Now().UTC()
	campaign, err := CreateEmailCampaign(ctx, model.EmailCampaign{Kind: "announcement", Subject: "Queue test", Body: "Fixture", Audience: "all", DeliveryMode: "important"}, "root")
	if err != nil {
		t.Fatal(err)
	}
	row := model.EmailDelivery{ID: uuid.NewString(), DedupeKey: uuid.NewString(), CampaignID: campaign.ID, UserID: userID, Email: "fixture@example.com", Category: "productUpdates", Status: "queued", DueAt: now, CreatedAt: now, UpdatedAt: now}
	var group sync.WaitGroup
	launches := make(chan error, 2)
	for range 2 {
		group.Add(1)
		go func() { defer group.Done(); launches <- QueueEmailCampaign(ctx, campaign, []model.EmailDelivery{row}) }()
	}
	group.Wait()
	close(launches)
	launched := 0
	for err := range launches {
		if err == nil {
			launched++
		} else if !errors.Is(err, ErrEmailQueueConflict) {
			t.Fatal(err)
		}
	}
	if launched != 1 {
		t.Fatal("campaign launch not atomic", launched)
	}
	other := row
	other.ID = uuid.NewString()
	other.CampaignID = uuid.NewString()
	other.DedupeKey = uuid.NewString()
	if err = InsertEmailDeliveries(ctx, []model.EmailDelivery{other}); err != nil {
		t.Fatal(err)
	}
	claims := make(chan model.EmailDelivery, 8)
	errs := make(chan error, 8)
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			claimed, err := ClaimEmailDelivery(ctx, time.Now().UTC())
			if err == nil {
				claims <- claimed
			} else if !errors.Is(err, ErrNoEmailDelivery) {
				errs <- err
			}
		}()
	}
	group.Wait()
	close(claims)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var claimed model.EmailDelivery
	count := 0
	for r := range claims {
		claimed = r
		count++
	}
	if count != 1 {
		t.Fatal("concurrent per-user delivery allowed", count)
	}
	if err = FinishEmailDelivery(ctx, claimed, "sent", "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	available, err := EmailFrequencyAvailable(ctx, userID, "important", time.Now().UTC())
	if err != nil || available {
		t.Fatal("sent event did not enforce frequency cap", err)
	}
	next, err := ClaimEmailDelivery(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err = FinishEmailDelivery(ctx, next, "suppressed", "frequency_cap", time.Time{}); err != nil {
		t.Fatal(err)
	}
	// Identity deduplication survives a copied legacy dedupe-key string.
	duplicate := row
	duplicate.ID = uuid.NewString()
	duplicate.DedupeKey = "changed-after-user-remap"
	if err = InsertEmailDeliveries(ctx, []model.EmailDelivery{duplicate}); err != nil {
		t.Fatal(err)
	}
	var persisted int64
	d.Model(&model.EmailDelivery{}).Where("campaign_id = ? AND user_id = ?", campaign.ID, userID).Count(&persisted)
	if persisted != 1 {
		t.Fatal("recipient identity deduplication failed")
	}
}
