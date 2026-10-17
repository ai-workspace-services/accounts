package service

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"account/internal/model"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrEmailQueueConflict = errors.New("email campaign or delivery changed; reload before continuing")
var ErrNoEmailDelivery = errors.New("no due email delivery")
var ErrOnboardingStopped = errors.New("previous onboarding step was stopped")

func ValidateEmailContent(subject, body, label, link string) error {
	if strings.TrimSpace(subject) == "" || len(subject) > 200 || strings.ContainsAny(subject, "\r\n") || strings.TrimSpace(body) == "" || len(body) > 12000 {
		return errors.New("subject (1–200 characters) and plain-text content (up to 12000 bytes) are required")
	}
	if len(label) > 120 || (label == "") != (link == "") {
		return errors.New("action label and URL must be supplied together")
	}
	if link != "" {
		u, err := url.Parse(link)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || strings.ContainsAny(link, "\r\n") {
			return errors.New("action URL must be HTTPS")
		}
	}
	return nil
}
func ValidateEmailCampaign(c model.EmailCampaign) error {
	if c.Kind != "feature" && c.Kind != "app_release" && c.Kind != "announcement" {
		return errors.New("invalid announcement kind")
	}
	if c.Audience != "all" && c.Audience != "free" && c.Audience != "subscriber" && c.Audience != "team" {
		return errors.New("invalid audience")
	}
	if c.DeliveryMode != "important" && c.DeliveryMode != "monthly" {
		return errors.New("invalid delivery mode")
	}
	return ValidateEmailContent(c.Subject, c.Body, c.ActionLabel, c.ActionURL)
}
func ListEmailCampaigns(ctx context.Context) ([]model.EmailCampaign, error) {
	d := currentDB()
	if d == nil {
		return nil, ErrServiceDBNotInitialized
	}
	rows := []model.EmailCampaign{}
	err := d.WithContext(ctx).Order("created_at DESC, id DESC").Limit(100).Find(&rows).Error
	if err == nil {
		for i := range rows {
			if rows[i].Status != "queued" {
				continue
			}
			var pending int64
			if countErr := d.WithContext(ctx).Model(&model.EmailDelivery{}).Where("campaign_id = ? AND status IN ?", rows[i].ID, []string{"queued", "retry", "processing", "sending"}).Count(&pending).Error; countErr != nil {
				return nil, countErr
			}
			if pending == 0 {
				rows[i].Status = "completed"
			}
		}
	}
	return rows, err
}
func GetEmailCampaign(ctx context.Context, id string) (model.EmailCampaign, error) {
	var c model.EmailCampaign
	d := currentDB()
	if d == nil {
		return c, ErrServiceDBNotInitialized
	}
	err := d.WithContext(ctx).First(&c, "id = ?", id).Error
	return c, err
}
func CreateEmailCampaign(ctx context.Context, c model.EmailCampaign, actor string) (model.EmailCampaign, error) {
	if err := ValidateEmailCampaign(c); err != nil {
		return c, err
	}
	d := currentDB()
	if d == nil {
		return c, ErrServiceDBNotInitialized
	}
	now := time.Now().UTC()
	c.ID = uuid.NewString()
	c.Version = 1
	c.Status = "draft"
	c.CreatedBy = actor
	c.CreatedAt = now
	c.UpdatedAt = now
	return c, d.WithContext(ctx).Create(&c).Error
}
func SaveEmailCampaign(ctx context.Context, c model.EmailCampaign) error {
	if err := ValidateEmailCampaign(c); err != nil {
		return err
	}
	d := currentDB()
	if d == nil {
		return ErrServiceDBNotInitialized
	}
	result := d.WithContext(ctx).Model(&model.EmailCampaign{}).Where("id = ? AND version = ? AND status = ?", c.ID, c.Version, "draft").Updates(map[string]any{"kind": c.Kind, "subject": c.Subject, "body": c.Body, "action_label": c.ActionLabel, "action_url": c.ActionURL, "audience": c.Audience, "delivery_mode": c.DeliveryMode, "version": c.Version + 1, "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrEmailQueueConflict
	}
	return nil
}
func QueueEmailCampaign(ctx context.Context, c model.EmailCampaign, deliveries []model.EmailDelivery) error {
	d := currentDB()
	if d == nil {
		return ErrServiceDBNotInitialized
	}
	return d.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&model.EmailCampaign{}).Where("id = ? AND version = ? AND status = ?", c.ID, c.Version, "draft").Updates(map[string]any{"status": "queued", "version": c.Version + 1, "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrEmailQueueConflict
		}
		if len(deliveries) > 0 {
			return tx.CreateInBatches(&deliveries, 100).Error
		}
		return nil
	})
}
func CancelEmailCampaign(ctx context.Context, id string) error {
	d := currentDB()
	if d == nil {
		return ErrServiceDBNotInitialized
	}
	return d.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&model.EmailCampaign{}).Where("id = ? AND status IN ?", id, []string{"draft", "queued"}).Updates(map[string]any{"status": "cancelled", "version": gorm.Expr("version + 1"), "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrEmailQueueConflict
		}
		return tx.Model(&model.EmailDelivery{}).Where("campaign_id = ? AND status IN ?", id, []string{"queued", "retry", "processing"}).Updates(map[string]any{"status": "cancelled", "updated_at": time.Now().UTC()}).Error
	})
}
func EmailDeliveryStats(ctx context.Context, campaignID string) (map[string]int64, error) {
	d := currentDB()
	if d == nil {
		return nil, ErrServiceDBNotInitialized
	}
	type count struct {
		Status string
		Count  int64
	}
	rows := []count{}
	q := d.WithContext(ctx).Model(&model.EmailDelivery{})
	if campaignID != "" {
		q = q.Where("campaign_id = ?", campaignID)
	}
	err := q.Select("status, count(*) AS count").Group("status").Scan(&rows).Error
	result := map[string]int64{}
	for _, r := range rows {
		result[r.Status] = r.Count
	}
	return result, err
}
func ListEmailDeliveries(ctx context.Context, campaignID string) ([]model.EmailDelivery, error) {
	d := currentDB()
	if d == nil {
		return nil, ErrServiceDBNotInitialized
	}
	rows := []model.EmailDelivery{}
	q := d.WithContext(ctx)
	if campaignID != "" {
		q = q.Where("campaign_id = ?", campaignID)
	}
	err := q.Order("created_at DESC, id DESC").Limit(100).Find(&rows).Error
	return rows, err
}
func ListOnboardingSteps(ctx context.Context) ([]model.EmailOnboardingStep, error) {
	d := currentDB()
	if d == nil {
		return nil, ErrServiceDBNotInitialized
	}
	defaults := []model.EmailOnboardingStep{
		{ID: "hello", Version: 1, Enabled: true, Subject: "Hello, welcome to XWorkmate", Body: "Your account is ready. Review account settings, security options and your current plan to get started.", DelayHours: 0, ActivatedAt: time.Now().UTC()},
		{ID: "first_steps", Version: 1, Enabled: true, Subject: "Your first steps", Body: "Open the regional entry pool, choose an entry available to your account, and connect in the app.", DelayHours: 24, ActivatedAt: time.Now().UTC()},
		{ID: "discover_more", Version: 1, Enabled: true, Subject: "Discover more", Body: "After basic setup, visit integrations to connect the services you use.", DelayHours: 72, ActivatedAt: time.Now().UTC()},
	}
	for i := range defaults {
		d.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&defaults[i])
	}
	rows := []model.EmailOnboardingStep{}
	err := d.WithContext(ctx).Order("delay_hours ASC, id ASC").Find(&rows).Error
	return rows, err
}
func SaveOnboardingStep(ctx context.Context, s model.EmailOnboardingStep, actor string) error {
	if s.ID != "hello" && s.ID != "first_steps" && s.ID != "discover_more" {
		return errors.New("invalid onboarding step")
	}
	if s.DelayHours < 0 || s.DelayHours > 720 || (s.ID == "hello" && s.DelayHours != 0) || (s.ID != "hello" && s.DelayHours < 24) {
		return errors.New("Hello is immediate; tutorial delay must be 24–720 hours")
	}
	if err := ValidateEmailContent(s.Subject, s.Body, s.ActionLabel, s.ActionURL); err != nil {
		return err
	}
	d := currentDB()
	if d == nil {
		return ErrServiceDBNotInitialized
	}
	return d.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var old model.EmailOnboardingStep
		err := tx.First(&old, "id = ?", s.ID).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil && old.Version != s.Version {
			return ErrEmailQueueConflict
		}
		now := time.Now().UTC()
		s.UpdatedAt = now
		s.UpdatedBy = actor
		s.ActivatedAt = old.ActivatedAt
		if s.Enabled && (!old.Enabled || s.ActivatedAt.IsZero()) {
			s.ActivatedAt = now
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if s.Version != 0 {
				return ErrEmailQueueConflict
			}
			s.Version = 1
			result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&s)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrEmailQueueConflict
			}
			return nil
		}
		result := tx.Model(&model.EmailOnboardingStep{}).Where("id = ? AND version = ?", s.ID, s.Version).Updates(map[string]any{"version": s.Version + 1, "enabled": s.Enabled, "subject": s.Subject, "body": s.Body, "action_label": s.ActionLabel, "action_url": s.ActionURL, "delay_hours": s.DelayHours, "activated_at": s.ActivatedAt, "updated_by": actor, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrEmailQueueConflict
		}
		return nil
	})
}
func InsertEmailDeliveries(ctx context.Context, rows []model.EmailDelivery) error {
	if len(rows) == 0 {
		return nil
	}
	d := currentDB()
	if d == nil {
		return ErrServiceDBNotInitialized
	}
	return d.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, 100).Error
}
func EmailSuppressed(ctx context.Context, email string) (bool, error) {
	d := currentDB()
	if d == nil {
		return false, ErrServiceDBNotInitialized
	}
	var n int64
	err := d.WithContext(ctx).Model(&model.EmailSuppression{}).Where("email = ?", strings.ToLower(strings.TrimSpace(email))).Count(&n).Error
	return n > 0, err
}
func PutEmailSuppression(ctx context.Context, email, reason, actor string) error {
	d := currentDB()
	if d == nil {
		return ErrServiceDBNotInitialized
	}
	row := model.EmailSuppression{Email: strings.ToLower(strings.TrimSpace(email)), Reason: reason, UpdatedBy: actor, CreatedAt: time.Now().UTC()}
	return d.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
}
func ListEmailSuppressions(ctx context.Context) ([]model.EmailSuppression, error) {
	d := currentDB()
	if d == nil {
		return nil, ErrServiceDBNotInitialized
	}
	rows := []model.EmailSuppression{}
	err := d.WithContext(ctx).Order("created_at DESC").Limit(100).Find(&rows).Error
	return rows, err
}
func EmailFrequencyAvailable(ctx context.Context, userID, frequency string, now time.Time) (bool, error) {
	d := currentDB()
	if d == nil {
		return false, ErrServiceDBNotInitialized
	}
	var week, month, uncertain int64
	query := func() *gorm.DB {
		return d.WithContext(ctx).Model(&model.EmailDelivery{}).Where("user_id = ? AND category <> ''", userID)
	}
	if err := query().Where("status = ?", "uncertain").Count(&uncertain).Error; err != nil {
		return false, err
	}
	if uncertain > 0 {
		return false, nil
	}
	if err := query().Where("status = ? AND sent_at >= ?", "sent", now.Add(-7*24*time.Hour)).Count(&week).Error; err != nil {
		return false, err
	}
	if err := query().Where("status = ? AND sent_at >= ?", "sent", now.Add(-30*24*time.Hour)).Count(&month).Error; err != nil {
		return false, err
	}
	if week >= 1 || month >= 3 {
		return false, nil
	}
	if frequency == "monthly" {
		var calendarMonth int64
		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		if err := query().Where("status = ? AND sent_at >= ?", "sent", start).Count(&calendarMonth).Error; err != nil {
			return false, err
		}
		if calendarMonth > 0 {
			return false, nil
		}
	}
	return true, nil
}

