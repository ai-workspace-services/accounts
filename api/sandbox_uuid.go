package api

import (
	"context"
	"strings"
	"time"

	"account/internal/store"
)

const (
	sandboxUserEmail          = "sandbox@svc.plus"
	sandboxUUIDRotationWindow = time.Hour
)

// ensureSandboxProxyUUID renews sandbox access metadata without rotating the
// canonical account UUID used by Portal and Xray.
// It is intentionally strict: only the hard-coded sandbox email is eligible.
func (h *handler) ensureSandboxProxyUUID(ctx context.Context, user *store.User) error {
	if h == nil || user == nil {
		return nil
	}
	email := strings.ToLower(strings.TrimSpace(user.Email))
	if email != sandboxUserEmail {
		return nil
	}

	now := time.Now().UTC()
	needsRenewal := strings.TrimSpace(user.ProxyUUID) != strings.TrimSpace(user.ID) ||
		user.ProxyUUIDExpiresAt == nil ||
		!now.Before(*user.ProxyUUIDExpiresAt)

	if !needsRenewal {
		return nil
	}

	exp := now.Add(sandboxUUIDRotationWindow)
	user.ProxyUUID = strings.TrimSpace(user.ID)
	user.ProxyUUIDExpiresAt = &exp
	return h.store.UpdateUser(ctx, user)
}
