package subscription

import (
	"encoding/json"
	"time"

	"account/internal/routingpolicy"
)

type onePoolSummary struct {
	PublicResource   bool     `json:"public_resource"`
	MediaOnProAction string   `json:"media_on_pro_action"`
	FallbackDefault  bool     `json:"fallback_default"`
	PricingGroups    []string `json:"pricing_groups"`
	SubPoolTags      []string `json:"sub_pool_tags,omitempty"`
	Nodes            []Node   `json:"nodes"`
}

type onePayload struct {
	SchemaVersion  int                       `json:"schema_version"`
	Format         string                    `json:"format"`
	GeneratedAt    time.Time                 `json:"generated_at"`
	PolicyRevision string                    `json:"policy_revision"`
	Pools          map[string]onePoolSummary `json:"pools"`
	Nodes          []Node                    `json:"nodes"`
	Routes         []RouteRule               `json:"routes"`
	VlessURIs      []string                  `json:"vless_uris"`
}

func renderOne(input RenderInput, b nodeBuckets, policy routingpolicy.Policy) ([]byte, error) {
	generatedAt := input.GeneratedAt.UTC()
	if generatedAt.IsZero() {
		generatedAt = time.Now().UTC()
	}

	pools := make(map[string]onePoolSummary)
	addPool := func(name string, nodes []Node) {
		if len(nodes) == 0 {
			return
		}
		cfg, _ := policy.Pool(name)
		pools[name] = onePoolSummary{
			PublicResource:   policy.IsPublicPool(name),
			MediaOnProAction: policy.MediaOnProActionFor(name),
			FallbackDefault:  cfg.FallbackDefault,
			PricingGroups:    append([]string(nil), cfg.PricingGroups...),
			SubPoolTags:      append([]string(nil), cfg.SubPoolTags...),
			Nodes:            nodes,
		}
	}
	addPool(routingpolicy.RoutingPoolMediaPriority, b.media)
	addPool(routingpolicy.RoutingPoolProEC2, b.pro)
	if input.AllowCustom {
		addPool(routingpolicy.RoutingPoolCustomDedicated, b.custom)
	}

	uris := make([]string, 0, len(b.all))
	for _, node := range b.all {
		uris = append(uris, BuildVLESSURI(node))
	}

	routes := BuildThreeTierRoutes(policy, len(b.media) > 0, len(b.pro) > 0, input.AllowCustom && len(b.custom) > 0)
	payload := onePayload{
		SchemaVersion:  1,
		Format:         string(FormatOne),
		GeneratedAt:    generatedAt,
		PolicyRevision: policy.Revision,
		Pools:          pools,
		Nodes:          b.all,
		Routes:         routes,
		VlessURIs:      uris,
	}
	return json.MarshalIndent(payload, "", "  ")
}
