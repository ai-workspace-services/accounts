package routingpolicy

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// RoutingPoolMediaPriority is the public high-bandwidth pool serving free,
	// shared, and plus plans with media priority and MATCH fallback.
	RoutingPoolMediaPriority = "Media-Pool"
	// RoutingPoolProEC2 is the public low-latency EC2 pool serving pro plans
	// strictly for AI, dev, and business office whitelisted traffic.
	RoutingPoolProEC2 = "Pro-EC2-Pool"
	// RoutingPoolCustomDedicated is the non-public whitelist-bound pool serving
	// custom and team dedicated accounts without traffic restrictions.
	RoutingPoolCustomDedicated = "Custom-Dedicated"

	// TierTargetDirect is the Tier 1 local/CN bypass target.
	TierTargetDirect = "DIRECT"

	// MediaOnProActionAllow permits streaming/media traffic on a pool.
	MediaOnProActionAllow = "allow"
	// MediaOnProActionBlackhole blocks streaming/media traffic on Pro-EC2-Pool.
	MediaOnProActionBlackhole = "blackhole"

	// DefaultPolicyPathEnv optionally points to an external GitOps YAML policy file.
	DefaultPolicyPathEnv = "XCONNECT_ROUTING_POLICY_PATH"
)

// URLTestConfig describes Clash/Mihomo url-test health probe settings for a pool.
type URLTestConfig struct {
	URL      string `yaml:"url" json:"url"`
	Interval int    `yaml:"interval" json:"interval"`
}

// PoolPolicy defines the capabilities, access isolation, and node inventory of a routing pool.
type PoolPolicy struct {
	PublicResource   bool           `yaml:"public_resource" json:"publicResource"`
	MediaOnProAction string         `yaml:"media_on_pro_action" json:"mediaOnProAction"`
	FallbackDefault  bool           `yaml:"fallback_default" json:"fallbackDefault"`
	Unlimited        bool           `yaml:"unlimited,omitempty" json:"unlimited,omitempty"`
	Description      string         `yaml:"description,omitempty" json:"description,omitempty"`
	PricingGroups    []string       `yaml:"pricing_groups" json:"pricingGroups"`
	SubPoolTags      []string       `yaml:"sub_pool_tags_reserved,omitempty" json:"subPoolTags,omitempty"`
	URLTest          *URLTestConfig `yaml:"url_test,omitempty" json:"urlTest,omitempty"`
	Nodes            []string       `yaml:"nodes,omitempty" json:"nodes,omitempty"`
}

// TierPolicy defines one priority tier in the three-tier split routing policy.
type TierPolicy struct {
	Priority                     int      `yaml:"priority" json:"priority"`
	Target                       string   `yaml:"target" json:"target"`
	ServerOutboundTag            string   `yaml:"server_outbound_tag,omitempty" json:"serverOutboundTag,omitempty"`
	PrivateOutboundTag           string   `yaml:"private_outbound_tag,omitempty" json:"privateOutboundTag,omitempty"`
	ServerOutboundTagOnMediaPool string   `yaml:"server_outbound_tag_on_media_pool,omitempty" json:"serverOutboundTagOnMediaPool,omitempty"`
	ServerOutboundTagOnProPool   string   `yaml:"server_outbound_tag_on_pro_pool,omitempty" json:"serverOutboundTagOnProPool,omitempty"`
	ClashRules                   []string `yaml:"clash_rules,omitempty" json:"clashRules,omitempty"`
	XrayDomains                  []string `yaml:"xray_domains,omitempty" json:"xrayDomains,omitempty"`
	XrayIPs                      []string `yaml:"xray_ips,omitempty" json:"xrayIps,omitempty"`
}

// Tiers groups the canonical four-step / three-tier routing rules.
type Tiers struct {
	DirectCNPrivate   TierPolicy `yaml:"direct_cn_private" json:"directCnPrivate"`
	MediaPoolPriority TierPolicy `yaml:"media_pool_priority" json:"mediaPoolPriority"`
	ProEC2Whitelist   TierPolicy `yaml:"pro_ec2_whitelist" json:"proEc2Whitelist"`
	GlobalFallback    TierPolicy `yaml:"global_fallback" json:"globalFallback"`
}

