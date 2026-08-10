package xrayconfig

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"gorm.io/gorm"
)

// GormClientSource reads Xray client credentials from the users table using GORM.
type GormClientSource struct {
	DB     *gorm.DB
	Logger *slog.Logger
}

// NewGormClientSource constructs a ClientSource backed by the provided GORM instance.
func NewGormClientSource(db *gorm.DB) (*GormClientSource, error) {
	if db == nil {
		return nil, errors.New("gorm db is required")
	}
	return &GormClientSource{
		DB:     db,
		Logger: slog.Default().With("component", "xray-gorm-source"),
	}, nil
}

// ListClients returns all users ordered by creation time.
func (s *GormClientSource) ListClients(ctx context.Context) ([]Client, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("gorm client source is not configured")
	}

	type row struct {
		UUID  string  `gorm:"column:uuid"`
		Email *string `gorm:"column:email"`
	}

	var rows []row
	if err := s.DB.WithContext(ctx).
		Table("users").
		Select("uuid, email").
		Order("created_at ASC, uuid ASC").
		Find(&rows).Error; err != nil {
		if s.Logger != nil {
			s.Logger.Error("failed to list clients from users table", "err", err)
		}
		return nil, err
	}

	clients := make([]Client, 0, len(rows))
	for _, r := range rows {
		id := strings.TrimSpace(r.UUID)
		if id == "" {
			continue
		}
		client := Client{ID: id}
		if r.Email != nil {
			client.Email = strings.TrimSpace(*r.Email)
		}
		clients = append(clients, client)
	}
	return clients, nil
}
