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
	"account/internal/routingpolicy"
	"account/internal/store"
	"account/internal/subscription"
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
	Private                 bool     `json:"private,omitempty"`
	Name                    string   `json:"name"`
	Region                  string   `json:"region,omitempty"`
	PoolCount               int      `json:"pool_count,omitempty"`
	OpenToUsers             bool     `json:"open_to_users"`
	Address                 string   `json:"address"`
	Port                    int      `json:"port,omitempty"`
	Users                   []string `json:"users,omitempty"`
	Transport               string   `json:"transport,omitempty"`
	Path                    string   `json:"path,omitempty"`
	Mode                    string   `json:"mode,omitempty"`
	Security                string   `json:"security,omitempty"`
	Flow                    string   `json:"flow,omitempty"`
	ResourceClass           string   `json:"resource_class,omitempty"`
	RoutingPool             string   `json:"routing_pool,omitempty"`
	PricingGroup            string   `json:"pricing_group,omitempty"`
	SubPoolTags             []string `json:"sub_pool_tags,omitempty"`
	ServerName              string   `json:"server_name,omitempty"`
	XHTTPPort               int      `json:"xhttp_port,omitempty"`
	TCPPort                 int      `json:"tcp_port,omitempty"`
	URISchemeXHTTP          string   `json:"uri_scheme_xhttp,omitempty"`
	URISchemeTCP            string   `json:"uri_scheme_tcp,omitempty"`
	HighSpeedQuotaExhausted bool     `json:"highSpeedQuotaExhausted,omitempty"`
}

type userNodesContext struct {
	user                    *store.User
	access                  networkAccess
	packageName             string
	resourceClass           string
	highSpeedQuotaExhausted bool
	nodes                   []VlessNode
	subNodes                []subscription.Node
}

