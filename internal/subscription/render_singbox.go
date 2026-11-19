package subscription

import (
	"encoding/json"
	"strings"

	"account/internal/routingpolicy"
)

func renderSingBox(b nodeBuckets, policy routingpolicy.Policy) ([]byte, error) {
	outbounds := make([]map[string]any, 0, len(b.all)+5)

	mediaTags := make([]string, 0, len(b.media))
	proTags := make([]string, 0, len(b.pro))
	customTags := make([]string, 0, len(b.custom))

	for _, node := range b.all {
		tag := node.Name
		outbound := map[string]any{
			"type":        "vless",
			"tag":         tag,
			"server":      node.Address,
			"server_port": node.Port,
			"uuid":        node.UUID,
			"tls": map[string]any{
				"enabled":     true,
				"server_name": node.ServerName,
				"utls": map[string]any{
					"enabled":     true,
					"fingerprint": node.Fingerprint,
				},
			},
		}
		transport := strings.ToLower(strings.TrimSpace(node.Transport))
		if transport == "tcp" {
			if strings.TrimSpace(node.Flow) != "" {
				outbound["flow"] = node.Flow
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
			outbound["transport"] = map[string]any{
				"type": "xhttp",
				"host": node.Address,
				"path": path,
				"mode": mode,
			}
		}
		outbounds = append(outbounds, outbound)

		switch node.RoutingPool {
		case routingpolicy.RoutingPoolProEC2:
			proTags = append(proTags, tag)
		case routingpolicy.RoutingPoolCustomDedicated:
			customTags = append(customTags, tag)
		default:
			mediaTags = append(mediaTags, tag)
		}
	}

	if len(mediaTags) > 0 {
		urlTestURL, _ := urlTestSettings(policy, routingpolicy.RoutingPoolMediaPriority, 300)
		outbounds = append(outbounds, map[string]any{
			"type":      "urltest",
			"tag":       routingpolicy.RoutingPoolMediaPriority,
			"outbounds": mediaTags,
			"url":       urlTestURL,
			"interval":  "5m",
		})
	}
	if len(proTags) > 0 {
		urlTestURL, _ := urlTestSettings(policy, routingpolicy.RoutingPoolProEC2, 180)
		outbounds = append(outbounds, map[string]any{
			"type":      "urltest",
			"tag":       routingpolicy.RoutingPoolProEC2,
			"outbounds": proTags,
			"url":       urlTestURL,
			"interval":  "3m",
		})
	}
	if len(customTags) > 0 {
		outbounds = append(outbounds, map[string]any{
			"type":      "selector",
			"tag":       routingpolicy.RoutingPoolCustomDedicated,
			"outbounds": customTags,
		})
	}

	outbounds = append(outbounds,
		map[string]any{"type": "direct", "tag": "direct"},
		map[string]any{"type": "block", "tag": "block"},
	)

	fallbackTarget := singBoxTarget(defaultFallbackTarget(len(mediaTags) > 0, len(customTags) > 0))
	proTarget := fallbackTarget
	if len(proTags) > 0 {
		proTarget = routingpolicy.RoutingPoolProEC2
	}

	rules := []map[string]any{
		{"ip_is_private": true, "outbound": "direct"},
	}
	if cnDomains := singBoxDomainConditions(policy.Tiers.DirectCNPrivate.XrayDomains); len(cnDomains) > 0 {
		rule := cloneMap(cnDomains)
		rule["outbound"] = "direct"
		rules = append(rules, rule)
	}
	if len(policy.Tiers.DirectCNPrivate.XrayIPs) > 0 {
		rules = append(rules, map[string]any{
			"geoip":    stripPrefixes(policy.Tiers.DirectCNPrivate.XrayIPs, "geoip:"),
			"outbound": "direct",
		})
	}
	if mediaRule := singBoxDomainConditions(policy.Tiers.MediaPoolPriority.XrayDomains); len(mediaRule) > 0 {
		mediaRule["outbound"] = fallbackTarget
		rules = append(rules, mediaRule)
	}
	if proRule := singBoxDomainConditions(policy.Tiers.ProEC2Whitelist.XrayDomains); len(proRule) > 0 {
		proRule["outbound"] = proTarget
		rules = append(rules, proRule)
	}

	doc := map[string]any{
		"log": map[string]any{"level": "info"},
		"inbounds": []map[string]any{
			{"type": "mixed", "tag": "mixed-in", "listen": "127.0.0.1", "listen_port": 7890},
		},
		"outbounds": outbounds,
		"route": map[string]any{
			"auto_detect_interface": true,
			"rules":                 rules,
			"final":                 fallbackTarget,
		},
	}
	return json.MarshalIndent(doc, "", "  ")
}

func singBoxTarget(target string) string {
	if target == routingpolicy.TierTargetDirect {
		return "direct"
	}
	return target
}

func singBoxDomainConditions(xrayDomains []string) map[string]any {
	var geosites, suffixes []string
	for _, item := range xrayDomains {
		trimmed := strings.TrimSpace(item)
		switch {
		case strings.HasPrefix(trimmed, "geosite:"):
			geosites = append(geosites, strings.TrimPrefix(trimmed, "geosite:"))
		case strings.HasPrefix(trimmed, "domain:"):
			suffixes = append(suffixes, strings.TrimPrefix(trimmed, "domain:"))
		case trimmed != "":
			suffixes = append(suffixes, trimmed)
		}
	}
	cond := map[string]any{}
	if len(geosites) > 0 {
		cond["geosite"] = geosites
	}
	if len(suffixes) > 0 {
		cond["domain_suffix"] = suffixes
	}
	return cond
}

func stripPrefixes(items []string, prefix string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, strings.TrimPrefix(strings.TrimSpace(item), prefix))
	}
	return out
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
