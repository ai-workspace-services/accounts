package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	if err := h.ensureExpiredSubscriptionDowngrade(c.Request.Context(), user); err != nil {
		respondError(c, http.StatusInternalServerError, "subscription_downgrade_failed", "failed to apply expired subscription downgrade")
		return
	}
	if strings.TrimSpace(user.ProxyUUID) == "" {
		respondError(c, http.StatusServiceUnavailable, "proxy_uuid_unavailable", "proxy UUID is not provisioned for this account")
		return
	}
	if exhausted, err := h.quotaExhaustedForUser(c.Request.Context(), user); err != nil {
		respondError(c, 503, "quota_unavailable", "failed to evaluate quota")
		return
	} else if exhausted {
		respondError(c, 403, "quota_exhausted", "natural-month quota exhausted; configuration sync resumes next month")
		return
	}

	access, err := h.networkAccessForUser(c.Request.Context(), user)
	if err != nil {
		respondError(c, 503, "network_access_unavailable", "network access is unavailable")
		return
	}
	if !access.Allowed {
		respondError(c, 403, "team_access_inactive", "team access is inactive")
		return
	}
	scopedReader := h.networkStatusReader(access)
	resourceClass := effectiveAgentResourceClass(access, user)
	selectedPools := regionalPoolsForUser(scopedReader, access, user, time.Now().UTC())
	teamHosts := []string{}
	if access.Mode == "dedicated" {
		for _, pool := range selectedPools {
			if pool.OpenToUsers {
				teamHosts = append(teamHosts, pool.Entry)
			}
		}
		if len(teamHosts) == 0 {
			respondError(c, 503, "team_nodes_unavailable", "dedicated team nodes are unavailable")
			return
		}
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

	// Team authorization and node health are independent of user.UpdatedAt.
	changed := sinceVersion < version || access.Configured || access.Mode == "dedicated"
	renderedJSON := ""
	digest := ""
	warnings := []string{}
	if changed {
		var err error
		if access.Mode == "dedicated" {
			renderedJSON, digest, warnings, err = renderDedicatedClientConfig(user, teamHosts[0], poolXHTTPPath(envOrDefault("XRAY_XHTTP_PATH", defaultXHTTPPath), resourceClass))
		} else {
			renderedJSON, digest, warnings, err = h.renderUserXrayConfig(user)
		}
		if err != nil && access.Mode == "dedicated" {
			respondError(c, 503, "team_config_unavailable", "team configuration is unavailable")
			return
		}
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
		proxyUUID := strings.TrimSpace(user.ProxyUUID)

		// The entrypoint is sourced from the authenticated agent status report.
		// Do not synthesize a static domain list: the same entrypoint can route
		// free/plus/team traffic by the entitlement path.
		xhttpPath := poolXHTTPPath(envOrDefault("XRAY_XHTTP_PATH", defaultXHTTPPath), resourceClass)
		xhttpMode := envOrDefault("XRAY_XHTTP_MODE", defaultXHTTPMode)
		xhttpScheme := xrayconfig.VLESSXHTTPScheme()

		for _, pool := range selectedPools {
			if !pool.OpenToUsers {
				continue
			}
			host := pool.Entry
			nodeName := pool.DisplayName
			if strings.TrimSpace(nodeName) == "" {
				nodeName = resolveNodeName(host, map[string]string{})
			}
			countryCode := pool.Code
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
				"id":             nodeID,
				"remark":         nodeName,
				"display_name":   nodeName,
				"host":           host,
				"address":        host,
				"port":           443,
				"uuid":           proxyUUID,
				"flow":           "",
				"transport":      "xhttp",
				"security":       "tls",
				"source":         "server",
				"country_code":   countryCode,
				"resource_class": resourceClass,
				"vless_uri":      vlessURI,
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
				"resource_class":   resourceClass,
				"updated_at":       updatedAt,
				"vless_uri":        vlessURI,
				"uri_scheme_xhttp": vlessURI,
			})
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"network_access": access,
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
	if h.reservedDedicatedHost(domain) {
		return "", "", nil, errors.New("configured domain is reserved for a dedicated team")
	}
	if domain == "" {
		return "", "", nil, errors.New("xray domain is not configured")
	}

	return renderNetworkXrayConfig(user, domain)
}

func renderNetworkXrayConfig(user *store.User, domain string) (string, string, []string, error) {
	clientID := strings.TrimSpace(user.ProxyUUID)
	if clientID == "" {
		return "", "", nil, fmt.Errorf("proxy UUID is not provisioned for account %q", strings.TrimSpace(user.ID))
	}
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

// A team snapshot is a client configuration, with the sole proxy outbound
// targeting an authorized team entry. It must never render the server template.
func renderDedicatedClientConfig(user *store.User, host, path string) (string, string, []string, error) {
	config := map[string]any{
		"log":      map[string]any{"loglevel": "warning"},
		"inbounds": []any{map[string]any{"listen": "127.0.0.1", "port": 1080, "protocol": "socks", "settings": map[string]any{"udp": true}}},
		"outbounds": []any{map[string]any{
			"tag": "proxy", "protocol": "vless",
			"settings":       map[string]any{"vnext": []any{map[string]any{"address": host, "port": envIntOrDefault("XRAY_XHTTP_PORT", defaultXHTTPPort), "users": []any{map[string]any{"id": user.ProxyUUID, "encryption": "none"}}}}},
			"streamSettings": map[string]any{"network": "xhttp", "security": "tls", "tlsSettings": map[string]any{"serverName": host, "fingerprint": defaultTLSFP}, "xhttpSettings": map[string]any{"path": path, "mode": envOrDefault("XRAY_XHTTP_MODE", defaultXHTTPMode)}},
		}},
	}
	buf, err := json.Marshal(config)
	if err != nil {
		return "", "", nil, err
	}
	digest := sha256.Sum256(buf)
	return string(buf), hex.EncodeToString(digest[:]), []string{}, nil
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
