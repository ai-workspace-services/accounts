package model

import "time"

// Email settings and consent are independent of subscriptions and billing.
type EmailManagementSetting struct {
	ID            string    `gorm:"primaryKey;type:text"`
	Version       uint64    `gorm:"not null"`
	CompanyName   string    `gorm:"not null;type:text"`
	PostalAddress string    `gorm:"not null;type:text"`
	ContactEmail  string    `gorm:"not null;type:text"`
	ConsoleURL    string    `gorm:"not null;type:text"`
	UpdatedBy     string    `gorm:"not null;type:text"`
	UpdatedAt     time.Time `gorm:"not null"`
}

type EmailPreference struct {
	UserID         string    `gorm:"primaryKey;type:text"`
	ProductUpdates bool      `gorm:"not null"`
	Tutorials      bool      `gorm:"not null"`
	Offers         bool      `gorm:"not null"`
	Frequency      string    `gorm:"not null;type:text"`
	Version        uint64    `gorm:"not null"`
	UpdatedAt      time.Time `gorm:"not null"`
}

type EmailPreferenceToken struct {
	Hash      string    `gorm:"primaryKey;type:text"`
	UserID    string    `gorm:"not null;type:text;index"`
	Email     string    `gorm:"not null;type:text"`
	Purpose   string    `gorm:"not null;type:text"`
	ExpiresAt time.Time `gorm:"not null;index"`
}

type EmailPreferenceEvent struct {
	ID            string    `gorm:"primaryKey;type:text"`
	UserID        string    `gorm:"not null;type:text;index"`
	Source        string    `gorm:"not null;type:text"`
	PolicyVersion string    `gorm:"not null;type:text"`
	Preferences   string    `gorm:"not null;type:text"`
	CreatedAt     time.Time `gorm:"not null"`
}
