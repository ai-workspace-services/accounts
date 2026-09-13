package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

type userGroupsBatchUpdate struct {
	UserID string   `json:"userId"`
	Groups []string `json:"groups"`
}

type userGroupsBatchRequest struct {
	Updates []userGroupsBatchUpdate `json:"updates"`
}

// updateUserGroupsBatch updates several users in one admin request. Each
// update replaces only the group's tags supplied by the console; users and
// their credentials are never deleted or recreated.
func (h *handler) updateUserGroupsBatch(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminUsersRoleWrite); !ok {
		return
	}

	var req userGroupsBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Updates) == 0 {
		respondError(c, http.StatusBadRequest, "invalid_request", "updates must contain at least one user")
		return
	}
	if len(req.Updates) > 500 {
		respondError(c, http.StatusBadRequest, "too_many_users", "a maximum of 500 users can be updated at once")
		return
	}

	users := make([]*store.User, 0, len(req.Updates))
	seen := make(map[string]struct{}, len(req.Updates))
	for _, update := range req.Updates {
		userID := strings.TrimSpace(update.UserID)
		if userID == "" {
			respondError(c, http.StatusBadRequest, "userId_required", "userId is required")
			return
		}
		if _, ok := seen[userID]; ok {
			continue
		}
		seen[userID] = struct{}{}
		user, err := h.store.GetUserByID(c.Request.Context(), userID)
		if err != nil {
			if errors.Is(err, store.ErrUserNotFound) {
				respondError(c, http.StatusNotFound, "user_not_found", "user not found")
				return
			}
			respondError(c, http.StatusInternalServerError, "user_lookup_failed", "failed to fetch user")
			return
		}
		if h.isRootAccount(user) {
			respondError(c, http.StatusForbidden, "root_protected", "root account groups cannot be modified")
			return
		}
		user.Groups = normalizeGroups(update.Groups)
		users = append(users, user)
	}

	for _, user := range users {
		if err := h.store.UpdateUser(c.Request.Context(), user); err != nil {
			respondError(c, http.StatusInternalServerError, "update_failed", "failed to update user groups")
			return
		}
	}

	result := make([]gin.H, 0, len(users))
	for _, user := range users {
		result = append(result, sanitizeUser(user, nil))
	}
	c.JSON(http.StatusOK, gin.H{"message": "groups updated", "users": result})
}
