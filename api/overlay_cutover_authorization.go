package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/overlay/cutoverauth"
	"account/internal/overlay/domain"
	"account/internal/store"
)

const (
	defaultCutoverAuthorizationTTL = 10 * time.Minute
	maxCutoverAuthorizationTTL     = 15 * time.Minute
)

type cutoverAuthorizationRequest struct {
	RequestedMode    string `json:"requested_mode"`
	ExpiresInSeconds int64  `json:"expires_in_seconds,omitempty"`
}

type cutoverProjectionDevice struct {
	DeviceID  string   `json:"device_id"`
	PublicKey string   `json:"public_key"`
	Addresses []string `json:"addresses"`
}

func newOverlayCutoverAuthorizationSignerFromEnvironment(st store.Store) (*cutoverauth.Signer, error) {
	encoded := strings.TrimSpace(os.Getenv("OVERLAY_CUTOVER_AUTHORIZATION_PRIVATE_KEY"))
	if encoded == "" {
		return nil, nil
	}
	if st == nil || !st.OverlayProjectionDurable() {
		return nil, errors.New("durable Accounts store is required for cutover authorization signing")
	}
	keyID := strings.TrimSpace(os.Getenv("OVERLAY_CUTOVER_AUTHORIZATION_KEY_ID"))
	if keyID == "" {
		return nil, errors.New("OVERLAY_CUTOVER_AUTHORIZATION_KEY_ID is required when cutover authorization signing is enabled")
	}
	return cutoverauth.NewSignerFromBase64(encoded, keyID)
}

