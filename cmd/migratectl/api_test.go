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