// Policy represents the canonical XConnect three-tier routing policy.
type Policy struct {
	Version  int                   `yaml:"version" json:"version"`
	Revision string                `yaml:"revision" json:"revision"`
	Pools    map[string]PoolPolicy `yaml:"pools" json:"pools"`
	Tiers    Tiers                 `yaml:"tiers" json:"tiers"`
}

// DefaultPolicy returns a fresh copy of the canonical built-in three-tier routing policy
// matching xconnect-routing-policy.yaml so callers work standalone without disk YAML.
func DefaultPolicy() Policy {
	return Policy{
		Version:  1,
		Revision: "2026-11-19.1",
		Pools: map[string]PoolPolicy{
			RoutingPoolMediaPriority: {
				PublicResource:   true,
				MediaOnProAction: MediaOnProActionAllow,
				FallbackDefault:  true,
				Unlimited:        true,
				Description:      "Public high-bandwidth pool (Free/Shared/Plus): media priority + global MATCH fallback, unlimited",
				PricingGroups:    []string{"free", "shared", "plus"},
				SubPoolTags:      []string{"media:video", "media:shopping", "media:ai-app"},
				URLTest: &URLTestConfig{
					URL:      "https://cp.cloudflare.com/generate_204",
					Interval: 300,
				},
				Nodes: []string{
					"jp-xconnect-01.svc.plus",
					"ph-xconnect.svc.plus",
					"jp-xconnect-free-01.svc.plus",
				},
			},
			RoutingPoolProEC2: {
				PublicResource:   true,
				MediaOnProAction: MediaOnProActionAllow,
				FallbackDefault:  false,
				Unlimited:        false,
				Description:      "Public EC2 pool: AI / dev / business office priority; allows normal traffic with reminder + traffic control for heavy media",
				PricingGroups:    []string{"pro"},
				URLTest: &URLTestConfig{
					URL:      "https://cp.cloudflare.com/generate_204",
					Interval: 180,
				},
				Nodes: []string{
					"jp-xconnect-02.svc.plus",
				},
			},
			RoutingPoolCustomDedicated: {
				PublicResource:   false,
				MediaOnProAction: MediaOnProActionAllow,
				FallbackDefault:  false,
				Unlimited:        true,
				Description:      "Non-public dedicated pool (Custom/Team whitelist-bound): never enters public discovery, unlimited",
				PricingGroups:    []string{"custom", "team"},
				Nodes: []string{
					"us-xconnect-los-angeles-custom-01.svc.plus",
					"us-xconnect.svc.plus",
				},
			},
		},
		Tiers: Tiers{
			DirectCNPrivate: TierPolicy{
				Priority:           1,
				Target:             TierTargetDirect,
				ServerOutboundTag:  "direct-cn",
				PrivateOutboundTag: "blocked-private",
				ClashRules: []string{
					"GEOIP,private,DIRECT,no-resolve",
					"GEOSITE,cn,DIRECT",
					"GEOIP,cn,DIRECT,no-resolve",
				},
				XrayDomains: []string{"geosite:cn"},
				XrayIPs:     []string{"geoip:cn"},
			},
			MediaPoolPriority: TierPolicy{
				Priority:                     2,
				Target:                       RoutingPoolMediaPriority,
				ServerOutboundTagOnMediaPool: "pool-media-stream",
				ServerOutboundTagOnProPool:   "blocked-media-on-pro",
				ClashRules: []string{
					"GEOSITE,youtube,Media-Pool",
					"GEOSITE,netflix,Media-Pool",
					"GEOSITE,disney,Media-Pool",
					"GEOSITE,tiktok,Media-Pool",
					"GEOSITE,twitch,Media-Pool",
					"GEOSITE,spotify,Media-Pool",
					"GEOSITE,bilibili,Media-Pool",
					"DOMAIN-SUFFIX,speedtest.net,Media-Pool",
					"DOMAIN-SUFFIX,fast.com,Media-Pool",
					"DOMAIN-SUFFIX,steamcontent.com,Media-Pool",
					"DOMAIN-SUFFIX,icloud-content.com,Media-Pool",
					"DOMAIN-SUFFIX,mega.nz,Media-Pool",
				},
				XrayDomains: []string{
					"geosite:youtube",
					"geosite:netflix",
					"geosite:disney",
					"geosite:tiktok",
					"geosite:twitch",
					"geosite:spotify",
					"geosite:bilibili",
					"domain:speedtest.net",
					"domain:fast.com",
					"domain:steamcontent.com",
					"domain:icloud-content.com",
					"domain:mega.nz",
				},
			},
			ProEC2Whitelist: TierPolicy{
				Priority:          3,
				Target:            RoutingPoolProEC2,
				ServerOutboundTag: "pool-pro-ai-dev",
				ClashRules: []string{
					"GEOSITE,openai,Pro-EC2-Pool",
					"GEOSITE,anthropic,Pro-EC2-Pool",
					"DOMAIN-SUFFIX,claude.ai,Pro-EC2-Pool",
					"DOMAIN-SUFFIX,cursor.sh,Pro-EC2-Pool",
					"DOMAIN-SUFFIX,cursor.com,Pro-EC2-Pool",
					"GEOSITE,github,Pro-EC2-Pool",
					"GEOSITE,google,Pro-EC2-Pool",
					"GEOSITE,cloudflare,Pro-EC2-Pool",
				},
				XrayDomains: []string{
					"geosite:openai",
					"geosite:anthropic",
					"domain:claude.ai",
					"domain:cursor.sh",
					"domain:cursor.com",
					"geosite:github",
					"geosite:google",
					"geosite:cloudflare",
				},
			},
			GlobalFallback: TierPolicy{
				Priority:                     4,
				Target:                       RoutingPoolMediaPriority,
				ServerOutboundTagOnMediaPool: "pool-media-fallback",
				ServerOutboundTagOnProPool:   "pool-pro-unclassified",
				ClashRules: []string{
					"MATCH,Media-Pool",
				},
			},
		},
	}
}

