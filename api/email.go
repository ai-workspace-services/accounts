package api

import (
	"context"
	"log/slog"
)

// EmailMessage represents the contents of an email notification.
type EmailMessage struct {
	To             []string
	Subject        string
	PlainBody      string
	HTMLBody       string
	Marketing      *MarketingRecipient
	UnsubscribeURL string
}

// EmailSender sends email notifications.
type EmailSender interface {
	Send(ctx context.Context, msg EmailMessage) error
}

// EmailSenderFunc adapts a function so it can be used as an EmailSender.
type EmailSenderFunc func(ctx context.Context, msg EmailMessage) error

// Send implements EmailSender.
func (f EmailSenderFunc) Send(ctx context.Context, msg EmailMessage) error {
	if f == nil {
		return nil
	}
	return f(ctx, msg)
}

var noopEmailSender EmailSender = EmailSenderFunc(func(ctx context.Context, msg EmailMessage) error {
	_ = ctx
	slog.Warn("email sender not configured; suppressing email delivery", "subject", msg.Subject)
	return nil
})
