package subscription

import (
	"encoding/json"
	"fmt"
	"strings"

	"account/internal/routingpolicy"
)

func renderXray(b nodeBuckets, policy routingpolicy.Policy) ([]byte, error) {
	outbounds := make([]map[string]any, 0, len(b.all)+2)
	balancers := make([]map[string]any, 0, 3)

	hasMedia := len(b.media) > 0
	hasPro := len(b.pro) > 0
	hasCustom := len(b.custom) > 0

	appendNodeOutbound := func(prefix string, idx int, node Node) {
		tag := fmt.Sprintf("%s-%d", prefix, idx+1)
		stream := map[string]any{
			"network":  "xhttp",
			"security": "tls",
			"tlsSettings": map[string]any{
				"serverName":  node.ServerName,
				"fingerprint": node.Fingerprint,
			},
		}
		user := map[string]any{
			"id":         node.UUID,
			"encryption": "none",
		}
		if strings.ToLower(strings.TrimSpace(node.Transport)) == "tcp" {
			stream["network"] = "tcp"
			if strings.TrimSpace(node.Flow) != "" {
				user["flow"] = node.Flow
			}
		} else {
			path := strings.TrimSpace(node.Path)
			if path == "" {
				path = "/split"
			}
			mode := strings.TrimSpace(node.Mode)
			if mode == "" {
				mode = "auto"
			}
			stream["xhttpSettings"] = map[string]any{
				"host": node.Address,
				"path": path,
				"mode": mode,
			}
		}
		outbounds = append(outbounds, map[string]any{
			"tag":      tag,
			"protocol": "vless",
			"settings": map[string]any{
				"vnext": []map[string]any{{
					"address": node.Address,
					"port":    node.Port,
					"users":   []map[string]any{user},
				}},
			},
			"streamSettings": stream,
		})
	}

	// Put fallback pool outbounds first so Xray's default outbound[0] aligns with Media-Pool.
	for idx, node := range b.media {
		appendNodeOutbound("media", idx, node)
	}
	for idx, node := range b.pro {
		appendNodeOutbound("pro", idx, node)
	}
	for idx, node := range b.custom {
		appendNodeOutbound("custom", idx, node)
	}

	if hasMedia {
		balancers = append(balancers, map[string]any{
			"tag":      routingpolicy.RoutingPoolMediaPriority,
			"selector": []string{"media-"},
		})
	}
	if hasPro {
		balancers = append(balancers, map[string]any{
			"tag":      routingpolicy.RoutingPoolProEC2,
			"selector": []string{"pro-"},
		})
	}
	if hasCustom {
		balancers = append(balancers, map[string]any{
			"tag":      routingpolicy.RoutingPoolCustomDedicated,
			"selector": []string{"custom-"},
		})
	}

	outbounds = append(outbounds,
		map[string]any{"tag": "direct", "protocol": "freedom"},
		map[string]any{"tag": "block", "protocol": "blackhole"},
	)

	rules := make([]map[string]any, 0, 6)
	rules = append(rules, map[string]any{
		"type":        "field",
		"ip":          []string{"geoip:private"},
		"outboundTag": "direct",
	})
	if len(policy.Tiers.DirectCNPrivate.XrayDomains) > 0 {
		rules = append(rules, map[string]any{
			"type":        "field",
			"domain":      policy.Tiers.DirectCNPrivate.XrayDomains,
			"outboundTag": "direct",
		})
	}
	if len(policy.Tiers.DirectCNPrivate.XrayIPs) > 0 {
		rules = append(rules, map[string]any{
			"type":        "field",
			"ip":          policy.Tiers.DirectCNPrivate.XrayIPs,
			"outboundTag": "direct",
		})
	}

	fallbackTarget := defaultFallbackTarget(hasMedia, hasCustom)
	addPoolRule := func(rule map[string]any, target string) {
		if target == routingpolicy.TierTargetDirect {
			rule["outboundTag"] = "direct"
		} else {
			rule["balancerTag"] = target
		}
		rules = append(rules, rule)
	}

	if len(policy.Tiers.MediaPoolPriority.XrayDomains) > 0 {
		addPoolRule(map[string]any{
			"type":   "field",
			"domain": policy.Tiers.MediaPoolPriority.XrayDomains,
		}, fallbackTarget)
	}
	if len(policy.Tiers.ProEC2Whitelist.XrayDomains) > 0 {
		proTarget := fallbackTarget
		if hasPro {
			proTarget = routingpolicy.RoutingPoolProEC2
		}
		addPoolRule(map[string]any{
			"type":   "field",
			"domain": policy.Tiers.ProEC2Whitelist.XrayDomains,
		}, proTarget)
	}
	addPoolRule(map[string]any{
		"type":    "field",
		"network": "tcp,udp",
	}, fallbackTarget)

	routing := map[string]any{
		"domainStrategy": "IPIfNonMatch",
		"rules":          rules,
	}
	if len(balancers) > 0 {
		routing["balancers"] = balancers
	}

	doc := map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"inbounds": []map[string]any{
			{"listen": "127.0.0.1", "port": 1080, "protocol": "socks", "settings": map[string]any{"udp": true}},
			{"listen": "127.0.0.1", "port": 1081, "protocol": "http"},
		},
		"outbounds": outbounds,
		"routing":   routing,
	}
	return json.MarshalIndent(doc, "", "  ")
}
