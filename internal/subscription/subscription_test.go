package subscription

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"account/internal/routingpolicy"
)

func sampleNodes() []Node {
	return []Node{
		{
			ID:            "jp-media",
			Name:          "JP-Connect",
			Region:        "jp",
			Address:       "jp-xconnect-01.svc.plus",
			Port:          443,
			UUID:          "11111111-1111-1111-1111-111111111111",
			Transport:     "xhttp",
			Path:          "/split",
			Mode:          "auto",
			ResourceClass: "shared",
			RoutingPool:   routingpolicy.RoutingPoolMediaPriority,
			PricingGroup:  "shared",
		},
		{
			ID:            "jp-pro",
			Name:          "JP-Pro-EC2",
			Region:        "jp",
			Address:       "jp-xconnect-02.svc.plus",
			Port:          443,
			UUID:          "11111111-1111-1111-1111-111111111111",
			Transport:     "xhttp",
			Path:          "/split",
			Mode:          "auto",
			ResourceClass: "pro",
			RoutingPool:   routingpolicy.RoutingPoolProEC2,
			PricingGroup:  "pro",
		},
		{
			ID:            "us-custom",
			Name:          "US-Custom-Dedicated",
			Region:        "us",
			Address:       "us-xconnect-los-angeles-custom-01.svc.plus",
			Port:          443,
			UUID:          "11111111-1111-1111-1111-111111111111",
			Transport:     "xhttp",
			Path:          "/split",
			Mode:          "auto",
			ResourceClass: "custom",
			RoutingPool:   routingpolicy.RoutingPoolCustomDedicated,
			PricingGroup:  "custom",
		},
	}
}

