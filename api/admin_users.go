package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"account/internal/store"
)

type createCustomUserRequest struct {
	Email  string   `json:"email"`
	UUID   string   `json:"uuid"`
	Groups []string `json:"groups"`
}

func normalizeGroups(values []string) []string {
	if len(values) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(values))
	groups := make([]string, 0, len(values))
	for _, value := range values {
		normalized := strings.TrimSpace(value)
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		groups = append(groups, normalized)
	}
	if len(groups) == 0 {
		return nil
	}
	return groups
}

func generatePasswordHash() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(hex.EncodeToString(b)), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func (h *handler) createCustomUser(c *gin.Context) {
	requestUser, ok := h.requireAdminPermission(c, permissionAdminUsersRoleWrite)
	if !ok {
		return
	}

	if !h.isRootAccount(requestUser) {
		respondError(c, http.StatusForbidden, "root_only", "only root account can create custom uuid users")
		return
	}

	var req createCustomUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" || !strings.Contains(email, "@") {
		respondError(c, http.StatusBadRequest, "invalid_email", "email must be a valid address")
		return
	}

	userUUID := strings.TrimSpace(req.UUID)
	if _, err := uuid.Parse(userUUID); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_uuid", "uuid must be a valid UUID")
		return
	}

	groups := normalizeGroups(req.Groups)
	if len(groups) == 0 {
		respondError(c, http.StatusBadRequest, "invalid_groups", "at least one group is required")
		return
	}

	blacklisted, err := h.store.IsBlacklisted(c.Request.Context(), email)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "blacklist_check_failed", "failed to verify email status")
		return
	}
	if blacklisted {
		respondError(c, http.StatusForbidden, "email_blacklisted", "this email address is blocked")
		return
	}

	passwordHash, err := generatePasswordHash()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "password_generation_failed", "failed to prepare account credentials")
		return
	}

	user := &store.User{
		ID:            userUUID,
		Name:          email,
		Email:         email,
		PasswordHash:  passwordHash,
		EmailVerified: true,
		Level:         store.LevelUser,
		Role:          store.RoleUser,
		Groups:        groups,
		Active:        true,
		ProxyUUID:     userUUID,
	}

	if err := h.store.CreateUser(c.Request.Context(), user); err != nil {
		switch {
		case errors.Is(err, store.ErrEmailExists):
			respondError(c, http.StatusConflict, "email_exists", "user with this email already exists")
			return
		case errors.Is(err, store.ErrNameExists):
			respondError(c, http.StatusConflict, "name_exists", "user with this name already exists")
			return
		case errors.Is(err, store.ErrInvalidName):
			respondError(c, http.StatusBadRequest, "invalid_name", "name is invalid")
			return
		default:
			respondError(c, http.StatusInternalServerError, "user_creation_failed", "failed to create user")
			return
		}
	}

	createdUser, err := h.store.GetUserByID(c.Request.Context(), user.ID)
	if err != nil {
		createdUser = user
	}

	c.JSON(http.StatusCreated, gin.H{
		"message": "user_created",
		"user":    sanitizeUser(createdUser, nil),
	})
}

func (h *handler) pauseUser(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminUsersPause); !ok {
		return
	}

	userID := c.Param("userId")
	user, err := h.store.GetUserByID(c.Request.Context(), userID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "user_lookup_failed", "failed to find user")
		return
	}
	if h.isRootAccount(user) {
		respondError(c, http.StatusForbidden, "root_protected", "root account cannot be paused")
		return
	}

	state := &store.AccountQuotaState{AccountUUID: user.ID, ThrottleState: "normal", SuspendState: "active", ProxyAccessState: "paused"}
	if existing, err := h.store.GetAccountQuotaState(c.Request.Context(), user.ID); err == nil && existing != nil {
		state = existing
		state.ProxyAccessState = "paused"
	}
	if err := h.store.UpsertAccountQuotaState(c.Request.Context(), state); err != nil {
		respondError(c, http.StatusInternalServerError, "update_failed", "failed to pause proxy access")
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "vless access paused"})
}