// LoadFromBytes decodes a YAML policy document and normalizes/validates it against canonical defaults.
func LoadFromBytes(data []byte) (Policy, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return Policy{}, errors.New("routing policy YAML is empty")
	}
	var parsed Policy
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return Policy{}, fmt.Errorf("parse routing policy YAML: %w", err)
	}
	return normalizeAndValidate(parsed)
}

// LoadFromFile loads a routing policy from a GitOps YAML file on disk.
func LoadFromFile(path string) (Policy, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return Policy{}, errors.New("routing policy path is required")
	}
	data, err := os.ReadFile(trimmed)
	if err != nil {
		return Policy{}, fmt.Errorf("read routing policy %q: %w", trimmed, err)
	}
	return LoadFromBytes(data)
}

// LoadOrDefault loads the policy from path (or XCONNECT_ROUTING_POLICY_PATH if path is empty),
// falling back to DefaultPolicy() when no path is configured.
func LoadOrDefault(path string) (Policy, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		trimmed = strings.TrimSpace(os.Getenv(DefaultPolicyPathEnv))
	}
	if trimmed == "" {
		return DefaultPolicy(), nil
	}
	return LoadFromFile(trimmed)
}

func normalizeAndValidate(parsed Policy) (Policy, error) {
	def := DefaultPolicy()
	if parsed.Version < 0 {
		return Policy{}, fmt.Errorf("invalid routing policy version %d", parsed.Version)
	}
	if parsed.Version == 0 {
		parsed.Version = def.Version
	}
	if strings.TrimSpace(parsed.Revision) == "" {
		parsed.Revision = def.Revision
	}
	if parsed.Pools == nil {
		parsed.Pools = make(map[string]PoolPolicy, len(def.Pools))
	}
	for name, defPool := range def.Pools {
		pool, ok := parsed.Pools[name]
		if !ok {
			parsed.Pools[name] = clonePoolPolicy(defPool)
			continue
		}
		if name == RoutingPoolCustomDedicated {
			// Custom-Dedicated must never become a public resource.
			if pool.PublicResource {
				return Policy{}, fmt.Errorf("pool %q must have public_resource=false", name)
			}
			pool.Unlimited = true
		}
		if name == RoutingPoolMediaPriority {
			pool.Unlimited = true
		}
		if strings.TrimSpace(pool.MediaOnProAction) == "" {
			pool.MediaOnProAction = defPool.MediaOnProAction
		}
		if pool.MediaOnProAction != MediaOnProActionAllow && pool.MediaOnProAction != MediaOnProActionBlackhole {
			return Policy{}, fmt.Errorf("pool %q has invalid media_on_pro_action %q", name, pool.MediaOnProAction)
		}
		if len(pool.PricingGroups) == 0 {
			pool.PricingGroups = append([]string(nil), defPool.PricingGroups...)
		}
		if pool.URLTest == nil && defPool.URLTest != nil {
			pool.URLTest = &URLTestConfig{URL: defPool.URLTest.URL, Interval: defPool.URLTest.Interval}
		}
		parsed.Pools[name] = pool
	}

	// Validate any custom-named pools as well.
	for name, pool := range parsed.Pools {
		if pool.MediaOnProAction == "" {
			pool.MediaOnProAction = MediaOnProActionAllow
		}
		if pool.MediaOnProAction != MediaOnProActionAllow && pool.MediaOnProAction != MediaOnProActionBlackhole {
			return Policy{}, fmt.Errorf("pool %q has invalid media_on_pro_action %q", name, pool.MediaOnProAction)
		}
		parsed.Pools[name] = pool
	}

	parsed.Tiers.DirectCNPrivate = mergeTierDefaults(parsed.Tiers.DirectCNPrivate, def.Tiers.DirectCNPrivate)
	parsed.Tiers.MediaPoolPriority = mergeTierDefaults(parsed.Tiers.MediaPoolPriority, def.Tiers.MediaPoolPriority)
	parsed.Tiers.ProEC2Whitelist = mergeTierDefaults(parsed.Tiers.ProEC2Whitelist, def.Tiers.ProEC2Whitelist)
	parsed.Tiers.GlobalFallback = mergeTierDefaults(parsed.Tiers.GlobalFallback, def.Tiers.GlobalFallback)

	return parsed, nil
}

