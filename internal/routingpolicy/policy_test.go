package routingpolicy

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDefaultPolicyMatchesCanonicalThreeTierContract(t *testing.T) {
	p := DefaultPolicy()
	if p.Version != 1 || p.Revision == "" {
		t.Fatalf("unexpected version/revision: %d %q", p.Version, p.Revision)
	}

	media, ok := p.Pool(RoutingPoolMediaPriority)
	if !ok || !media.PublicResource || !media.FallbackDefault || !media.Unlimited || media.MediaOnProAction != MediaOnProActionAllow {
		t.Fatalf("Media-Pool policy mismatch: %+v ok=%v", media, ok)
	}
	if !reflect.DeepEqual(media.PricingGroups, []string{"free", "shared", "plus"}) {
		t.Fatalf("Media-Pool pricing groups = %v", media.PricingGroups)
	}
	if !reflect.DeepEqual(media.SubPoolTags, []string{"media:video", "media:shopping", "media:ai-app"}) {
		t.Fatalf("Media-Pool sub-pool tags = %v", media.SubPoolTags)
	}

	pro, ok := p.Pool(RoutingPoolProEC2)
	if !ok || !pro.PublicResource || pro.FallbackDefault || pro.Unlimited || pro.MediaOnProAction != MediaOnProActionAllow {
		t.Fatalf("Pro-EC2-Pool policy mismatch: %+v ok=%v", pro, ok)
	}
	if !reflect.DeepEqual(pro.PricingGroups, []string{"pro"}) {
		t.Fatalf("Pro-EC2-Pool pricing groups = %v", pro.PricingGroups)
	}

	custom, ok := p.Pool(RoutingPoolCustomDedicated)
	if !ok || custom.PublicResource || custom.FallbackDefault || !custom.Unlimited || custom.MediaOnProAction != MediaOnProActionAllow {
		t.Fatalf("Custom-Dedicated policy mismatch: %+v ok=%v", custom, ok)
	}
	if !reflect.DeepEqual(custom.PricingGroups, []string{"custom", "team"}) {
		t.Fatalf("Custom-Dedicated pricing groups = %v", custom.PricingGroups)
	}

	if p.Tiers.DirectCNPrivate.Target != TierTargetDirect || p.Tiers.DirectCNPrivate.Priority != 1 {
		t.Fatalf("Tier 1 mismatch: %+v", p.Tiers.DirectCNPrivate)
	}
	if p.Tiers.MediaPoolPriority.Target != RoutingPoolMediaPriority || p.Tiers.MediaPoolPriority.Priority != 2 {
		t.Fatalf("Tier 2 mismatch: %+v", p.Tiers.MediaPoolPriority)
	}
	if p.Tiers.ProEC2Whitelist.Target != RoutingPoolProEC2 || p.Tiers.ProEC2Whitelist.Priority != 3 {
		t.Fatalf("Tier 3 mismatch: %+v", p.Tiers.ProEC2Whitelist)
	}
	if p.Tiers.GlobalFallback.Target != RoutingPoolMediaPriority || p.Tiers.GlobalFallback.Priority != 4 {
		t.Fatalf("Tier 4 fallback mismatch: %+v", p.Tiers.GlobalFallback)
	}

	for class, want := range map[string]string{
		"free":   RoutingPoolMediaPriority,
		"shared": RoutingPoolMediaPriority,
		"plus":   RoutingPoolMediaPriority,
		"pro":    RoutingPoolProEC2,
		"custom": RoutingPoolCustomDedicated,
		"team":   RoutingPoolCustomDedicated,
	} {
		if got := p.PoolForResourceClass(class); got != want {
			t.Fatalf("PoolForResourceClass(%q) = %q, want %q", class, got, want)
		}
	}
	if p.IsPublicPool(RoutingPoolCustomDedicated) {
		t.Fatal("Custom-Dedicated must never be public")
	}
	if !p.IsPublicPool(RoutingPoolMediaPriority) || !p.IsPublicPool(RoutingPoolProEC2) {
		t.Fatal("Media-Pool and Pro-EC2-Pool must be public")
	}
}

func TestLoadFromFileAndValidation(t *testing.T) {
	yamlBody := `
version: 1
revision: "2026-11-19.2"
pools:
  Media-Pool:
    public_resource: true
    media_on_pro_action: "allow"
    fallback_default: true
    pricing_groups: ["free", "shared", "plus"]
    sub_pool_tags_reserved: ["media:video", "media:gaming"]
    nodes: ["jp-01.example"]
  Pro-EC2-Pool:
    public_resource: true
    media_on_pro_action: "blackhole"
    fallback_default: false
    pricing_groups: ["pro"]
    nodes: ["jp-pro.example"]
  Custom-Dedicated:
    public_resource: false
    media_on_pro_action: "allow"
    fallback_default: false
    pricing_groups: ["custom", "team"]
    nodes: ["us-custom.example"]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(path, []byte(yamlBody), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile failed: %v", err)
	}
	if loaded.Revision != "2026-11-19.2" {
		t.Fatalf("revision = %q, want 2026-11-19.2", loaded.Revision)
	}
	if got := loaded.SubPoolTagsFor(RoutingPoolMediaPriority); !reflect.DeepEqual(got, []string{"media:video", "media:gaming"}) {
		t.Fatalf("subPoolTags = %v", got)
	}
	// Missing tiers in custom YAML inherit canonical defaults.
	if len(loaded.Tiers.ProEC2Whitelist.ClashRules) == 0 || len(loaded.Tiers.GlobalFallback.ClashRules) == 0 {
		t.Fatalf("expected default tiers to be populated: %+v", loaded.Tiers)
	}

	// Reject Custom-Dedicated marked as public_resource: true.
	invalidPublic := `
version: 1
pools:
  Custom-Dedicated:
    public_resource: true
    media_on_pro_action: "allow"
`
	if _, err := LoadFromBytes([]byte(invalidPublic)); err == nil {
		t.Fatal("expected Custom-Dedicated with public_resource=true to fail validation")
	}

	// Reject invalid media_on_pro_action.
	invalidAction := `
version: 1
pools:
  Pro-EC2-Pool:
    public_resource: true
    media_on_pro_action: "throttle"
`
	if _, err := LoadFromBytes([]byte(invalidAction)); err == nil {
		t.Fatal("expected invalid media_on_pro_action to fail validation")
	}

	// LoadOrDefault falls back to built-in default when unset.
	t.Setenv(DefaultPolicyPathEnv, "")
	def, err := LoadOrDefault("")
	if err != nil {
		t.Fatalf("LoadOrDefault empty failed: %v", err)
	}
	if def.Revision != DefaultPolicy().Revision {
		t.Fatalf("LoadOrDefault revision = %q", def.Revision)
	}
}