func (h *handler) resumeUser(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminUsersResume); !ok {
		return
	}

	userID := c.Param("userId")
	user, err := h.store.GetUserByID(c.Request.Context(), userID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "user_lookup_failed", "failed to find user")
		return
	}
	if h.isRootAccount(user) {
		respondError(c, http.StatusForbidden, "root_protected", "root account is always active")
		return
	}

	state := &store.AccountQuotaState{AccountUUID: user.ID, ThrottleState: "normal", SuspendState: "active", ProxyAccessState: "active"}
	if existing, err := h.store.GetAccountQuotaState(c.Request.Context(), user.ID); err == nil && existing != nil {
		state = existing
		state.ProxyAccessState = "active"
	}
	if err := h.store.UpsertAccountQuotaState(c.Request.Context(), state); err != nil {
		respondError(c, http.StatusInternalServerError, "update_failed", "failed to resume proxy access")
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "vless access resumed"})
}

func (h *handler) deleteUser(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminUsersDelete); !ok {
		return
	}

	userID := c.Param("userId")
	user, err := h.store.GetUserByID(c.Request.Context(), userID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "user_lookup_failed", "failed to find user")
		return
	}
	if h.isRootAccount(user) {
		respondError(c, http.StatusForbidden, "root_protected", "root account cannot be deleted")
		return
	}
	if err := h.store.DeleteUser(c.Request.Context(), userID); err != nil {
		respondError(c, http.StatusInternalServerError, "delete_failed", "failed to delete user")
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "user deleted"})
}

func (h *handler) renewProxyUUID(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminUsersRenewUUID); !ok {
		return
	}

	userID := c.Param("userId")
	var req struct {
		ExpiresInDays int    `json:"expires_in_days"`
		ExpiresAt     string `json:"expires_at"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	user, err := h.store.GetUserByID(c.Request.Context(), userID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "user_lookup_failed", "failed to find user")
		return
	}
	if req.ExpiresAt != "" {
		t, err := time.Parse("2006-01-02", req.ExpiresAt)
		if err != nil {
			respondError(c, http.StatusBadRequest, "invalid_date", "invalid date format, use YYYY-MM-DD")
			return
		}
		expiration := t.UTC()
		user.ProxyUUIDExpiresAt = &expiration
	} else if req.ExpiresInDays > 0 {
		expiration := time.Now().UTC().AddDate(0, 0, req.ExpiresInDays)
		user.ProxyUUIDExpiresAt = &expiration
	} else {
		user.ProxyUUIDExpiresAt = nil
	}

	credentialID, err := uuid.NewV7()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "uuid_generation_failed", "failed to rotate credential UUID")
		return
	}
	// This rotates the network credential only. users.uuid remains immutable.
	user.ProxyUUID = credentialID.String()

	if err := h.store.UpdateUser(c.Request.Context(), user); err != nil {
		respondError(c, http.StatusInternalServerError, "update_failed", "failed to renew proxy UUID")
		return
	}
	if err := h.syncRotatedCredentialUUIDs(c.Request.Context(), user.ID, user.ProxyUUID); err != nil {
		respondError(c, http.StatusInternalServerError, "credential_update_failed", "failed to align tenant bridge credentials")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":    "proxy UUID renewed",
		"proxy_uuid": user.ProxyUUID,
		"expires_at": user.ProxyUUIDExpiresAt,
	})
}

func (h *handler) listBlacklist(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminBlacklistRead); !ok {
		return
	}

	list, err := h.store.ListBlacklist(c.Request.Context())
	if err != nil {
		respondError(c, http.StatusInternalServerError, "list_failed", "failed to list blacklist")
		return
	}

	c.JSON(http.StatusOK, gin.H{"blacklist": list})
}

func (h *handler) addToBlacklist(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminBlacklistWrite); !ok {
		return
	}

	var req struct {
		Email string `json:"email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	if err := h.store.AddToBlacklist(c.Request.Context(), req.Email); err != nil {
		respondError(c, http.StatusInternalServerError, "add_failed", "failed to add to blacklist")
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "email added to blacklist"})
}

func (h *handler) removeFromBlacklist(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminBlacklistWrite); !ok {
		return
	}

	email := c.Param("email")
	if err := h.store.RemoveFromBlacklist(c.Request.Context(), email); err != nil {
		respondError(c, http.StatusInternalServerError, "remove_failed", "failed to remove from blacklist")
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "email removed from blacklist"})
}

func generateRandomUUID() string {
	return uuid.New().String()
}