func mergeTierDefaults(tier, def TierPolicy) TierPolicy {
	if tier.Priority == 0 {
		tier.Priority = def.Priority
	}
	if strings.TrimSpace(tier.Target) == "" {
		tier.Target = def.Target
	}
	if strings.TrimSpace(tier.ServerOutboundTag) == "" {
		tier.ServerOutboundTag = def.ServerOutboundTag
	}
	if strings.TrimSpace(tier.PrivateOutboundTag) == "" {
		tier.PrivateOutboundTag = def.PrivateOutboundTag
	}
	if strings.TrimSpace(tier.ServerOutboundTagOnMediaPool) == "" {
		tier.ServerOutboundTagOnMediaPool = def.ServerOutboundTagOnMediaPool
	}
	if strings.TrimSpace(tier.ServerOutboundTagOnProPool) == "" {
		tier.ServerOutboundTagOnProPool = def.ServerOutboundTagOnProPool
	}
	if len(tier.ClashRules) == 0 && len(def.ClashRules) > 0 {
		tier.ClashRules = append([]string(nil), def.ClashRules...)
	}
	if len(tier.XrayDomains) == 0 && len(def.XrayDomains) > 0 {
		tier.XrayDomains = append([]string(nil), def.XrayDomains...)
	}
	if len(tier.XrayIPs) == 0 && len(def.XrayIPs) > 0 {
		tier.XrayIPs = append([]string(nil), def.XrayIPs...)
	}
	return tier
}

