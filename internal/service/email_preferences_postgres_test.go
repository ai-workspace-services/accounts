package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestEmailPreferencesPostgresMigration(t *testing.T) {
	dsn := os.Getenv("EMAIL_PREFERENCES_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL preference fixture not configured")
	}
	if !strings.Contains(dsn, "email_preferences_test") {
		t.Fatal("test database must be isolated")
	}
	d, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := d.DB()
	defer sqlDB.Close()
	body, err := os.ReadFile(filepath.Join("..", "..", "sql", "migrations", "2026101702_email_preferences.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = d.Exec(string(body)).Error; err != nil {
			t.Fatal(err)
		}
	}
	SetDB(d)
	t.Cleanup(func() { SetDB(nil) })
	id := uuid.NewString()
	ctx := context.Background()
	initial, err := GetEmailPreferences(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Offers || initial.ProductUpdates || initial.Tutorials {
		t.Fatal("default marketing consent")
	}
	initial.Offers = true
	saved, err := SaveEmailPreferences(ctx, id, initial, "account_settings")
	if err != nil {
		t.Fatal(err)
	}
	if err = UnsubscribeEmail(ctx, id, "one_click"); err != nil {
		t.Fatal(err)
	}
	if _, err = SaveEmailPreferences(ctx, id, saved, "account_settings"); !errors.Is(err, ErrEmailPreferenceConflict) {
		t.Fatal("stale consent overwrite", err)
	}
	token, err := IssueEmailPreferenceToken(ctx, id, "user@example.com", "unsubscribe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ResolveEmailPreferenceToken(ctx, token, "preferences"); !errors.Is(err, ErrInvalidEmailPreferenceToken) {
		t.Fatal("token purpose bypass", err)
	}
	// A fresh DB connection must see the same refusal.
	fresh, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	freshSQL, _ := fresh.DB()
	defer freshSQL.Close()
	SetDB(fresh)
	loaded, err := GetEmailPreferences(ctx, id)
	if err != nil || loaded.Offers {
		t.Fatal("refusal lost after reconnect", err)
	}
}
