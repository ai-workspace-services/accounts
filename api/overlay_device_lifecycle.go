package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"account/internal/overlay/acl"
	"account/internal/store"
)

type overlayDeviceScopeRequest struct {
	NetworkID            string `json:"network_id"`
	OwnerUserID          string `json:"owner_user_id,omitempty"`
	ExpectedStateVersion uint64 `json:"expected_state_version"`
}

type overlayDeviceRotateKeyRequest struct {
	NetworkID          string `json:"network_id"`
	OwnerUserID        string `json:"owner_user_id,omitempty"`
	PublicKey          string `json:"wireguard_public_key"`
	ExpectedKeyVersion uint64 `json:"expected_key_version"`
}

type overlayDeviceStateRequest struct {
	NetworkID            string `json:"network_id"`
	OwnerUserID          string `json:"owner_user_id,omitempty"`
	Status               string `json:"status"`
	ExpectedStateVersion uint64 `json:"expected_state_version"`
	Reason               string `json:"reason,omitempty"`
}

func (h *handler) overlayDeviceOwner(c *gin.Context, requested, permission string) (*store.User, *store.User, bool) {
	actor, ok := h.currentAuthenticatedUser(c)
	if !ok {
		return nil, nil, false
	}
	requested = strings.TrimSpace(requested)
	if requested == "" || requested == actor.ID {
		return actor, actor, true
	}
	if !(store.IsRootRole(actor.Role) || strings.EqualFold(actor.Role, store.RoleAdmin) || hasPermission(actor.Permissions, permission)) {
		respondError(c, http.StatusForbidden, "device_scope_denied", "device owner scope is not permitted")
		return nil, nil, false
	}
	owner, err := h.store.GetUserByID(c.Request.Context(), requested)
	if err != nil {
		respondError(c, http.StatusNotFound, "device_owner_not_found", "device owner was not found")
		return nil, nil, false
	}
	return actor, owner, true
}

func (h *handler) getOverlayDeviceDetail(c *gin.Context) {
	_, owner, ok := h.overlayDeviceOwner(c, c.Query("owner_user_id"), permissionAdminSettingsRead)
	if !ok {
		return
	}
	device, err := h.store.GetOverlayDevice(c.Request.Context(), owner.ID, sanitizeOverlayID(c.Param("device_id")))
	if err != nil || (strings.TrimSpace(c.Query("network_id")) != "" && device.NetworkID != normalizeOverlayNetworkID(c.Query("network_id"))) {
		respondError(c, http.StatusNotFound, "overlay_device_not_found", "overlay device was not found in this scope")
		return
	}
	c.JSON(http.StatusOK, gin.H{"device": overlayDevicePayload(device)})
}

func (h *handler) rotateOverlayDeviceKey(c *gin.Context) {
	var request overlayDeviceRotateKeyRequest
	if !strictGatewayJSON(c, &request) {
		return
	}
	actor, owner, ok := h.overlayDeviceOwner(c, request.OwnerUserID, permissionAdminSettingsWrite)
	if !ok {
		return
	}
	request.NetworkID = normalizeOverlayNetworkID(request.NetworkID)
	request.PublicKey = strings.TrimSpace(request.PublicKey)
	if !isWireGuardKey(request.PublicKey) || request.ExpectedKeyVersion == 0 {
		respondError(c, http.StatusBadRequest, "invalid_key_rotation", "a WireGuard public key and expected_key_version are required")
		return
	}
	deviceID := sanitizeOverlayID(c.Param("device_id"))
	audit := &store.AuditLog{Action: store.AuditActionOverlayDeviceKeyRotate, ActorUUID: actor.ID, Details: map[string]any{"owner_user_id": owner.ID, "network_id": request.NetworkID, "device_id": deviceID}}
	device, duplicate, err := h.store.RotateOverlayDeviceKey(c.Request.Context(), owner.ID, request.NetworkID, deviceID, request.PublicKey, request.ExpectedKeyVersion, audit)
	if !h.respondOverlayLifecycleError(c, err) {
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"device": overlayDevicePayload(device), "duplicate": duplicate})
}

