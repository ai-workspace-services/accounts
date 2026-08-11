package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/auth"
	"account/internal/store"
	"account/internal/xrayconfig"
)

const (
	defaultXHTTPPath = "/split"
	defaultXHTTPMode = "auto"
	defaultXHTTPPort = 443
	defaultTCPPort   = 1443
	defaultTLSFP     = "chrome"
	defaultTCPFlow   = "xtls-rprx-vision"
)

type VlessNode struct {
	Name           string   `json:"name"`
	Address        string   `json:"address"`
	Port           int      `json:"port,omitempty"`
	Users          []string `json:"users,omitempty"`
	Transport      string   `json:"transport,omitempty"`
	Path           string   `json:"path,omitempty"`
	Mode           string   `json:"mode,omitempty"`
	Security       string   `json:"security,omitempty"`
	Flow           string   `json:"flow,omitempty"`
	ServerName     string   `json:"server_name,omitempty"`
	XHTTPPort      int      `json:"xhttp_port,omitempty"`
	TCPPort        int      `json:"tcp_port,omitempty"`
	URISchemeXHTTP string   `json:"uri_scheme_xhttp,omitempty"`
	URISchemeTCP   string   `json:"uri_scheme_tcp,omitempty"`
}

func (h *handler) listAgentNodes(c *gin.Context) {
	user, ok := h.resolveAgentNodeUser(c)
	if !ok {
		return
	}

	if !user.Active {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   "account_paused",
			"message": "account is paused",
		})
		return
	}

	proxyUUID := strings.TrimSpace(user.ProxyUUID)

	if user.ProxyUUIDExpiresAt != nil && time.Now().UTC().After(*user.ProxyUUIDExpiresAt) {
		// Sandbox renews access metadata hourly; never block it on expiry.
		if strings.EqualFold(strings.TrimSpace(user.Email), sandboxUserEmail) {
			if err := h.ensureSandboxProxyUUID(c.Request.Context(), user); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "sandbox_uuid_rotation_failed"})
				return
			}
			proxyUUID = strings.TrimSpace(user.ProxyUUID)
		} else {
			c.JSON(http.StatusForbidden, gin.H{
				"error":   "proxy_uuid_expired",
				"message": "proxy access has expired, please renew",
			})
			return
		}
	}

	// Add panic recovery for this handler
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("PANIC in listAgentNodes: %v\n", r)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error", "message": fmt.Sprintf("%v", r)})
		}
	}()

	registeredHosts, registeredNames := registeredNodeMetadata(h.agentStatusReader)
	hosts := parseProxyNodeHosts(h.publicURL, registeredHosts)

	if len(hosts) == 0 {
		c.JSON(http.StatusOK, []VlessNode{})
		return
	}

	xhttpPath := envOrDefault("XRAY_XHTTP_PATH", defaultXHTTPPath)
	xhttpMode := envOrDefault("XRAY_XHTTP_MODE", defaultXHTTPMode)
	xhttpPort := envIntOrDefault("XRAY_XHTTP_PORT", defaultXHTTPPort)
	tcpPort := envIntOrDefault("XRAY_TCP_PORT", defaultTCPPort)

	xhttpScheme := xrayconfig.VLESSXHTTPScheme()
	tcpScheme := xrayconfig.VLESSTCPScheme()

	users := []string{proxyUUID}
	nodes := make([]VlessNode, 0, len(hosts))
	for _, host := range hosts {
		nodeName := resolveNodeName(host, registeredNames)
		nodes = append(nodes, VlessNode{
			Name:       nodeName,
			Address:    host,
			Port:       xhttpPort,
			Users:      users,
			Transport:  "xhttp",
			Path:       xhttpPath,
			Mode:       xhttpMode,
			Security:   "tls",
			Flow:       defaultTCPFlow,
			ServerName: host,
			XHTTPPort:  xhttpPort,
			TCPPort:    tcpPort,
			URISchemeXHTTP: renderVLESSURIScheme(xhttpScheme, map[string]string{
				"UUID":   proxyUUID,
				"DOMAIN": host,
				"NODE":   host,
				"PATH":   url.QueryEscape(xhttpPath),
				"MODE":   url.QueryEscape(xhttpMode),
				"SNI":    host,
				"FP":     defaultTLSFP,
				"TAG":    url.QueryEscape(nodeName),
			}),
			URISchemeTCP: renderVLESSURIScheme(tcpScheme, map[string]string{
				"UUID":   proxyUUID,
				"DOMAIN": host,
				"NODE":   host,
				"SNI":    host,
				"FP":     defaultTLSFP,
				"FLOW":   defaultTCPFlow,
				"TAG":    url.QueryEscape(nodeName),
			}),
		})
	}

	// Final safety for Sandbox: if no nodes are available, the UI will be blocked.
	email := strings.ToLower(strings.TrimSpace(user.Email))
	if len(nodes) == 0 && email == sandboxUserEmail {
		host := normalizeHost(h.publicURL)
		if host == "" {
			host = normalizeHost(c.Request.Host)
		}
		if host == "" {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "proxy_endpoint_unavailable"})
			return
		}

		nodeName := nodeNameForHost(host)
		nodes = append(nodes, VlessNode{
			Name:       nodeName,
			Address:    host,
			Port:       xhttpPort,
			Users:      users,
			Transport:  "xhttp",
			Path:       xhttpPath,
			Mode:       xhttpMode,
			Security:   "tls",
			Flow:       defaultTCPFlow,
			ServerName: host,
			XHTTPPort:  xhttpPort,
			TCPPort:    tcpPort,
			URISchemeXHTTP: renderVLESSURIScheme(xhttpScheme, map[string]string{
				"UUID":   proxyUUID,
				"DOMAIN": host,
				"NODE":   host,
				"PATH":   url.QueryEscape(xhttpPath),
				"MODE":   url.QueryEscape(xhttpMode),
				"SNI":    host,
				"FP":     defaultTLSFP,
				"TAG":    url.QueryEscape(nodeName),
			}),
			URISchemeTCP: renderVLESSURIScheme(tcpScheme, map[string]string{
				"UUID":   proxyUUID,
				"DOMAIN": host,
				"NODE":   host,
				"SNI":    host,
				"FP":     defaultTLSFP,
				"FLOW":   defaultTCPFlow,
				"TAG":    url.QueryEscape(nodeName),
			}),
		})
	}

	c.JSON(http.StatusOK, nodes)
}

