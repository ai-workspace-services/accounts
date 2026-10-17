package api

import (
	"context"
	"errors"
	"strings"

	"account/internal/service"
)

// MarketingRecipient marks promotional messages explicitly. Transactional mail
// leaves this nil, and must not contain promotions. The durable dispatcher
// prepares marketing at the final SMTP boundary so revoked consent is checked.
type MarketingRecipient struct {
	UserID        string
	ExpectedEmail string
	Category      string
	Important     bool
	MonthlyDigest bool
}

var ErrMarketingSuppressed = errors.New("marketing recipient has not consented to this category or frequency")

func (h *handler) deliverEmail(ctx context.Context, msg EmailMessage) error {
	prepared, err := h.prepareEmail(ctx, msg)
	if err != nil {
		return err
	}
	return h.emailSender.Send(ctx, prepared)
}
func (h *handler) prepareEmail(ctx context.Context, msg EmailMessage) (EmailMessage, error) {
	if msg.Marketing == nil {
		return msg, nil
	}
	target := msg.Marketing
	user, err := h.store.GetUserByID(ctx, target.UserID)
	if err != nil || user == nil || !user.Active || !user.EmailVerified || (target.ExpectedEmail != "" && !strings.EqualFold(strings.TrimSpace(user.Email), target.ExpectedEmail)) {
		return msg, ErrMarketingSuppressed
	}
	p, err := service.GetEmailPreferences(ctx, user.ID)
	if err != nil {
		return msg, err
	}
	if (p.Frequency != "important" && p.Frequency != "monthly") || !service.EmailCategoryAllowed(p, target.Category) || (p.Frequency == "important" && !target.Important) || (p.Frequency == "monthly" && !target.MonthlyDigest) {
		return msg, ErrMarketingSuppressed
	}
	s, err := service.GetEmailManagementSettings(ctx)
	if err != nil {
		return msg, err
	}
	if _, err = service.ValidateEmailManagementSettings(s); err != nil {
		return msg, err
	}
	pref, err := service.IssueEmailPreferenceToken(ctx, user.ID, user.Email, "preferences")
	if err != nil {
		return msg, err
	}
	unsub, err := service.IssueEmailPreferenceToken(ctx, user.ID, user.Email, "unsubscribe")
	if err != nil {
		return msg, err
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
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(msg.Subject)), "AD:") {
		msg.Subject = "AD: " + msg.Subject
	}
	msg.ConsentVersion = p.Version
	msg.UnsubscribeURL = s.ConsoleURL + "/api/auth/email-unsubscribe?key=" + unsub
	// Recheck after preparing the message, immediately before passing it to SMTP.
	latest, err := service.GetEmailPreferences(ctx, user.ID)
	if err != nil {
		return msg, err
	}
	if latest.Version != p.Version {
		return msg, ErrMarketingSuppressed
	}
	return msg, nil
}
