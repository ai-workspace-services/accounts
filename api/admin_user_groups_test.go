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

	"account/internal/store"
)

func TestUpdateUserGroups(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	st := store.NewMemoryStore()
	RegisterRoutes(router, WithStore(st), WithEmailVerification(false))

	testPass := "scrubbed"
	hashed, err := bcrypt.GenerateFromPassword([]byte(testPass), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}

	admin := &store.User{
		ID:            "admin-1",
		Name:          "administrator",
		Email:         "admin@example.com",
		PasswordHash:  string(hashed),
		EmailVerified: true,
		Role:          store.RoleAdmin,
	}
	if err := st.CreateUser(context.Background(), admin); err != nil {
		t.Fatalf("failed to seed admin user: %v", err)
	}

	target := &store.User{
		ID:            "user-1",
		Name:          "existing user",
		Email:         "existing@example.com",
		PasswordHash:  string(hashed),
		EmailVerified: true,
		Role:          store.RoleUser,
	}
	if err := st.CreateUser(context.Background(), target); err != nil {
		t.Fatalf("failed to seed target user: %v", err)
	}

	rootUser := &store.User{
		ID:            "root-1",
		Name:          "root",
		Email:         "root@example.com",
		PasswordHash:  string(hashed),
		EmailVerified: true,
		Role:          store.RoleRoot,
	}
	if err := st.CreateUser(context.Background(), rootUser); err != nil {
		t.Fatalf("failed to seed root user: %v", err)
	}

	loginPayload := map[string]string{"identifier": admin.Email, "password": testPass}
	body, err := json.Marshal(loginPayload)
	if err != nil {
		t.Fatalf("failed to marshal login payload: %v", err)
	}
	loginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	loginReq.Header.Set("Content-Type", "application/json")
	loginRec := httptest.NewRecorder()
	router.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("expected admin login success, got %d: %s", loginRec.Code, loginRec.Body.String())
	}
	token := decodeResponse(t, loginRec).Token
	if token == "" {
		t.Fatalf("expected session token from admin login response")
	}

	putGroups := func(userID string, groups []string) *httptest.ResponseRecorder {
		payload, err := json.Marshal(map[string]any{"groups": groups})
		if err != nil {
			t.Fatalf("failed to marshal groups payload: %v", err)
		}
		req := httptest.NewRequest(http.MethodPut, "/api/auth/admin/users/"+userID+"/groups", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	t.Run("sets segment tags on an existing user", func(t *testing.T) {
		rec := putGroups(target.ID, []string{"segment:subscribed", "segment:beta", "segment:subscribed"})
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}

		var payload struct {
			User struct {
				Groups []string `json:"groups"`
			} `json:"user"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if got, want := payload.User.Groups, []string{"segment:subscribed", "segment:beta"}; !equalStrings(got, want) {
			t.Fatalf("expected deduped groups %v, got %v", want, got)
		}

		stored, err := st.GetUserByID(context.Background(), target.ID)
		if err != nil {
			t.Fatalf("failed to reload user: %v", err)
		}
		if !equalStrings(stored.Groups, []string{"segment:subscribed", "segment:beta"}) {
			t.Fatalf("expected persisted groups to match, got %v", stored.Groups)
		}
	})

	t.Run("empty groups clears existing tags", func(t *testing.T) {
		rec := putGroups(target.ID, []string{})
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		stored, err := st.GetUserByID(context.Background(), target.ID)
		if err != nil {
			t.Fatalf("failed to reload user: %v", err)
		}
		if len(stored.Groups) != 0 {
			t.Fatalf("expected groups cleared, got %v", stored.Groups)
		}
	})

	t.Run("root account groups cannot be modified", func(t *testing.T) {
		rec := putGroups(rootUser.ID, []string{"segment:operations"})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for root account, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("batch updates selected users", func(t *testing.T) {
		payload, err := json.Marshal(map[string]any{
			"updates": []map[string]any{{
				"userId": target.ID,
				"groups": []string{store.MonthlyPlusQuotaLimitGroup, "segment:beta"},
			}},
		})
		if err != nil {
			t.Fatalf("failed to marshal batch payload: %v", err)
		}
		req := httptest.NewRequest(http.MethodPut, "/api/auth/admin/users/groups/batch", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		stored, err := st.GetUserByID(context.Background(), target.ID)
		if err != nil {
			t.Fatalf("failed to reload batch-updated user: %v", err)
		}
		if !equalStrings(stored.Groups, []string{store.MonthlyPlusQuotaLimitGroup, "segment:beta"}) {
			t.Fatalf("expected batch groups to persist, got %v", stored.Groups)
		}
	})

	t.Run("expired validity downgrades without deleting user", func(t *testing.T) {
		past := time.Now().UTC().AddDate(0, 0, -2).Format("2006-01-02")
		payload, err := json.Marshal(map[string]any{"validUntil": past})
		if err != nil {
			t.Fatalf("failed to marshal validity payload: %v", err)
		}
		req := httptest.NewRequest(http.MethodPut, "/api/auth/admin/users/"+target.ID+"/subscription-validity", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		stored, err := st.GetUserByID(context.Background(), target.ID)
		if err != nil {
			t.Fatalf("expected expired user to remain, got: %v", err)
		}
		if store.MonthlyQuotaGroup(stored) != store.MonthlyFreeQuotaLimitGroup {
			t.Fatalf("expected expired user to downgrade to Free 5GB, got groups %v", stored.Groups)
		}
		if stored.SubscriptionValidUntil == nil || stored.SubscriptionValidUntil.Format("2006-01-02") != past {
			t.Fatalf("expected validity to persist, got %#v", stored.SubscriptionValidUntil)
		}
	})

	t.Run("unknown user returns 404", func(t *testing.T) {
		rec := putGroups("does-not-exist", []string{"segment:beta"})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for unknown user, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
