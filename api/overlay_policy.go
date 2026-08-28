package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"account/internal/overlay/acl"
	"account/internal/store"
)

const gatewayPolicyMediaType = "application/vnd.xconnect.gateway-policy.v1+json"

var policyDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type policyDocumentRequest struct {
	NetworkID    string `json:"network_id"`
	Source       string `json:"source"`
	SourceFormat string `json:"source_format"`
}
type policyExplainRequest struct {
	NetworkID   string `json:"network_id"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Protocol    string `json:"protocol"`
	Port        int    `json:"port"`
}

func (h *handler) requireOverlayPolicyPermission(c *gin.Context, permission string) (*store.User, bool) {
	user, ok := h.currentAuthenticatedUser(c)
	if !ok {
		return nil, false
	}
	if store.IsRootRole(user.Role) || strings.EqualFold(user.Role, store.RoleAdmin) {
		return user, true
	}
	if !hasPermission(user.Permissions, permission) {
		respondError(c, http.StatusForbidden, "forbidden", "overlay policy permission denied")
		return nil, false
	}
	return user, true
}

func strictPolicyJSON(c *gin.Context, target any) bool {
	if !requireJSONContentType(c) {
		return false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, acl.MaxDocumentSize+64<<10))
	if err != nil {
		respondError(c, http.StatusRequestEntityTooLarge, "payload_too_large", "policy request is too large")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(target); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "policy request is invalid")
		return false
	}
	if err = dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		respondError(c, http.StatusBadRequest, "invalid_request", "policy request must contain one object")
		return false
	}
	return true
}
func policyContentType(format string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "json":
		return "application/json", true
	case "yaml", "yml":
		return "application/yaml", true
	default:
		return "", false
	}
}
func validatePolicyRequest(c *gin.Context, request *policyDocumentRequest) (string, bool) {
	request.NetworkID = strings.TrimSpace(request.NetworkID)
	if request.NetworkID == "" || len(request.NetworkID) > 128 || request.Source == "" {
		respondError(c, http.StatusBadRequest, "invalid_policy", "network_id and source are required")
		return "", false
	}
	contentType, ok := policyContentType(request.SourceFormat)
	if !ok {
		respondError(c, http.StatusBadRequest, "invalid_policy", "source_format must be json or yaml")
		return "", false
	}
	return contentType, true
}
func policyResponse(p *store.OverlayPolicyRevision) gin.H {
	var warnings any = []any{}
	_ = json.Unmarshal(p.Warnings, &warnings)
	return gin.H{"network_id": p.NetworkID, "revision": p.Revision, "name": p.Name, "artifact_sha256": p.ArtifactSHA256, "compiler_version": p.CompilerVersion, "warnings": warnings, "status": p.Status, "generation": p.Generation, "created_at": p.CreatedAt, "validated_at": p.ValidatedAt, "activated_at": p.ActivatedAt}
}
func (h *handler) validateOverlayPolicy(c *gin.Context) {
	if _, ok := h.requireOverlayPolicyPermission(c, permissionAdminSettingsRead); !ok {
		return
	}
	var request policyDocumentRequest
	if !strictPolicyJSON(c, &request) {
		return
	}
	contentType, ok := validatePolicyRequest(c, &request)
	if !ok {
		return
	}
	build, _, err := h.overlayACL.Validate(c.Request.Context(), request.NetworkID, []byte(request.Source), contentType)
	if err != nil {
		respondError(c, http.StatusUnprocessableEntity, "policy_invalid", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"valid": true, "compiler_version": acl.CompilerVersion, "artifact_sha256": build.Digest, "warnings": build.Warnings})
}
func (h *handler) createOverlayPolicy(c *gin.Context) {
	user, ok := h.requireOverlayPolicyPermission(c, permissionAdminSettingsWrite)
	if !ok {
		return
	}
	var request policyDocumentRequest
	if !strictPolicyJSON(c, &request) {
		return
	}
	contentType, ok := validatePolicyRequest(c, &request)
	if !ok {
		return
	}
	p, err := h.overlayACL.Create(c.Request.Context(), request.NetworkID, user.ID, []byte(request.Source), contentType)
	if err != nil {
		respondError(c, http.StatusUnprocessableEntity, "policy_invalid", err.Error())
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, policyResponse(p))
}
func policyRevision(c *gin.Context) (uint64, bool) {
	revision, err := strconv.ParseUint(c.Param("revision"), 10, 64)
	if err != nil || revision == 0 {
		respondError(c, http.StatusBadRequest, "invalid_revision", "policy revision is invalid")
		return 0, false
	}
	return revision, true
}
func (h *handler) getOverlayPolicy(c *gin.Context) {
	user, ok := h.requireOverlayPolicyPermission(c, permissionAdminSettingsRead)
	if !ok {
		return
	}
	revision, ok := policyRevision(c)
	if !ok {
		return
	}
	networkID := strings.TrimSpace(c.Query("network_id"))
	p, err := h.overlayACL.Get(c.Request.Context(), networkID, revision, user.ID)
	if err != nil {
		respondError(c, http.StatusNotFound, "policy_not_found", "policy revision was not found")
		return
	}
	c.JSON(http.StatusOK, policyResponse(p))
}
func (h *handler) activateOverlayPolicy(c *gin.Context) {
	user, ok := h.requireOverlayPolicyPermission(c, permissionAdminSettingsWrite)
	if !ok {
		return
	}
	revision, ok := policyRevision(c)
	if !ok {
		return
	}
	var request struct {
		NetworkID string `json:"network_id"`
	}
	if !strictPolicyJSON(c, &request) {
		return
	}
	request.NetworkID = strings.TrimSpace(request.NetworkID)
	p, err := h.overlayACL.Activate(c.Request.Context(), request.NetworkID, revision, user.ID)
	if errors.Is(err, store.ErrOverlayPolicyConflict) {
		respondError(c, http.StatusForbidden, "policy_scope_denied", "policy belongs to another owner")
		return
	} else if err != nil {
		respondError(c, http.StatusNotFound, "policy_not_found", "policy revision was not found")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, policyResponse(p))
}
func (h *handler) explainOverlayPolicy(c *gin.Context) {
	user, ok := h.requireOverlayPolicyPermission(c, permissionAdminSettingsRead)
	if !ok {
		return
	}
	revision, ok := policyRevision(c)
	if !ok {
		return
	}
	var request policyExplainRequest
	if !strictPolicyJSON(c, &request) {
		return
	}
	explanation, err := h.overlayACL.Explain(c.Request.Context(), strings.TrimSpace(request.NetworkID), revision, user.ID, acl.Query{Source: request.Source, Destination: request.Destination, Protocol: request.Protocol, Port: request.Port})
	if err != nil {
		respondError(c, http.StatusNotFound, "policy_not_found", "policy revision was not found")
		return
	}
	c.JSON(http.StatusOK, explanation)
}

func (h *handler) gatewayNodePolicyArtifact(c *gin.Context) {
	gatewayNoStore(c)
	credential, ok := h.requireGatewayNode(c)
	if !ok {
		return
	}
	nodeID := strings.TrimSpace(c.Param("node_id"))
	if credential.NodeID != nodeID {
		respondError(c, http.StatusForbidden, "node_scope_mismatch", "credential is bound to another node")
		return
	}
	generation, err := strconv.ParseUint(c.Param("generation"), 10, 64)
	digest := strings.TrimSpace(c.Param("digest"))
	if err != nil || generation == 0 || !policyDigestPattern.MatchString(digest) {
		respondError(c, http.StatusBadRequest, "invalid_policy_identity", "policy generation or digest is invalid")
		return
	}
	node, err := h.store.GetOverlayNode(c.Request.Context(), nodeID)
	if err != nil {
		respondError(c, http.StatusNotFound, "overlay_node_not_found", "overlay node was not found")
		return
	}
	artifact, err := acl.ResolveActive(c.Request.Context(), h.store, node.NetworkID)
	if err != nil {
		respondError(c, http.StatusServiceUnavailable, "policy_unavailable", "active policy is unavailable")
		return
	}
	sum := sha256.Sum256(artifact.Canonical)
	actual := hex.EncodeToString(sum[:])
	if artifact.Generation != generation || artifact.Digest != digest || actual != digest {
		respondError(c, http.StatusNotFound, "policy_artifact_not_found", "policy artifact is not active for this node")
		return
	}
	c.Header("ETag", `"sha256-`+digest+`"`)
	c.Header("X-XConnect-Policy-Generation", strconv.FormatUint(generation, 10))
	c.Data(http.StatusOK, gatewayPolicyMediaType, artifact.Canonical)
}

func (h *handler) updateOverlayDeviceTags(c *gin.Context) {
	user, ok := h.currentAuthenticatedUser(c)
	if !ok {
		return
	}
	var request struct {
		NetworkID string   `json:"network_id"`
		Tags      []string `json:"tags"`
	}
	if !strictPolicyJSON(c, &request) {
		return
	}
	if len(request.Tags) > 64 {
		respondError(c, http.StatusBadRequest, "invalid_tags", "too many tags")
		return
	}
	set := map[string]bool{}
	for _, tag := range request.Tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if !strings.HasPrefix(tag, "tag:") {
			respondError(c, http.StatusBadRequest, "invalid_tags", "tags must use tag: prefix")
			return
		}
		set[tag] = true
	}
	normalized := make([]string, 0, len(set))
	for tag := range set {
		normalized = append(normalized, tag)
	}
	sort.Strings(normalized)
	networkID := strings.TrimSpace(request.NetworkID)
	if err := h.overlayACL.UpdateDeviceTags(c.Request.Context(), networkID, strings.TrimSpace(c.Param("device_id")), user.ID, user.Email, normalized); err != nil {
		respondError(c, http.StatusForbidden, "tag_assignment_denied", "tag assignment is not authorized by the active policy")
		return
	}
	artifact, err := acl.ResolveActive(c.Request.Context(), h.store, networkID)
	if err != nil {
		respondError(c, http.StatusServiceUnavailable, "policy_recompile_pending", "device tags changed but policy recompilation must be retried")
		return
	}
	c.JSON(http.StatusOK, gin.H{"device_id": strings.TrimSpace(c.Param("device_id")), "network_id": networkID, "tags": normalized, "policy_generation": artifact.Generation, "policy_digest": artifact.Digest})
}
