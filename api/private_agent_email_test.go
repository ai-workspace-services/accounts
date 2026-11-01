package api

import (
	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/store"
	"testing"
	"time"
)

func TestAgentEmailAllowed(t *testing.T) {
	for _, tc := range []struct {
		emails []string
		user   *store.User
		want   bool
	}{
		{nil, nil, true},
		{[]string{"Alice@Example.com"}, &store.User{Email: " alice@example.com "}, true},
		{[]string{"alice@example.com"}, &store.User{Email: "bob@example.com"}, false},
		{[]string{"alice@example.com"}, nil, false},
	} {
		if got := agentEmailAllowed(tc.emails, tc.user); got != tc.want {
			t.Fatalf("allowed=%v want=%v", got, tc.want)
		}
	}
}

func TestPrivateRegionalPoolVisibility(t *testing.T) {
	now := time.Now().UTC()
	reader := stubAgentStatusReader{statuses: []agentserver.StatusSnapshot{{
		Agent: agentserver.Identity{ID: "private", Groups: []string{"resource:custom"}}, UpdatedAt: now,
		Report: agentproto.StatusReport{Role: "agent-proxy", Healthy: true, Xray: agentproto.XrayStatus{Running: true, Region: "jp", EntryPoint: "private.example", AllowedEmails: []string{"alice@example.com"}}},
	}}}
	access := networkAccess{Mode: "personal", Allowed: true}
	for _, tc := range []struct {
		email, group string
		want         int
	}{
		{"alice@example.com", store.MonthlyPlusQuotaLimitGroup, 1},
		{"bob@example.com", store.MonthlyPlusQuotaLimitGroup, 0},
		{"alice@example.com", store.MonthlyFreeQuotaLimitGroup, 0},
	} {
		pools := regionalPoolsForUser(reader, access, &store.User{Email: tc.email, Groups: []string{tc.group}}, "", now)
		if len(pools) != tc.want || (len(pools) > 0 && !pools[0].Private) {
			t.Fatalf("email=%s pools=%+v", tc.email, pools)
		}
	}
}
