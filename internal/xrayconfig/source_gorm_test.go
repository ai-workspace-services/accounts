package xrayconfig

import (
	"context"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestGormClientSourceListClients(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Exec(`CREATE TABLE users (uuid TEXT PRIMARY KEY, proxy_uuid TEXT, email TEXT, created_at TIMESTAMP)`).Error; err != nil {
		t.Fatalf("create table: %v", err)
	}

	older := time.Now().Add(-time.Hour)
	newer := time.Now()
	if err := db.Exec(`INSERT INTO users (uuid, proxy_uuid, email, created_at) VALUES (?, ?, ?, ?)`, "uuid-b", "legacy-proxy-b", "b@example.com", older).Error; err != nil {
		t.Fatalf("insert older: %v", err)
	}
	if err := db.Exec(`INSERT INTO users (uuid, proxy_uuid, email, created_at) VALUES (?, ?, ?, ?)`, "uuid-a", "legacy-proxy-a", nil, newer).Error; err != nil {
		t.Fatalf("insert newer: %v", err)
	}

	source, err := NewGormClientSource(db)
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	clients, err := source.ListClients(context.Background())
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	if len(clients) != 2 {
		t.Fatalf("expected 2 clients, got %d", len(clients))
	}
	if clients[0].ID != "uuid-b" || clients[0].Email != "b@example.com" {
		t.Fatalf("unexpected first client: %+v", clients[0])
	}
	if clients[1].ID != "uuid-a" || clients[1].Email != "" {
		t.Fatalf("unexpected second client: %+v", clients[1])
	}
}

func TestNewGormClientSourceNilDB(t *testing.T) {
	if _, err := NewGormClientSource(nil); err == nil {
		t.Fatalf("expected error")
	}
}
