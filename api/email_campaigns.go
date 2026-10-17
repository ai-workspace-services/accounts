package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"html"
	"log/slog"
	"net/http"
	"net/mail"
	"net/textproto"
	"strings"
	"time"

	"account/internal/model"
	"account/internal/service"
	"account/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func emailCampaignError(c *gin.Context, err error) {
	if errors.Is(err, service.ErrEmailQueueConflict) {
		respondError(c, 409, "email_queue_conflict", err.Error())
		return
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		respondError(c, 404, "not_found", "email record not found")
		return
	}
	emailPreferenceError(c, err)
}
func (h *handler) listEmailCampaigns(c *gin.Context) {
	if _, ok := h.requireEmailRoot(c); !ok {
		return
	}
	rows, err := service.ListEmailCampaigns(c.Request.Context())
	if err != nil {
		emailCampaignError(c, err)
		return
	}
	c.JSON(200, gin.H{"campaigns": rows, "senderReady": h.emailSenderConfigured, "backgroundEnabled": h.emailWorkerContext != nil})
}
func (h *handler) createEmailCampaign(c *gin.Context) {
	actor, ok := h.requireEmailRoot(c)
	if !ok {
		return
	}
	var req model.EmailCampaign
	limitReferralRequestBody(c, 20*1024)
	if c.ShouldBindJSON(&req) != nil {
		respondError(c, 400, "invalid_request", "invalid email campaign")
		return
	}
	if err := service.ValidateEmailCampaign(req); err != nil {
		respondError(c, 400, "invalid_campaign", err.Error())
		return
	}
	row, err := service.CreateEmailCampaign(c.Request.Context(), req, actor.ID)
	if err != nil {
		emailCampaignError(c, err)
		return
	}
	c.JSON(201, row)
}
func (h *handler) updateEmailCampaign(c *gin.Context) {
	if _, ok := h.requireEmailRoot(c); !ok {
		return
	}
	var req model.EmailCampaign
	limitReferralRequestBody(c, 20*1024)
	if c.ShouldBindJSON(&req) != nil {
		respondError(c, 400, "invalid_request", "invalid campaign")
		return
	}
	req.ID = c.Param("campaignId")
	if err := service.ValidateEmailCampaign(req); err != nil {
		respondError(c, 400, "invalid_campaign", err.Error())
		return
	}
	if err := service.SaveEmailCampaign(c.Request.Context(), req); err != nil {
		emailCampaignError(c, err)
		return
	}
	row, err := service.GetEmailCampaign(c.Request.Context(), req.ID)
	if err != nil {
		emailCampaignError(c, err)
		return
	}
	c.JSON(200, row)
}

