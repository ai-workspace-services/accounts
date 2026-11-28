package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

const defaultStripeProfileName = "hk"

var stripeProfilePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// StripeProfileConfig defines one Stripe merchant account configuration inside
// STRIPE_PROFILES_JSON or STRIPE_PROFILES_FILE.
type StripeProfileConfig struct {
	AccountID             string   `json:"accountId,omitempty"`
	MerchantEntity        string   `json:"merchantEntity,omitempty"`
	Mode                  string   `json:"mode,omitempty"`
	SecretKey             string   `json:"secretKey,omitempty"`
	WebhookSecret         string   `json:"webhookSecret,omitempty"`
	AllowedPriceIDs       []string `json:"allowedPriceIds,omitempty"`
	FrontendURL           string   `json:"frontendUrl,omitempty"`
	PortalConfigurationID string   `json:"portalConfigurationId,omitempty"`
	APIBaseURL            string   `json:"apiBaseUrl,omitempty"`
}

type stripeProfilesFilePayload struct {
	DefaultProfile string                         `json:"defaultProfile,omitempty"`
	Profiles       map[string]StripeProfileConfig `json:"profiles"`
}

type stripeProfileSummary struct {
	Profile        string `json:"profile"`
	AccountID      string `json:"accountId,omitempty"`
	MerchantEntity string `json:"merchantEntity,omitempty"`
	Mode           string `json:"mode"`
	LiveMode       bool   `json:"liveMode"`
	Enabled        bool   `json:"enabled"`
	WebhookPath    string `json:"webhookPath"`
	IsDefault      bool   `json:"isDefault"`
}

