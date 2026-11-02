package migrate

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAccountsMigrationAuthorizerDelegatesTokenLifetime(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/admin/migration/authorize" || r.Header.Get("Authorization") != "Bearer opaque-accounts-session" {
			t.Error("credential transport changed")
		}
		w.WriteHeader(int(status.Load()))
		if status.Load() == 200 {
			fmt.Fprintf(w, `{"operator_id":"%s","scope":"source.read"}`, strings.Repeat("a", 64))
		}
	}))
	defer server.Close()
	authorize, err := NewAccountsMigrationAuthorizer(server.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	subject, code := authorize(context.Background(), "opaque-accounts-session", "source.read")
	if code != 0 || len(subject) != 64 {
		t.Fatal("Accounts token refused")
	}
	status.Store(401)
	if _, code = authorize(context.Background(), "opaque-accounts-session", "source.read"); code != 401 {
		t.Fatal("Accounts revocation ignored")
	}
	if calls.Load() != 2 {
		t.Fatal("authorization cached")
	}
	status.Store(200)
	if _, code = authorize(context.Background(), "opaque-accounts-session", "target.apply"); code != 503 {
		t.Fatal("wrong scope response accepted")
	}
	if _, err = NewAccountsMigrationAuthorizer("http://192.0.2.1", true); err == nil {
		t.Fatal("non-loopback plaintext accepted")
	}
}

func TestAccountsMigrationAuthorizerRefusesRedirects(t *testing.T) {
	reached := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, http.StatusFound) }))
	defer source.Close()
	authorize, err := NewAccountsMigrationAuthorizer(source.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, code := authorize(context.Background(), "do-not-forward", "source.read"); code != 503 || reached {
		t.Fatal("token followed redirect")
	}
}

func TestAccountsMigrationAllowsDistinctAuthorizedActors(t *testing.T) {
	s := &migrationServer{MigrationServerOptions: MigrationServerOptions{SourceAudience: "source", TargetAudience: "target", JobID: "approved-job"}}
	actual := s.authorization("source-administrator")
	if s.matchesSourceAuthorization(actual, "target-administrator") {
		t.Fatal("legacy same-subject gate changed")
	}
	s.AccountsAuthorize = func(context.Context, string, string) (string, int) { return "", 401 }
	if !s.matchesSourceAuthorization(actual, "target-administrator") {
		t.Fatal("independent Accounts administrators rejected")
	}
	actual.JobID = "different-job"
	if s.matchesSourceAuthorization(actual, "target-administrator") {
		t.Fatal("different job accepted")
	}
	actual = s.authorization("source-administrator")
	actual.TargetAudience = "wrong-target"
	if s.matchesSourceAuthorization(actual, "target-administrator") {
		t.Fatal("wrong target accepted")
	}
}
