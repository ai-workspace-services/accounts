package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/auth"
	"account/internal/overlay"
	"account/internal/store"
)

const (
	permissionXConnectZeroRead   = "xconnect.zero.read"
	permissionXConnectZeroManage = "xconnect.zero.manage"
)

type overlayAdminBootstrapRequest struct {
	ControllerURL string `json:"controller_url"`
	Network       struct {
		ID                      string `json:"id"`
		DisplayName             string `json:"display_name"`
		CIDR                    string `json:"cidr"`
		GatewayID               string `json:"gateway_id"`
		GatewayWireGuardKey     string `json:"gateway_wireguard_public_key"`
		GatewayWireGuardAddress string `json:"gateway_wireguard_address"`
		GatewayEndpointHost     string `json:"gateway_endpoint_host"`
		GatewayEndpointPort     int    `json:"gateway_endpoint_port"`
		TransportServerName     string `json:"transport_server_name"`
		TransportPort           int    `json:"transport_port"`
		TransportAuthID         string `json:"transport_auth_id"`
	} `json:"network"`
	Invite struct {
		DeviceID string    `json:"device_id"`
		Platform string    `json:"platform"`
		Role     string    `json:"role"`
		Expires  time.Time `json:"expires_at"`
	} `json:"invite"`
}

type overlayInternalBootstrapRequest struct {
	OwnerEmail string                       `json:"owner_email"`
	Bootstrap  overlayAdminBootstrapRequest `json:"bootstrap"`
}

func (h *handler) registerOverlayAdminRoutes(r *gin.Engine) {
	if h.overlayService == nil || h.tokenService == nil {
		return
	}
	group := r.Group("/api/overlay/v1/admin")
	group.Use(h.tokenService.AuthMiddleware())
	group.Use(auth.RequireActiveUser(h.store))
	group.GET("/overview", h.overlayAdminOverview)
	group.GET("/networks", h.overlayAdminNetworks)
	group.GET("/devices", h.overlayAdminDevices)
	group.GET("/invites", h.overlayAdminInvites)
	group.GET("/networks/:networkID/policy", h.overlayAdminPolicy)
	group.POST("/networks/bootstrap", h.overlayAdminBootstrap)
	group.PUT("/networks/:networkID/policy", h.overlayAdminUpdatePolicy)
	group.POST("/devices/:deviceID/revoke", h.overlayAdminRevokeDevice)
}

func (h *handler) overlayAdminOverview(c *gin.Context) {
	if _, ok := h.requireXConnectZeroAccess(c, permissionXConnectZeroRead); !ok {
		return
	}
	value, err := h.overlayService.AdminOverview(c.Request.Context(), auth.GetUserID(c))
	if err != nil {
		respondError(c, http.StatusInternalServerError, "overlay_admin_read_failed", "failed to load XConnect Zero overview")
		return
	}
	c.JSON(http.StatusOK, value)
}

func (h *handler) overlayAdminNetworks(c *gin.Context) {
	if _, ok := h.requireXConnectZeroAccess(c, permissionXConnectZeroRead); !ok {
		return
	}
	value, err := h.overlayService.AdminNetworks(c.Request.Context(), auth.GetUserID(c))
	if err != nil {
		respondError(c, http.StatusInternalServerError, "overlay_admin_read_failed", "failed to load XConnect Zero networks")
		return
	}
	c.JSON(http.StatusOK, gin.H{"networks": value})
}

func (h *handler) overlayAdminDevices(c *gin.Context) {
	if _, ok := h.requireXConnectZeroAccess(c, permissionXConnectZeroRead); !ok {
		return
	}
	value, err := h.overlayService.AdminDevices(c.Request.Context(), auth.GetUserID(c))
	if err != nil {
		respondError(c, http.StatusInternalServerError, "overlay_admin_read_failed", "failed to load XConnect Zero devices")
		return
	}
	c.JSON(http.StatusOK, gin.H{"devices": value})
}

func (h *handler) overlayAdminInvites(c *gin.Context) {
	if _, ok := h.requireXConnectZeroAccess(c, permissionXConnectZeroRead); !ok {
		return
	}
	value, err := h.overlayService.AdminInvites(c.Request.Context(), auth.GetUserID(c))
	if err != nil {
		respondError(c, http.StatusInternalServerError, "overlay_admin_read_failed", "failed to load XConnect Zero invites")
		return
	}
	c.JSON(http.StatusOK, gin.H{"invites": value})
}

func (h *handler) overlayAdminBootstrap(c *gin.Context) {
	if _, ok := h.requireXConnectZeroAccess(c, permissionXConnectZeroManage); !ok {
		return
	}
	var request overlayAdminBootstrapRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid XConnect Zero bootstrap request")
		return
	}
	result, err := h.overlayService.AdminBootstrap(c.Request.Context(), overlayBootstrapConfig(request, auth.GetUserID(c)), request.ControllerURL, "")
	if err != nil {
		respondOverlayAdminError(c, err)
		return
	}
	c.JSON(http.StatusCreated, result)
}