func normalizeStripeProfile(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

func validStripeProfile(raw string) bool {
	return stripeProfilePattern.MatchString(normalizeStripeProfile(raw))
}

// WithStripeProfiles configures multiple named Stripe merchant profiles for tests or embedded wiring.
func WithStripeProfiles(defaultProfile string, profiles map[string]StripeConfig) Option {
	return func(h *handler) {
		h.configureStripeProfiles(defaultProfile, profiles)
	}
}

func (h *handler) configureStripe(baseCfg StripeConfig) {
	defaultProfile := normalizeStripeProfile(baseCfg.Profile)
	if defaultProfile == "" {
		defaultProfile = normalizeStripeProfile(os.Getenv("STRIPE_DEFAULT_PROFILE"))
	}
	if defaultProfile == "" {
		defaultProfile = defaultStripeProfileName
	}

	profiles := map[string]StripeConfig{}
	if payload, ok := loadStripeProfilesPayloadFromEnv(); ok {
		if envDefault := normalizeStripeProfile(payload.DefaultProfile); envDefault != "" && validStripeProfile(envDefault) {
			defaultProfile = envDefault
		}
		for rawName, pCfg := range payload.Profiles {
			name := normalizeStripeProfile(rawName)
			if !validStripeProfile(name) {
				slog.Warn("ignoring invalid stripe profile name", "profile", rawName)
				continue
			}
			merged := StripeConfig{
				Profile:               name,
				AccountID:             strings.TrimSpace(pCfg.AccountID),
				MerchantEntity:        strings.TrimSpace(pCfg.MerchantEntity),
				Mode:                  strings.TrimSpace(pCfg.Mode),
				SecretKey:             strings.TrimSpace(pCfg.SecretKey),
				WebhookSecret:         strings.TrimSpace(pCfg.WebhookSecret),
				AllowedPriceIDs:       pCfg.AllowedPriceIDs,
				FrontendURL:           firstNonEmpty(pCfg.FrontendURL, baseCfg.FrontendURL),
				PortalConfigurationID: strings.TrimSpace(pCfg.PortalConfigurationID),
				APIBaseURL:            firstNonEmpty(pCfg.APIBaseURL, baseCfg.APIBaseURL),
			}
			if merged.SecretKey == "" && name == defaultProfile {
				merged.SecretKey = strings.TrimSpace(baseCfg.SecretKey)
				merged.WebhookSecret = firstNonEmpty(merged.WebhookSecret, baseCfg.WebhookSecret)
				merged.Mode = firstNonEmpty(merged.Mode, baseCfg.Mode)
				merged.PortalConfigurationID = firstNonEmpty(merged.PortalConfigurationID, baseCfg.PortalConfigurationID)
				merged.AccountID = firstNonEmpty(merged.AccountID, baseCfg.AccountID)
				merged.MerchantEntity = firstNonEmpty(merged.MerchantEntity, baseCfg.MerchantEntity)
			}
			if len(merged.AllowedPriceIDs) == 0 && len(baseCfg.AllowedPriceIDs) > 0 {
				merged.AllowedPriceIDs = baseCfg.AllowedPriceIDs
			}
			profiles[name] = merged
		}
	}

	if len(profiles) == 0 {
		baseCfg.Profile = defaultProfile
		profiles[defaultProfile] = baseCfg
	} else if _, exists := profiles[defaultProfile]; !exists && strings.TrimSpace(baseCfg.SecretKey) != "" {
		baseCfg.Profile = defaultProfile
		profiles[defaultProfile] = baseCfg
	}

	h.configureStripeProfiles(defaultProfile, profiles)
}

func (h *handler) configureStripeProfiles(defaultProfile string, configs map[string]StripeConfig) {
	resolvedDefault := normalizeStripeProfile(defaultProfile)
	if !validStripeProfile(resolvedDefault) {
		resolvedDefault = defaultStripeProfileName
	}
	clients := make(map[string]*stripeClient, len(configs))
	for rawName, cfg := range configs {
		name := normalizeStripeProfile(firstNonEmpty(rawName, cfg.Profile))
		if !validStripeProfile(name) {
			continue
		}
		cfg.Profile = name
		if client := newStripeClient(cfg); client != nil {
			clients[name] = client
		}
	}
	if _, ok := clients[resolvedDefault]; !ok && len(clients) > 0 {
		// Keep a predictable default only if resolvedDefault wasn't present in an explicit map without it.
		if _, configured := configs[resolvedDefault]; !configured {
			keys := make([]string, 0, len(clients))
			for k := range clients {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			resolvedDefault = keys[0]
		}
	}
	h.stripeDefaultProfile = resolvedDefault
	h.stripeProfiles = clients
	h.stripe = clients[resolvedDefault]
}

func loadStripeProfilesPayloadFromEnv() (stripeProfilesFilePayload, bool) {
	var raw []byte
	if path := strings.TrimSpace(os.Getenv("STRIPE_PROFILES_FILE")); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Error("failed to read STRIPE_PROFILES_FILE", "path", path, "error", err)
			return stripeProfilesFilePayload{}, false
		}
		raw = data
	} else if inline := strings.TrimSpace(os.Getenv("STRIPE_PROFILES_JSON")); inline != "" {
		raw = []byte(inline)
	}
	if len(raw) == 0 {
		return stripeProfilesFilePayload{}, false
	}
	var payload stripeProfilesFilePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		slog.Error("failed to parse Stripe profiles JSON", "error", err)
		return stripeProfilesFilePayload{}, false
	}
	return payload, true
}

func (h *handler) defaultStripeProfile() string {
	if h != nil && validStripeProfile(h.stripeDefaultProfile) {
		return normalizeStripeProfile(h.stripeDefaultProfile)
	}
	return defaultStripeProfileName
}

func (h *handler) stripeClientForProfile(profile string) (*stripeClient, string) {
	if h == nil {
		return nil, defaultStripeProfileName
	}
	resolved := normalizeStripeProfile(profile)
	if resolved == "" {
		resolved = h.defaultStripeProfile()
	}
	if h.stripeProfiles != nil {
		if client, ok := h.stripeProfiles[resolved]; ok && client != nil {
			// Preserve compatibility if a test mutates h.stripe on the default profile.
			if resolved == h.defaultStripeProfile() && h.stripe != nil {
				return h.stripe, resolved
			}
			return client, resolved
		}
		if resolved == h.defaultStripeProfile() && h.stripe != nil {
			return h.stripe, resolved
		}
		return nil, resolved
	}
	if resolved == h.defaultStripeProfile() && h.stripe != nil {
		return h.stripe, resolved
	}
	return nil, resolved
}

func (h *handler) planStripeProfile(plan *store.BillingPlan) string {
	if plan != nil && plan.Features != nil {
		if raw, ok := plan.Features["stripe_profile"].(string); ok && validStripeProfile(raw) {
			return normalizeStripeProfile(raw)
		}
	}
	return h.defaultStripeProfile()
}

func (h *handler) stripeClientForPlan(plan *store.BillingPlan) (*stripeClient, string) {
	return h.stripeClientForProfile(h.planStripeProfile(plan))
}