func TestParseFormatAliases(t *testing.T) {
	cases := map[string]Format{
		"":             FormatClash,
		"clash":        FormatClash,
		"mihomo":       FormatClash,
		"singbox":      FormatSingBox,
		"sing-box":     FormatSingBox,
		"xray":         FormatXray,
		"xray-json":    FormatXray,
		"one":          FormatOne,
		"xconnect-one": FormatOne,
		"vless":        FormatVLESS,
	}
	for raw, want := range cases {
		got, err := ParseFormat(raw)
		if err != nil || got != want {
			t.Fatalf("ParseFormat(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := ParseFormat("unknown-format"); err == nil {
		t.Fatal("expected unsupported format error")
	}
}

func TestClashSubscriptionThreeTierAndCustomIsolation(t *testing.T) {
	// Non-whitelisted Pro user: Media-Pool + Pro-EC2-Pool, NEVER Custom-Dedicated.
	data, contentType, err := Render(FormatClash, RenderInput{
		UserID:      "u-pro",
		AllowCustom: false,
		AllowPro:    true,
		Nodes:       sampleNodes(),
	})
	if err != nil {
		t.Fatalf("Render clash failed: %v", err)
	}
	if !strings.Contains(contentType, "yaml") {
		t.Fatalf("unexpected content-type: %q", contentType)
	}

	var parsed clashConfig
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("invalid clash yaml: %v\n%s", err, string(data))
	}
	if len(parsed.Proxies) != 2 {
		t.Fatalf("expected 2 proxies (custom filtered), got %d: %+v", len(parsed.Proxies), parsed.Proxies)
	}
	for _, p := range parsed.Proxies {
		if p["server"] == "us-xconnect-los-angeles-custom-01.svc.plus" {
			t.Fatalf("Custom-Dedicated node leaked to non-whitelisted user: %+v", p)
		}
	}

	groupNames := make(map[string]bool)
	for _, g := range parsed.ProxyGroups {
		if name, _ := g["name"].(string); name != "" {
			groupNames[name] = true
		}
	}
	if !groupNames[routingpolicy.RoutingPoolMediaPriority] || !groupNames[routingpolicy.RoutingPoolProEC2] {
		t.Fatalf("expected Media-Pool and Pro-EC2-Pool groups, got %+v", parsed.ProxyGroups)
	}
	if groupNames[routingpolicy.RoutingPoolCustomDedicated] {
		t.Fatalf("Custom-Dedicated group leaked to non-whitelisted user: %+v", parsed.ProxyGroups)
	}

	rulesJoined := strings.Join(parsed.Rules, "\n")
	if !strings.Contains(rulesJoined, "GEOSITE,youtube,Media-Pool") {
		t.Fatalf("missing Media-Pool media priority rule:\n%s", rulesJoined)
	}
	if !strings.Contains(rulesJoined, "GEOSITE,openai,Pro-EC2-Pool") {
		t.Fatalf("missing Pro-EC2-Pool AI whitelist rule:\n%s", rulesJoined)
	}
	if parsed.Rules[len(parsed.Rules)-1] != "MATCH,Media-Pool" {
		t.Fatalf("expected MATCH,Media-Pool fallback, got %q", parsed.Rules[len(parsed.Rules)-1])
	}

	// Whitelisted custom user gets Custom-Dedicated group and nodes.
	customData, _, err := Render(FormatClash, RenderInput{
		UserID:      "u-custom",
		AllowCustom: true,
		AllowPro:    true,
		Nodes:       sampleNodes(),
	})
	if err != nil {
		t.Fatalf("Render custom clash failed: %v", err)
	}
	var customParsed clashConfig
	if err := yaml.Unmarshal(customData, &customParsed); err != nil {
		t.Fatal(err)
	}
	if len(customParsed.Proxies) != 3 {
		t.Fatalf("expected 3 proxies for custom user, got %d", len(customParsed.Proxies))
	}
	hasCustomGroup := false
	for _, g := range customParsed.ProxyGroups {
		if g["name"] == routingpolicy.RoutingPoolCustomDedicated {
			hasCustomGroup = true
		}
	}
	if !hasCustomGroup {
		t.Fatalf("expected Custom-Dedicated group for whitelisted user: %+v", customParsed.ProxyGroups)
	}
}

func TestClashFreeUserFallsBackProWhitelistToMediaPool(t *testing.T) {
	nodes := []Node{sampleNodes()[0]} // Media-Pool only
	data, _, err := Render(FormatClash, RenderInput{
		UserID: "u-free",
		Nodes:  nodes,
	})
	if err != nil {
		t.Fatal(err)
	}
	var parsed clashConfig
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	for _, g := range parsed.ProxyGroups {
		if g["name"] == routingpolicy.RoutingPoolProEC2 {
			t.Fatalf("Free user should not get Pro-EC2-Pool group: %+v", parsed.ProxyGroups)
		}
	}
	for _, rule := range parsed.Rules {
		if strings.Contains(rule, routingpolicy.RoutingPoolProEC2) {
			t.Fatalf("Free user rule referenced missing Pro-EC2-Pool group: %q", rule)
		}
	}
	if !strings.Contains(strings.Join(parsed.Rules, "\n"), "GEOSITE,openai,Media-Pool") {
		t.Fatalf("expected openai rule to fall back to Media-Pool for Free user: %v", parsed.Rules)
	}
}

func TestSingBoxXrayOneAndVLESSURIFormats(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	// Sing-box
	sbBytes, _, err := Render(FormatSingBox, RenderInput{AllowCustom: false, AllowPro: true, Nodes: sampleNodes(), GeneratedAt: now})
	if err != nil {
		t.Fatalf("singbox render failed: %v", err)
	}
	var sbDoc map[string]any
	if err := json.Unmarshal(sbBytes, &sbDoc); err != nil {
		t.Fatalf("invalid singbox json: %v", err)
	}
	if strings.Contains(string(sbBytes), "us-xconnect-los-angeles-custom-01.svc.plus") {
		t.Fatal("Custom-Dedicated leaked into sing-box for non-whitelisted user")
	}
	routeObj := sbDoc["route"].(map[string]any)
	if routeObj["final"] != routingpolicy.RoutingPoolMediaPriority {
		t.Fatalf("sing-box final = %v, want Media-Pool", routeObj["final"])
	}

	// Xray JSON
	xrayBytes, _, err := Render(FormatXray, RenderInput{AllowCustom: false, AllowPro: true, Nodes: sampleNodes(), GeneratedAt: now})
	if err != nil {
		t.Fatalf("xray render failed: %v", err)
	}
	var xrayDoc map[string]any
	if err := json.Unmarshal(xrayBytes, &xrayDoc); err != nil {
		t.Fatalf("invalid xray json: %v", err)
	}
	if strings.Contains(string(xrayBytes), "us-xconnect-los-angeles-custom-01.svc.plus") {
		t.Fatal("Custom-Dedicated leaked into xray json for non-whitelisted user")
	}
	routingObj := xrayDoc["routing"].(map[string]any)
	balancers := routingObj["balancers"].([]any)
	if len(balancers) != 2 {
		t.Fatalf("expected 2 balancers (Media-Pool + Pro-EC2-Pool), got %d", len(balancers))
	}

	// One
	oneBytes, _, err := Render(FormatOne, RenderInput{AllowCustom: true, AllowPro: true, Nodes: sampleNodes(), GeneratedAt: now})
	if err != nil {
		t.Fatalf("one render failed: %v", err)
	}
	var oneDoc onePayload
	if err := json.Unmarshal(oneBytes, &oneDoc); err != nil {
		t.Fatalf("invalid one json: %v", err)
	}
	if oneDoc.SchemaVersion != 1 || len(oneDoc.Routes) != 4 || len(oneDoc.Nodes) != 3 {
		t.Fatalf("unexpected one payload: %+v", oneDoc)
	}
	if _, ok := oneDoc.Pools[routingpolicy.RoutingPoolCustomDedicated]; !ok {
		t.Fatal("expected Custom-Dedicated pool in One payload for whitelisted user")
	}

	// VLESS URIs must stay completely config-agnostic.
	vlessBytes, _, err := Render(FormatVLESS, RenderInput{AllowCustom: true, AllowPro: true, Nodes: sampleNodes()})
	if err != nil {
		t.Fatalf("vless render failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(vlessBytes)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 vless URIs, got %d", len(lines))
	}
	for _, rawURI := range lines {
		u, err := url.Parse(rawURI)
		if err != nil {
			t.Fatalf("invalid vless URI %q: %v", rawURI, err)
		}
		if u.Scheme != "vless" {
			t.Fatalf("unexpected scheme: %q", u.Scheme)
		}
		q := u.Query()
		for _, forbidden := range []string{"routing_pool", "pricing_group", "media_on_pro_action", "sub_pool_tags", "routes"} {
			if q.Has(forbidden) {
				t.Fatalf("vless URI %q leaked config metadata key %q", rawURI, forbidden)
			}
		}
		if strings.Contains(u.Fragment, "Media-Pool") || strings.Contains(u.Fragment, "Pro-EC2-Pool") {
			t.Fatalf("vless URI fragment %q should stay config-agnostic", u.Fragment)
		}
	}
}

func TestPlusAndFreeSubscriptionsHideProNodes(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for _, format := range []Format{FormatClash, FormatSingBox, FormatXray, FormatOne, FormatVLESS} {
		data, _, err := Render(format, RenderInput{
			UserID:      "u-plus",
			AllowCustom: false,
			AllowPro:    false,
			Nodes:       sampleNodes(),
			GeneratedAt: now,
		})
		if err != nil {
			t.Fatalf("render %s failed: %v", format, err)
		}
		text := string(data)
		if strings.Contains(text, "jp-xconnect-02.svc.plus") {
			t.Fatalf("format %s leaked Pro-EC2-Pool node to Plus/Free user:\n%s", format, text)
		}
		if strings.Contains(text, "us-xconnect-los-angeles-custom-01.svc.plus") || strings.Contains(text, routingpolicy.RoutingPoolCustomDedicated) {
			t.Fatalf("format %s leaked Custom-Dedicated node/group to Plus/Free user:\n%s", format, text)
		}
		if format == FormatOne {
			var doc onePayload
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatalf("invalid one json: %v", err)
			}
			if _, ok := doc.Pools[routingpolicy.RoutingPoolProEC2]; ok {
				t.Fatalf("One payload leaked Pro-EC2-Pool to Plus/Free user: %+v", doc.Pools)
			}
			if len(doc.Nodes) != 1 || doc.Nodes[0].RoutingPool != routingpolicy.RoutingPoolMediaPriority {
				t.Fatalf("One payload should only include Media-Pool nodes: %+v", doc.Nodes)
			}
		} else if strings.Contains(text, routingpolicy.RoutingPoolProEC2) {
			t.Fatalf("format %s leaked Pro-EC2-Pool group/rule to Plus/Free user:\n%s", format, text)
		}
	}
}
