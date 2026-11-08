package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/store"
)

func TestAdminAgentFreeGrants(t *testing.T) {
	gin.SetMode(gin.TestMode)

	now := time.Now().UTC().Truncate(time.Millisecond)
	st := store.NewMemoryStore()

	testPass := "scrubbed-pass-123"
	hashed, err := bcrypt.GenerateFromPassword([]byte(testPass), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}

	adminUser := &store.User{
		ID:            "admin-grant-1",
		Name:          "Admin Grant",
		Email:         "admin-grant@example.com",
		PasswordHash:  string(hashed),
		EmailVerified: true,
		Role:          store.RoleAdmin,
	}
	if err := st.CreateUser(context.Background(), adminUser); err != nil {
		t.Fatalf("failed to seed admin user: %v", err)
	}

	regularUser := &store.User{
		ID:            "target-user-1",
		Name:          "Target User",
		Email:         "target@example.com",
		PasswordHash:  string(hashed),
		EmailVerified: true,
		Role:          store.RoleUser,
	}
	if err := st.CreateUser(context.Background(), regularUser); err != nil {
		t.Fatalf("failed to seed target user: %v", err)
	}

	operatorUser := &store.User{
		ID:            "operator-grant-1",
		Name:          "Operator Grant",
		Email:         "operator-grant@example.com",
		PasswordHash:  string(hashed),
		EmailVerified: true,
		Role:          store.RoleOperator,
		Permissions:   []string{permissionAdminAgentsStatus},
	}
	if err := st.CreateUser(context.Background(), operatorUser); err != nil {
		t.Fatalf("failed to seed operator user: %v", err)
	}

	const (
		nodeID       = "jp-edge-custom-01"
		credentialID = "custom-credential"
		legacyNodeID = "legacy-custom-node"
		plusNodeID   = "plus-node"
	)
	registry, err := agentserver.NewRegistry(agentserver.Config{Credentials: []agentserver.Credential{
		{ID: credentialID, NodeID: nodeID, Token: "test-custom-token", Groups: []string{"resource:custom"}, AllowedEmails: []string{"enrolled@example.com"}},
		{ID: "*", Token: "test-shared-token", Groups: []string{"resource:shared"}},
		{ID: legacyNodeID, Token: "test-legacy-token", Groups: []string{"resource:custom"}},
		{ID: plusNodeID, NodeID: plusNodeID, Token: "test-plus-token", Groups: []string{"resource:plus"}},
	}})
	if err != nil {
		t.Fatalf("failed to configure agent registry: %v", err)
	}
	registry.ReportStatus(agentserver.Identity{
		ID: legacyNodeID, NodeID: legacyNodeID, Groups: []string{"resource:custom"},
	}, agentproto.StatusReport{})
	registry.ReportStatus(agentserver.Identity{
		ID: plusNodeID, NodeID: plusNodeID, Groups: []string{"resource:custom"},
	}, agentproto.StatusReport{})

	reader := stubAgentStatusReader{
		statuses: []agentserver.StatusSnapshot{
			{
				Agent: agentserver.Identity{
					ID:     credentialID,
					NodeID: nodeID,
					Groups: []string{"resource:custom"},
				},
				Report: agentproto.StatusReport{
					Role:    "agent-proxy",
					Healthy: true,
					Xray: agentproto.XrayStatus{
						Running:  true,
						LastSync: &now,
					},
				},
				UpdatedAt: now,
			},
		},
	}

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithAgentRegistry(registry), WithAgentStatusReader(reader), WithEmailVerification(false))

	loginAs := func(router *gin.Engine, email string) string {
		payload, _ := json.Marshal(map[string]string{"identifier": email, "password": testPass})
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("login failed for %s: %d, body: %s", email, rec.Code, rec.Body.String())
		}
		var resp struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode login token: %v", err)
		}
		return resp.Token
	}

	adminToken := loginAs(router, adminUser.Email)
	regularToken := loginAs(router, regularUser.Email)
	operatorToken := loginAs(router, operatorUser.Email)

	// The second router represents a server after the credential was removed.
	// A previous status report may still be available for display.
	routerWithoutEnrollment := gin.New()
	RegisterRoutes(routerWithoutEnrollment, WithStore(st), WithAgentStatusReader(reader), WithEmailVerification(false))
	adminTokenWithoutEnrollment := loginAs(routerWithoutEnrollment, adminUser.Email)

	t.Run("grant creation requires canonical Custom credential binding", func(t *testing.T) {
		for _, tc := range []struct {
			name, nodeID, token string
			router              *gin.Engine
		}{
			{"stale status without registry", nodeID, adminTokenWithoutEnrollment, routerWithoutEnrollment},
			{"credential ID alias", credentialID, adminToken, router},
			{"wildcard credential", "*", adminToken, router},
			{"legacy credential and spoofed heartbeat", legacyNodeID, adminToken, router},
			{"Plus credential and spoofed Custom heartbeat", plusNodeID, adminToken, router},
			{"unregistered node", "unknown-node", adminToken, router},
		} {
			t.Run(tc.name, func(t *testing.T) {
				body, _ := json.Marshal(map[string]any{
					"nodeId":        tc.nodeID,
					"accountId":     regularUser.ID,
					"expiresInDays": 3,
					"reason":        "verify grant binding",
				})
				req := httptest.NewRequest(http.MethodPost, "/api/auth/admin/agents/grants", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+tc.token)
				rec := httptest.NewRecorder()
				tc.router.ServeHTTP(rec, req)
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("expected 400 for unbound grant target, got %d: %s", rec.Code, rec.Body.String())
				}
			})
		}
	})

	t.Run("admin can create temporary Free grant", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"nodeId":        nodeID,
			"accountId":     regularUser.ID,
			"expiresInDays": 3,
			"reason":        "Temporary Free trial approved for customer review",
		})
		req := httptest.NewRequest(http.MethodPost, "/api/auth/admin/agents/grants", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+adminToken)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on grant create, got %d: %s", rec.Code, rec.Body.String())
		}

		var resp struct {
			Message string                   `json:"message"`
			Grant   adminAgentFreeGrantEntry `json:"grant"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to unmarshal grant create response: %v", err)
		}

		if resp.Grant.NodeID != nodeID {
			t.Errorf("expected nodeId %q, got %q", nodeID, resp.Grant.NodeID)
		}
		if resp.Grant.AccountID != regularUser.ID {
			t.Errorf("expected accountId %q, got %q", regularUser.ID, resp.Grant.AccountID)
		}
		if resp.Grant.Status != store.AgentFreeGrantStatusActive {
			t.Errorf("expected status %q, got %q", store.AgentFreeGrantStatusActive, resp.Grant.Status)
		}
		// Before edge node syncs, state is pending
		if resp.Grant.State != "pending" {
			t.Errorf("expected computed state 'pending' before sync, got %q", resp.Grant.State)
		}
		if resp.Grant.ActorID != adminUser.ID {
			t.Errorf("expected actorId %q, got %q", adminUser.ID, resp.Grant.ActorID)
		}
	})

	t.Run("operator can list but cannot create without write permission", func(t *testing.T) {
		// List succeeds
		reqList := httptest.NewRequest(http.MethodGet, "/api/auth/admin/agents/grants", nil)
		reqList.Header.Set("Authorization", "Bearer "+operatorToken)
		recList := httptest.NewRecorder()
		router.ServeHTTP(recList, reqList)
		if recList.Code != http.StatusOK {
			t.Errorf("expected operator user to list grants, got %d: %s", recList.Code, recList.Body.String())
		}

		// Create fails
		body, _ := json.Marshal(map[string]any{
			"nodeId":        nodeID,
			"accountId":     regularUser.ID,
			"expiresInDays": 1,
			"reason":        "unauthorized test",
		})
		reqCreate := httptest.NewRequest(http.MethodPost, "/api/auth/admin/agents/grants", bytes.NewReader(body))
		reqCreate.Header.Set("Content-Type", "application/json")
		reqCreate.Header.Set("Authorization", "Bearer "+operatorToken)
		recCreate := httptest.NewRecorder()
		router.ServeHTTP(recCreate, reqCreate)
		if recCreate.Code != http.StatusForbidden {
			t.Errorf("expected 403 Forbidden for operator user on create, got %d", recCreate.Code)
		}
	})

	t.Run("regular user is denied access", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/auth/admin/agents/grants", nil)
		req.Header.Set("Authorization", "Bearer "+regularToken)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("expected 403 Forbidden for regular user, got %d", rec.Code)
		}
	})

	t.Run("admin can renew existing grant", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"nodeId":        nodeID,
			"accountId":     regularUser.ID,
			"expiresInDays": 4,
			"reason":        "Renewed grant for 4 more days",
		})
		req := httptest.NewRequest(http.MethodPut, "/api/auth/admin/agents/grants", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+adminToken)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on grant renew, got %d: %s", rec.Code, rec.Body.String())
		}

		var resp struct {
			Grant adminAgentFreeGrantEntry `json:"grant"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to unmarshal renew response: %v", err)
		}
		if resp.Grant.Reason != "Renewed grant for 4 more days" {
			t.Errorf("expected updated reason, got %q", resp.Grant.Reason)
		}
	})

	t.Run("removed node cannot renew but can revoke existing grant", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"nodeId":        nodeID,
			"accountId":     regularUser.ID,
			"expiresInDays": 3,
			"reason":        "verify renewal after removal",
		})
		req := httptest.NewRequest(http.MethodPut, "/api/auth/admin/agents/grants", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+adminTokenWithoutEnrollment)
		rec := httptest.NewRecorder()
		routerWithoutEnrollment.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 on renewal after removal, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("admin can revoke temporary Free grant after node removal", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"nodeId":    nodeID,
			"accountId": regularUser.ID,
			"reason":    "Revoking early as testing concluded",
		})
		req := httptest.NewRequest(http.MethodDelete, "/api/auth/admin/agents/grants", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+adminTokenWithoutEnrollment)
		rec := httptest.NewRecorder()
		routerWithoutEnrollment.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK on revoke, got %d: %s", rec.Code, rec.Body.String())
		}

		var resp struct {
			Grant adminAgentFreeGrantEntry `json:"grant"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to unmarshal revoke response: %v", err)
		}
		if resp.Grant.Status != store.AgentFreeGrantStatusRevoked {
			t.Errorf("expected status revoked, got %q", resp.Grant.Status)
		}
		if resp.Grant.RevokedAt == nil {
			t.Errorf("expected revokedAt to be set")
		}
	})

	t.Run("validation rejects invalid parameters", func(t *testing.T) {
		// TTL too large (> 5 days)
		body, _ := json.Marshal(map[string]any{
			"nodeId":        nodeID,
			"accountId":     regularUser.ID,
			"expiresInDays": 10,
			"reason":        "invalid ttl",
		})
		req := httptest.NewRequest(http.MethodPost, "/api/auth/admin/agents/grants", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+adminToken)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400 Bad Request for TTL > 5 days, got %d", rec.Code)
		}

		// Empty reason
		bodyEmptyReason, _ := json.Marshal(map[string]any{
			"nodeId":        nodeID,
			"accountId":     regularUser.ID,
			"expiresInDays": 2,
			"reason":        "",
		})
		reqEmpty := httptest.NewRequest(http.MethodPost, "/api/auth/admin/agents/grants", bytes.NewReader(bodyEmptyReason))
		reqEmpty.Header.Set("Content-Type", "application/json")
		reqEmpty.Header.Set("Authorization", "Bearer "+adminToken)
		recEmpty := httptest.NewRecorder()
		router.ServeHTTP(recEmpty, reqEmpty)
		if recEmpty.Code != http.StatusBadRequest {
			t.Errorf("expected 400 Bad Request for empty reason, got %d", recEmpty.Code)
		}
	})
}
