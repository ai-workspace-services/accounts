package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"account/internal/model"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const EmailConsentPolicyVersion = "email-preferences-v1"

var ErrEmailPreferenceConflict = errors.New("email preferences changed; reload and try again")
var ErrInvalidEmailPreferenceToken = errors.New("invalid or expired email preference link")

type EmailManagementSettings struct {
	Version       uint64 `json:"version"`
	CompanyName   string `json:"companyName"`
	PostalAddress string `json:"postalAddress"`
	ContactEmail  string `json:"contactEmail"`
	ConsoleURL    string `json:"consoleUrl"`
}

type EmailPreferences struct {
	ProductUpdates bool   `json:"productUpdates"`
	Tutorials      bool   `json:"tutorials"`
	Offers         bool   `json:"offers"`
	Frequency      string `json:"frequency"`
	Version        uint64 `json:"version"`
}

func GetEmailManagementSettings(ctx context.Context) (EmailManagementSettings, error) {
	d := currentDB()
	if d == nil {
		return EmailManagementSettings{}, ErrServiceDBNotInitialized
	}
	var row model.EmailManagementSetting
	err := d.WithContext(ctx).First(&row, "id = ?", "default").Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return EmailManagementSettings{}, nil
	}
	return EmailManagementSettings{row.Version, row.CompanyName, row.PostalAddress, row.ContactEmail, row.ConsoleURL}, err
}

func ValidateEmailManagementSettings(s EmailManagementSettings) (EmailManagementSettings, error) {
	s.CompanyName = strings.TrimSpace(s.CompanyName)
	s.PostalAddress = strings.TrimSpace(s.PostalAddress)
	s.ContactEmail = strings.TrimSpace(s.ContactEmail)
	s.ConsoleURL = strings.TrimRight(strings.TrimSpace(s.ConsoleURL), "/")
	if s.CompanyName == "" || len(s.CompanyName) > 200 || s.PostalAddress == "" || len(s.PostalAddress) > 1000 {
		return s, errors.New("company name and postal address are required")
	}
	address, err := mail.ParseAddress(s.ContactEmail)
	if err != nil || address.Address != s.ContactEmail || strings.ContainsAny(s.ContactEmail, "\r\n") {
		return s, errors.New("valid contact email is required")
	}
	u, err := url.Parse(s.ConsoleURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return s, errors.New("console URL must be an HTTPS origin")
	}
	return s, nil
}

func SaveEmailManagementSettings(ctx context.Context, s EmailManagementSettings, actor string) (EmailManagementSettings, error) {
	s, err := ValidateEmailManagementSettings(s)
	if err != nil {
		return s, err
	}
	d := currentDB()
	if d == nil {
		return s, ErrServiceDBNotInitialized
	}
	row := model.EmailManagementSetting{ID: "default", Version: s.Version + 1, CompanyName: s.CompanyName, PostalAddress: s.PostalAddress, ContactEmail: s.ContactEmail, ConsoleURL: s.ConsoleURL, UpdatedBy: actor, UpdatedAt: time.Now().UTC()}
	err = d.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if s.Version == 0 {
			result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrEmailPreferenceConflict
			}
			return nil
		}
		result := tx.Model(&model.EmailManagementSetting{}).Where("id = ? AND version = ?", "default", s.Version).Updates(map[string]any{"version": row.Version, "company_name": row.CompanyName, "postal_address": row.PostalAddress, "contact_email": row.ContactEmail, "console_url": row.ConsoleURL, "updated_by": actor, "updated_at": row.UpdatedAt})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrEmailPreferenceConflict
		}
		return nil
	})
	if err == nil {
		s.Version = row.Version
	}
	return s, err
}

func GetEmailPreferences(ctx context.Context, userID string) (EmailPreferences, error) {
	d := currentDB()
	if d == nil {
		return EmailPreferences{}, ErrServiceDBNotInitialized
	}
	var row model.EmailPreference
	err := d.WithContext(ctx).First(&row, "user_id = ?", userID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return EmailPreferences{Frequency: "monthly"}, nil
	}
	return EmailPreferences{row.ProductUpdates, row.Tutorials, row.Offers, row.Frequency, row.Version}, err
}

