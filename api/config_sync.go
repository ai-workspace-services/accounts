package api

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/store"
	"account/internal/xrayconfig"
)

type syncConfigAckRequest struct {
	Version   int64  `json:"version"`
	DeviceID  string `json:"device_id"`
	AppliedAt string `json:"applied_at"`
}

func (h *handler) syncConfigSnapshot(c *gin.Context) {
	h.respondSyncConfigSnapshot(c)
}

func (h *handler) syncConfig(c *gin.Context) {
	// Backward-compatible endpoint: old clients call POST /api/auth/config/sync.
	h.respondSyncConfigSnapshot(c)
}

func (h *handler) respondSyncConfigSnapshot(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}

	sinceVersion := int64(0)
	if raw := strings.TrimSpace(c.Query("since_version")); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			respondError(c, http.StatusBadRequest, "invalid_since_version", "since_version must be a non-negative integer")
			return
		}
		sinceVersion = v
	}

	version := deriveSyncVersion(user)
	updatedAt := time.Now().UTC()
	if !user.UpdatedAt.IsZero() {
		updatedAt = user.UpdatedAt.UTC()
	}

	changed := sinceVersion < version
	renderedJSON := ""
	digest := ""
	warnings := []string{}
	if changed {
		var err error
		renderedJSON, digest, warnings, err = h.renderUserXrayConfig(user)
		if err != nil {
			slog.Warn(
				"desktop sync config render failed; continuing with node metadata only",
				"user_id", strings.TrimSpace(user.ID),
				"user_email", strings.TrimSpace(user.Email),
				"error", err,
			)
			renderedJSON = ""
			digest = ""
			warnings = append(warnings, "rendered xray config unavailable; falling back to node metadata")
		}
	}

	profiles := []gin.H{}
	nodes := []gin.H{}
	if changed {
		proxyUUID := strings.TrimSpace(user.ID)

		// Collect node hosts from registered agents + publicURL fallback.
		registeredHosts, registeredNames := registeredNodeMetadata(h.agentStatusReader)
		hosts := parseProxyNodeHosts(h.publicURL, registeredHosts)

		xhttpPath := envOrDefault("XRAY_XHTTP_PATH", defaultXHTTPPath)
		xhttpMode := envOrDefault("XRAY_XHTTP_MODE", defaultXHTTPMode)
		xhttpScheme := xrayconfig.VLESSXHTTPScheme()

		for _, host := range hosts {
			nodeName := resolveNodeName(host, registeredNames)
			countryCode := countryCodeForHost(host)
			nodeID := host
			vlessURI := renderVLESSURIScheme(xhttpScheme, map[string]string{
				"UUID":   proxyUUID,
				"DOMAIN": host,
				"NODE":   host,
				"PATH":   url.QueryEscape(xhttpPath),
				"MODE":   url.QueryEscape(xhttpMode),
				"SNI":    host,
				"FP":     defaultTLSFP,
				"TAG":    url.QueryEscape(nodeName),
			})

			profiles = append(profiles, gin.H{
				"id":           nodeID,
				"remark":       nodeName,
				"display_name": nodeName,
				"host":         host,
				"address":      host,
				"port":         443,
				"uuid":         proxyUUID,
				"flow":         "",
				"transport":    "xhttp",
				"security":     "tls",
				"source":       "server",
				"country_code": countryCode,
				"vless_uri":    vlessURI,
			})
			nodes = append(nodes, gin.H{
				"id":               nodeID,
				"name":             nodeName,
				"display_name":     nodeName,
				"remark":           nodeName,
				"host":             host,
				"protocol":         "vless",
				"transport":        "xhttp",
				"security":         "tls",
				"address":          host,
				"port":             443,
				"server_name":      host,
				"uuid":             proxyUUID,
				"flow":             "",
				"source":           "server",
				"country_code":     countryCode,
				"updated_at":       updatedAt,
				"vless_uri":        vlessURI,
				"uri_scheme_xhttp": vlessURI,
			})
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"schema_version": 1,
		"changed":        changed,
		"version":        version,
		"updated_at":     updatedAt,
		"profiles":       profiles,
		"nodes":          nodes,
		"rendered_json":  renderedJSON,
		"routes":         []gin.H{},
		"dns": gin.H{
			"mode":    "secure_tunnel",
			"servers": []string{},
		},
		"meta": gin.H{
			"digest":   digest,
			"warnings": warnings,
		},
		"digest":   digest,
		"warnings": warnings,
	})
}

func (h *handler) syncConfigAck(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}

	var req syncConfigAckRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}

	if req.Version <= 0 {
		respondError(c, http.StatusBadRequest, "invalid_version", "version must be positive")
		return
	}
	if strings.TrimSpace(req.DeviceID) == "" {
		respondError(c, http.StatusBadRequest, "device_id_required", "device_id is required")
		return
	}
	if strings.TrimSpace(req.AppliedAt) == "" {
		respondError(c, http.StatusBadRequest, "applied_at_required", "applied_at is required")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"acked":       true,
		"version":     req.Version,
		"device_id":   strings.TrimSpace(req.DeviceID),
		"user_id":     strings.TrimSpace(user.ID),
		"received_at": time.Now().UTC(),
	})
}

func deriveSyncVersion(user *store.User) int64 {
	if user == nil {
		return time.Now().UTC().Unix()
	}
	if !user.UpdatedAt.IsZero() {
		return user.UpdatedAt.UTC().Unix()
	}
	if !user.CreatedAt.IsZero() {
		return user.CreatedAt.UTC().Unix()
	}
	return time.Now().UTC().Unix()
}

func (h *handler) renderUserXrayConfig(user *store.User) (string, string, []string, error) {
	if h.xrayConfigRenderer != nil {
		return h.xrayConfigRenderer(user)
	}

	domain := extractHostFromPublicURL(h.publicURL)
	if domain == "" {
		domain = "accounts.svc.plus"
	}

	clientID := strings.TrimSpace(user.ID)
	clients := []xrayconfig.Client{{
		ID:    clientID,
		Email: strings.TrimSpace(user.ID),
		Flow:  xrayconfig.DefaultFlow,
	}}

	gen := xrayconfig.Generator{
		Definition: xrayconfig.TCPDefinition(),
		Domain:     domain,
	}
	buf, err := gen.Render(clients)
	if err != nil {
		return "", "", nil, err
	}
	sum := sha256.Sum256(buf)
	return string(buf), hex.EncodeToString(sum[:]), []string{}, nil
}

func extractHostFromPublicURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.Hostname())
}
