package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"account/internal/service"
	"account/internal/store"
)

const (
	permissionAdminSettingsRead   = "admin.settings.read"
	permissionAdminSettingsWrite  = "admin.settings.write"
	permissionAdminUsersMetrics   = "admin.users.metrics.read"
	permissionAdminUsersListRead  = "admin.users.list.read"
	permissionAdminAgentsStatus   = "admin.agents.status.read"
	permissionAdminUsersPause     = "admin.users.pause.write"
	permissionAdminUsersResume    = "admin.users.resume.write"
	permissionAdminUsersDelete    = "admin.users.delete.write"
	permissionAdminUsersRenewUUID = "admin.users.renew_uuid.write"
	permissionAdminUsersRoleWrite = "admin.users.role.write"
	permissionAdminBlacklistRead  = "admin.blacklist.read"
	permissionAdminBlacklistWrite = "admin.blacklist.write"
	// permissionAdminBillingMoneyWrite gates the two operations that move real
	// money: crediting/debiting an account balance, and publishing the price
	// catalog the public pricing page reads. Being able to edit the price list
	// and being able to put money into an account are not the same level of
	// trust, so they no longer share admin.settings.write.
	permissionAdminBillingMoneyWrite = "admin.billing.money.write"
)

var defaultOperatorPermissions = map[string]bool{
	permissionAdminSettingsRead:   true,
	permissionAdminSettingsWrite:  false,
	permissionAdminUsersMetrics:   true,
	permissionAdminUsersListRead:  true,
	permissionAdminAgentsStatus:   true,
	permissionAdminUsersPause:     true,
	permissionAdminUsersResume:    true,
	permissionAdminUsersDelete:    false,
	permissionAdminUsersRenewUUID: true,
	permissionAdminUsersRoleWrite: false,
	permissionAdminBlacklistRead:  true,
	permissionAdminBlacklistWrite: true,
	// Denied to operators by default. It can still be granted deliberately
	// through the permission matrix, but it is not something an operator holds
	// simply by being an operator.
	permissionAdminBillingMoneyWrite: false,
}

func (h *handler) adminUsersMetrics(c *gin.Context) {
	if h.metricsProvider == nil {
		respondError(c, http.StatusServiceUnavailable, "metrics_unavailable", "user metrics provider is not configured")
		return
	}

	if _, ok := h.requireAdminPermission(c, permissionAdminUsersMetrics); !ok {
		return
	}

	metrics, err := h.metricsProvider.Compute(c.Request.Context())
	if err != nil {
		status := http.StatusInternalServerError
		message := "failed to compute user metrics"
		if errors.Is(err, service.ErrUserRepositoryNotConfigured) || errors.Is(err, service.ErrSubscriptionProviderNotConfigured) {
			status = http.StatusServiceUnavailable
			message = "user metrics dependency is not available"
		}
		respondError(c, status, "metrics_unavailable", message)
		return
	}

	c.JSON(http.StatusOK, metrics)
}

func (h *handler) requireAdminPermission(c *gin.Context, permission string) (*store.User, bool) {
	token := h.resolveSessionToken(c)
	if token == "" {
		respondError(c, http.StatusUnauthorized, "session_token_required", "session token is required")
		return nil, false
	}

	sess, ok := h.lookupSession(token)
	if !ok {
		respondError(c, http.StatusUnauthorized, "invalid_session", "session not found or expired")
		return nil, false
	}

	user, err := h.store.GetUserByID(c.Request.Context(), sess.userID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "session_user_lookup_failed", "failed to load session user")
		return nil, false
	}
	if !user.Active {
		respondError(c, http.StatusForbidden, "account_suspended", "your account has been suspended")
		return nil, false
	}

	if h.isReadOnlyAccount(user) && c.Request.Method != http.MethodGet {
		respondError(c, http.StatusForbidden, "read_only_account", "demo account is read-only")
		return nil, false
	}

	if store.IsRootRole(user.Role) {
		return user, true
	}
	if strings.EqualFold(strings.TrimSpace(user.Role), store.RoleAdmin) {
		return user, true
	}

	if store.IsOperatorRole(user.Role) {
		if permission != "" && !h.operatorPermissionAllowed(c, permission) {
			respondError(c, http.StatusForbidden, "forbidden", "operator permission denied")
			return nil, false
		}
		return user, true
	}

	if strings.EqualFold(strings.TrimSpace(user.Role), store.RoleReadOnly) {
		method := c.Request.Method
		if method != http.MethodGet && method != http.MethodHead {
			respondError(c, http.StatusForbidden, "read_only_account", "demo account is read-only")
			return nil, false
		}
		if permission == "" || !hasPermission(user.Permissions, permission) {
			respondError(c, http.StatusForbidden, "forbidden", "readonly permission denied")
			return nil, false
		}
		return user, true
	}

	respondError(c, http.StatusForbidden, "forbidden", "insufficient permissions")
	return nil, false
}

func (h *handler) requireAdminOrOperator(c *gin.Context) (*store.User, bool) {
	return h.requireAdminPermission(c, "")
}

func (h *handler) operatorPermissionAllowed(c *gin.Context, permission string) bool {
	defaultAllowed := defaultOperatorPermissions[permission]
	settings, err := service.GetAdminSettings(c.Request.Context())
	if err != nil {
		return defaultAllowed
	}

	module, ok := settings.Matrix[permission]
	if !ok {
		return defaultAllowed
	}

	allowed, ok := module[store.RoleOperator]
	if !ok {
		return defaultAllowed
	}
	return allowed
}

func hasPermission(permissions []string, target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}
	for _, permission := range permissions {
		normalized := strings.TrimSpace(permission)
		if normalized == "*" || strings.EqualFold(normalized, target) {
			return true
		}
	}
	return false
}

func (h *handler) resolveSessionToken(c *gin.Context) string {
	token := extractToken(c.GetHeader("Authorization"))
	if token == "" {
		if value := c.Query("token"); value != "" {
			token = value
		}
	}
	if token == "" {
		if cookie, err := c.Cookie(sessionCookieName); err == nil {
			cookie = strings.TrimSpace(cookie)
			if cookie != "" {
				token = cookie
			}
		}
	}
	return strings.TrimSpace(token)
}

func registerAdminRoutes(group *gin.RouterGroup, h *handler) {
	admin := group.Group("/admin")
	admin.GET("/users/metrics", h.adminUsersMetrics)
	admin.GET("/agents/status", h.adminAgentStatus)
	admin.GET("/traffic/nodes", h.adminTrafficNodes)
	admin.GET("/traffic/accounts/:uuid", h.adminTrafficAccount)
	admin.GET("/collector/status", h.adminCollectorStatus)
	admin.GET("/scheduler/status", h.adminSchedulerStatus)

	// User management
	admin.POST("/users", h.createCustomUser)
	admin.POST("/users/:userId/pause", h.pauseUser)
	admin.POST("/users/:userId/resume", h.resumeUser)
	admin.DELETE("/users/:userId", h.deleteUser)
	admin.POST("/users/:userId/renew-uuid", h.renewProxyUUID)

	// Email blacklist
	admin.GET("/blacklist", h.listBlacklist)
	admin.POST("/blacklist", h.addToBlacklist)
	admin.DELETE("/blacklist/:email", h.removeFromBlacklist)

	// Sandbox mode
	admin.GET("/sandbox/binding", h.getSandboxBinding)
	admin.POST("/sandbox/bind", h.bindSandboxNode)
}
