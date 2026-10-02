package api

import (
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const regionalHeartbeatTTL = 5 * time.Minute

type regionalPool struct {
	Code        string `json:"code"`
	Entry       string `json:"entry"`
	PoolCount   int    `json:"poolCount"`
	OpenToUsers bool   `json:"openToUsers"`
}

func (h *handler) listRegionalPools(c *gin.Context) {
	if _, ok := h.resolveAgentNodeUser(c); !ok {
		return
	}
	if h.agentStatusReader == nil {
		respondError(c, http.StatusServiceUnavailable, "agent_status_unavailable", "agent registry is not configured")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, registeredRegionalPools(h.agentStatusReader, time.Now().UTC()))
}

// Aggregate only authenticated status reports. Configured credentials without
// a report, and Gateway/One reports, never create regional proxy entries.
func registeredRegionalPools(reader agentStatusReader, now time.Time) []regionalPool {
	pools := make([]regionalPool, 0)
	if reader == nil {
		return pools
	}
	type aggregate struct {
		pool regionalPool
		ids  map[string]bool
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
			group = &aggregate{pool: regionalPool{Code: region, Entry: entry}, ids: make(map[string]bool)}
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
	}
	for _, group := range groups {
		group.pool.PoolCount = len(group.ids)
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