func (h *handler) resolveUserNodes(c *gin.Context) (*userNodesContext, bool) {
	user, ok := h.resolveAgentNodeUser(c)
	if !ok {
		return nil, false
	}
	if blocked, err := h.proxyAccountBlocked(c.Request.Context(), user); err != nil {
		respondError(c, 503, "network_access_unavailable", "network access is unavailable")
		return nil, false
	} else if blocked {
		respondError(c, 403, "proxy_access_blocked", "proxy access is blocked")
		return nil, false
	}
	if err := h.ensureExpiredSubscriptionDowngrade(c.Request.Context(), user); err != nil {
		respondError(c, 503, "quota_unavailable", "failed to reconcile quota")
		return nil, false
	}
	quotaStatus, err := h.evaluateDualBucketQuotaForUser(c.Request.Context(), user)
	if err != nil {
		respondError(c, 503, "quota_unavailable", "failed to evaluate quota")
		return nil, false
	}
	if quotaStatus.fullyExhausted {
		respondError(c, 403, "quota_exhausted", "natural-month quota exhausted")
		return nil, false
	}

	if !user.Active {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   "account_paused",
			"message": "account is paused",
		})
		return nil, false
	}

	proxyUUID := strings.TrimSpace(user.ProxyUUID)
	if proxyUUID == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":   "proxy_uuid_unavailable",
			"message": "proxy UUID is not provisioned for this account",
		})
		return nil, false
	}

	if user.ProxyUUIDExpiresAt != nil && time.Now().UTC().After(*user.ProxyUUIDExpiresAt) {
		// Sandbox renews access metadata hourly; never block it on expiry.
		if strings.EqualFold(strings.TrimSpace(user.Email), sandboxUserEmail) {
			if err := h.ensureSandboxProxyUUID(c.Request.Context(), user); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "sandbox_uuid_rotation_failed"})
				return nil, false
			}
			proxyUUID = strings.TrimSpace(user.ProxyUUID)
			if proxyUUID == "" {
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"error":   "proxy_uuid_unavailable",
					"message": "proxy UUID is not provisioned for this account",
				})
				return nil, false
			}
		} else {
			c.JSON(http.StatusForbidden, gin.H{
				"error":   "proxy_uuid_expired",
				"message": "proxy access has expired, please renew",
			})
			return nil, false
		}
	}

	access, err := h.networkAccessForUser(c.Request.Context(), user)
	if err != nil {
		respondError(c, 503, "network_access_unavailable", "network access is unavailable")
		return nil, false
	}
	if !access.Allowed {
		respondError(c, 403, "team_access_inactive", "team access is inactive")
		return nil, false
	}
	packageName, err := h.billingPackageName(c.Request.Context(), user)
	if err != nil {
		respondError(c, 503, "billing_profile_unavailable", "failed to evaluate subscription resource class")
		return nil, false
	}
	resourceClass := effectiveAgentResourceClass(access, user, packageName)
	pools := regionalPoolsForUser(h.networkStatusReader(access), access, user, packageName, time.Now().UTC())
	xhttpMode := envOrDefault("XRAY_XHTTP_MODE", defaultXHTTPMode)
	xhttpPort := envIntOrDefault("XRAY_XHTTP_PORT", defaultXHTTPPort)
	tcpPort := envIntOrDefault("XRAY_TCP_PORT", defaultTCPPort)

	xhttpScheme := xrayconfig.VLESSXHTTPScheme()
	tcpScheme := xrayconfig.VLESSTCPScheme()
	policy := routingpolicy.DefaultPolicy()

	users := []string{proxyUUID}
	nodes := make([]VlessNode, 0, len(pools))
	subNodes := make([]subscription.Node, 0, len(pools))
	for _, pool := range pools {
		if !pool.OpenToUsers {
			continue
		}
		nodeClass := resourceClass
		if pool.ResourceClass == agentResourceFree {
			nodeClass = agentResourceFree
		}
		pricingGroup := strings.TrimSpace(pool.ResourceClass)
		if pricingGroup == "" {
			pricingGroup = nodeClass
		}
		routingPool := strings.TrimSpace(pool.RoutingPool)
		if routingPool == "" {
			routingPool = policy.PoolForResourceClass(pricingGroup)
		}
		if !poolAllowedForQuota(routingPool, quotaStatus) {
			continue
		}
		xhttpPath := poolXHTTPPath(envOrDefault("XRAY_XHTTP_PATH", defaultXHTTPPath), nodeClass)
		host := pool.Entry
		nodeName := pool.DisplayName
		if strings.TrimSpace(nodeName) == "" {
			nodeName = pool.Code
		}
		vlessXHTTP := renderVLESSURIScheme(xhttpScheme, map[string]string{
			"UUID":   proxyUUID,
			"DOMAIN": host,
			"NODE":   host,
			"PATH":   url.QueryEscape(xhttpPath),
			"MODE":   url.QueryEscape(xhttpMode),
			"SNI":    host,
			"FP":     defaultTLSFP,
			"TAG":    url.QueryEscape(nodeName),
		})
		vlessTCP := renderVLESSURIScheme(tcpScheme, map[string]string{
			"UUID":   proxyUUID,
			"DOMAIN": host,
			"NODE":   host,
			"SNI":    host,
			"FP":     defaultTLSFP,
			"FLOW":   defaultTCPFlow,
			"TAG":    url.QueryEscape(nodeName),
		})
		nodes = append(nodes, VlessNode{
			Private:                 pool.Private,
			Name:                    nodeName,
			Region:                  pool.Code,
			PoolCount:               pool.PoolCount,
			OpenToUsers:             true,
			Address:                 host,
			Port:                    xhttpPort,
			Users:                   users,
			Transport:               "xhttp",
			Path:                    xhttpPath,
			Mode:                    xhttpMode,
			Security:                "tls",
			Flow:                    defaultTCPFlow,
			ResourceClass:           nodeClass,
			RoutingPool:             routingPool,
			PricingGroup:            pricingGroup,
			SubPoolTags:             append([]string(nil), pool.SubPoolTags...),
			ServerName:              host,
			XHTTPPort:               xhttpPort,
			TCPPort:                 tcpPort,
			URISchemeXHTTP:          vlessXHTTP,
			URISchemeTCP:            vlessTCP,
			HighSpeedQuotaExhausted: quotaStatus.gateEnabled && quotaStatus.highSpeedExhausted,
		})
		subNodes = append(subNodes, subscription.Node{
			ID:            host,
			Name:          nodeName,
			Region:        pool.Code,
			Address:       host,
			Port:          xhttpPort,
			UUID:          proxyUUID,
			Flow:          defaultTCPFlow,
			Transport:     "xhttp",
			Path:          xhttpPath,
			Mode:          xhttpMode,
			Security:      "tls",
			ServerName:    host,
			Fingerprint:   defaultTLSFP,
			ResourceClass: pricingGroup,
			RoutingPool:   routingPool,
			PricingGroup:  pricingGroup,
			SubPoolTags:   append([]string(nil), pool.SubPoolTags...),
			VlessURI:      vlessXHTTP,
		})
	}

	if access.Mode == "dedicated" && len(nodes) == 0 {
		respondError(c, 503, "team_nodes_unavailable", "dedicated team nodes are unavailable")
		return nil, false
	}

	return &userNodesContext{
		user:                    user,
		access:                  access,
		packageName:             packageName,
		resourceClass:           resourceClass,
		highSpeedQuotaExhausted: quotaStatus.gateEnabled && quotaStatus.highSpeedExhausted,
		nodes:                   nodes,
		subNodes:                subNodes,
	}, true
}

