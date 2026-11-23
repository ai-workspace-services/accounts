package api

import (
	"account/internal/store"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type internalNetworkIdentity struct {
	UUID        string `json:"uuid"`
	Email       string `json:"email"`
	AccountUUID string `json:"accountUuid"`
}

func (h *handler) internalNetworkIdentities(c *gin.Context) {
	if h.store == nil {
		respondError(c, http.StatusServiceUnavailable, "store_unavailable", "identity store is not available")
		return
	}

	users, err := h.store.ListUsers(c.Request.Context())
	if err != nil {
		respondError(c, http.StatusInternalServerError, "list_users_failed", "failed to load network identities")
		return
	}

	// P1.5: accounts suspended for prolonged billing arrears (suspend_state
	// set by billing-service's SuspendSyncer) are excluded from identity
	// enrichment so they stop accruing usage attribution once cut off.
	blocked, err := h.store.ListSuspendedAccountUUIDs(c.Request.Context())
	if err != nil {
		respondError(c, http.StatusInternalServerError, "list_suspended_failed", "failed to load suspended accounts")
		return
	}

	identities := make([]internalNetworkIdentity, 0, len(users))
	for _, user := range users {
		if !user.Active {
			continue
		}
		if !store.AccountAccessVerified(&user) {
			continue
		}

		if blocked[strings.TrimSpace(user.ID)] {
			continue
		}
		uuid := strings.TrimSpace(user.ProxyUUID)
		if uuid == "" {
			continue
		}

		identities = append(identities, internalNetworkIdentity{
			UUID:        uuid,
			Email:       strings.ToLower(strings.TrimSpace(user.Email)),
			AccountUUID: strings.TrimSpace(user.ID),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"generatedAt": time.Now().UTC(),
		"identities":  identities,
	})
}
