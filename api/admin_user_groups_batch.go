package api

import "github.com/gin-gonic/gin"

// Keep the existing route while routing it through the same preview, RBAC,
// idempotency, transactional entitlement update, and audit path as plan edits.
func (h *handler) updateUserGroupsBatch(c *gin.Context) {
	h.adminChangePlanGroups(c, nil)
}