// A per-recipient durable lease serializes sends across processes/campaigns.
// Expired pre-send leases are retryable; an interrupted SMTP send is uncertain.
func ClaimEmailDelivery(ctx context.Context, now time.Time) (model.EmailDelivery, error) {
	var result model.EmailDelivery
	d := currentDB()
	if d == nil {
		return result, ErrServiceDBNotInitialized
	}
	if err := d.WithContext(ctx).Model(&model.EmailDelivery{}).Where("status = ? AND lease_until < ?", "sending", now).Updates(map[string]any{"status": "uncertain", "last_error": "delivery outcome unknown; verify before retry", "updated_at": now}).Error; err != nil {
		return result, err
	}
	if err := d.WithContext(ctx).Model(&model.EmailDelivery{}).Where("status = ? AND lease_until < ?", "processing", now).Updates(map[string]any{"status": "retry", "due_at": now, "updated_at": now}).Error; err != nil {
		return result, err
	}
	var candidates []model.EmailDelivery
	if err := d.WithContext(ctx).Where("status IN ? AND due_at <= ?", []string{"queued", "retry"}, now).Order("due_at ASC, id ASC").Limit(30).Find(&candidates).Error; err != nil {
		return result, err
	}
	for _, candidate := range candidates {
		token := uuid.NewString()
		until := now.Add(2 * time.Minute)
		err := d.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			lease := model.EmailRecipientLease{UserID: candidate.UserID, Token: token, ExpiresAt: until}
			acquired := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}}, DoUpdates: clause.Assignments(map[string]any{"token": token, "expires_at": until}), Where: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "email_recipient_leases.expires_at < ?", Vars: []any{now}}}}}).Create(&lease)
			if acquired.Error != nil {
				return acquired.Error
			}
			if acquired.RowsAffected != 1 {
				return ErrEmailQueueConflict
			}
			claimed := tx.Model(&model.EmailDelivery{}).Where("id = ? AND status IN ? AND due_at <= ?", candidate.ID, []string{"queued", "retry"}, now).Updates(map[string]any{"status": "processing", "lease_token": token, "lease_until": until, "attempts": gorm.Expr("attempts + 1"), "updated_at": now})
			if claimed.Error != nil {
				return claimed.Error
			}
			if claimed.RowsAffected != 1 {
				return ErrEmailQueueConflict
			}
			candidate.LeaseToken = token
			candidate.LeaseUntil = &until
			candidate.Attempts++
			candidate.Status = "processing"
			result = candidate
			return nil
		})
		if err == nil {
			return result, nil
		}
		if !errors.Is(err, ErrEmailQueueConflict) {
			return result, err
		}
	}
	return result, ErrNoEmailDelivery
}
func MarkEmailSending(ctx context.Context, row model.EmailDelivery, consentVersion ...uint64) error {
	d := currentDB()
	if d == nil {
		return ErrServiceDBNotInitialized
	}
	query := d.WithContext(ctx).Model(&model.EmailDelivery{}).Where("id = ? AND status = ? AND lease_token = ? AND lease_until > ?", row.ID, "processing", row.LeaseToken, time.Now().UTC())
	query = query.Where("NOT EXISTS (SELECT 1 FROM email_suppressions WHERE email = ?)", row.Email)
	if row.StepID != "" {
		query = query.Where("EXISTS (SELECT 1 FROM email_onboarding_steps WHERE id = ? AND enabled = ? AND activated_at <= ?)", row.StepID, true, row.CreatedAt)
	}
	if row.Category != "" && len(consentVersion) > 0 {
		column := map[string]string{"productUpdates": "product_updates", "tutorials": "tutorials", "offers": "offers"}[row.Category]
		if column == "" {
			return ErrEmailQueueConflict
		}
		query = query.Where("EXISTS (SELECT 1 FROM email_preferences WHERE user_id = ? AND version = ? AND "+column+" = ?)", row.UserID, consentVersion[0], true)
	}
	result := query.Updates(map[string]any{"status": "sending", "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrEmailQueueConflict
	}
	return nil
}
func FinishEmailDelivery(ctx context.Context, row model.EmailDelivery, status, reason string, due time.Time) error {
	d := currentDB()
	if d == nil {
		return ErrServiceDBNotInitialized
	}
	now := time.Now().UTC()
	return d.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		updates := map[string]any{"status": status, "last_error": reason, "lease_token": "", "lease_until": nil, "updated_at": now}
		if status == "sent" {
			updates["sent_at"] = now
		}
		if status == "retry" {
			updates["due_at"] = due
		}
		result := tx.Model(&model.EmailDelivery{}).Where("id = ? AND lease_token = ? AND status IN ?", row.ID, row.LeaseToken, []string{"processing", "sending"}).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if err := tx.Where("user_id = ? AND token = ?", row.UserID, row.LeaseToken).Delete(&model.EmailRecipientLease{}).Error; err != nil {
			return err
		}
		if result.RowsAffected != 1 {
			return ErrEmailQueueConflict
		}
		return nil
	})
}
func RetryEmailDelivery(ctx context.Context, id string, ackUnknown bool) error {
	d := currentDB()
	if d == nil {
		return ErrServiceDBNotInitialized
	}
	var delivery model.EmailDelivery
	if err := d.WithContext(ctx).First(&delivery, "id = ?", id).Error; err != nil {
		return err
	}
	if delivery.CampaignID != "" {
		var campaign model.EmailCampaign
		if err := d.WithContext(ctx).First(&campaign, "id = ?", delivery.CampaignID).Error; err != nil {
			return err
		}
		if campaign.Status == "cancelled" {
			return ErrEmailQueueConflict
		}
	}
	statuses := []string{"failed"}
	if ackUnknown {
		statuses = append(statuses, "uncertain")
	}
	result := d.WithContext(ctx).Model(&model.EmailDelivery{}).Where("id = ? AND status IN ?", id, statuses).Updates(map[string]any{"status": "retry", "due_at": time.Now().UTC(), "attempts": 0, "last_error": "", "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrEmailQueueConflict
	}
	return nil
}

func DeferEmailDelivery(ctx context.Context, row model.EmailDelivery, due time.Time, reason string) error {
	d := currentDB()
	if d == nil {
		return ErrServiceDBNotInitialized
	}
	return d.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&model.EmailDelivery{}).Where("id = ? AND status = ? AND lease_token = ?", row.ID, "processing", row.LeaseToken).Updates(map[string]any{"status": "retry", "due_at": due, "attempts": gorm.Expr("attempts - 1"), "last_error": reason, "lease_token": "", "lease_until": nil, "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrEmailQueueConflict
		}
		return tx.Where("user_id = ? AND token = ?", row.UserID, row.LeaseToken).Delete(&model.EmailRecipientLease{}).Error
	})
}
func OnboardingPredecessorSent(ctx context.Context, row model.EmailDelivery) (bool, error) {
	previous := ""
	if row.StepID == "first_steps" {
		previous = "hello"
	}
	if row.StepID == "discover_more" {
		previous = "first_steps"
	}
	if previous == "" {
		return true, nil
	}
	d := currentDB()
	if d == nil {
		return false, ErrServiceDBNotInitialized
	}
	var prior model.EmailDelivery
	err := d.WithContext(ctx).First(&prior, "user_id = ? AND step_id = ? AND campaign_id = ?", row.UserID, previous, "").Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return true, nil
	}
	if err == nil && (prior.Status == "suppressed" || prior.Status == "cancelled") {
		return false, ErrOnboardingStopped
	}
	return prior.Status == "sent", err
}

// Root may resolve a provider-verified acceptance without sending again.
func ConfirmEmailAcceptance(ctx context.Context, id string) error {
	d := currentDB()
	if d == nil {
		return ErrServiceDBNotInitialized
	}
	now := time.Now().UTC()
	result := d.WithContext(ctx).Model(&model.EmailDelivery{}).Where("id = ? AND status = ?", id, "uncertain").Updates(map[string]any{"status": "sent", "sent_at": now, "last_error": "smtp_acceptance_confirmed_by_root", "lease_token": "", "lease_until": nil, "updated_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrEmailQueueConflict
	}
	return nil
}
