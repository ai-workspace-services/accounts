package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"account/internal/store"
)

func TestOverlayDeviceLifecycleAPIRejectsSecretsAndRemovesRevokedDevice(t *testing.T) {
	router, st, token := policyAPIHarness(t)
	if rec := policyRequest(t, router, token, http.MethodPost, "/api/overlay/v1/devices/dev-a/rotate-key", `{"network_id":"net","wireguard_public_key":"AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=","expected_key_version":1,"private_key":"secret"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("private key accepted: %d %s", rec.Code, rec.Body.String())
	}
	rotate := policyRequest(t, router, token, http.MethodPost, "/api/overlay/v1/devices/dev-a/rotate-key", `{"network_id":"net","wireguard_public_key":"AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=","expected_key_version":1}`)
	if rotate.Code != http.StatusOK || !strings.Contains(rotate.Body.String(), `"key_version":2`) || strings.Contains(rotate.Body.String(), "private") {
		t.Fatalf("rotate=%d %s", rotate.Code, rotate.Body.String())
	}
	revoke := policyRequest(t, router, token, http.MethodPost, "/api/overlay/v1/devices/dev-a/revoke", `{"network_id":"net","expected_state_version":2}`)
	if revoke.Code != http.StatusOK || !strings.Contains(revoke.Body.String(), `"status":"revoked"`) {
		t.Fatalf("revoke=%d %s", revoke.Code, revoke.Body.String())
	}
	retry := policyRequest(t, router, token, http.MethodPost, "/api/overlay/v1/devices/dev-a/revoke", `{"network_id":"net","expected_state_version":2}`)
	if retry.Code != http.StatusOK || !strings.Contains(retry.Body.String(), `"duplicate":true`) {
		t.Fatalf("revoke retry=%d %s", retry.Code, retry.Body.String())
	}
	device, err := st.GetOverlayDevice(t.Context(), policyOwnerID(t, st), "dev-a")
	if err != nil || device.Status != store.OverlayDeviceRevoked {
		t.Fatalf("stored device=%+v err=%v", device, err)
	}
	events := policyRequest(t, router, token, http.MethodGet, "/api/overlay/v1/events?network_id=net", "")
	if events.Code != http.StatusOK || !strings.Contains(events.Body.String(), `"type":"revoked"`) {
		t.Fatalf("events=%d %s", events.Code, events.Body.String())
	}
}

func TestPolicyReconcileOutboxCanBeRetriedByServiceBoundary(t *testing.T) {
	t.Setenv("INTERNAL_SERVICE_TOKEN", "internal-test")
	router, st, _ := policyAPIHarness(t)
	if err := st.MarkOverlayPolicyReconcilePending(t.Context(), "net", "transient"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/internal/overlay/v1/reconcile-pending", nil)
	req.Header.Set("X-Service-Token", "internal-test")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Body.String(), `"completed":1`) {
		t.Fatalf("reconcile=%d %s", rec.Code, rec.Body.String())
	}
	pending, err := st.ListOverlayPolicyReconcilePending(t.Context(), 100)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
}

func policyOwnerID(t *testing.T, st store.Store) string {
	t.Helper()
	user, err := st.GetUserByEmail(t.Context(), "policy@example.com")
	if err != nil {
		t.Fatal(err)
	}
	return user.ID
}
