package api

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"account/internal/store"
)

const (
	sandboxUserEmail          = "sandbox@svc.plus"
	sandboxUUIDRotationWindow = time.Hour
)

// ensureSandboxProxyUUID renews sandbox access metadata without rotating the
// issued network credential used by Xray. The permanent account UUID is
// never changed here.
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
	needsRenewal := strings.TrimSpace(user.ProxyUUID) == "" || user.ProxyUUIDExpiresAt == nil ||
		!now.Before(*user.ProxyUUIDExpiresAt)

	if !needsRenewal {
		return nil
	}

	exp := now.Add(sandboxUUIDRotationWindow)
	credentialID, err := uuid.NewV7()
	if err != nil {
		return err
	}
	user.ProxyUUID = credentialID.String()
	user.ProxyUUIDExpiresAt = &exp
	return h.store.UpdateUser(ctx, user)
}
