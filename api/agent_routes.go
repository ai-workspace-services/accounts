package api

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"account/internal/store"
)

const agentResourceTeam = "team"

// effectiveAgentResourceClass is the single control-plane decision used when
// rendering a subscription path. Team access wins over the personal plan;
// otherwise the billing package selects Pro, quota groups select Plus, and
// Free is the safe fallback. The agent independently enforces this scope.
func effectiveAgentResourceClass(access networkAccess, user *store.User, packageName string) string {
	if access.Mode == "dedicated" {
		return agentResourceTeam
	}
	group := store.MonthlyQuotaGroup(user)
	if strings.EqualFold(strings.TrimSpace(packageName), agentResourcePro) &&
		(group == store.MonthlyPlusQuotaLimitGroup || group == store.MonthlyUnlimitedBetaQuotaGroup) {
		return agentResourcePro
	}
	if group == store.MonthlyPlusQuotaLimitGroup || group == store.MonthlyUnlimitedBetaQuotaGroup {
		return agentResourcePlus
	}
	return agentResourceFree
}

func (h *handler) billingPackageName(ctx context.Context, user *store.User) (string, error) {
	if user == nil {
		return "", nil
	}
	profile, err := h.store.GetAccountBillingProfile(ctx, user.ID)
	if errors.Is(err, store.ErrUserNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if profile == nil {
		return "", nil
	}
	return strings.ToLower(strings.TrimSpace(profile.PackageName)), nil
}

// poolXHTTPPath keeps the transport path independent from the resource class.
// Resource classes are authorization metadata; the edge route remains /split.
func poolXHTTPPath(basePath, _ string) string {
	basePath = "/" + strings.TrimLeft(strings.TrimSpace(basePath), "/")
	return basePath
}

func regionalPoolsForUser(reader agentStatusReader, access networkAccess, user *store.User, packageName string, now time.Time) []regionalPool {
	wantClass := effectiveAgentResourceClass(access, user, packageName)
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
		publicShared := resourceClass == agentResourceShared && wantClass != agentResourceTeam
		plusOnFree := resourceClass == agentResourceFree && wantClass == agentResourcePlus
		if (!publicShared && !plusOnFree && resourceClass != wantClass) || (resourceClass == agentResourceTeam && teamID != wantTeam) {
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
