package dbruntime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func runtimeEnv(t *testing.T, role, background, dsn string) {
	t.Helper()
	for _, k := range []string{"DATABASE_RUNTIME_ROLE", "DATABASE_BACKGROUND_WRITERS", "DATABASE_URL", "DATABASE_IDENTITY_SHA256", "SUPABASE_CONNECT_URI", "SUPABASE_CONNECT_URL"} {
		t.Setenv(k, "")
	}
	t.Setenv("DATABASE_RUNTIME_ROLE", role)
	t.Setenv("DATABASE_BACKGROUND_WRITERS", background)
	t.Setenv("DATABASE_URL", dsn)
	identity, _ := ConnectionIdentity(dsn)
	t.Setenv("DATABASE_IDENTITY_SHA256", identity)
}

func TestDatabaseRoleRequiresExplicitReviewedConnectionAndNoAlias(t *testing.T) {
	dsn := "postgres://app:synthetic@localhost:5432/account?sslmode=disable"
	for _, role := range []string{Primary, Standby} {
		runtimeEnv(t, role, "false", dsn)
		cfg, actual, err := FromEnvironment()
		if err != nil || actual != dsn || cfg.Role != role || cfg.MayRunBackgroundWriters() {
			t.Fatal("valid paused role refused")
		}
		for _, key := range []string{"SUPABASE_CONNECT_URI", "SUPABASE_CONNECT_URL"} {
			t.Setenv(key, dsn)
			if _, _, err := FromEnvironment(); err == nil {
				t.Fatal("lingering Supabase alias accepted")
			}
			t.Setenv(key, "")
		}
		t.Setenv("DATABASE_IDENTITY_SHA256", strings.Repeat("0", 64))
		if _, _, err := FromEnvironment(); err == nil {
			t.Fatal("unreviewed database identity accepted")
		}
	}
}

func TestDatabaseRoleBackgroundAndSourcePrimaryRefused(t *testing.T) {
	dsn := "postgres://app:synthetic@localhost:5432/account?sslmode=disable"
	for _, v := range []struct{ role, bg, dsn string }{
		{Standby, "true", dsn}, {Primary, "", dsn}, {Primary, "yes", dsn}, {"invalid", "false", dsn},
		{Primary, "false", "postgres://readonly_release.fake:synthetic@aws-0-ap-southeast-1.pooler.supabase.com:5432/postgres?sslmode=require"},
		{Primary, "false", "postgres://app:synthetic@localhost:5432/postgres?sslmode=disable"},
	} {
		runtimeEnv(t, v.role, v.bg, v.dsn)
		if _, _, err := FromEnvironment(); err == nil {
			t.Fatal("invalid handover controls accepted")
		}
	}
	runtimeEnv(t, Primary, "true", dsn)
	cfg, _, err := FromEnvironment()
	if err != nil || !cfg.MayRunBackgroundWriters() {
		t.Fatal("explicit active primary refused")
	}
	for _, key := range []string{"DATABASE_BACKGROUND_WRITERS", "DATABASE_IDENTITY_SHA256"} {
		runtimeEnv(t, "", "", dsn)
		t.Setenv("DATABASE_IDENTITY_SHA256", "")
		t.Setenv(key, "false")
		if _, _, err := FromEnvironment(); err == nil {
			t.Fatal("unmanaged path silently accepted handover controls")
		}
	}
}

func TestDatabaseRoleStandbyBlocksEveryBusinessVerbAndOnlySafeProbes(t *testing.T) {
	called := 0
	image := func() map[string]any {
		return map[string]any{"image": "accounts:sha-" + strings.Repeat("a", 40), "commit": strings.Repeat("a", 40)}
	}
	cfg := Config{Role: Standby, ConnectionSHA256: strings.Repeat("b", 64)}
	h := cfg.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called++ }), image)
	for _, method := range []string{"GET", "HEAD", "OPTIONS", "POST", "PUT", "PATCH", "DELETE"} {
		for _, path := range []string{"/api/auth/login", "/api/users", "/v1/ingest/snapshots", "/v1/jobs/collect-and-rate", "/unknown"} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
			if w.Code != 503 || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("standby business route admitted")
			}
		}
	}
	for _, path := range []string{"/api/ping", "/healthz", "/readyz"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		var body map[string]any
		if json.Unmarshal(w.Body.Bytes(), &body) != nil || body["database_role"] != Standby || body["bootstrap_writes"] != false || body["background_writers"] != false || body["business_requests_enabled"] != false || body["schema_version"] != float64(0) {
			t.Fatal("invalid standby probe")
		}
		if path == "/readyz" && w.Code != 503 || path != "/readyz" && w.Code != 200 {
			t.Fatal("standby readiness/liveness differs")
		}
	}
	if called != 0 {
		t.Fatal("standby called business handler")
	}
}

func TestDatabaseRolePrimaryKeepsRealReadinessAndBusinessHandlers(t *testing.T) {
	cfg := Config{Role: Primary, SchemaVersion: 2026100701}
	called := 0
	h := cfg.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called++; w.WriteHeader(503) }), func() map[string]any { return map[string]any{} })
	for _, path := range []string{"/readyz", "/api/users"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 503 {
			t.Fatal("real readiness/handler result hidden")
		}
	}
	if called != 2 {
		t.Fatal("primary bypassed handler")
	}
	if !(Config{}).MayRunBackgroundWriters() {
		t.Fatal("unmanaged path behavior changed")
	}
}