// PoolForResourceClass maps a control-plane resource class / pricing group (free, shared, plus, pro, custom, team)
// to its canonical routing pool name.
func (p Policy) PoolForResourceClass(resourceClass string) string {
	normalized := strings.ToLower(strings.TrimSpace(resourceClass))
	if normalized == "" {
		return RoutingPoolMediaPriority
	}
	for _, poolName := range []string{RoutingPoolCustomDedicated, RoutingPoolProEC2, RoutingPoolMediaPriority} {
		if pool, ok := p.Pools[poolName]; ok {
			for _, group := range pool.PricingGroups {
				if strings.ToLower(strings.TrimSpace(group)) == normalized {
					return poolName
				}
			}
		}
	}
	switch normalized {
	case "pro":
		return RoutingPoolProEC2
	case "custom", "team":
		return RoutingPoolCustomDedicated
	default:
		return RoutingPoolMediaPriority
	}
}

// Pool returns a copy of the PoolPolicy for the named pool.
func (p Policy) Pool(name string) (PoolPolicy, bool) {
	pool, ok := p.Pools[name]
	if !ok {
		return PoolPolicy{}, false
	}
	return clonePoolPolicy(pool), true
}

// IsPublicPool reports whether the named pool is eligible for public discovery.
func (p Policy) IsPublicPool(name string) bool {
	if name == RoutingPoolCustomDedicated {
		return false
	}
	pool, ok := p.Pools[name]
	if !ok {
		return false
	}
	return pool.PublicResource
}

// MediaOnProActionFor returns the configured media action for the named pool ("allow" or "blackhole").
func (p Policy) MediaOnProActionFor(name string) string {
	pool, ok := p.Pools[name]
	if !ok || strings.TrimSpace(pool.MediaOnProAction) == "" {
		return MediaOnProActionAllow
	}
	return pool.MediaOnProAction
}

// SubPoolTagsFor returns a copy of reserved sub-pool tags for the named pool.
func (p Policy) SubPoolTagsFor(name string) []string {
	pool, ok := p.Pools[name]
	if !ok || len(pool.SubPoolTags) == 0 {
		return nil
	}
	return append([]string(nil), pool.SubPoolTags...)
}

// Clone returns a deep copy of Policy.
func (p Policy) Clone() Policy {
	out := p
	if p.Pools != nil {
		out.Pools = make(map[string]PoolPolicy, len(p.Pools))
		for k, v := range p.Pools {
			out.Pools[k] = clonePoolPolicy(v)
		}
	}
	out.Tiers.DirectCNPrivate = cloneTierPolicy(p.Tiers.DirectCNPrivate)
	out.Tiers.MediaPoolPriority = cloneTierPolicy(p.Tiers.MediaPoolPriority)
	out.Tiers.ProEC2Whitelist = cloneTierPolicy(p.Tiers.ProEC2Whitelist)
	out.Tiers.GlobalFallback = cloneTierPolicy(p.Tiers.GlobalFallback)
	return out
}

func clonePoolPolicy(in PoolPolicy) PoolPolicy {
	out := in
	if in.PricingGroups != nil {
		out.PricingGroups = append([]string(nil), in.PricingGroups...)
	}
	if in.SubPoolTags != nil {
		out.SubPoolTags = append([]string(nil), in.SubPoolTags...)
	}
	if in.Nodes != nil {
		out.Nodes = append([]string(nil), in.Nodes...)
	}
	if in.URLTest != nil {
		u := *in.URLTest
		out.URLTest = &u
	}
	return out
}

func cloneTierPolicy(in TierPolicy) TierPolicy {
	out := in
	if in.ClashRules != nil {
		out.ClashRules = append([]string(nil), in.ClashRules...)
	}
	if in.XrayDomains != nil {
		out.XrayDomains = append([]string(nil), in.XrayDomains...)
	}
	if in.XrayIPs != nil {
		out.XrayIPs = append([]string(nil), in.XrayIPs...)
	}
	return out
}