func (h *handler) issueOverlayCutoverAuthorization(c *gin.Context) {
	if h.overlayCutoverAuthorization == nil {
		respondError(c, http.StatusServiceUnavailable, "cutover_authorization_unavailable", "Accounts cutover authorization signing is not configured")
		return
	}
	var request cutoverAuthorizationRequest
	if !strictGatewayJSON(c, &request) {
		return
	}
	if request.RequestedMode != cutoverauth.RequestedMode {
		respondError(c, http.StatusBadRequest, "invalid_cutover_mode", "requested_mode must be accounts-only")
		return
	}
	ttl := defaultCutoverAuthorizationTTL
	if request.ExpiresInSeconds != 0 {
		ttl = time.Duration(request.ExpiresInSeconds) * time.Second
	}
	if ttl <= 0 || ttl > maxCutoverAuthorizationTTL {
		respondError(c, http.StatusBadRequest, "invalid_expiry", "expires_in_seconds must be between 1 and 900")
		return
	}
	nodeID := strings.TrimSpace(c.Param("node_id"))
	node, err := h.store.GetOverlayNode(c.Request.Context(), nodeID)
	if errors.Is(err, store.ErrOverlayNodeNotFound) {
		respondError(c, http.StatusNotFound, "overlay_node_not_found", "overlay node was not bootstrapped")
		return
	}
	if err != nil {
		respondError(c, http.StatusServiceUnavailable, "cutover_evidence_unavailable", "cutover evidence is unavailable")
		return
	}
	// Accounts-only is an explicit authority change. Refuse to pre-sign a
	// shadow node or a non-gateway record.
	if node.Role != "gateway" || node.GatewayMode != "apply" {
		respondError(c, http.StatusConflict, "cutover_not_authorized", "node is not explicitly authorized for apply mode")
		return
	}
	snapshotRecord, err := h.store.GetLatestOverlayGatewaySnapshot(c.Request.Context(), nodeID)
	if errors.Is(err, store.ErrOverlayGatewaySnapshotNotFound) {
		respondError(c, http.StatusConflict, "cutover_evidence_incomplete", "no signed gateway snapshot exists")
		return
	}
	if err != nil {
		respondError(c, http.StatusServiceUnavailable, "cutover_evidence_unavailable", "cutover evidence is unavailable")
		return
	}
	snapshot, err := domain.DecodeGatewaySnapshot(snapshotRecord.SignedPayload)
	if err != nil || snapshot.NodeID != nodeID || snapshot.Generation != snapshotRecord.Generation || snapshot.SnapshotID != snapshotRecord.SnapshotID || snapshot.Policy.RulesetSHA256 == "" {
		respondError(c, http.StatusConflict, "cutover_evidence_incomplete", "stored gateway snapshot is not valid for cutover")
		return
	}
	now := time.Now().UTC().Truncate(time.Second)
	if !snapshot.ExpiresAt.After(now) {
		respondError(c, http.StatusConflict, "cutover_evidence_incomplete", "stored gateway snapshot is expired")
		return
	}
	applied, err := h.store.IsOverlayGatewayGenerationApplied(c.Request.Context(), nodeID, snapshot.Generation)
	if err != nil {
		respondError(c, http.StatusServiceUnavailable, "cutover_evidence_unavailable", "gateway apply evidence is unavailable")
		return
	}
	if !applied {
		respondError(c, http.StatusConflict, "cutover_evidence_incomplete", "gateway generation has no successful runtime apply evidence")
		return
	}
	receipt, err := h.store.GetLatestOverlayStaticImportReceipt(c.Request.Context(), node.NetworkID)
	if errors.Is(err, store.ErrOverlayStaticImportNotFound) {
		respondError(c, http.StatusConflict, "cutover_evidence_incomplete", "reviewed static import baseline is missing")
		return
	}
	if err != nil {
		respondError(c, http.StatusServiceUnavailable, "cutover_evidence_unavailable", "cutover evidence is unavailable")
		return
	}
	if receipt.NetworkID != node.NetworkID || receipt.BaselineSHA256 == "" {
		respondError(c, http.StatusConflict, "cutover_evidence_incomplete", "static import baseline does not bind this node network")
		return
	}
	pending, err := h.store.ListOverlayPolicyReconcilePending(c.Request.Context(), 1000)
	if err != nil {
		respondError(c, http.StatusServiceUnavailable, "cutover_evidence_unavailable", "reconcile evidence is unavailable")
		return
	}
	for _, item := range pending {
		if item.NetworkID == node.NetworkID {
			respondError(c, http.StatusConflict, "cutover_reconcile_pending", "control-plane reconciliation remains pending")
			return
		}
	}
	projection, err := cutoverProjectionFromSnapshot(snapshot)
	if err != nil {
		respondError(c, http.StatusConflict, "cutover_evidence_incomplete", "gateway snapshot peer projection is unsafe")
		return
	}
	rawProjection, _ := json.Marshal(projection)
	projectionSum := sha256.Sum256(rawProjection)
	if now.Add(ttl).After(snapshot.ExpiresAt) {
		respondError(c, http.StatusConflict, "cutover_evidence_incomplete", "requested authorization window exceeds the signed snapshot validity")
		return
	}
	authorization := cutoverauth.Authorization{SchemaVersion: cutoverauth.SchemaVersion, Kind: cutoverauth.Kind, RequestedMode: cutoverauth.RequestedMode, NodeID: nodeID, NetworkID: node.NetworkID, Generation: snapshot.Generation, SnapshotID: snapshot.SnapshotID, BaselineSHA256: receipt.BaselineSHA256, ProjectionSHA256: hex.EncodeToString(projectionSum[:]), PolicySHA256: snapshot.Policy.RulesetSHA256, Reconcile: cutoverauth.ReconcileEvidence{Processed: 0, Completed: 0, Failed: 0, Pending: 0}, IssuedAt: now, ExpiresAt: now.Add(ttl).UTC()}
	if err := h.overlayCutoverAuthorization.Sign(&authorization); err != nil {
		respondError(c, http.StatusServiceUnavailable, "cutover_authorization_unavailable", "cutover authorization signing failed")
		return
	}
	if err := h.store.InsertAuditLog(c.Request.Context(), &store.AuditLog{Action: store.AuditActionOverlayCutoverAuthorization, Details: map[string]any{"node_id": nodeID, "network_id": node.NetworkID, "generation": authorization.Generation, "snapshot_id": authorization.SnapshotID, "baseline_sha256": authorization.BaselineSHA256, "projection_sha256": authorization.ProjectionSHA256, "policy_sha256": authorization.PolicySHA256, "expires_at": authorization.ExpiresAt, "signing_key_id": authorization.Signature.KeyID}}); err != nil {
		respondError(c, http.StatusServiceUnavailable, "cutover_authorization_audit_failed", "cutover authorization audit persistence failed")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Vary", "X-Service-Token")
	c.JSON(http.StatusOK, authorization)
}

func cutoverProjectionFromSnapshot(snapshot domain.GatewaySnapshot) ([]cutoverProjectionDevice, error) {
	if len(snapshot.WireGuard.Peers) == 0 {
		return nil, errors.New("empty peer projection")
	}
	projection := make([]cutoverProjectionDevice, 0, len(snapshot.WireGuard.Peers))
	seen := make(map[string]bool, len(snapshot.WireGuard.Peers))
	for _, peer := range snapshot.WireGuard.Peers {
		if peer.DeviceID == "" || peer.PublicKey == "" || len(peer.AllowedIPs) != 1 || seen[peer.DeviceID] {
			return nil, errors.New("invalid peer projection")
		}
		seen[peer.DeviceID] = true
		projection = append(projection, cutoverProjectionDevice{DeviceID: peer.DeviceID, PublicKey: peer.PublicKey, Addresses: []string{peer.AllowedIPs[0]}})
	}
	sort.Slice(projection, func(i, j int) bool { return projection[i].DeviceID < projection[j].DeviceID })
	return projection, nil
}