func (h *handler) updateOverlayDeviceState(c *gin.Context) {
	var request overlayDeviceStateRequest
	if !strictGatewayJSON(c, &request) {
		return
	}
	h.setOverlayDeviceState(c, request, false)
}

func (h *handler) revokeOverlayDevice(c *gin.Context) {
	var request overlayDeviceScopeRequest
	if !strictGatewayJSON(c, &request) {
		return
	}
	h.setOverlayDeviceState(c, overlayDeviceStateRequest{NetworkID: request.NetworkID, OwnerUserID: request.OwnerUserID, Status: store.OverlayDeviceRevoked, ExpectedStateVersion: request.ExpectedStateVersion, Reason: "explicit_leave"}, true)
}

func (h *handler) setOverlayDeviceState(c *gin.Context, request overlayDeviceStateRequest, leave bool) {
	actor, owner, ok := h.overlayDeviceOwner(c, request.OwnerUserID, permissionAdminSettingsWrite)
	if !ok {
		return
	}
	request.NetworkID = normalizeOverlayNetworkID(request.NetworkID)
	request.Status = strings.ToLower(strings.TrimSpace(request.Status))
	if (request.Status != store.OverlayDeviceActive && request.Status != store.OverlayDeviceInactive && request.Status != store.OverlayDeviceRevoked) || request.ExpectedStateVersion == 0 || len(request.Reason) > 512 {
		respondError(c, http.StatusBadRequest, "invalid_device_state", "status and expected_state_version are required")
		return
	}
	deviceID := sanitizeOverlayID(c.Param("device_id"))
	action := store.AuditActionOverlayDeviceStateUpdate
	if request.Status == store.OverlayDeviceRevoked {
		action = store.AuditActionOverlayDeviceRevoke
	}
	audit := &store.AuditLog{Action: action, ActorUUID: actor.ID, Details: map[string]any{"owner_user_id": owner.ID, "network_id": request.NetworkID, "device_id": deviceID, "status": request.Status}}
	device, duplicate, err := h.store.SetOverlayDeviceStatus(c.Request.Context(), owner.ID, request.NetworkID, deviceID, request.Status, request.ExpectedStateVersion, request.Reason, audit)
	if !h.respondOverlayLifecycleError(c, err) {
		return
	}
	artifact, err := acl.ResolveActive(c.Request.Context(), h.store, request.NetworkID)
	if err != nil {
		_ = h.store.MarkOverlayPolicyReconcilePending(c.Request.Context(), request.NetworkID, "active policy recompilation failed")
		respondError(c, http.StatusServiceUnavailable, "policy_recompile_pending", "device state changed but policy recompilation must be retried")
		return
	}
	_ = h.store.ClearOverlayPolicyReconcilePending(c.Request.Context(), request.NetworkID)
	c.Header("Cache-Control", "no-store")
	if leave {
		c.JSON(http.StatusOK, gin.H{"revoked": true, "duplicate": duplicate, "device": overlayDevicePayload(device), "policy_generation": artifact.Generation, "policy_digest": artifact.Digest})
		return
	}
	c.JSON(http.StatusOK, gin.H{"device": overlayDevicePayload(device), "duplicate": duplicate, "policy_generation": artifact.Generation, "policy_digest": artifact.Digest})
}

func (h *handler) respondOverlayLifecycleError(c *gin.Context, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrOverlayDeviceNotFound):
		respondError(c, http.StatusNotFound, "overlay_device_not_found", "overlay device was not found in this scope")
	case errors.Is(err, store.ErrOverlayDeviceRevoked):
		respondError(c, http.StatusGone, "overlay_device_revoked", "overlay device has been revoked")
	case errors.Is(err, store.ErrOverlayDeviceVersionConflict), errors.Is(err, store.ErrOverlayDeviceKeyConflict):
		respondError(c, http.StatusConflict, "overlay_device_conflict", "device state or public key changed concurrently")
	default:
		respondError(c, http.StatusServiceUnavailable, "overlay_device_update_failed", "device lifecycle update failed")
	}
	return false
}

