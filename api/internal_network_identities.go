package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type internalNetworkIdentity struct {
	UUID        string `json:"uuid"`
	Email       string `json:"email"`
	AccountUUID string `json:"accountUuid"`
}

func (h *handler) internalNetworkIdentities(c *gin.Context) {
	ctx, span := otel.Tracer("accounts").Start(
		c.Request.Context(),
		"accounts.network.identities",
		trace.WithAttributes(attribute.String("stage", "network_identities")),
	)
	defer span.End()

	if h.store == nil {
		span.SetAttributes(
			attribute.String("outcome", "error"),
			attribute.String("error_class", "store_unavailable"),
		)
		span.SetStatus(codes.Error, "store_unavailable")
		respondError(c, http.StatusServiceUnavailable, "store_unavailable", "identity store is not available")
		return
	}

	users, err := h.store.ListUsers(ctx)
	if err != nil {
		span.SetAttributes(
			attribute.String("outcome", "error"),
			attribute.String("error_class", "list_users_failed"),
		)
		span.SetStatus(codes.Error, "list_users_failed")
		respondError(c, http.StatusInternalServerError, "list_users_failed", "failed to load network identities")
		return
	}

	// P1.5: accounts suspended for prolonged billing arrears (suspend_state
	// set by billing-service's SuspendSyncer) are excluded from identity
	// enrichment so they stop accruing usage attribution once cut off.
	blocked, err := h.store.ListSuspendedAccountUUIDs(ctx)
	if err != nil {
		span.SetAttributes(
			attribute.String("outcome", "error"),
			attribute.String("error_class", "list_suspended_failed"),
		)
		span.SetStatus(codes.Error, "list_suspended_failed")
		respondError(c, http.StatusInternalServerError, "list_suspended_failed", "failed to load suspended accounts")
		return
	}

	identities := make([]internalNetworkIdentity, 0, len(users))
	for _, user := range users {
		if !user.Active {
			continue
		}
		isSandbox := strings.EqualFold(strings.TrimSpace(user.Email), sandboxUserEmail)

		// Mirror the proxy-access gate in listAgentUsers: only verified users
		// (whose email round trip activated the trial) get a proxy client, so
		// only they should be attributed. Sandbox is exempt for parity.
		if !user.EmailVerified && !isSandbox {
			continue
		}

		// Sandbox stays exempt (it is never cut off by agent sync either);
		// dropping it here would leave its demo traffic unattributed.
		if blocked[strings.TrimSpace(user.ID)] && !isSandbox {
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

	span.SetAttributes(
		attribute.Int("identity_count", len(identities)),
		attribute.String("outcome", "ok"),
	)

	c.JSON(http.StatusOK, gin.H{
		"generatedAt": time.Now().UTC(),
		"identities":  identities,
	})
}