// Audience classification reads persisted team membership and billing facts.
// A temporary unlimited override does not turn a Free user into a subscriber.
func (h *handler) emailAudienceMatches(ctx context.Context, u *store.User, audience string, now time.Time) (bool, error) {
	if u == nil || !u.Active || !u.EmailVerified || !strings.EqualFold(u.Role, store.RoleUser) || h.isReadOnlyAccount(u) {
		return false, nil
	}
	if audience == "all" {
		return true, nil
	}
	team := false
	if st, ok := h.store.(store.NetworkTeamStore); ok {
		member, err := st.GetNetworkMembership(ctx, u.ID)
		if err != nil {
			return false, err
		}
		if member.TeamID != "" {
			teams, err := st.ListNetworkTeams(ctx)
			if err != nil {
				return false, err
			}
			for _, t := range teams {
				if t.ID == member.TeamID && t.Active && !now.Before(t.ValidFrom) && now.Before(t.ValidUntil) {
					team = true
					break
				}
			}
		}
	}
	if audience == "team" {
		return team, nil
	}
	if team {
		return false, nil
	}
	subscribed := subscriptionValidityActive(u, now)
	subs, err := h.store.ListSubscriptionsByUser(ctx, u.ID)
	if err != nil {
		return false, err
	}
	for i := range subs {
		sub := &subs[i]
		if sub.Kind != "subscription" {
			continue
		}
		status := strings.ToLower(sub.Status)
		if status == "active" || status == "trialing" || status == "canceling" || status == "canceled" || status == "cancelled" {
			end, err := h.subscriptionEffectivePeriodEnd(ctx, sub)
			if err != nil {
				return false, err
			}
			if !end.IsZero() && now.Before(end) {
				subscribed = true
			}
		}
	}
	if audience == "subscriber" {
		return subscribed, nil
	}
	return !subscribed, nil
}
func (h *handler) collectEmailRecipients(ctx context.Context, campaign model.EmailCampaign) ([]store.User, int, error) {
	var after string
	result := []store.User{}
	matched := 0
	now := time.Now().UTC()
	for {
		page, err := h.store.ListUsersPage(ctx, after, 200)
		if err != nil {
			return nil, 0, err
		}
		if len(page) == 0 {
			break
		}
		for i := range page {
			u := page[i]
			match, err := h.emailAudienceMatches(ctx, &u, campaign.Audience, now)
			if err != nil {
				return nil, 0, err
			}
			if !match {
				continue
			}
			matched++
			suppressed, err := service.EmailSuppressed(ctx, u.Email)
			if err != nil {
				return nil, 0, err
			}
			if suppressed {
				continue
			}
			blocked, err := h.store.IsBlacklisted(ctx, u.Email)
			if err != nil {
				return nil, 0, err
			}
			if blocked {
				continue
			}
			p, err := service.GetEmailPreferences(ctx, u.ID)
			if err != nil {
				return nil, 0, err
			}
			if !service.EmailCategoryAllowed(p, "productUpdates") || p.Frequency != campaign.DeliveryMode {
				continue
			}
			allowed, err := service.EmailFrequencyAvailable(ctx, u.ID, p.Frequency, now)
			if err != nil {
				return nil, 0, err
			}
			if !allowed {
				continue
			}
			result = append(result, u)
			if len(result) > 10000 {
				return nil, 0, errors.New("audience exceeds 10000 recipients; split the campaign")
			}
		}
		after = page[len(page)-1].ID
	}
	return result, matched, nil
}
func (h *handler) previewEmailCampaign(c *gin.Context) {
	if _, ok := h.requireEmailRoot(c); !ok {
		return
	}
	row, err := service.GetEmailCampaign(c.Request.Context(), c.Param("campaignId"))
	if err != nil {
		emailCampaignError(c, err)
		return
	}
	recipients, matched, err := h.collectEmailRecipients(c.Request.Context(), row)
	if err != nil {
		emailCampaignError(c, err)
		return
	}
	plain, body := renderAnnouncement("AD: "+row.Subject, row.Body, row.ActionLabel, row.ActionURL, "")
	c.JSON(200, gin.H{"version": row.Version, "recipients": len(recipients), "audienceDigest": emailAudienceDigest(recipients), "matched": matched, "excluded": matched - len(recipients), "plainBody": plain, "htmlBody": body})
}
func (h *handler) launchEmailCampaign(c *gin.Context) {
	if _, ok := h.requireEmailRoot(c); !ok {
		return
	}
	if !h.emailSenderConfigured {
		respondError(c, 503, "smtp_unavailable", "SMTP sender is not configured")
		return
	}
	var req struct {
		Version        uint64 `json:"version"`
		Recipients     int    `json:"recipients"`
		AudienceDigest string `json:"audienceDigest"`
	}
	if c.ShouldBindJSON(&req) != nil {
		respondError(c, 400, "invalid_request", "preview and confirm the campaign first")
		return
	}
	row, err := service.GetEmailCampaign(c.Request.Context(), c.Param("campaignId"))
	if err != nil {
		emailCampaignError(c, err)
		return
	}
	if row.Version != req.Version || row.Status != "draft" {
		emailCampaignError(c, service.ErrEmailQueueConflict)
		return
	}
	settings, err := service.GetEmailManagementSettings(c.Request.Context())
	if err != nil {
		emailCampaignError(c, err)
		return
	}
	if _, err = service.ValidateEmailManagementSettings(settings); err != nil {
		respondError(c, 400, "settings_required", err.Error())
		return
	}
	recipients, _, err := h.collectEmailRecipients(c.Request.Context(), row)
	if err != nil {
		emailCampaignError(c, err)
		return
	}
	if len(recipients) != req.Recipients || len(recipients) == 0 || emailAudienceDigest(recipients) != req.AudienceDigest {
		respondError(c, 409, "audience_changed", "recipient count changed or is zero; preview again")
		return
	}
	now := time.Now().UTC()
	deliveries := make([]model.EmailDelivery, 0, len(recipients))
	for _, u := range recipients {
		deliveries = append(deliveries, newEmailDelivery(u, row.ID, "", row.Subject, row.Body, row.ActionLabel, row.ActionURL, "productUpdates", row.DeliveryMode, row.Audience, now))
	}
	if err = service.QueueEmailCampaign(c.Request.Context(), row, deliveries); err != nil {
		emailCampaignError(c, err)
		return
	}
	c.JSON(202, gin.H{"queued": len(deliveries)})
}
func emailAudienceDigest(users []store.User) string {
	sum := sha256.New()
	for _, u := range users {
		sum.Write([]byte(u.ID + "\n" + strings.ToLower(u.Email) + "\n"))
	}
	return hex.EncodeToString(sum.Sum(nil))
}
func newEmailDelivery(u store.User, campaignID, stepID, subject, body, label, link, category, mode, audience string, due time.Time) model.EmailDelivery {
	key := "campaign:" + campaignID + ":" + u.ID
	if stepID != "" {
		key = "onboarding:" + stepID + ":" + u.ID
	}
	return model.EmailDelivery{ID: uuid.NewString(), DedupeKey: key, CampaignID: campaignID, StepID: stepID, UserID: u.ID, Email: strings.ToLower(strings.TrimSpace(u.Email)), Subject: subject, Body: body, ActionLabel: label, ActionURL: link, Category: category, DeliveryMode: mode, Audience: audience, Status: "queued", DueAt: due, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
}
func (h *handler) cancelEmailCampaign(c *gin.Context) {
	if _, ok := h.requireEmailRoot(c); !ok {
		return
	}
	if err := service.CancelEmailCampaign(c.Request.Context(), c.Param("campaignId")); err != nil {
		emailCampaignError(c, err)
		return
	}
	c.Status(204)
}
func (h *handler) emailCampaignDeliveries(c *gin.Context) {
	if _, ok := h.requireEmailRoot(c); !ok {
		return
	}
	id := c.Param("campaignId")
	if id == "all" {
		id = ""
	}
	stats, err := service.EmailDeliveryStats(c.Request.Context(), id)
	if err != nil {
		emailCampaignError(c, err)
		return
	}
	rows, err := service.ListEmailDeliveries(c.Request.Context(), id)
	if err != nil {
		emailCampaignError(c, err)
		return
	}
	c.JSON(200, gin.H{"stats": stats, "deliveries": rows})
}
func (h *handler) retryEmailDelivery(c *gin.Context) {
	if _, ok := h.requireEmailRoot(c); !ok {
		return
	}
	var req struct {
		AcknowledgeUnknown bool `json:"acknowledgeUnknown"`
	}
	if c.ShouldBindJSON(&req) != nil {
		respondError(c, 400, "invalid_request", "invalid retry request")
		return
	}
	if err := service.RetryEmailDelivery(c.Request.Context(), c.Param("deliveryId"), req.AcknowledgeUnknown); err != nil {
		emailCampaignError(c, err)
		return
	}
	c.Status(204)
}
func (h *handler) getEmailOnboarding(c *gin.Context) {
	if _, ok := h.requireEmailRoot(c); !ok {
		return
	}
	rows, err := service.ListOnboardingSteps(c.Request.Context())
	if err != nil {
		emailCampaignError(c, err)
		return
	}
	c.JSON(200, gin.H{"steps": rows})
}
func (h *handler) putEmailOnboarding(c *gin.Context) {
	actor, ok := h.requireEmailRoot(c)
	if !ok {
		return
	}
	var row model.EmailOnboardingStep
	limitReferralRequestBody(c, 20*1024)
	if c.ShouldBindJSON(&row) != nil {
		respondError(c, 400, "invalid_request", "invalid onboarding step")
		return
	}
	row.ID = c.Param("stepId")
	if row.Enabled {
		if !h.emailSenderConfigured {
			respondError(c, 503, "smtp_unavailable", "configure SMTP before enabling onboarding")
			return
		}
		settings, err := service.GetEmailManagementSettings(c.Request.Context())
		if err != nil {
			emailCampaignError(c, err)
			return
		}
		if _, err = service.ValidateEmailManagementSettings(settings); err != nil {
			respondError(c, 400, "settings_required", err.Error())
			return
		}
	}
	if err := service.SaveOnboardingStep(c.Request.Context(), row, actor.ID); err != nil {
		if errors.Is(err, service.ErrEmailQueueConflict) || errors.Is(err, service.ErrServiceDBNotInitialized) {
			emailCampaignError(c, err)
		} else {
			respondError(c, 400, "invalid_step", err.Error())
		}
		return
	}
	c.Status(204)
}
func (h *handler) manageEmailSuppressions(c *gin.Context) {
	actor, ok := h.requireEmailRoot(c)
	if !ok {
		return
	}
	if c.Request.Method == http.MethodPut {
		var req struct {
			Email  string `json:"email"`
			Reason string `json:"reason"`
		}
		if c.ShouldBindJSON(&req) != nil {
			respondError(c, 400, "invalid_request", "invalid suppression")
			return
		}
		address, err := mail.ParseAddress(req.Email)
		if err != nil || address.Address != req.Email || (req.Reason != "bounce" && req.Reason != "complaint") {
			respondError(c, 400, "invalid_request", "valid email and bounce/complaint reason required")
			return
		}
		if err = service.PutEmailSuppression(c.Request.Context(), req.Email, req.Reason, actor.ID); err != nil {
			emailCampaignError(c, err)
			return
		}
	}
	rows, err := service.ListEmailSuppressions(c.Request.Context())
	if err != nil {
		emailCampaignError(c, err)
		return
	}
	c.JSON(200, gin.H{"suppressions": rows})
}

func renderAnnouncement(subject, body, label, link, name string) (string, string) {
	subject = strings.ReplaceAll(subject, "{{name}}", name)
	body = strings.ReplaceAll(body, "{{name}}", name)
	plain := subject + "\n\n" + body
	rendered := "<h1>" + html.EscapeString(subject) + "</h1><p>" + strings.ReplaceAll(html.EscapeString(body), "\n", "<br>") + "</p>"
	if link != "" {
		plain += "\n\n" + label + ": " + link
		rendered += "<p><a href=\"" + html.EscapeString(link) + "\">" + html.EscapeString(label) + "</a></p>"
	}
	return plain, rendered
}
func (h *handler) enrollEmailOnboarding(ctx context.Context, u *store.User) error {
	if h.db == nil || u == nil || !u.Active || !u.EmailVerified || u.Role != store.RoleUser {
		return nil
	}
	steps, err := service.ListOnboardingSteps(ctx)
	if err != nil {
		return err
	}
	rows := []model.EmailDelivery{}
	for _, s := range steps {
		if !s.Enabled || u.CreatedAt.IsZero() || u.CreatedAt.Before(s.ActivatedAt) {
			continue
		}
		category := "tutorials"
		if s.ID == "hello" {
			category = ""
		}
		rows = append(rows, newEmailDelivery(*u, "", s.ID, s.Subject, s.Body, s.ActionLabel, s.ActionURL, category, "important", "all", u.CreatedAt.UTC().Add(time.Duration(s.DelayHours)*time.Hour)))
	}
	return service.InsertEmailDeliveries(ctx, rows)
}

func WithEmailWorkerContext(ctx context.Context) Option {
	return func(h *handler) { h.emailWorkerContext = ctx }
}
func (h *handler) startEmailWorker() {
	if h.emailWorkerContext == nil || h.db == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		var after string
		for {
			select {
			case <-h.emailWorkerContext.Done():
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(h.emailWorkerContext, 45*time.Second)
				if h.emailSenderConfigured {
					page, err := h.store.ListUsersPage(ctx, after, 100)
					if err == nil {
						for i := range page {
							if err = h.enrollEmailOnboarding(ctx, &page[i]); err != nil {
								break
							}
						}
						if len(page) < 100 {
							after = ""
						} else {
							after = page[len(page)-1].ID
						}
					}
					if err != nil {
						slog.Error("email onboarding reconciliation failed")
					}
					if _, err = h.dispatchEmailBatch(ctx, 10); err != nil {
						slog.Error("email dispatch failed")
					}
				}
				cancel()
			}
		}
	}()
}
func (h *handler) dispatchEmails(c *gin.Context) {
	if _, ok := h.requireEmailRoot(c); !ok {
		return
	}
	if !h.emailSenderConfigured {
		respondError(c, 503, "smtp_unavailable", "SMTP sender is not configured")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()
	n, err := h.dispatchEmailBatch(ctx, 10)
	if err != nil {
		emailCampaignError(c, err)
		return
	}
	c.JSON(200, gin.H{"processed": n})
}
func (h *handler) dispatchEmailBatch(ctx context.Context, limit int) (int, error) {
	for i := 0; i < limit; i++ {
		if ctx.Err() != nil {
			return i, ctx.Err()
		}
		row, err := service.ClaimEmailDelivery(ctx, time.Now().UTC())
		if errors.Is(err, service.ErrNoEmailDelivery) {
			return i, nil
		}
		if err != nil {
			return i, err
		}
		if err = h.processEmailDelivery(ctx, row); err != nil {
			return i, err
		}
	}
	return limit, nil
}
func (h *handler) processEmailDelivery(ctx context.Context, row model.EmailDelivery) error {
	finish := func(status, reason string) error {
		if status == "retry" && row.Attempts >= 3 {
			status = "failed"
		}
		return service.FinishEmailDelivery(ctx, row, status, reason, time.Now().UTC().Add(time.Minute*time.Duration(row.Attempts*row.Attempts)))
	}
	u, err := h.store.GetUserByID(ctx, row.UserID)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			return finish("suppressed", "account_missing")
		}
		return finish("retry", "account_lookup_unavailable")
	}
	if !u.Active || !u.EmailVerified || !strings.EqualFold(u.Email, row.Email) {
		return finish("suppressed", "account_or_mailbox_changed")
	}
	suppressed, err := service.EmailSuppressed(ctx, u.Email)
	if err != nil {
		return finish("retry", "suppression_lookup_unavailable")
	}
	if suppressed {
		return finish("suppressed", "delivery_suppression")
	}
	blocked, err := h.store.IsBlacklisted(ctx, u.Email)
	if err != nil {
		return finish("retry", "blacklist_lookup_unavailable")
	}
	if blocked {
		return finish("suppressed", "email_blacklisted")
	}
	if row.StepID != "" {
		steps, err := service.ListOnboardingSteps(ctx)
		if err != nil {
			return finish("retry", "onboarding_lookup_unavailable")
		}
		enabled := false
		for _, s := range steps {
			if s.ID == row.StepID && s.Enabled && !u.CreatedAt.Before(s.ActivatedAt) {
				enabled = true
			}
		}
		if !enabled {
			return finish("cancelled", "onboarding_disabled")
		}
	} else {
		campaign, err := service.GetEmailCampaign(ctx, row.CampaignID)
		if err != nil {
			return finish("retry", "campaign_lookup_unavailable")
		}
		if campaign.Status == "cancelled" {
			return finish("cancelled", "campaign_cancelled")
		}
		match, err := h.emailAudienceMatches(ctx, u, row.Audience, time.Now().UTC())
		if err != nil {
			return finish("retry", "audience_lookup_unavailable")
		}
		if !match {
			return finish("suppressed", "audience_changed")
		}
	}
	if row.Category != "" {
		p, err := service.GetEmailPreferences(ctx, u.ID)
		if err != nil {
			return finish("retry", "consent_lookup_unavailable")
		}
		if !service.EmailCategoryAllowed(p, row.Category) {
			return finish("suppressed", "consent_revoked")
		}
	}
	if row.StepID != "" {
		ready, err := service.OnboardingPredecessorSent(ctx, row)
		if errors.Is(err, service.ErrOnboardingStopped) {
			return finish("cancelled", "previous_step_stopped")
		}
		if err != nil {
			return finish("retry", "onboarding_order_unavailable")
		}
		if !ready {
			return service.DeferEmailDelivery(ctx, row, time.Now().UTC().Add(24*time.Hour), "waiting_for_previous_step")
		}
	}
	plain, body := renderAnnouncement(row.Subject, row.Body, row.ActionLabel, row.ActionURL, u.Name)
	msg := EmailMessage{MessageID: row.ID, To: []string{u.Email}, Subject: strings.ReplaceAll(row.Subject, "{{name}}", strings.NewReplacer("\r", " ", "\n", " ").Replace(u.Name)), PlainBody: plain, HTMLBody: body}
	if row.Category != "" {
		p, err := service.GetEmailPreferences(ctx, u.ID)
		if err != nil {
			return finish("retry", "consent_lookup_unavailable")
		}
		if !service.EmailCategoryAllowed(p, row.Category) {
			return finish("suppressed", "consent_revoked")
		}
		available, err := service.EmailFrequencyAvailable(ctx, u.ID, p.Frequency, time.Now().UTC())
		if err != nil {
			return finish("retry", "frequency_lookup_unavailable")
		}
		if !available {
			if row.StepID != "" {
				now := time.Now().UTC()
				due := now.Add(7 * 24 * time.Hour)
				if p.Frequency == "monthly" {
					due = time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
				}
				return service.DeferEmailDelivery(ctx, row, due, "deferred_by_frequency")
			}
			return finish("suppressed", "frequency_cap")
		}
		if row.StepID != "" {
			row.DeliveryMode = p.Frequency
		}
		msg.Marketing = &MarketingRecipient{UserID: u.ID, ExpectedEmail: row.Email, Category: row.Category, Important: row.DeliveryMode == "important", MonthlyDigest: row.DeliveryMode == "monthly"}
	} else {
		settings, err := service.GetEmailManagementSettings(ctx)
		if err != nil {
			return finish("retry", "sender_settings_unavailable")
		}
		if _, err = service.ValidateEmailManagementSettings(settings); err != nil {
			return finish("failed", "sender_settings_required")
		}
		token, err := service.IssueEmailPreferenceToken(ctx, u.ID, u.Email, "preferences")
		if err != nil {
			return finish("retry", "preference_link_unavailable")
		}
		link := settings.ConsoleURL + "/email/preferences?key=" + token
		identity := settings.CompanyName + " · " + settings.PostalAddress + " · " + settings.ContactEmail
		msg.PlainBody += "\n\n管理邮件偏好：" + link + "\n" + identity
		msg.HTMLBody += "<footer><a href=\"" + html.EscapeString(link) + "\">管理邮件偏好</a><p>" + html.EscapeString(identity) + "</p></footer>"
	}
	prepared, err := h.prepareEmail(ctx, msg)
	if errors.Is(err, ErrMarketingSuppressed) {
		return finish("suppressed", "consent_or_frequency")
	}
	if err != nil {
		if row.Attempts >= 3 {
			return finish("failed", "preparation_failed")
		}
		return finish("retry", "preparation_unavailable")
	}
	if err = service.MarkEmailSending(ctx, row, prepared.ConsentVersion); err != nil {
		if errors.Is(err, service.ErrEmailQueueConflict) {
			return finish("suppressed", "eligibility_changed_before_send")
		}
		return err
	}
	sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err = h.emailSender.Send(sendCtx, prepared)
	if err == nil {
		return finish("sent", "")
	}
	var smtpErr *textproto.Error
	if errors.As(err, &smtpErr) {
		if smtpErr.Code >= 400 && smtpErr.Code < 500 {
			if row.Attempts < 3 {
				return finish("retry", "smtp_temporary_rejection")
			}
			return finish("failed", "smtp_retry_exhausted")
		}
		if smtpErr.Code >= 500 && smtpErr.Code < 600 {
			return finish("failed", "smtp_rejected")
		}
	}
	return finish("uncertain", "smtp_outcome_unknown_verify_before_retry")
}

func (h *handler) confirmEmailAcceptance(c *gin.Context) {
	if _, ok := h.requireEmailRoot(c); !ok {
		return
	}
	var req struct {
		Confirmed bool `json:"confirmed"`
	}
	if c.ShouldBindJSON(&req) != nil || !req.Confirmed {
		respondError(c, 400, "confirmation_required", "confirm provider acceptance before resolving")
		return
	}
	if err := service.ConfirmEmailAcceptance(c.Request.Context(), c.Param("deliveryId")); err != nil {
		emailCampaignError(c, err)
		return
	}
	c.Status(204)
}
