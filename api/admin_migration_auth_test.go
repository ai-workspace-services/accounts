package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMigrationAccountsAdministratorAuthorization(t *testing.T) {
	h := newActiveHarness(t)
	call := func(token, scope string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/migration/authorize", bytes.NewBufferString(`{"scope":"`+scope+`"}`))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.router.ServeHTTP(w, req)
		return w
	}
	for _, scope := range []string{"source.read", "target.plan", "target.apply"} {
		w := call(h.adminToken, scope)
		if w.Code != 200 {
			t.Fatalf("admin scope %s: %d", scope, w.Code)
		}
		var result map[string]string
		if json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result["operator_id"]) != 64 || result["scope"] != scope {
			t.Fatal("invalid authorization response")
		}
		if w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), h.adminToken) || strings.Contains(w.Body.String(), "admin@example.com") {
			t.Fatal("sensitive/cached authorization response")
		}
	}
	if call("", "source.read").Code != 401 || call("invalid-token", "source.read").Code != 401 {
		t.Fatal("missing/invalid token accepted")
	}
	if call(h.adminToken, "unknown").Code != 400 {
		t.Fatal("unknown permission accepted")
	}
	userToken := loginFor(t, h.router, h.target.Email, h.password)
	if call(userToken, "source.read").Code != 403 {
		t.Fatal("ordinary user can export")
	}
	// The same previously accepted credential is rechecked after account disable.
	admin, err := h.store.GetUserByEmail(context.Background(), "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	admin.Active = false
	if err = h.store.UpdateUser(context.Background(), admin); err != nil {
		t.Fatal(err)
	}
	if call(h.adminToken, "target.apply").Code != 403 {
		t.Fatal("disabled administrator accepted")
	}
	access, err := h.tokenService.GenerateTokenPair(h.root.ID, h.root.Email, []string{"root"})
	if err != nil {
		t.Fatal(err)
	}
	if call(access.AccessToken, "source.read").Code != 200 {
		t.Fatal("existing Accounts access JWT refused")
	}
	h.root.Active = false
	if err = h.store.UpdateUser(context.Background(), h.root); err != nil {
		t.Fatal(err)
	}
	if call(access.AccessToken, "source.read").Code != 403 {
		t.Fatal("JWT claims bypassed current user state")
	}
	h.root.Active = true
	if err = h.store.UpdateUser(context.Background(), h.root); err != nil {
		t.Fatal(err)
	}
	rootToken := loginFor(t, h.router, h.root.Email, h.password)
	if call(rootToken, "target.plan").Code != 200 {
		t.Fatal("root token refused")
	}
	if err = h.store.DeleteSession(context.Background(), rootToken); err != nil {
		t.Fatal(err)
	}
	if call(rootToken, "target.plan").Code != 401 {
		t.Fatal("revoked session accepted")
	}
}
