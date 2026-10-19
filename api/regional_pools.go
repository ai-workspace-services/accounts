package api

import (
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/agentserver"
	"account/internal/store"
)

const regionalHeartbeatTTL = 5 * time.Minute
const regionalDiscoveryVersion = "availability-v1"

func canViewRegionalPoolInventory(user *store.User) bool {
	if user == nil {
		return false
	}
	return isRootUser(user) || store.IsOperatorRole(user.Role) || user.Level == store.LevelOperator
}

type regionalClosureReason string

const (
	regionalExplicitDisabled regionalClosureReason = "explicit_disabled"
	regionalStale            regionalClosureReason = "stale"
	regionalUnhealthy        regionalClosureReason = "unhealthy"
	regionalXrayNotRunning   regionalClosureReason = "xray_not_running"
)

type regionalPool struct {
	Code          string                  `json:"code"`
	Entry         string                  `json:"entry"`
	DisplayName   string                  `json:"displayName,omitempty"`
	ResourceClass string                  `json:"resourceClass,omitempty"`
	TeamID        string                  `json:"teamId,omitempty"`
	PoolCount     int                     `json:"poolCount"`
	OpenToUsers   bool                    `json:"openToUsers"`
	ClosedReasons []regionalClosureReason `json:"closedReasons"`
}

func (h *handler) listRegionalPools(c *gin.Context) {
	user, ok := h.resolveAgentNodeUser(c)
	if !ok {
		return
	}
	if h.agentStatusReader == nil {
		respondError(c, http.StatusServiceUnavailable, "agent_status_unavailable", "agent registry is not configured")
		return
	}
	c.Header("Cache-Control", "no-store")
	var pools []regionalPool
	// Administrators and operators can inspect the full inventory, including
	// closed and team-dedicated pools. Ordinary users remain constrained by
	// their effective entitlement scope below.
	if canViewRegionalPoolInventory(user) {
		pools = registeredRegionalPools(h.agentStatusReader, time.Now().UTC())
	} else {
		if blocked, err := h.proxyAccountBlocked(c.Request.Context(), user); err != nil {
			respondError(c, 503, "network_access_unavailable", "network access is unavailable")
			return
		} else if blocked || !user.Active {
			c.JSON(http.StatusOK, []regionalPool{})
			return
		}
		if err := h.ensureExpiredSubscriptionDowngrade(c.Request.Context(), user); err != nil {
			respondError(c, 503, "quota_unavailable", "failed to reconcile quota")
			return
		}
		access, err := h.networkAccessForUser(c.Request.Context(), user)
		if err != nil {
			respondError(c, 503, "network_access_unavailable", "network access is unavailable")
			return
		}
		if !access.Allowed {
			c.JSON(http.StatusOK, []regionalPool{})
			return
		}
		packageName, err := h.billingPackageName(c.Request.Context(), user)
		if err != nil {
			respondError(c, 503, "billing_profile_unavailable", "failed to evaluate subscription resource class")
			return
		}
		pools = regionalPoolsForUser(h.networkStatusReader(access), access, user, packageName, time.Now().UTC())
	}
	if !canViewRegionalPoolInventory(user) {
		visible := make([]regionalPool, 0, len(pools))
		for _, pool := range pools {
			if pool.OpenToUsers {
				visible = append(visible, pool)
			}
		}
		pools = visible
	}
	c.JSON(http.StatusOK, pools)
}

// Aggregate only authenticated status reports. Configured credentials without
// a report, and Gateway/One reports, never create regional proxy entries.
func registeredRegionalPools(reader agentStatusReader, now time.Time) []regionalPool {
	pools := make([]regionalPool, 0)
	if reader == nil {
		return pools
	}
	type aggregate struct {
		pool    regionalPool
		ids     map[string]bool
		reasons map[regionalClosureReason]bool
	}
	groups := make(map[string]*aggregate)
	for _, snapshot := range reader.Statuses() {
		report := snapshot.Report
		if snapshot.UpdatedAt.IsZero() || (report.Role != "" && report.Role != "agent-proxy") {
			continue
		}
		region := strings.ToLower(strings.TrimSpace(report.Xray.Region))
		entry := regionalEntryHost(report.Xray.EntryPoint)
		if report.Xray.EntryPoint == "" {
			// Compatibility with agents whose ID is already their public domain.
			entry = regionalEntryHost(snapshot.Agent.ID)
		}
		if region == "" || entry == "" {
			continue
		}
		resourceClass, teamID := reportedResourceClass(snapshot)
		key := region + "\x00" + entry + "\x00" + resourceClass + "\x00" + teamID
		group := groups[key]
		if group == nil {
			group = &aggregate{
				pool:    regionalPool{Code: region, Entry: entry, DisplayName: strings.TrimSpace(snapshot.Agent.Name), ResourceClass: resourceClass, TeamID: teamID, ClosedReasons: make([]regionalClosureReason, 0)},
				ids:     make(map[string]bool),
				reasons: make(map[regionalClosureReason]bool),
			}
			groups[key] = group
		}
		if group.pool.DisplayName == "" {
			group.pool.DisplayName = strings.TrimSpace(snapshot.Agent.Name)
		}
		id := strings.TrimSpace(report.Xray.Pool)
		if id == "" {
			id = snapshot.Agent.ID
		}
		group.ids[id] = true
		fresh := !snapshot.UpdatedAt.After(now) && now.Sub(snapshot.UpdatedAt) <= regionalHeartbeatTTL
		open := report.Xray.OpenToUsers == nil || *report.Xray.OpenToUsers
		group.pool.OpenToUsers = group.pool.OpenToUsers || (open && fresh && report.Healthy && report.Xray.Running)
		if !open {
			group.reasons[regionalExplicitDisabled] = true
		}
		if !fresh {
			group.reasons[regionalStale] = true
		}
		if !report.Healthy {
			group.reasons[regionalUnhealthy] = true
		}
		if !report.Xray.Running {
			group.reasons[regionalXrayNotRunning] = true
		}
	}
	for _, group := range groups {
		group.pool.PoolCount = len(group.ids)
		// One available member makes the regional entry usable. Failed members
		// must not leave a contradictory closure reason on an open aggregate.
		if !group.pool.OpenToUsers {
			for _, reason := range []regionalClosureReason{regionalExplicitDisabled, regionalStale, regionalUnhealthy, regionalXrayNotRunning} {
				if group.reasons[reason] {
					group.pool.ClosedReasons = append(group.pool.ClosedReasons, reason)
				}
			}
		}
		pools = append(pools, group.pool)
	}
	sort.Slice(pools, func(i, j int) bool {
		if pools[i].Code == pools[j].Code {
			return pools[i].Entry < pools[j].Entry
		}
		return pools[i].Code < pools[j].Code
	})
	return pools
}

// Credential scope is authoritative, including explicit shared registrations.
// Pool names are arbitrary labels and must never change node eligibility.
func reportedResourceClass(snapshot agentserver.StatusSnapshot) (string, string) {
	if scope, valid := agentResourceScope(snapshot.Agent.Groups); valid {
		return scope.ResourceClass, strings.TrimSpace(scope.TeamID)
	}
	return "", ""
}

func regionalEntryHost(raw string) string {
	value := strings.TrimSpace(raw)
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	u, err := url.Parse(value)
	if err != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || host == "*" || net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return ""
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return ""
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return ""
			}
		}
	}
	return host
}
