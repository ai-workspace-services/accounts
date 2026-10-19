package api

import (
	"account/internal/store"
	"context"
	"strings"
	"time"
)

func (h *handler) proxyAccountBlocked(ctx context.Context, user *store.User) (bool, error) {
	// Reconcile calendar grants before inspecting blocked IDs. Otherwise an
	// exhausted account can never reach the subsequent config/renewal checks.
	if err := h.ensureFreeQuotaPeriod(ctx, user, time.Now().UTC()); err != nil {
		return false, err
	}
	blacklisted, err := h.store.IsBlacklisted(ctx, strings.ToLower(strings.TrimSpace(user.Email)))
	if err != nil || blacklisted {
		return blacklisted, err
	}
	blocked, err := h.store.ListProxyBlockedAccountUUIDs(ctx)
	return blocked[strings.TrimSpace(user.ID)], err
}