func (h *handler) subscriptionStripeProfile(sub *store.Subscription) string {
	if sub != nil {
		if raw := strings.TrimSpace(subscriptionMetaString(sub.Meta, "stripe_profile")); validStripeProfile(raw) {
			return normalizeStripeProfile(raw)
		}
	}
	return h.defaultStripeProfile()
}

func (h *handler) stripeClientForSubscription(sub *store.Subscription) (*stripeClient, string) {
	return h.stripeClientForProfile(h.subscriptionStripeProfile(sub))
}

func (h *handler) webhookEventStorageKey(profile, eventID string) string {
	id := strings.TrimSpace(eventID)
	resolved := normalizeStripeProfile(profile)
	if resolved == "" || resolved == h.defaultStripeProfile() || id == "" {
		return id
	}
	return resolved + ":" + id
}

func (h *handler) adminListStripeProfiles(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminSettingsRead); !ok {
		return
	}
	defaultProfile := h.defaultStripeProfile()
	byName := map[string]*stripeClient{}
	for name, client := range h.stripeProfiles {
		byName[name] = client
	}
	if h.stripe != nil {
		if _, exists := byName[defaultProfile]; !exists {
			byName[defaultProfile] = h.stripe
		}
	}

	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)

	items := make([]stripeProfileSummary, 0, len(names))
	for _, name := range names {
		client := byName[name]
		if client == nil {
			continue
		}
		mode := "test"
		if client.liveMode {
			mode = "live"
		}
		items = append(items, stripeProfileSummary{
			Profile:        name,
			AccountID:      client.accountID,
			MerchantEntity: client.merchantEntity,
			Mode:           mode,
			LiveMode:       client.liveMode,
			Enabled:        client.enabled(),
			WebhookPath:    "/api/billing/stripe/webhook/" + name,
			IsDefault:      name == defaultProfile,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"defaultProfile": defaultProfile,
		"profiles":       items,
	})
}

type stripeReconciliationMeta struct {
	StripeProfile     string
	StripeAccountID   string
	BusinessLine      string
	ReconciliationTag string
	RegionScope       string
	MerchantEntity    string
	PlanFamily        string
}

func (h *handler) planReconciliationMeta(plan *store.BillingPlan, client *stripeClient) stripeReconciliationMeta {
	profile := h.planStripeProfile(plan)
	if profile == "" && client != nil {
		profile = normalizeStripeProfile(client.profile)
	}
	if profile == "" {
		profile = h.defaultStripeProfile()
	}
	family := normalizePlanFamily(plan)
	businessLine := billingPlanStringFeature(plan, "business_line")
	reconciliationTag := billingPlanStringFeature(plan, "reconciliation_tag")
	regionScope := billingPlanStringFeature(plan, "region_scope")
	merchantEntity := billingPlanStringFeature(plan, "merchant_entity")

	switch {
	case family == "plus":
		if businessLine == "" {
			businessLine = "hk_apac_plus"
		}
		if reconciliationTag == "" {
			reconciliationTag = "FIN-HK-PLUS-APAC"
		}
		if regionScope == "" {
			regionScope = "JP,PH,APAC"
		}
	case (family == "pro" || family == "custom") && profile == "us":
		if businessLine == "" {
			businessLine = "us_edge_pro"
		}
		if reconciliationTag == "" {
			reconciliationTag = "FIN-US-PRO-EDGE"
		}
		if regionScope == "" {
			regionScope = "US"
		}
	case family == "pro" || family == "custom":
		if businessLine == "" {
			businessLine = "hk_apac_" + family
		}
		if reconciliationTag == "" {
			reconciliationTag = "FIN-HK-" + strings.ToUpper(family)
		}
		if regionScope == "" {
			regionScope = "APAC"
		}
	}

	var accountID string
	if client != nil {
		accountID = strings.TrimSpace(client.accountID)
		if merchantEntity == "" {
			merchantEntity = strings.TrimSpace(client.merchantEntity)
		}
	}

	return stripeReconciliationMeta{
		StripeProfile:     profile,
		StripeAccountID:   accountID,
		BusinessLine:      businessLine,
		ReconciliationTag: reconciliationTag,
		RegionScope:       regionScope,
		MerchantEntity:    merchantEntity,
		PlanFamily:        family,
	}
}
