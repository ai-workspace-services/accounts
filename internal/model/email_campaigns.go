package model

import "time"

type EmailCampaign struct {
	ID           string    `gorm:"primaryKey;type:text" json:"id"`
	Version      uint64    `gorm:"not null" json:"version"`
	Kind         string    `gorm:"not null;type:text" json:"kind"`
	Subject      string    `gorm:"not null;type:text" json:"subject"`
	Body         string    `gorm:"not null;type:text" json:"body"`
	ActionLabel  string    `gorm:"not null;type:text" json:"actionLabel"`
	ActionURL    string    `gorm:"not null;type:text" json:"actionUrl"`
	Audience     string    `gorm:"not null;type:text" json:"audience"`
	DeliveryMode string    `gorm:"not null;type:text" json:"deliveryMode"`
	Status       string    `gorm:"not null;type:text;index" json:"status"`
	CreatedBy    string    `gorm:"not null;type:text" json:"createdBy"`
	CreatedAt    time.Time `gorm:"not null" json:"createdAt"`
	UpdatedAt    time.Time `gorm:"not null" json:"updatedAt"`
}

type EmailOnboardingStep struct {
	ID          string    `gorm:"primaryKey;type:text" json:"id"`
	Version     uint64    `gorm:"not null" json:"version"`
	Enabled     bool      `gorm:"not null" json:"enabled"`
	Subject     string    `gorm:"not null;type:text" json:"subject"`
	Body        string    `gorm:"not null;type:text" json:"body"`
	ActionLabel string    `gorm:"not null;type:text" json:"actionLabel"`
	ActionURL   string    `gorm:"not null;type:text" json:"actionUrl"`
	DelayHours  int       `gorm:"not null" json:"delayHours"`
	ActivatedAt time.Time `gorm:"not null" json:"activatedAt"`
	UpdatedBy   string    `gorm:"not null;type:text" json:"updatedBy"`
	UpdatedAt   time.Time `gorm:"not null" json:"updatedAt"`
}

type EmailDelivery struct {
	ID           string     `gorm:"primaryKey;type:text" json:"id"`
	DedupeKey    string     `gorm:"not null;type:text;uniqueIndex" json:"-"`
	CampaignID   string     `gorm:"not null;type:text;index;uniqueIndex:idx_email_delivery_identity,priority:1" json:"campaignId"`
	StepID       string     `gorm:"not null;type:text;uniqueIndex:idx_email_delivery_identity,priority:2" json:"stepId"`
	UserID       string     `gorm:"not null;type:text;index;uniqueIndex:idx_email_delivery_identity,priority:3" json:"userId"`
	Email        string     `gorm:"not null;type:text" json:"-"`
	Subject      string     `gorm:"not null;type:text" json:"subject"`
	Body         string     `gorm:"not null;type:text" json:"-"`
	ActionLabel  string     `gorm:"not null;type:text" json:"-"`
	ActionURL    string     `gorm:"not null;type:text" json:"-"`
	Category     string     `gorm:"not null;type:text" json:"category"`
	DeliveryMode string     `gorm:"not null;type:text" json:"deliveryMode"`
	Audience     string     `gorm:"not null;type:text" json:"-"`
	Status       string     `gorm:"not null;type:text;index" json:"status"`
	Attempts     int        `gorm:"not null" json:"attempts"`
	DueAt        time.Time  `gorm:"not null;index" json:"dueAt"`
	LeaseToken   string     `gorm:"not null;type:text" json:"-"`
	LeaseUntil   *time.Time `json:"-"`
	SentAt       *time.Time `gorm:"index" json:"sentAt"`
	LastError    string     `gorm:"not null;type:text" json:"lastError"`
	CreatedAt    time.Time  `gorm:"not null" json:"createdAt"`
	UpdatedAt    time.Time  `gorm:"not null" json:"updatedAt"`
}

type EmailRecipientLease struct {
	UserID    string    `gorm:"primaryKey;type:text"`
	Token     string    `gorm:"not null;type:text"`
	ExpiresAt time.Time `gorm:"not null"`
}

type EmailSuppression struct {
	Email     string    `gorm:"primaryKey;type:text" json:"email"`
	Reason    string    `gorm:"not null;type:text" json:"reason"`
	UpdatedBy string    `gorm:"not null;type:text" json:"updatedBy"`
	CreatedAt time.Time `gorm:"not null" json:"createdAt"`
}
