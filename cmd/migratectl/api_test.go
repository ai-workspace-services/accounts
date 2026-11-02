package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMigrationAPICLIContract(t *testing.T) {
	for _, args := range [][]string{
		{"--source-token", "plaintext-is-not-allowed"},
		{"--target-token", "plaintext-is-not-allowed"},
		{"--apply"},
		{"--apply", "--status"},
		{"--source-url", "https://source.example.invalid", "--target-url", "https://target.example.invalid", "--source-token-env", "SOURCE", "--target-token-env", "TARGET", "--target-auth-kv", "kv/fixture"},
	} {
		cmd := newMigrationAPICmd()
		out := new(bytes.Buffer)
		cmd.SetOut(out)
		cmd.SetErr(out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("invalid flags accepted: %v", args)
		}
		if strings.Contains(out.String(), "plaintext-is-not-allowed") {
			t.Fatal("plaintext flag value echoed")
		}
	}
	t.Setenv("MIGRATION_API_SOURCE_TOKEN", "fixture-source-token")
	t.Setenv("MIGRATION_API_TARGET_TOKEN", "fixture-target-token")
	requests := 0
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "POST" || r.URL.Path != "/api/migrations/v1/snapshots" {
			t.Error("default did not start source preview snapshot")
		}
		w.WriteHeader(404)
	}))
	defer endpoint.Close()
	cmd := newMigrationAPICmd()
	cmd.SetArgs([]string{"--source-url", endpoint.URL, "--target-url", "http://127.0.0.1:1", "--source-token-env", "MIGRATION_API_SOURCE_TOKEN", "--target-token-env", "MIGRATION_API_TARGET_TOKEN", "--allow-loopback-http"})
	out := new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(out)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "unsupported migration endpoint; no fallback") || requests != 1 {
		t.Fatal("unsupported endpoint contract", err, requests)
	}
	if strings.Contains(out.String(), "fixture-source-token") || strings.Contains(out.String(), "fixture-target-token") {
		t.Fatal("credentials leaked")
	}
}

func TestMigrationServeAuthenticationModeSelection(t *testing.T) {
	cmd := newMigrationServeCmd()
	cmd.SetArgs([]string{"--accounts-auth-url", "https://accounts.example.invalid", "--auth-key-env", "UNUSED_SIGNING_KEY"})
	out := new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(out)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatal("ambiguous auth mode accepted", err)
	}
}

func TestCatchupApplyRequiresExactReviewAndNoSourceCredential(t *testing.T) {
	t.Setenv("FIXTURE_TARGET", "synthetic-target-token")
	t.Setenv("FIXTURE_SOURCE", "synthetic-source-token")
	for _, args := range [][]string{
		{"--apply", "--target-token-env", "FIXTURE_TARGET"},
		{"--apply", "--target-token-env", "FIXTURE_TARGET", "--request-id", "11111111-1111-4111-8111-111111111111", "--report-sha256", strings.Repeat("a", 64), "--source-token-env", "FIXTURE_SOURCE"},
		{"--target-token-env", "FIXTURE_TARGET", "--report-sha256", strings.Repeat("a", 64)},
	} {
		cmd := newMigrationCatchupCmd()
		out := new(bytes.Buffer)
		cmd.SetOut(out)
		cmd.SetErr(out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("invalid catchup arguments accepted: %v", args)
		}
		if strings.Contains(out.String(), "synthetic-target-token") || strings.Contains(out.String(), "synthetic-source-token") {
			t.Fatal("catchup credential leaked")
		}
	}
}
