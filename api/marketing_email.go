package api

import (
	"context"
	"errors"
	"strings"

	"account/internal/service"
)

// MarketingRecipient marks promotional messages explicitly. Transactional mail
// leaves this nil, and must not contain promotions. Queued marketing must call
// deliverEmail at delivery time, so revoked consent is checked again.
type MarketingRecipient struct {
	UserID        string
	Category      string
	Important     bool
	MonthlyDigest bool
}

var ErrMarketingSuppressed = errors.New("marketing recipient has not consented to this category or frequency")

func (h *handler) deliverEmail(ctx context.Context, msg EmailMessage) error {
	if msg.Marketing == nil {
		return h.emailSender.Send(ctx, msg)
	}
	target := msg.Marketing
	user, err := h.store.GetUserByID(ctx, target.UserID)
	if err != nil || user == nil || !user.Active || !user.EmailVerified {
		return ErrMarketingSuppressed
	}
	p, err := service.GetEmailPreferences(ctx, user.ID)
	if err != nil {
		return err
	}
	if !service.EmailCategoryAllowed(p, target.Category) || (p.Frequency == "important" && !target.Important) || (p.Frequency == "monthly" && !target.MonthlyDigest) {
		return ErrMarketingSuppressed
	}
	s, err := service.GetEmailManagementSettings(ctx)
	if err != nil {
		return err
	}
	if _, err = service.ValidateEmailManagementSettings(s); err != nil {
		return err
	}
	pref, err := service.IssueEmailPreferenceToken(ctx, user.ID, user.Email, "preferences")
	if err != nil {
		return err
	}
	unsub, err := service.IssueEmailPreferenceToken(ctx, user.ID, user.Email, "unsubscribe")
	if err != nil {
		return err
	}
	plain, body := renderMarketingFooter(s, s.ConsoleURL+"/email/preferences?key="+pref, s.ConsoleURL+"/email/unsubscribe?key="+unsub)
	msg.To = []string{user.Email}
	msg.PlainBody = strings.TrimSpace(msg.PlainBody) + "\n\n" + plain
	if msg.HTMLBody != "" {
		if index := strings.LastIndex(strings.ToLower(msg.HTMLBody), "</body>"); index >= 0 {
			msg.HTMLBody = msg.HTMLBody[:index] + body + msg.HTMLBody[index:]
		} else {
			msg.HTMLBody = strings.TrimSpace(msg.HTMLBody) + body
		}
	}
	msg.UnsubscribeURL = s.ConsoleURL + "/api/auth/email-unsubscribe?key=" + unsub
	// Recheck after preparing the message, immediately before passing it to SMTP.
	latest, err := service.GetEmailPreferences(ctx, user.ID)
	if err != nil {
		return err
	}
	if latest.Version != p.Version {
		return ErrMarketingSuppressed
	}
	return h.emailSender.Send(ctx, msg)
}
