package api

import (
	"encoding/json"
	"errors"
	"io"
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
		TransportKind           string `json:"transport_kind"`
		TransportPath           string `json:"transport_path"`
		TransportMode           string `json:"transport_mode"`
		TransportHost           string `json:"transport_host"`
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

type overlayInternalStableGatewayReconcileRequest struct {
	Environment                string `json:"environment"`
	NetworkID                  string `json:"network_id"`
	GatewayID                  string `json:"gateway_id"`
	GatewayEndpointHost        string `json:"gateway_endpoint_host"`
	CurrentGatewayEndpointHost string `json:"current_gateway_endpoint_host"`
	OwnerEmail                 string `json:"owner_email"`
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
	group.POST("/invites", h.overlayAdminCreateInvite)
	group.GET("/registrations", h.overlayAdminRegistrations)
	group.POST("/registrations/:registrationID/approve", h.overlayAdminApproveRegistration)
	group.POST("/registrations/:registrationID/reject", h.overlayAdminRejectRegistration)
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

func (h *handler) overlayAdminCreateInvite(c *gin.Context) {
	if _, ok := h.requireXConnectZeroAccess(c, permissionXConnectZeroManage); !ok {
		return
	}
	var request overlay.AdminInviteRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid XConnect Zero invitation request")
		return
	}
	result, err := h.overlayService.AdminCreateInvite(c.Request.Context(), auth.GetUserID(c), request.ControllerURL, request)
	if err != nil {
		respondOverlayAdminError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, result)
}

func (h *handler) overlayAdminRegistrations(c *gin.Context) {
	if _, ok := h.requireXConnectZeroAccess(c, permissionXConnectZeroManage); !ok {
		return
	}
	registrations, err := h.overlayService.AdminRegistrations(c.Request.Context(), auth.GetUserID(c), c.Query("cursor"))
	if err != nil {
		respondOverlayAdminError(c, err)
		return
	}
	c.JSON(http.StatusOK, registrations)
}

func (h *handler) overlayAdminApproveRegistration(c *gin.Context) {
	if _, ok := h.requireXConnectZeroAccess(c, permissionXConnectZeroManage); !ok {
		return
	}
	var request overlay.ApproveRegistrationRequest
	if !decodeOverlayAdminJSON(c, &request) {
		return
	}
	registration, err := h.overlayService.AdminApproveRegistration(c.Request.Context(), auth.GetUserID(c), c.Param("registrationID"), request.NetworkID)
	if err != nil {
		respondOverlayAdminError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"registration": registration})
}

func (h *handler) overlayAdminRejectRegistration(c *gin.Context) {
	if _, ok := h.requireXConnectZeroAccess(c, permissionXConnectZeroManage); !ok {
		return
	}
	// Reject has no request contract, but still bounds and rejects an accidental
	// non-empty JSON body rather than silently accepting unreviewed fields.
	if c.Request.ContentLength != 0 {
		respondError(c, http.StatusBadRequest, "invalid_request", "XConnect Zero registration rejection does not accept a request body")
		return
	}
	registration, err := h.overlayService.AdminRejectRegistration(c.Request.Context(), auth.GetUserID(c), c.Param("registrationID"))
	if err != nil {
		respondOverlayAdminError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"registration": registration})
}

func decodeOverlayAdminJSON(c *gin.Context, target any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64*1024)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid XConnect Zero registration request")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid XConnect Zero registration request")
		return false
	}
	return true
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