func (h *handler) resolveAgentNodeUser(c *gin.Context) (*store.User, bool) {
	if userID := strings.TrimSpace(auth.GetUserID(c)); userID != "" && userID != "system" {
		user, err := h.store.GetUserByID(c.Request.Context(), userID)
		if err != nil {
			if errors.Is(err, store.ErrUserNotFound) {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "user_not_found"})
				return nil, false
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_fetch_user"})
			return nil, false
		}
		return user, true
	}

	token := extractToken(c.GetHeader("Authorization"))
	if token == "" {
		token = strings.TrimSpace(c.Query("token"))
	}
	if token == "" {
		if cookie, err := c.Cookie(sessionCookieName); err == nil {
			token = strings.TrimSpace(cookie)
		}
	}
	if token == "" {
		// Console Guest/Demo path: allow the trusted Console BFF to resolve the
		// sandbox user without requiring an end-user session.
		if isInternalServiceRequest(c) {
			sandboxUser, err := h.store.GetUserByEmail(c.Request.Context(), sandboxUserEmail)
			if err != nil {
				if errors.Is(err, store.ErrUserNotFound) {
					c.JSON(http.StatusNotFound, gin.H{"error": "sandbox_missing"})
					return nil, false
				}
				c.JSON(http.StatusInternalServerError, gin.H{"error": "sandbox_lookup_failed"})
				return nil, false
			}
			if err := h.ensureSandboxProxyUUID(c.Request.Context(), sandboxUser); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "sandbox_uuid_rotation_failed"})
				return nil, false
			}
			return sandboxUser, true
		}

		c.JSON(http.StatusUnauthorized, gin.H{"error": "session_token_required", "message": "session token is required"})
		return nil, false
	}

	sess, ok := h.lookupSession(token)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_session", "message": "session token is invalid or expired"})
		return nil, false
	}

	user, err := h.store.GetUserByID(c.Request.Context(), sess.userID)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "user_not_found"})
			return nil, false
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_fetch_user"})
		return nil, false
	}

	return user, true
}