func (h *handler) listAgentNodes(c *gin.Context) {
	// Add panic recovery for this handler
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("PANIC in listAgentNodes: %v\n", r)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error", "message": fmt.Sprintf("%v", r)})
		}
	}()

	resolved, ok := h.resolveUserNodes(c)
	if !ok {
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, resolved.nodes)
}

func (h *handler) getSubscription(c *gin.Context) {
	rawFormat := strings.TrimSpace(c.Param("format"))
	if rawFormat == "" {
		rawFormat = strings.TrimSpace(c.Query("format"))
	}
	format, err := subscription.ParseFormat(rawFormat)
	if err != nil {
		respondError(c, http.StatusBadRequest, "unsupported_subscription_format", err.Error())
		return
	}

	resolved, ok := h.resolveUserNodes(c)
	if !ok {
		return
	}

	allowCustom := resolved.resourceClass == agentResourceCustom ||
		resolved.resourceClass == agentResourceTeam ||
		(resolved.access.Mode != "dedicated" && isCustomDedicatedUser(resolved.user, resolved.packageName))
	allowPro := resolved.resourceClass == agentResourcePro && !resolved.highSpeedQuotaExhausted
	body, contentType, err := subscription.Render(format, subscription.RenderInput{
		UserID:      resolved.user.ID,
		UserEmail:   resolved.user.Email,
		AllowCustom: allowCustom,
		AllowPro:    allowPro,
		Policy:      routingpolicy.DefaultPolicy(),
		Nodes:       resolved.subNodes,
		GeneratedAt: time.Now().UTC(),
	})
	if err != nil {
		respondError(c, http.StatusInternalServerError, "subscription_render_failed", "failed to render subscription")
		return
	}

	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, contentType, body)
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

func resolveNodeName(host string, names map[string]string) string {
	if regionalName := regionalVLESSDisplayName(host); regionalName != "" {
		return regionalName
	}
	if len(names) > 0 {
		if name := strings.TrimSpace(names[host]); name != "" && !nodeNameConflictsWithHostRegion(name, host) {
			return name
		}
	}
	return nodeNameForHost(host)
}

func regionalVLESSDisplayName(host string) string {
	switch strings.ToLower(strings.SplitN(strings.TrimSpace(host), ".", 2)[0]) {
	case "jp-xconnect":
		return "JP-Connect"
	case "hk-xconnect":
		return "HK-Connect"
	case "ph-xconnect":
		return "PH-Connect"
	case "us-xconnect":
		return "US-Connect"
	default:
		return ""
	}
}

func nodeNameConflictsWithHostRegion(name, host string) bool {
	hostRegion := regionalCode(host)
	nameRegion := regionalCode(name)
	return hostRegion != "" && nameRegion != "" && hostRegion != nameRegion
}

func regionalCode(value string) string {
	for _, part := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return r < 'a' || r > 'z'
	}) {
		switch part {
		case "jp", "us", "hk", "ph":
			return strings.ToUpper(part)
		}
	}
	return ""
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