func (h *handler) listOverlayDeviceEvents(c *gin.Context) {
	_, owner, ok := h.overlayDeviceOwner(c, c.Query("owner_user_id"), permissionAdminSettingsRead)
	if !ok {
		return
	}
	after, err := strconv.ParseUint(firstNonEmpty(strings.TrimSpace(c.Query("after")), "0"), 10, 64)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_cursor", "after must be an unsigned event sequence")
		return
	}
	events, err := h.store.ListOverlayDeviceEvents(c.Request.Context(), owner.ID, strings.TrimSpace(c.Query("network_id")), after, 100)
	if err != nil {
		respondError(c, http.StatusServiceUnavailable, "device_events_unavailable", "device events are unavailable")
		return
	}
	next := after
	if len(events) > 0 {
		next = events[len(events)-1].Sequence
	}
	c.JSON(http.StatusOK, gin.H{"events": events, "next_after": next})
}

func (h *handler) enrollmentRevokeOverlayDevice(c *gin.Context) {
	session, ok := h.requireOverlayEnrollment(c)
	if !ok {
		return
	}
	var request struct{}
	if !strictGatewayJSON(c, &request) {
		return
	}
	device, err := h.store.GetOverlayDevice(c.Request.Context(), session.UserID, session.DeviceID)
	if err != nil || device.NetworkID != session.NetworkID || device.WireGuardPublicKey != session.WireGuardPublicKey {
		respondError(c, http.StatusForbidden, "enrollment_scope_denied", "enrollment is not bound to the current device identity")
		return
	}
	audit := &store.AuditLog{Action: store.AuditActionOverlayDeviceRevoke, ActorUUID: session.UserID, Details: map[string]any{"owner_user_id": session.UserID, "network_id": session.NetworkID, "device_id": session.DeviceID, "source": "enrollment_leave"}}
	device, duplicate, err := h.store.SetOverlayDeviceStatus(c.Request.Context(), session.UserID, session.NetworkID, session.DeviceID, store.OverlayDeviceRevoked, device.StateVersion, "enrollment_leave", audit)
	if !h.respondOverlayLifecycleError(c, err) {
		return
	}
	artifact, err := acl.ResolveActive(c.Request.Context(), h.store, session.NetworkID)
	if err != nil {
		_ = h.store.MarkOverlayPolicyReconcilePending(c.Request.Context(), session.NetworkID, "active policy recompilation failed")
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusAccepted, gin.H{"revoked": true, "duplicate": duplicate, "device": overlayDevicePayload(device), "policy_reconcile_pending": true})
		return
	}
	_ = h.store.ClearOverlayPolicyReconcilePending(c.Request.Context(), session.NetworkID)
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"revoked": true, "duplicate": duplicate, "device": overlayDevicePayload(device), "policy_generation": artifact.Generation, "policy_digest": artifact.Digest})
}

func (h *handler) retryOverlayPolicyReconciles(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Vary", "X-Service-Token")
	pending, err := h.store.ListOverlayPolicyReconcilePending(c.Request.Context(), 100)
	if err != nil {
		respondError(c, http.StatusServiceUnavailable, "policy_reconcile_queue_unavailable", "policy reconcile queue is unavailable")
		return
	}
	completed, failed := 0, 0
	for _, item := range pending {
		if _, err = acl.ResolveActive(c.Request.Context(), h.store, item.NetworkID); err != nil {
			failed++
			_ = h.store.MarkOverlayPolicyReconcilePending(c.Request.Context(), item.NetworkID, "active policy recompilation failed")
			continue
		}
		completed++
		_ = h.store.ClearOverlayPolicyReconcilePending(c.Request.Context(), item.NetworkID)
	}
	c.JSON(http.StatusOK, gin.H{"processed": completed + failed, "completed": completed, "failed": failed})
}
