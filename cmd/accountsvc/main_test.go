package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"account/config"
	"account/internal/store"
)

func TestBillingSchemaStatementsCoverSharedAccountingControlPlane(t *testing.T) {
	t.Parallel()

	statements := strings.ToLower(strings.Join(billingSchemaStatements(), "\n"))
	for _, table := range []string{
		"traffic_stat_checkpoints",
		"traffic_minute_buckets",
		"billing_ledger",
		"account_quota_states",
		"account_billing_profiles",
		"billing_source_sync_state",
		"account_policy_snapshots",
		"node_health_snapshots",
		"scheduler_decisions",
		"audit_logs",
	} {
		want := "create table if not exists public." + table
		if !strings.Contains(statements, want) {
			t.Errorf("bootstrap schema missing %s", want)
		}
	}

	for _, index := range []string{
		"idx_traffic_minute_buckets_account_bucket",
		"idx_billing_ledger_account_created",
		"idx_node_health_snapshots_sampled",
		"idx_scheduler_decisions_generated",
		"idx_audit_logs_created_at",
	} {
		if !strings.Contains(statements, "create index if not exists "+index) {
			t.Errorf("bootstrap schema missing idempotent index %s", index)
		}
	}

	for _, column := range []string{"arrears_since", "period_start", "period_end"} {
		want := "alter table public.account_quota_states add column if not exists " + column
		if !strings.Contains(statements, want) {
			t.Errorf("bootstrap schema missing additive quota column %s", column)
		}
	}

	if strings.Contains(statements, "drop ") || strings.Contains(statements, "truncate ") {
		t.Fatal("startup bootstrap must not include destructive DDL")
	}
}

func TestEnsureSharedXWorkmateProfileBootstrapsManagedBridgeContractForAllUsers(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := store.NewMemoryStore()
	if err := st.EnsureTenant(ctx, &store.Tenant{
		ID:      store.SharedXWorkmateTenantID,
		Name:    store.SharedXWorkmateTenantName,
		Edition: store.SharedPublicTenantEdition,
	}); err != nil {
		t.Fatalf("ensure tenant: %v", err)
	}

	err := ensureSharedXWorkmateProfile(
		ctx,
		st,
		sharedXWorkmateBootstrapConfig{
			BridgeServerURL: "https://acp-bridge.uat.example.test",
		},
		slog.Default(),
	)
	if err != nil {
		t.Fatalf("ensure shared review xworkmate profile: %v", err)
	}

	profile, err := st.GetXWorkmateProfile(
		ctx,
		store.SharedXWorkmateTenantID,
		"",
		store.XWorkmateProfileScopeTenantShared,
	)
	if err != nil {
		t.Fatalf("load shared profile: %v", err)
	}

	if got := profile.BridgeServerURL; got != "https://acp-bridge.uat.example.test" {
		t.Fatalf("expected configured bridge server url, got %q", got)
	}
	if got := profile.BridgeServerOrigin; got != "https://acp-bridge.uat.example.test" {
		t.Fatalf("expected normalized bridge server origin, got %q", got)
	}
	if len(profile.SecretLocators) != 0 {
		t.Fatalf("startup must not write a shared user credential, got %#v", profile.SecretLocators)
	}
}

func TestEnsureSharedXWorkmateProfileSkipsWhenBridgeIsNotConfigured(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := store.NewMemoryStore()
	if err := st.EnsureTenant(ctx, &store.Tenant{
		ID:      store.SharedXWorkmateTenantID,
		Name:    store.SharedXWorkmateTenantName,
		Edition: store.SharedPublicTenantEdition,
	}); err != nil {
		t.Fatalf("ensure tenant: %v", err)
	}

	err := ensureSharedXWorkmateProfile(
		ctx,
		st,
		sharedXWorkmateBootstrapConfig{},
		nil,
	)
	if err != nil {
		t.Fatalf("expected unconfigured bridge to be a no-op, got %v", err)
	}
	_, err = st.GetXWorkmateProfile(ctx, store.SharedXWorkmateTenantID, "", store.XWorkmateProfileScopeTenantShared)
	if err != store.ErrXWorkmateProfileNotFound {
		t.Fatalf("expected no shared profile without a bridge endpoint, got %v", err)
	}
}

func TestResolveAllowedOriginsMergesEnvironmentOrigins(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	serverCfg := config.Server{AllowedOrigins: []string{"https://console.svc.plus"}}

	t.Setenv(allowedOriginsEnv, " https://console-cloudflare-uat.onwalk.net , console-cloudflare-sit.onwalk.net ,, https://console.svc.plus ")

	origins, allowAll := resolveAllowedOrigins(logger, serverCfg)
	if allowAll {
		t.Fatal("allowAll must stay false when no entry is *")
	}

	want := []string{
		"https://console.svc.plus",
		"https://console-cloudflare-uat.onwalk.net",
		"https://console-cloudflare-sit.onwalk.net",
	}
	if len(origins) != len(want) {
		t.Fatalf("origins = %v, want %v", origins, want)
	}
	for i, origin := range want {
		if origins[i] != origin {
			t.Errorf("origins[%d] = %q, want %q", i, origins[i], origin)
		}
	}
}

func TestResolveAllowedOriginsIgnoresUnsetEnvironmentOrigins(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	serverCfg := config.Server{AllowedOrigins: []string{"https://console.svc.plus"}}

	t.Setenv(allowedOriginsEnv, "")

	origins, allowAll := resolveAllowedOrigins(logger, serverCfg)
	if allowAll {
		t.Fatal("allowAll must stay false when no entry is *")
	}
	if len(origins) != 1 || origins[0] != "https://console.svc.plus" {
		t.Fatalf("origins = %v, want [https://console.svc.plus]", origins)
	}
}

func TestResolveAllowedOriginsHonoursWildcardFromEnvironment(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	serverCfg := config.Server{AllowedOrigins: []string{"https://console.svc.plus"}}

	t.Setenv(allowedOriginsEnv, "*")

	origins, allowAll := resolveAllowedOrigins(logger, serverCfg)
	if !allowAll {
		t.Fatal("allowAll must be true when the environment supplies *")
	}
	if origins != nil {
		t.Fatalf("origins = %v, want nil when allowAll is set", origins)
	}
}
