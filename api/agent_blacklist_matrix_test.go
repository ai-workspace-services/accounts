package api

import (
	"account/internal/store"
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestAgentBlacklistAcrossEveryResourceClass(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	teams := st.(store.NetworkTeamStore)
	now := time.Now()
	for _, id := range []string{"a", "b"} {
		if err := teams.PutNetworkTeam(ctx, store.NetworkTeam{ID: id, Name: id, Active: true, MemberLimit: 20, ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour)}, &store.AuditLog{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ name, group, team string }{
		{"free", "", ""}, {"plus", store.MonthlyPlusQuotaLimitGroup, ""},
		{"pro", store.MonthlyPlusQuotaLimitGroup, ""},
		{"admin", "team-admin:a", "a"}, {"member", "", "a"}, {"other", "", "b"},
		{"blocked-free", "", ""}, {"blocked-plus", store.MonthlyPlusQuotaLimitGroup, ""}, {"blocked-team", "", "a"},
		{"blocked-pro", store.MonthlyPlusQuotaLimitGroup, ""},
	} {
		u := &store.User{Name: tc.name, Email: tc.name + "@example.test", Active: true, EmailVerified: true, ProxyUUID: "proxy-" + tc.name, Groups: []string{tc.group}}
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		if tc.name == "pro" || tc.name == "blocked-pro" {
			if err := st.UpsertAccountBillingProfile(ctx, &store.AccountBillingProfile{AccountUUID: u.ID, PackageName: "pro", IncludedQuotaBytes: 50 << 30}); err != nil {
				t.Fatal(err)
			}
			start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
			end := start.AddDate(0, 1, 0)
			if err := st.UpsertAccountQuotaState(ctx, &store.AccountQuotaState{AccountUUID: u.ID, RemainingIncludedQuota: 50 << 30, PeriodStart: &start, PeriodEnd: &end, SuspendState: "active"}); err != nil {
				t.Fatal(err)
			}
		}
		if tc.team != "" {
			if err := teams.PutNetworkMembership(ctx, store.NetworkMembership{UserID: u.ID, DedicatedOnly: true}, &store.AuditLog{}); err != nil {
				t.Fatal(err)
			}
			if err := teams.PutNetworkMembership(ctx, store.NetworkMembership{UserID: u.ID, TeamID: tc.team, DedicatedOnly: true}, &store.AuditLog{}); err != nil {
				t.Fatal(err)
			}
		}
		if strings.HasPrefix(tc.name, "blocked-") {
			if err := st.AddToBlacklist(ctx, u.Email); err != nil {
				t.Fatal(err)
			}
		}
	}
	h := &handler{store: st}
	for _, tc := range []struct {
		class        string
		groups, want []string
	}{
		{"shared", []string{"resource:shared"}, []string{"free@example.test", "plus@example.test", "pro@example.test"}},
		{"free", []string{"resource:free"}, []string{"free@example.test", "plus@example.test", "pro@example.test"}},
		{"plus", []string{"resource:plus"}, []string{"plus@example.test", "pro@example.test"}},
		{"pro", []string{"resource:pro"}, []string{"pro@example.test"}},
		{"team", []string{"team:a"}, []string{"admin@example.test", "member@example.test"}},
	} {
		t.Run(tc.class, func(t *testing.T) {
			clients, _, err := h.authorizedAgentClients(ctx, tc.groups)
			if err != nil {
				t.Fatal(err)
			}
			got := []string{}
			for _, c := range clients {
				got = append(got, c.Email)
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("clients=%v want=%v", got, tc.want)
			}
		})
	}
}
