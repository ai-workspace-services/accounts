package main

import (
	"context"
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

func TestEnsureSharedReviewXWorkmateProfileBootstrapsManagedBridgeContract(t *testing.T) {
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

	writes := make([]struct {
		locator store.XWorkmateSecretLocator
		value   string
	}, 0, 1)
	err := ensureSharedReviewXWorkmateProfile(
		ctx,
		st,
		config.ReviewAccount{Enabled: true},
		sharedXWorkmateBootstrapConfig{
			BridgeServerURL: SharedXWorkmateBridgeServerURL,
			BridgeAuthToken: "bridge-token",
		},
		func(
			ctx context.Context,
			locator store.XWorkmateSecretLocator,
			value string,
		) error {
			writes = append(writes, struct {
				locator store.XWorkmateSecretLocator
				value   string
			}{locator: locator, value: value})
			return nil
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

	if got := profile.BridgeServerURL; got != SharedXWorkmateBridgeServerURL {
		t.Fatalf("expected bridge server url %q, got %q", SharedXWorkmateBridgeServerURL, got)
	}
	if got := profile.BridgeServerOrigin; got != SharedXWorkmateBridgeServerURL {
		t.Fatalf("expected bridge server origin %q, got %q", SharedXWorkmateBridgeServerURL, got)
	}
	if len(profile.SecretLocators) != 1 {
		t.Fatalf("expected 1 secret locator, got %d", len(profile.SecretLocators))
	}
	locator := profile.SecretLocators[0]
	if locator.Target != store.XWorkmateSecretLocatorTargetBridgeAuthToken {
		t.Fatalf("expected bridge auth token locator, got %#v", locator)
	}
	if locator.SecretPath != "xworkmate/tenants/svc-plus-xworkmate/shared" {
		t.Fatalf("expected managed shared secret path, got %#v", locator)
	}
	if len(writes) != 1 {
		t.Fatalf("expected 1 secret write, got %d", len(writes))
	}
	if writes[0].value != "bridge-token" {
		t.Fatalf("expected secret value bridge-token, got %q", writes[0].value)
	}
}

func TestEnsureSharedReviewXWorkmateProfileRequiresBridgeContract(t *testing.T) {
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

	err := ensureSharedReviewXWorkmateProfile(
		ctx,
		st,
		config.ReviewAccount{Enabled: true},
		sharedXWorkmateBootstrapConfig{
			BridgeServerURL: SharedXWorkmateBridgeServerURL,
		},
		func(context.Context, store.XWorkmateSecretLocator, string) error {
			return nil
		},
		nil,
	)
	if err == nil || err.Error() != "shared xworkmate bridge auth token is required" {
		t.Fatalf("expected missing bridge token error, got %v", err)
	}
}