// SaveEmailPreferences persists explicit choices and their evidence atomically.
// No admin override exists for another user's consent.
func SaveEmailPreferences(ctx context.Context, userID string, p EmailPreferences, source string) (EmailPreferences, error) {
	if p.Frequency != "monthly" && p.Frequency != "important" {
		return p, errors.New("invalid email frequency")
	}
	d := currentDB()
	if d == nil {
		return p, ErrServiceDBNotInitialized
	}
	err := d.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row := model.EmailPreference{UserID: userID, ProductUpdates: p.ProductUpdates, Tutorials: p.Tutorials, Offers: p.Offers, Frequency: p.Frequency, Version: p.Version + 1, UpdatedAt: time.Now().UTC()}
		var result *gorm.DB
		if p.Version == 0 {
			result = tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		} else {
			result = tx.Model(&model.EmailPreference{}).Where("user_id = ? AND version = ?", userID, p.Version).Updates(map[string]any{"product_updates": p.ProductUpdates, "tutorials": p.Tutorials, "offers": p.Offers, "frequency": p.Frequency, "version": row.Version, "updated_at": row.UpdatedAt})
		}
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrEmailPreferenceConflict
		}
		p.Version = row.Version
		raw, _ := json.Marshal(p)
		return tx.Create(&model.EmailPreferenceEvent{ID: uuid.NewString(), UserID: userID, Source: source, PolicyVersion: EmailConsentPolicyVersion, Preferences: string(raw), CreatedAt: row.UpdatedAt}).Error
	})
	return p, err
}

// UnsubscribeEmail is idempotent and wins over stale preference-page saves.
func UnsubscribeEmail(ctx context.Context, userID, source string) error {
	d := currentDB()
	if d == nil {
		return ErrServiceDBNotInitialized
	}
	return d.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		row := model.EmailPreference{UserID: userID, Frequency: "monthly", Version: 1, UpdatedAt: now}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}}, DoUpdates: clause.Assignments(map[string]any{"product_updates": false, "tutorials": false, "offers": false, "version": gorm.Expr("email_preferences.version + 1"), "updated_at": now})}).Create(&row).Error; err != nil {
			return err
		}
		return tx.Create(&model.EmailPreferenceEvent{ID: uuid.NewString(), UserID: userID, Source: source, PolicyVersion: EmailConsentPolicyVersion, Preferences: `{"productUpdates":false,"tutorials":false,"offers":false}`, CreatedAt: now}).Error
	})
}

func hashEmailToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func IssueEmailPreferenceToken(ctx context.Context, userID, email, purpose string) (string, error) {
	if purpose != "preferences" && purpose != "unsubscribe" {
		return "", ErrInvalidEmailPreferenceToken
	}
	d := currentDB()
	if d == nil {
		return "", ErrServiceDBNotInitialized
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	ttl := 30 * 24 * time.Hour
	if purpose == "unsubscribe" {
		ttl = 180 * 24 * time.Hour
	}
	row := model.EmailPreferenceToken{Hash: hashEmailToken(token), UserID: userID, Email: strings.ToLower(strings.TrimSpace(email)), Purpose: purpose, ExpiresAt: time.Now().UTC().Add(ttl)}
	return token, d.WithContext(ctx).Create(&row).Error
}

func ResolveEmailPreferenceToken(ctx context.Context, token, purpose string) (model.EmailPreferenceToken, error) {
	var row model.EmailPreferenceToken
	if len(token) != 43 {
		return row, ErrInvalidEmailPreferenceToken
	}
	d := currentDB()
	if d == nil {
		return row, ErrServiceDBNotInitialized
	}
	err := d.WithContext(ctx).First(&row, "hash = ? AND purpose = ? AND expires_at > ?", hashEmailToken(token), purpose, time.Now().UTC()).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return row, ErrInvalidEmailPreferenceToken
	}
	return row, err
}

func EmailCategoryAllowed(p EmailPreferences, category string) bool {
	switch category {
	case "productUpdates":
		return p.ProductUpdates
	case "tutorials":
		return p.Tutorials
	case "offers":
		return p.Offers
	}
	return false
}
