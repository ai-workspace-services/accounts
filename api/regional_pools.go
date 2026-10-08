package api

import (
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

const regionalHeartbeatTTL = 5 * time.Minute
const regionalDiscoveryVersion = "availability-v1"

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
	pools := registeredRegionalPools(h.agentStatusReader, time.Now().UTC())
	if !store.IsAdminRole(user.Role) && user.Level != store.LevelAdmin {
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
		key := region + "\x00" + entry
		group := groups[key]
		if group == nil {
			group = &aggregate{
				pool:    regionalPool{Code: region, Entry: entry, ClosedReasons: make([]regionalClosureReason, 0)},
				ids:     make(map[string]bool),
				reasons: make(map[regionalClosureReason]bool),
			}
			groups[key] = group
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
