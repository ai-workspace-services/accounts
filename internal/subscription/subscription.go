package subscription

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"account/internal/routingpolicy"
	"account/internal/xrayconfig"
)

// Format identifies a supported subscription output format.
type Format string

const (
	FormatClash   Format = "clash"
	FormatSingBox Format = "singbox"
	FormatXray    Format = "xray"
	FormatOne     Format = "one"
	FormatVLESS   Format = "vless"
)

// ErrUnsupportedFormat is returned when a caller requests an unknown subscription format.
var ErrUnsupportedFormat = errors.New("unsupported subscription format")

// ParseFormat normalizes client format aliases into a canonical Format.
func ParseFormat(raw string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "clash", "mihomo", "clash-meta", "clash.meta", "yaml":
		return FormatClash, nil
	case "singbox", "sing-box":
		return FormatSingBox, nil
	case "xray", "xray-json", "xrayjson", "v2ray":
		return FormatXray, nil
	case "one", "xconnect-one":
		return FormatOne, nil
	case "vless", "uri", "raw":
		return FormatVLESS, nil
	default:
		return "", fmt.Errorf("%w: %s", ErrUnsupportedFormat, strings.TrimSpace(raw))
	}
}

// ContentType returns the HTTP Content-Type header value for format.
func ContentType(format Format) string {
	switch format {
	case FormatClash:
		return "text/yaml; charset=utf-8"
	case FormatSingBox, FormatXray, FormatOne:
		return "application/json; charset=utf-8"
	case FormatVLESS:
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// Node describes an authorized proxy node available to a user.
type Node struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Region        string   `json:"region,omitempty"`
	Address       string   `json:"address"`
	Port          int      `json:"port"`
	UUID          string   `json:"uuid"`
	Flow          string   `json:"flow,omitempty"`
	Transport     string   `json:"transport,omitempty"`
	Path          string   `json:"path,omitempty"`
	Mode          string   `json:"mode,omitempty"`
	Security      string   `json:"security,omitempty"`
	ServerName    string   `json:"serverName,omitempty"`
	Fingerprint   string   `json:"fingerprint,omitempty"`
	ALPN          []string `json:"alpn,omitempty"`
	ResourceClass string   `json:"resourceClass,omitempty"`
	RoutingPool   string   `json:"routingPool,omitempty"`
	PricingGroup  string   `json:"pricingGroup,omitempty"`
	SubPoolTags   []string `json:"subPoolTags,omitempty"`
	VlessURI      string   `json:"vlessUri,omitempty"`
}

// RouteRule describes one tier of the canonical three-tier split routing policy.
type RouteRule struct {
	Priority         int      `json:"priority"`
	Name             string   `json:"name"`
	Target           string   `json:"target"`
	EffectiveTarget  string   `json:"effective_target"`
	MediaOnProAction string   `json:"media_on_pro_action,omitempty"`
	Domains          []string `json:"domains,omitempty"`
	IPs              []string `json:"ips,omitempty"`
	ClashRules       []string `json:"clash_rules,omitempty"`
}

// RenderInput holds user-scoped nodes and routing policy inputs for subscription rendering.
type RenderInput struct {
	UserID      string
	UserEmail   string
	AllowCustom bool
	AllowPro    bool
	Policy      routingpolicy.Policy
	Nodes       []Node
	GeneratedAt time.Time
}

// Render builds a subscription document in the requested format.
func Render(format Format, input RenderInput) ([]byte, string, error) {
	policy := normalizePolicy(input.Policy)
	partitioned := partitionNodes(input, policy)

	var (
		data []byte
		err  error
	)
	switch format {
	case FormatClash:
		data, err = renderClash(partitioned, policy)
	case FormatSingBox:
		data, err = renderSingBox(partitioned, policy)
	case FormatXray:
		data, err = renderXray(partitioned, policy)
	case FormatOne:
		data, err = renderOne(input, partitioned, policy)
	case FormatVLESS:
		data, err = renderVLESS(partitioned)
	default:
		return nil, "", fmt.Errorf("%w: %s", ErrUnsupportedFormat, format)
	}
	if err != nil {
		return nil, "", err
	}
	return data, ContentType(format), nil
}

// BuildThreeTierRoutes returns the canonical three-tier routing ruleset for One and /sync/config.
func BuildThreeTierRoutes(policy routingpolicy.Policy, hasMedia, hasPro, hasCustom bool) []RouteRule {
	policy = normalizePolicy(policy)
	fallbackTarget := defaultFallbackTarget(hasMedia, hasCustom)
	proTarget := fallbackTarget
	if hasPro {
		proTarget = routingpolicy.RoutingPoolProEC2
	}

	directIPs := make([]string, 0, 1+len(policy.Tiers.DirectCNPrivate.XrayIPs))
	directIPs = append(directIPs, "geoip:private")
	directIPs = append(directIPs, policy.Tiers.DirectCNPrivate.XrayIPs...)

	return []RouteRule{
		{
			Priority:        policy.Tiers.DirectCNPrivate.Priority,
			Name:            "direct_cn_private",
			Target:          policy.Tiers.DirectCNPrivate.Target,
			EffectiveTarget: routingpolicy.TierTargetDirect,
			Domains:         append([]string(nil), policy.Tiers.DirectCNPrivate.XrayDomains...),
			IPs:             directIPs,
			ClashRules:      append([]string(nil), policy.Tiers.DirectCNPrivate.ClashRules...),
		},
		{
			Priority:         policy.Tiers.MediaPoolPriority.Priority,
			Name:             "media_pool_priority",
			Target:           policy.Tiers.MediaPoolPriority.Target,
			EffectiveTarget:  fallbackTarget,
			MediaOnProAction: policy.MediaOnProActionFor(routingpolicy.RoutingPoolMediaPriority),
			Domains:          append([]string(nil), policy.Tiers.MediaPoolPriority.XrayDomains...),
			ClashRules:       rewriteClashRules(policy.Tiers.MediaPoolPriority.ClashRules, routingpolicy.RoutingPoolMediaPriority, fallbackTarget),
		},
		{
			Priority:         policy.Tiers.ProEC2Whitelist.Priority,
			Name:             "pro_ec2_whitelist",
			Target:           policy.Tiers.ProEC2Whitelist.Target,
			EffectiveTarget:  proTarget,
			MediaOnProAction: policy.MediaOnProActionFor(routingpolicy.RoutingPoolProEC2),
			Domains:          append([]string(nil), policy.Tiers.ProEC2Whitelist.XrayDomains...),
			ClashRules:       rewriteClashRules(policy.Tiers.ProEC2Whitelist.ClashRules, routingpolicy.RoutingPoolProEC2, proTarget),
		},
		{
			Priority:        policy.Tiers.GlobalFallback.Priority,
			Name:            "global_fallback",
			Target:          policy.Tiers.GlobalFallback.Target,
			EffectiveTarget: fallbackTarget,
			ClashRules:      rewriteClashRules(policy.Tiers.GlobalFallback.ClashRules, routingpolicy.RoutingPoolMediaPriority, fallbackTarget),
		},
	}
}

// BuildVLESSURI returns a completely config-agnostic standard vless:// URI.
// Routing pool, pricing group, and split-rule metadata are never embedded into vless:// URIs.
func BuildVLESSURI(node Node) string {
	if clean := strings.TrimSpace(node.VlessURI); clean != "" {
		return clean
	}
	port := node.Port
	if port <= 0 {
		port = 443
	}
	transport := strings.ToLower(strings.TrimSpace(node.Transport))
	if transport == "" {
		transport = "xhttp"
	}
	sni := strings.TrimSpace(node.ServerName)
	if sni == "" {
		sni = strings.TrimSpace(node.Address)
	}
	fp := strings.TrimSpace(node.Fingerprint)
	if fp == "" {
		fp = "chrome"
	}
	tag := strings.TrimSpace(node.Name)
	if tag == "" {
		tag = strings.TrimSpace(node.Address)
	}

	if transport == "tcp" {
		flow := strings.TrimSpace(node.Flow)
		if flow == "" {
			flow = xrayconfig.DefaultFlow
		}
		return fmt.Sprintf(
			"vless://%s@%s:%s?encryption=none&type=tcp&security=tls&sni=%s&fp=%s&flow=%s#%s",
			strings.TrimSpace(node.UUID),
			strings.TrimSpace(node.Address),
			strconv.Itoa(port),
			url.QueryEscape(sni),
			url.QueryEscape(fp),
			url.QueryEscape(flow),
			url.QueryEscape(tag),
		)
	}

	path := strings.TrimSpace(node.Path)
	if path == "" {
		path = "/split"
	}
	mode := strings.TrimSpace(node.Mode)
	if mode == "" {
		mode = "auto"
	}
	return fmt.Sprintf(
		"vless://%s@%s:%s?encryption=none&type=xhttp&security=tls&host=%s&path=%s&mode=%s&sni=%s&fp=%s&alpn=h2%%2Chttp%%2F1.1%%2Ch3#%s",
		strings.TrimSpace(node.UUID),
		strings.TrimSpace(node.Address),
		strconv.Itoa(port),
		url.QueryEscape(strings.TrimSpace(node.Address)),
		url.QueryEscape(path),
		url.QueryEscape(mode),
		url.QueryEscape(sni),
		url.QueryEscape(fp),
		url.QueryEscape(tag),
	)
}

type nodeBuckets struct {
	all    []Node
	media  []Node
	pro    []Node
	custom []Node
}

func normalizePolicy(p routingpolicy.Policy) routingpolicy.Policy {
	if p.Version == 0 && len(p.Pools) == 0 {
		return routingpolicy.DefaultPolicy()
	}
	return p
}

func resolveNodeRoutingPool(node Node, policy routingpolicy.Policy) string {
	pool := strings.TrimSpace(node.RoutingPool)
	if pool == routingpolicy.RoutingPoolMediaPriority ||
		pool == routingpolicy.RoutingPoolProEC2 ||
		pool == routingpolicy.RoutingPoolCustomDedicated {
		return pool
	}
	return policy.PoolForResourceClass(node.ResourceClass)
}

func partitionNodes(input RenderInput, policy routingpolicy.Policy) nodeBuckets {
	var out nodeBuckets
	usedNames := make(map[string]int, len(input.Nodes))
	for _, node := range input.Nodes {
		pool := resolveNodeRoutingPool(node, policy)
		// Never leak Custom-Dedicated nodes to non-whitelisted users.
		if pool == routingpolicy.RoutingPoolCustomDedicated && !input.AllowCustom {
			continue
		}
		// Keep Pro-EC2-Pool nodes invisible to non-Pro subscriptions (Free/Shared/Plus).
		if pool == routingpolicy.RoutingPoolProEC2 && !input.AllowPro {
			continue
		}
		node.RoutingPool = pool
		if node.Port <= 0 {
			node.Port = 443
		}
		if strings.TrimSpace(node.Transport) == "" {
			node.Transport = "xhttp"
		}
		if strings.TrimSpace(node.Security) == "" {
			node.Security = "tls"
		}
		if strings.TrimSpace(node.ServerName) == "" {
			node.ServerName = node.Address
		}
		if strings.TrimSpace(node.Fingerprint) == "" {
			node.Fingerprint = "chrome"
		}
		if strings.TrimSpace(node.Name) == "" {
			node.Name = node.Address
		}
		usedNames[node.Name]++
		if usedNames[node.Name] > 1 {
			node.Name = fmt.Sprintf("%s-%d", node.Name, usedNames[node.Name])
		}
		node.VlessURI = BuildVLESSURI(node)

		out.all = append(out.all, node)
		switch pool {
		case routingpolicy.RoutingPoolProEC2:
			out.pro = append(out.pro, node)
		case routingpolicy.RoutingPoolCustomDedicated:
			out.custom = append(out.custom, node)
		default:
			out.media = append(out.media, node)
		}
	}
	return out
}

func defaultFallbackTarget(hasMedia, hasCustom bool) string {
	if hasMedia {
		return routingpolicy.RoutingPoolMediaPriority
	}
	if hasCustom {
		return routingpolicy.RoutingPoolCustomDedicated
	}
	return routingpolicy.TierTargetDirect
}

func rewriteClashRules(rules []string, fromTarget, toTarget string) []string {
	out := make([]string, 0, len(rules))
	for _, rule := range rules {
		if fromTarget != toTarget {
			parts := strings.Split(rule, ",")
			if len(parts) >= 2 && parts[len(parts)-1] == fromTarget {
				parts[len(parts)-1] = toTarget
				rule = strings.Join(parts, ",")
			} else if len(parts) >= 3 && parts[len(parts)-2] == fromTarget {
				parts[len(parts)-2] = toTarget
				rule = strings.Join(parts, ",")
			}
		}
		out = append(out, rule)
	}
	return out
}

func renderVLESS(b nodeBuckets) ([]byte, error) {
	lines := make([]string, 0, len(b.all))
	for _, node := range b.all {
		lines = append(lines, BuildVLESSURI(node))
	}
	if len(lines) == 0 {
		return []byte(""), nil
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}
