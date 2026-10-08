package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"account/internal/agentserver"
	"account/internal/store"
)

func TestAgentEventStreamSurvivesServerWriteTimeout(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		name := "http1"
		if http2 {
			name = "http2"
		}
		t.Run(name, func(t *testing.T) {
			registry, err := agentserver.NewRegistry(agentserver.Config{Credentials: []agentserver.Credential{{ID: "test", Token: "test-agent-token"}}})
			if err != nil {
				t.Fatal(err)
			}
			st := store.NewMemoryStore()
			router, _, _ := newAuthenticatedSyncHarness(t, WithStore(st), WithAgentRegistry(registry))
			server := httptest.NewUnstartedServer(router)
			server.Config.WriteTimeout = 250 * time.Millisecond
			server.EnableHTTP2 = http2
			if http2 {
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/agent-server/v1/users/events", nil)
			req.Header.Set("Authorization", "Bearer test-agent-token")
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK || (http2 && resp.ProtoMajor != 2) {
				t.Fatalf("stream response: %s %s", resp.Proto, resp.Status)
			}
			scanner := bufio.NewScanner(resp.Body)
			readRevision := func() string {
				t.Helper()
				for scanner.Scan() {
					if strings.HasPrefix(scanner.Text(), "data: ") {
						return strings.TrimPrefix(scanner.Text(), "data: ")
					}
				}
				t.Fatalf("stream ended before revision: %v", scanner.Err())
				return ""
			}
			initial := readRevision()
			// Wait beyond the ordinary request deadline, then require another
			// event on the same connection, not just a successful initial flush.
			time.Sleep(500 * time.Millisecond)
			if err := st.CreateUser(ctx, &store.User{Name: "new client", Email: "new@example.test", Active: true, EmailVerified: true, ProxyUUID: "test-client"}); err != nil {
				t.Fatal(err)
			}
			if next := readRevision(); next == initial {
				t.Fatal("new client did not change revision")
			}
		})
	}
}