func (h *handler) overlayInternalBootstrap(c *gin.Context) {
	var request overlayInternalBootstrapRequest
	if err := c.ShouldBindJSON(&request); err != nil || strings.TrimSpace(request.OwnerEmail) == "" {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid XConnect Zero automation request")
		return
	}
	owner, err := h.store.GetUserByEmail(c.Request.Context(), strings.TrimSpace(request.OwnerEmail))
	if err != nil || !owner.Active {
		respondError(c, http.StatusNotFound, "owner_not_found", "active XConnect Zero owner was not found")
		return
	}
	result, err := h.overlayService.AdminBootstrap(c.Request.Context(), overlayBootstrapConfig(request.Bootstrap, owner.ID), request.Bootstrap.ControllerURL, "")
	if err != nil {
		respondOverlayAdminError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, result)
}

func overlayBootstrapConfig(request overlayAdminBootstrapRequest, ownerUserID string) overlay.BootstrapConfig {
	return overlay.BootstrapConfig{
		Network: overlay.BootstrapNetwork{ID: request.Network.ID, DisplayName: request.Network.DisplayName, CIDR: request.Network.CIDR, GatewayID: request.Network.GatewayID, GatewayWireGuardKey: request.Network.GatewayWireGuardKey, GatewayWireGuardAddress: request.Network.GatewayWireGuardAddress, GatewayEndpointHost: request.Network.GatewayEndpointHost, GatewayEndpointPort: request.Network.GatewayEndpointPort, TransportServerName: request.Network.TransportServerName, TransportPort: request.Network.TransportPort, TransportAuthID: request.Network.TransportAuthID, OwnerUserID: ownerUserID},
		Invite:  overlay.BootstrapInvite{DeviceID: request.Invite.DeviceID, Platform: request.Invite.Platform, Role: request.Invite.Role, ExpiresAt: request.Invite.Expires},
	}
}

func (h *handler) overlayAdminRevokeDevice(c *gin.Context) {
	if _, ok := h.requireXConnectZeroAccess(c, permissionXConnectZeroManage); !ok {
		return
	}
	if err := h.overlayService.AdminRevokeDevice(c.Request.Context(), auth.GetUserID(c), c.Param("deviceID")); err != nil {
		respondOverlayAdminError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *handler) overlayAdminPolicy(c *gin.Context) {
	if _, ok := h.requireXConnectZeroAccess(c, permissionXConnectZeroRead); !ok {
		return
	}
	value, err := h.overlayService.AdminPolicy(c.Request.Context(), auth.GetUserID(c), c.Param("networkID"))
	if err != nil {
		respondOverlayAdminError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}

func (h *handler) overlayAdminUpdatePolicy(c *gin.Context) {
	if _, ok := h.requireXConnectZeroAccess(c, permissionXConnectZeroManage); !ok {
		return
	}
	var policy overlay.PolicyArtifact
	if err := c.ShouldBindJSON(&policy); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid XConnect Zero policy")
		return
	}
	value, err := h.overlayService.AdminUpdatePolicy(c.Request.Context(), auth.GetUserID(c), c.Param("networkID"), policy)
	if err != nil {
		respondOverlayAdminError(c, err)
		return
	}
	c.JSON(http.StatusOK, value)
}

// requireXConnectZeroAccess makes Zero a self-service user feature. Resource
// ownership is still enforced in every service query, so this grants no
// cross-user administrative view.
func (h *handler) requireXConnectZeroAccess(c *gin.Context, permission string) (*store.User, bool) {
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
	switch {
	case store.IsRootRole(user.Role):
		if !strings.EqualFold(strings.TrimSpace(user.Email), store.RootAdminEmail) {
			respondError(c, http.StatusForbidden, "root_email_enforced", "root role is restricted")
			return nil, false
		}
	case strings.EqualFold(strings.TrimSpace(user.Role), store.RoleAdmin), strings.EqualFold(strings.TrimSpace(user.Role), store.RoleUser):
		return user, true
	case store.IsOperatorRole(user.Role):
		if !h.operatorPermissionAllowed(c, permission) {
			respondError(c, http.StatusForbidden, "forbidden", "operator permission denied")
			return nil, false
		}
		return user, true
	case strings.EqualFold(strings.TrimSpace(user.Role), store.RoleReadOnly):
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead || !hasPermission(user.Permissions, permission) {
			respondError(c, http.StatusForbidden, "forbidden", "readonly permission denied")
			return nil, false
		}
		return user, true
	default:
		respondError(c, http.StatusForbidden, "forbidden", "insufficient permissions")
		return nil, false
	}
	return user, true
}

func respondOverlayAdminError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	code := "overlay_admin_failed"
	message := "XConnect Zero operation failed"
	switch {
	case errors.Is(err, overlay.ErrNotFound):
		status, code, message = http.StatusNotFound, "not_found", "XConnect Zero resource not found"
	case errors.Is(err, overlay.ErrInvalidInput):
		status, code, message = http.StatusBadRequest, "invalid_request", "invalid XConnect Zero resource"
	case errors.Is(err, overlay.ErrForbidden):
		status, code, message = http.StatusForbidden, "forbidden", "XConnect Zero resource belongs to another user"
	}
	respondError(c, status, code, message)
}

func overlayJoinURI(controllerURL, token string) string {
	return "xconnect://join/" + token + "?controller=" + url.QueryEscape(strings.TrimRight(controllerURL, "/"))
}