func (h *handler) overlayInternalReconcileStableGateway(c *gin.Context) {
	var request overlayInternalStableGatewayReconcileRequest
	if !decodeOverlayAdminJSON(c, &request) {
		return
	}
	if strings.TrimSpace(request.OwnerEmail) == "" {
		respondError(c, http.StatusBadRequest, "invalid_request", "owner_email is required for XConnect Zero reconciliation")
		return
	}
	owner, err := h.store.GetUserByEmail(c.Request.Context(), strings.TrimSpace(request.OwnerEmail))
	if err != nil || owner == nil || !owner.Active {
		respondError(c, http.StatusNotFound, "owner_not_found", "active XConnect Zero owner was not found")
		return
	}
	result, err := h.overlayService.ReconcileStableGateway(c.Request.Context(), overlay.StableGatewayReconcileRequest{
		Environment: request.Environment, NetworkID: request.NetworkID, GatewayID: request.GatewayID,
		GatewayEndpointHost: request.GatewayEndpointHost, CurrentGatewayEndpointHost: request.CurrentGatewayEndpointHost, OwnerUserID: owner.ID,
	})
	if err != nil {
		respondOverlayAdminError(c, err)
		return
	}
	if result.OwnerReconciled || result.EndpointNormalized {
		if err := h.recordAudit(c.Request.Context(), "system", store.AuditActionOverlayOwnerReconcile,
			auditDetails(result.NetworkID, "UAT stable Gateway ownership reconciliation",
				map[string]any{"owner_user_id": result.PreviousOwnerID, "gateway_id": result.GatewayID},
				map[string]any{"owner_user_id": result.OwnerUserID, "gateway_id": result.GatewayID, "gateway_endpoint_host": result.GatewayEndpointHost})); err != nil {
			respondError(c, http.StatusInternalServerError, "audit_write_failed", "Gateway ownership was reconciled but the audit entry could not be written")
			return
		}
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, result)
}

func overlayBootstrapConfig(request overlayAdminBootstrapRequest, ownerUserID string) overlay.BootstrapConfig {
	return overlay.BootstrapConfig{
		Network: overlay.BootstrapNetwork{ID: request.Network.ID, DisplayName: request.Network.DisplayName, CIDR: request.Network.CIDR, GatewayID: request.Network.GatewayID, GatewayWireGuardKey: request.Network.GatewayWireGuardKey, GatewayWireGuardAddress: request.Network.GatewayWireGuardAddress, GatewayEndpointHost: request.Network.GatewayEndpointHost, GatewayEndpointPort: request.Network.GatewayEndpointPort, TransportServerName: request.Network.TransportServerName, TransportPort: request.Network.TransportPort, TransportAuthID: request.Network.TransportAuthID, TransportKind: request.Network.TransportKind, TransportPath: request.Network.TransportPath, TransportMode: request.Network.TransportMode, TransportHost: request.Network.TransportHost, OwnerUserID: ownerUserID},
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
		// Root is an account role, not a singleton e-mail identity. Resource
		// ownership remains enforced by every overlay service query, so a valid
		// root session can manage only the Zero resources it owns.
		return user, true
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
	case errors.Is(err, overlay.ErrRegistrationExpired):
		status, code, message = http.StatusGone, "registration_expired", "XConnect One registration expired"
	case errors.Is(err, overlay.ErrRegistrationNotPending):
		status, code, message = http.StatusConflict, "registration_not_pending", "XConnect One registration is stale"
	case errors.Is(err, overlay.ErrRegistrationRejected):
		status, code, message = http.StatusConflict, "registration_rejected", "XConnect One registration was rejected"
	case errors.Is(err, overlay.ErrRegistrationConsumed):
		status, code, message = http.StatusConflict, "registration_consumed", "XConnect One registration was already consumed"
	case errors.Is(err, overlay.ErrForbidden):
		status, code, message = http.StatusForbidden, "forbidden", "XConnect Zero resource belongs to another user"
	case errors.Is(err, overlay.ErrConflict):
		status, code, message = http.StatusConflict, "conflict", "XConnect Zero stable Gateway identity is ambiguous"
	}
	respondError(c, status, code, message)
}

func overlayJoinURI(controllerURL, token string) string {
	return "xconnect://join/" + token + "?controller=" + url.QueryEscape(strings.TrimRight(controllerURL, "/"))
}
