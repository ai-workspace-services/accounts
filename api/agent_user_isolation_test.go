package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/store"
	"github.com/gin-gonic/gin"
)

// Exercise the actual polling, event-stream and status routes with the same
// credentials. A malformed dedicated scope must never become public access,
// including when the request uses the credential's own Agent ID.
func TestAgentUserIsolationAcrossRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()
	teamStore := st.(store.NetworkTeamStore)
	userIDs := map[string]string{}
	for _, name := range []string{"public", "alpha", "beta", "expired", "inactive", "removed"} {
		user := &store.User{Name: name, Email: name + "@example.invalid", Role: store.RoleUser, Active: true, EmailVerified: true, ProxyUUID: "proxy-" + name}
		if err := st.CreateUser(ctx, user); err != nil {
			t.Fatal(err)
		}
		userIDs[name] = user.ID
		if name == "public" {
			continue
		}
		validUntil := time.Now().Add(time.Hour)
		if name == "expired" {
			validUntil = time.Now().Add(-time.Minute)
		}
		if err := teamStore.PutNetworkTeam(ctx, store.NetworkTeam{ID: name, Name: name, MemberLimit: 10, Active: name != "inactive", ValidFrom: time.Now().Add(-time.Hour), ValidUntil: validUntil}, &store.AuditLog{}); err != nil {
			t.Fatal(err)
		}
		for _, teamID := range []string{"", name} {
			if name == "removed" && teamID != "" {
				continue
			}
			if err := teamStore.PutNetworkMembership(ctx, store.NetworkMembership{UserID: user.ID, TeamID: teamID, DedicatedOnly: true}, &store.AuditLog{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	cases := []struct {
		id     string
		groups []string
		status int
		email  string
	}{
		{"public", nil, 200, "public@example.invalid"},
		{"alpha", []string{"team:alpha", "access:dedicated-only"}, 200, "alpha@example.invalid"},
		{"beta", []string{"team:beta"}, 200, "beta@example.invalid"},
		{"expired", []string{"team:expired"}, 200, ""},
		{"inactive", []string{"team:inactive"}, 200, ""},
		{"removed", []string{"team:removed"}, 200, ""},
		{"unbound", []string{"access:dedicated-only"}, 403, ""},
		{"ambiguous", []string{"team:alpha", "team:beta"}, 403, ""},
		{"user-role", []string{"team-admin:alpha"}, 403, ""},
		{"wrong-case", []string{"TEAM:alpha"}, 403, ""},
		{"wrong-marker", []string{"Access:Dedicated-Only"}, 403, ""},
	}
	credentials := make([]agentserver.Credential, 0, len(cases))
	for _, tc := range cases {
		credentials = append(credentials, agentserver.Credential{ID: tc.id, Token: tc.id + "-token", Groups: tc.groups})
	}
	registry, err := agentserver.NewRegistry(agentserver.Config{Credentials: credentials})
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithAgentRegistry(registry))
	server := httptest.NewServer(router)
	defer server.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			revision := ""
			for _, path := range []string{"/users", "/users/events", "/status"} {
				func() {
					method, body := http.MethodGet, ""
					if path == "/status" {
						method, body = http.MethodPost, `{"agentId":"`+tc.id+`","healthy":true}`
					}
					req, err := http.NewRequest(method, server.URL+"/api/agent-server/v1"+path, strings.NewReader(body))
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Authorization", "Bearer "+tc.id+"-token")
					req.Header.Set("X-Agent-ID", tc.id)
					req.Header.Set("Content-Type", "application/json")
					resp, err := client.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					wantStatus := tc.status
					if path == "/status" && tc.status == http.StatusOK {
						wantStatus = http.StatusNoContent
					}
					if resp.StatusCode != wantStatus {
						t.Errorf("%s: got HTTP %d, want %d", path, resp.StatusCode, wantStatus)
						return
					}
					if tc.status != http.StatusOK || path == "/status" {
						return
					}
					if path == "/users" {
						var payload agentproto.ClientListResponse
						if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
							t.Fatal(err)
						}
						wantCount := 0
						if tc.email != "" {
							wantCount = 1
						}
						if payload.Total != wantCount || len(payload.Clients) != wantCount || (wantCount > 0 && payload.Clients[0].Email != tc.email) {
							t.Fatalf("cross-scope client list: %+v", payload)
						}
						revision = payload.Revision
						if revision == "" {
							t.Fatal("missing revision")
						}
						return
					}
					// The event must describe this credential's client list, rather
					// than the public or another team's revision.
					scanner := bufio.NewScanner(resp.Body)
					for scanner.Scan() {
						if strings.HasPrefix(scanner.Text(), "data: ") {
							if got := strings.TrimPrefix(scanner.Text(), "data: "); got != revision {
								t.Fatalf("event revision %q differs from scoped list %q", got, revision)
							}
							return
						}
					}
					t.Fatalf("stream ended without scoped revision: %v", scanner.Err())
				}()
			}
		})
	}
	// Changing the header, query or report body cannot select another scope.
	for _, pair := range [][2]string{{"public", "alpha"}, {"alpha", "beta"}, {"alpha", "public"}} {
		for _, path := range []string{"/users", "/users/events", "/status"} {
			for _, selector := range []string{"header", "query"} {
				method, body, query := http.MethodGet, "", ""
				if path == "/status" {
					method, body = http.MethodPost, `{"agentId":"`+pair[1]+`","healthy":true}`
				}
				if selector == "query" {
					query = "?agentId=" + pair[1]
				}
				req := httptest.NewRequest(method, "/api/agent-server/v1"+path+query, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+pair[0]+"-token")
				req.Header.Set("Content-Type", "application/json")
				if selector == "header" {
					req.Header.Set("X-Agent-ID", pair[1])
				}
				rr := httptest.NewRecorder()
				router.ServeHTTP(rr, req)
				if rr.Code != http.StatusForbidden {
					t.Fatalf("%s -> %s via %s %s: HTTP %d", pair[0], pair[1], selector, path, rr.Code)
				}
			}
		}
	}
	t.Run("remove-member-on-open-stream", func(t *testing.T) {
		streamCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, server.URL+"/api/agent-server/v1/users/events", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer alpha-token")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream: HTTP %d", resp.StatusCode)
		}
		scanner := bufio.NewScanner(resp.Body)
		readRevision := func() string {
			t.Helper()
			for scanner.Scan() {
				if strings.HasPrefix(scanner.Text(), "data: ") {
					return strings.TrimPrefix(scanner.Text(), "data: ")
				}
			}
			t.Fatalf("stream ended before revocation event: %v", scanner.Err())
			return ""
		}
		initial := readRevision()
		if err := teamStore.PutNetworkMembership(ctx, store.NetworkMembership{UserID: userIDs["alpha"], DedicatedOnly: true}, &store.AuditLog{}); err != nil {
			t.Fatal(err)
		}
		if next := readRevision(); next == initial {
			t.Fatal("member removal did not change the existing stream's revision")
		}
		h := &handler{store: st}
		for _, groups := range [][]string{nil, {"team:alpha"}, {"team:beta"}} {
			clients, _, err := h.authorizedAgentClients(ctx, groups)
			if err != nil {
				t.Fatal(err)
			}
			for _, client := range clients {
				if client.Email == "alpha@example.invalid" {
					t.Fatalf("removed member still distributed in scope %v", groups)
				}
			}
		}
	})
}

func TestAgentClientsRequireNetworkTeamStore(t *testing.T) {
	// Hide the optional team interface to represent an unsupported store.
	h := &handler{store: struct{ store.Store }{store.NewMemoryStore()}}
	clients, revision, err := h.authorizedAgentClients(context.Background(), nil)
	if err == nil || len(clients) != 0 || revision != "" {
		t.Fatalf("unsupported store must fail closed: clients=%v revision=%q err=%v", clients, revision, err)
	}
}
