package api

import (
	"account/internal/store"
	"context"
	"strings"
)

func (h *handler) proxyAccountBlocked(ctx context.Context, user *store.User) (bool, error) {
	blacklisted, err := h.store.IsBlacklisted(ctx, strings.ToLower(strings.TrimSpace(user.Email)))
	if err != nil || blacklisted {
		return blacklisted, err
	}
	blocked, err := h.store.ListProxyBlockedAccountUUIDs(ctx)
	return blocked[strings.TrimSpace(user.ID)], err
}