func parseProxyNodeHosts(publicURL string, extraHosts []string) []string {
	seen := make(map[string]struct{})
	hosts := make([]string, 0)

	appendHost := func(raw string) {
		host := normalizeHost(raw)
		if host == "" {
			return
		}
		if _, ok := seen[host]; ok {
			return
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}

	if raw := strings.TrimSpace(os.Getenv("XRAY_PROXY_NODES")); raw != "" {
		fields := strings.FieldsFunc(raw, func(r rune) bool {
			return r == ',' || r == ';' || r == '\n' || r == '\t' || r == ' '
		})
		for _, field := range fields {
			appendHost(field)
		}
	}

	for _, host := range extraHosts {
		appendHost(host)
	}

	if len(hosts) == 0 {
		appendHost(publicURL)
	}

	return hosts
}

func normalizeHost(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" || value == "*" {
		return ""
	}

	if strings.Contains(value, "://") {
		u, err := url.Parse(value)
		if err == nil {
			value = u.Hostname()
		}
	}

	value = strings.TrimPrefix(value, "https://")
	value = strings.TrimPrefix(value, "http://")
	if strings.Contains(value, "/") {
		parts := strings.SplitN(value, "/", 2)
		value = parts[0]
	}
	if strings.Contains(value, ":") {
		host, _, found := strings.Cut(value, ":")
		if found {
			value = host
		}
	}

	return strings.TrimSpace(value)
}

func registeredNodeMetadata(reader agentStatusReader) ([]string, map[string]string) {
	if reader == nil {
		return nil, nil
	}

	snapshots := reader.Statuses()
	hosts := make([]string, 0, len(snapshots))
	names := make(map[string]string, len(snapshots))
	for _, snapshot := range snapshots {
		host := normalizeHost(snapshot.Agent.ID)
		if host == "" {
			continue
		}
		hosts = append(hosts, host)
		if displayName := strings.TrimSpace(snapshot.Agent.Name); displayName != "" {
			names[host] = displayName
		}
	}

	return hosts, names
}

func resolveNodeName(host string, names map[string]string) string {
	if len(names) > 0 {
		if name := strings.TrimSpace(names[host]); name != "" {
			return name
		}
	}
	return nodeNameForHost(host)
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func envIntOrDefault(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func nodeNameForHost(host string) string {
	h := strings.TrimSpace(host)
	if h == "" {
		return "unknown-node"
	}
	return strings.ToLower(h)
}

// countryCodeForHost extracts a two-letter country code from the host's
// subdomain prefix. For example "jp-xhttp.svc.plus" yields "JP",
// "us-east.example.com" yields "US".
func countryCodeForHost(host string) string {
	prefix := host
	if idx := strings.Index(prefix, "."); idx > 0 {
		prefix = prefix[:idx]
	}
	// Take the part before the first dash: "jp-xhttp" -> "jp"
	if idx := strings.Index(prefix, "-"); idx > 0 {
		prefix = prefix[:idx]
	}
	prefix = strings.TrimSpace(prefix)
	if len(prefix) == 2 {
		return strings.ToUpper(prefix)
	}
	return ""
}

func renderVLESSURIScheme(tpl string, values map[string]string) string {
	rendered := strings.TrimSpace(tpl)
	if rendered == "" {
		return ""
	}

	for key, value := range values {
		rendered = strings.ReplaceAll(rendered, "${"+key+"}", value)
	}
	return rendered
}
