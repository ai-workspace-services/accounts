package subscription

import (
	"strings"

	"gopkg.in/yaml.v3"

	"account/internal/routingpolicy"
)

type clashConfig struct {
	Port        int              `yaml:"port"`
	SocksPort   int              `yaml:"socks-port"`
	AllowLan    bool             `yaml:"allow-lan"`
	Mode        string           `yaml:"mode"`
	LogLevel    string           `yaml:"log-level"`
	Proxies     []map[string]any `yaml:"proxies"`
	ProxyGroups []map[string]any `yaml:"proxy-groups"`
	Rules       []string         `yaml:"rules"`
}

func renderClash(b nodeBuckets, policy routingpolicy.Policy) ([]byte, error) {
	proxies := make([]map[string]any, 0, len(b.all))
	for _, node := range b.all {
		entry := map[string]any{
			"name":               node.Name,
			"type":               "vless",
			"server":             node.Address,
			"port":               node.Port,
			"uuid":               node.UUID,
			"tls":                true,
			"servername":         node.ServerName,
			"client-fingerprint": node.Fingerprint,
			"udp":                true,
			"skip-cert-verify":   false,
		}
		transport := strings.ToLower(strings.TrimSpace(node.Transport))
		if transport == "tcp" {
			entry["network"] = "tcp"
			if strings.TrimSpace(node.Flow) != "" {
				entry["flow"] = node.Flow
			}
		} else {
			entry["network"] = "xhttp"
			path := strings.TrimSpace(node.Path)
			if path == "" {
				path = "/split"
			}
			mode := strings.TrimSpace(node.Mode)
			if mode == "" {
				mode = "auto"
			}
			entry["xhttp-opts"] = map[string]any{
				"path": path,
				"mode": mode,
			}
		}
		if len(node.ALPN) > 0 {
			entry["alpn"] = node.ALPN
		}
		proxies = append(proxies, entry)
	}

	mediaNames := nodeNames(b.media)
	proNames := nodeNames(b.pro)
	customNames := nodeNames(b.custom)

	groups := make([]map[string]any, 0, 4)
	if len(mediaNames) > 0 {
		urlTestURL, interval := urlTestSettings(policy, routingpolicy.RoutingPoolMediaPriority, 300)
		groups = append(groups, map[string]any{
			"name":     routingpolicy.RoutingPoolMediaPriority,
			"type":     "url-test",
			"url":      urlTestURL,
			"interval": interval,
			"proxies":  mediaNames,
		})
	}
	if len(proNames) > 0 {
		urlTestURL, interval := urlTestSettings(policy, routingpolicy.RoutingPoolProEC2, 180)
		groups = append(groups, map[string]any{
			"name":     routingpolicy.RoutingPoolProEC2,
			"type":     "url-test",
			"url":      urlTestURL,
			"interval": interval,
			"proxies":  proNames,
		})
	}
	if len(customNames) > 0 {
		groups = append(groups, map[string]any{
			"name":    routingpolicy.RoutingPoolCustomDedicated,
			"type":    "select",
			"proxies": customNames,
		})
	}
	if len(groups) > 1 {
		topChoices := make([]string, 0, len(groups)+1)
		for _, g := range groups {
			if name, _ := g["name"].(string); name != "" {
				topChoices = append(topChoices, name)
			}
		}
		topChoices = append(topChoices, routingpolicy.TierTargetDirect)
		groups = append([]map[string]any{{
			"name":    "XConnect",
			"type":    "select",
			"proxies": topChoices,
		}}, groups...)
	}

	routes := BuildThreeTierRoutes(policy, len(mediaNames) > 0, len(proNames) > 0, len(customNames) > 0)
	rules := make([]string, 0, 32)
	for _, tier := range routes {
		rules = append(rules, tier.ClashRules...)
	}

	cfg := clashConfig{
		Port:        7890,
		SocksPort:   7891,
		AllowLan:    false,
		Mode:        "rule",
		LogLevel:    "info",
		Proxies:     proxies,
		ProxyGroups: groups,
		Rules:       rules,
	}
	return yaml.Marshal(cfg)
}

func nodeNames(nodes []Node) []string {
	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		names = append(names, node.Name)
	}
	return names
}

func urlTestSettings(policy routingpolicy.Policy, poolName string, defaultInterval int) (string, int) {
	url := "https://cp.cloudflare.com/generate_204"
	interval := defaultInterval
	if pool, ok := policy.Pool(poolName); ok && pool.URLTest != nil {
		if strings.TrimSpace(pool.URLTest.URL) != "" {
			url = pool.URLTest.URL
		}
		if pool.URLTest.Interval > 0 {
			interval = pool.URLTest.Interval
		}
	}
	return url, interval
}
