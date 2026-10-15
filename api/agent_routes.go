package api

import (
	"os"
	"sort"
	"strings"
	"time"

	"account/internal/store"
)

const defaultPoolRoutePrefix = "/pool"

const agentResourceTeam = "team"

// effectiveAgentResourceClass is the single control-plane decision used when
// rendering a subscription path. Team access wins over the personal plan;
// every other user is either a paid (plus) or free user, with free as the
// safe fallback. The agent independently enforces the same decision while
// building its Xray client list.
func effectiveAgentResourceClass(access networkAccess, user *store.User) string {
	if access.Mode == "dedicated" {
		return agentResourceTeam
	}
	if store.MonthlyQuotaGroup(user) == store.MonthlyPlusQuotaLimitGroup ||
		store.MonthlyQuotaGroup(user) == store.MonthlyUnlimitedBetaQuotaGroup {
		return agentResourcePlus
	}
	return agentResourceFree
}

// poolXHTTPPath prefixes the XHTTP path with the entitlement route consumed
// by APISIX. The prefix is configurable for a direct-agent compatibility
// environment, but UAT/prod default to /pool so the shared entrypoint can
// dispatch free, plus, and team traffic to different upstreams.
func poolXHTTPPath(basePath, resourceClass string) string {
	basePath = "/" + strings.TrimLeft(strings.TrimSpace(basePath), "/")
	resourceClass = strings.Trim(strings.ToLower(resourceClass), "/")
	if resourceClass == "" {
		return basePath
	}
	prefix := strings.TrimSpace(envOrDefault("XRAY_POOL_ROUTE_PREFIX", defaultPoolRoutePrefix))
	if raw, ok := os.LookupEnv("XRAY_POOL_ROUTE_PREFIX"); ok && strings.EqualFold(strings.TrimSpace(raw), "none") {
		return basePath
	}
	if prefix == "" {
		return basePath
	}
	prefix = "/" + strings.Trim(prefix, "/")
	return prefix + "/" + resourceClass + basePath
}

func regionalPoolsForUser(reader agentStatusReader, access networkAccess, user *store.User, now time.Time) []regionalPool {
	wantClass := effectiveAgentResourceClass(access, user)
	wantTeam := strings.TrimSpace(access.TeamID)
	if reader == nil {
		return nil
	}
	type aggregate struct {
		pool    regionalPool
		ids     map[string]bool
		reasons map[regionalClosureReason]bool
	}
	groups := map[string]*aggregate{}
	for _, snapshot := range reader.Statuses() {
		report := snapshot.Report
		if snapshot.UpdatedAt.IsZero() || (report.Role != "" && report.Role != "agent-proxy") || !networkScopeAllows(access, snapshot.Agent.Groups) {
			continue
		}
		region := strings.ToLower(strings.TrimSpace(report.Xray.Region))
		entry := regionalEntryHost(report.Xray.EntryPoint)
		if region == "" || entry == "" {
			continue
		}
		resourceClass, teamID := reportedResourceClass(snapshot)
		if resourceClass != wantClass || (resourceClass == agentResourceTeam && teamID != wantTeam) {
			continue
		}
		key := region + "\x00" + entry + "\x00" + resourceClass + "\x00" + teamID
		group := groups[key]
		if group == nil {
			group = &aggregate{pool: regionalPool{Code: region, Entry: entry, DisplayName: strings.TrimSpace(snapshot.Agent.Name), ResourceClass: resourceClass, TeamID: teamID, ClosedReasons: make([]regionalClosureReason, 0)}, ids: map[string]bool{}, reasons: map[regionalClosureReason]bool{}}
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
	selected := make([]regionalPool, 0, len(groups))
	for _, group := range groups {
		group.pool.PoolCount = len(group.ids)
		if !group.pool.OpenToUsers {
			for _, reason := range []regionalClosureReason{regionalExplicitDisabled, regionalStale, regionalUnhealthy, regionalXrayNotRunning} {
				if group.reasons[reason] {
					group.pool.ClosedReasons = append(group.pool.ClosedReasons, reason)
				}
			}
		}
		selected = append(selected, group.pool)
	}
	sort.Slice(selected, func(i, j int) bool {
		if selected[i].Code == selected[j].Code {
			return selected[i].Entry < selected[j].Entry
		}
		return selected[i].Code < selected[j].Code
	})
	return selected
}
