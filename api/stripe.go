package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

const stripeAPIBaseURL = "https://api.stripe.com/v1"
const stripeAPIVersion = "2026-01-28.clover"

// Stripe signs webhook payloads with a timestamp. Keep the same bounded
// tolerance used by Stripe's official SDKs so an old captured request cannot
// be replayed outside the event-id deduplication window.
const stripeWebhookTimestampTolerance = 5 * time.Minute

type StripeConfig struct {
	Mode                  string
	SecretKey             string
	WebhookSecret         string
	AllowedPriceIDs       []string
	FrontendURL           string
	PortalConfigurationID string
	// APIBaseURL overrides the Stripe REST base (default https://api.stripe.com/v1).
	// Only honoured for test-mode keys so a live deployment can never be pointed
	// at a mock; intended for egress-denied lanes and tests (STRIPE_API_BASE_URL).
	APIBaseURL string
}

type stripeClient struct {
	liveMode              bool
	configurationValid    bool
	secretKey             string
	webhookSecret         string
	frontendURL           string
	apiBaseURL            string
	allowedPriceID        map[string]struct{}
	httpClient            *http.Client
	portalConfigurationID string
}

type stripeCheckoutRequest struct {
	PlanID           string `json:"planId"`
	StripePriceID    string `json:"stripePriceId"`
	AmountMinor      int64  `json:"amountMinor,omitempty"`
	Currency         string `json:"currency,omitempty"`
	Mode             string `json:"mode"`
	ProductSlug      string `json:"productSlug"`
	SourcePath       string `json:"sourcePath"`
	PreviewToken     string `json:"previewToken,omitempty"`
	OperationKey     string `json:"-"`
	SessionExpiresAt int64  `json:"-"`
}

type stripePortalRequest struct {
	ReturnPath string `json:"returnPath"`
}

type stripeSessionResponse struct {
	URL string `json:"url"`
	ID  string `json:"id"`
}

type stripeCustomer struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

type stripeSubscription struct {
	ID                 string            `json:"id"`
	Status             string            `json:"status"`
	Customer           any               `json:"customer"`
	Metadata           map[string]string `json:"metadata"`
	CancelAtPeriodEnd  bool              `json:"cancel_at_period_end"`
	CurrentPeriodEnd   int64             `json:"current_period_end"`
	CurrentPeriodStart int64             `json:"current_period_start"`
	LatestInvoice      any               `json:"latest_invoice"`
	CollectionMethod   string            `json:"collection_method"`
	PendingUpdate      json.RawMessage   `json:"pending_update"`
	Schedule           any               `json:"schedule"`
	Items              struct {
		Data []struct {
			ID                 string `json:"id"`
			Quantity           int64  `json:"quantity"`
			CurrentPeriodStart int64  `json:"current_period_start"`
			CurrentPeriodEnd   int64  `json:"current_period_end"`
			Price              struct {
				ID string `json:"id"`
			} `json:"price"`
		} `json:"data"`
	} `json:"items"`
}

type stripeEvent struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Livemode *bool  `json:"livemode"`
	Account  string `json:"account"`
	Data     struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

type stripeCheckoutSession struct {
	ID                string `json:"id"`
	Mode              string `json:"mode"`
	ClientReferenceID string `json:"client_reference_id"`
	Subscription      string `json:"subscription"`
	PaymentIntent     string `json:"payment_intent"`
	Customer          string `json:"customer"`
	PaymentStatus     string `json:"payment_status"`
	Status            string `json:"status"`
	URL               string `json:"url"`
	// AmountTotal is in the currency's smallest unit (cents/分), which is how
	// Stripe reports every amount. It has to be divided by 100 before it means
	// anything in balance terms.
	AmountTotal int64             `json:"amount_total"`
	Currency    string            `json:"currency"`
	Metadata    map[string]string `json:"metadata"`
}

type stripeInvoice struct {
	AmountDue        int64  `json:"amount_due"`
	Total            int64  `json:"total"`
	HostedInvoiceURL string `json:"hosted_invoice_url"`
	ID               string `json:"id"`
	Customer         any    `json:"customer"`
	Subscription     any    `json:"subscription"`
	Status           string `json:"status"`
	PaymentIntent    any    `json:"payment_intent"`
	AmountPaid       int64  `json:"amount_paid"`
	PaidOutOfBand    bool   `json:"paid_out_of_band"`
	Currency         string `json:"currency"`
	Created          int64  `json:"created"`
	BillingReason    string `json:"billing_reason"`
	Parent           struct {
		SubscriptionDetails struct {
			Subscription any `json:"subscription"`
		} `json:"subscription_details"`
	} `json:"parent"`
	Payments struct {
		Data []struct {
			Status     string `json:"status"`
			AmountPaid int64  `json:"amount_paid"`
			Currency   string `json:"currency"`
			Payment    struct {
				Type          string `json:"type"`
				PaymentIntent any    `json:"payment_intent"`
			} `json:"payment"`
		} `json:"data"`
		HasMore bool `json:"has_more"`
	} `json:"payments"`
	StatusTransitions struct {
		PaidAt int64 `json:"paid_at"`
	} `json:"status_transitions"`
	Lines struct {
		Data []struct {
			Price  any `json:"price"`
			Period struct {
				Start int64 `json:"start"`
				End   int64 `json:"end"`
			} `json:"period"`
			Pricing struct {
				PriceDetails struct {
					Price any `json:"price"`
				} `json:"price_details"`
			} `json:"pricing"`
		} `json:"data"`
		HasMore bool `json:"has_more"`
	} `json:"lines"`
}

type stripeInvoiceList struct {
	Data    []stripeInvoice `json:"data"`
	HasMore bool            `json:"has_more"`
}

type stripeCharge struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	Invoice        any    `json:"invoice"`
	PaymentIntent  any    `json:"payment_intent"`
	Disputed       bool   `json:"disputed"`
	Refunded       bool   `json:"refunded"`
	Amount         int64  `json:"amount"`
	AmountRefunded int64  `json:"amount_refunded"`
	Refunds        struct {
		Data []struct {
			Status string `json:"status"`
		} `json:"data"`
	} `json:"refunds"`
}

type stripePaymentIntent struct {
	ID           string `json:"id"`
	LatestCharge any    `json:"latest_charge"`
}

type stripeDispute struct {
	ID      string `json:"id"`
	Charge  any    `json:"charge"`
	Reason  string `json:"reason"`
	Created int64  `json:"created"`
}

func newStripeClient(cfg StripeConfig) *stripeClient {
	secretKey := strings.TrimSpace(cfg.SecretKey)
	if secretKey == "" {
		return nil
	}
	liveMode, configurationValid := stripeRuntimeMode(secretKey, cfg.Mode)
	if !configurationValid {
		slog.Error("Stripe disabled: unsupported API key or conflicting payment mode")
	}

	allowed := make(map[string]struct{}, len(cfg.AllowedPriceIDs))
	for _, priceID := range cfg.AllowedPriceIDs {
		trimmed := strings.TrimSpace(priceID)
		if trimmed != "" {
			allowed[trimmed] = struct{}{}
		}
	}

	rawBaseURL := cfg.APIBaseURL
	if strings.TrimSpace(rawBaseURL) == "" {
		// Read here rather than in cmd/accountsvc so the native schema source
		// manifest (which hashes main.go) is unaffected.
		rawBaseURL = os.Getenv("STRIPE_API_BASE_URL")
	}
	apiBaseURL, baseErr := resolveStripeAPIBaseURL(rawBaseURL, liveMode)
	if baseErr != nil {
		slog.Error("Stripe disabled: invalid STRIPE_API_BASE_URL", "error", baseErr)
		configurationValid = false
	} else if apiBaseURL != "" {
		slog.Warn("Stripe API base URL overridden for test mode", "baseURL", apiBaseURL)
	}

	return &stripeClient{
		apiBaseURL:            apiBaseURL,
		liveMode:              liveMode,
		configurationValid:    configurationValid,
		secretKey:             secretKey,
		webhookSecret:         strings.TrimSpace(cfg.WebhookSecret),
		frontendURL:           strings.TrimRight(strings.TrimSpace(cfg.FrontendURL), "/"),
		allowedPriceID:        allowed,
		portalConfigurationID: strings.TrimSpace(cfg.PortalConfigurationID),
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

// resolveStripeAPIBaseURL validates an optional Stripe API base override.
// Empty means the real Stripe API. Live keys refuse any non-default value.
func resolveStripeAPIBaseURL(raw string, liveMode bool) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" || trimmed == stripeAPIBaseURL {
		return "", nil
	}
	if liveMode {
		return "", errors.New("STRIPE_API_BASE_URL override is not allowed with a live Stripe key")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("STRIPE_API_BASE_URL must be an absolute http(s) URL without credentials, query or fragment")
	}
	return trimmed, nil
}

func (c *stripeClient) enabled() bool {
	return c != nil && c.secretKey != "" && c.configurationValid
}

// Account secret and restricted keys encode their Stripe mode. Organization
// keys require an account/context contract that this integration does not yet
// support, so an unknown key must not silently select a mode.
func stripeRuntimeMode(key, configured string) (live, valid bool) {
	if len(key) <= len("sk_test_") {
		return false, false
	}
	switch {
	case strings.HasPrefix(key, "sk_test_"), strings.HasPrefix(key, "rk_test_"):
		live = false
	case strings.HasPrefix(key, "sk_live_"), strings.HasPrefix(key, "rk_live_"):
		live = true
	default:
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(configured)) {
	case "":
		return live, true
	case "test", "sandbox":
		return live, !live
	case "live":
		return live, live
	default:
		return false, false
	}
}

func validStripeClientReferenceID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 200 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') &&
			(char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && char != '-' && char != '_' {
			return false
		}
	}
	return true
}

func (c *stripeClient) signedClientReferenceID(userID string) (string, error) {
	userID = strings.TrimSpace(userID)
	if !validStripeClientReferenceID(userID) || strings.TrimSpace(c.webhookSecret) == "" {
		return "", errors.New("cannot sign stripe client reference")
	}
	mac := hmac.New(sha256.New, []byte(c.webhookSecret))
	_, _ = mac.Write([]byte(userID))
	encodedUserID := base64.RawURLEncoding.EncodeToString([]byte(userID))
	return encodedUserID + "_" + hex.EncodeToString(mac.Sum(nil)), nil
}

func (c *stripeClient) userIDFromClientReference(reference string) string {
	if c == nil || strings.TrimSpace(c.webhookSecret) == "" {
		return ""
	}
	separator := strings.LastIndex(strings.TrimSpace(reference), "_")
	if separator <= 0 || separator == len(strings.TrimSpace(reference))-1 {
		return ""
	}
	encodedUserID := strings.TrimSpace(reference)[:separator]
	providedSignature := strings.TrimSpace(reference)[separator+1:]
	userIDBytes, err := base64.RawURLEncoding.DecodeString(encodedUserID)
	if err != nil {
		return ""
	}
	userID := string(userIDBytes)
	if !validStripeClientReferenceID(userID) {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(c.webhookSecret))
	_, _ = mac.Write([]byte(userID))
	if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(providedSignature)) {
		return ""
	}
	return userID
}

func (c *stripeClient) validPriceID(priceID string) bool {
	priceID = strings.TrimSpace(priceID)
	if priceID == "" || !strings.HasPrefix(priceID, "price_") {
		return false
	}
	if len(c.allowedPriceID) == 0 {
		return true
	}
	_, ok := c.allowedPriceID[priceID]
	return ok
}

// validCheckoutPrice prefers the billing_plans catalog (billing P1): a price
// is purchasable when an active plan carries it. The STRIPE_ALLOWED_PRICE_IDS
// env allowlist remains as a bootstrap fallback while the catalog is empty.
func (h *handler) validCheckoutPrice(ctx context.Context, priceID string) bool {
	trimmed := strings.TrimSpace(priceID)
	if trimmed == "" || !strings.HasPrefix(trimmed, "price_") {
		return false
	}
	plan, err := h.store.GetBillingPlanByPriceID(ctx, trimmed)
	if err == nil {
		return plan.Active
	}
	if !errors.Is(err, store.ErrBillingPlanNotFound) {
		slog.Warn("billing plan lookup failed during checkout validation", "err", err, "priceID", trimmed)
		return false
	}
	// Catalog has no entry for this price: only allow via the legacy env
	// allowlist when no catalog price exists at all (bootstrap mode).
	plans, err := h.store.ListBillingPlans(ctx, false)
	if err != nil {
		slog.Warn("billing plan list failed during checkout validation", "err", err)
		return false
	}
	for _, p := range plans {
		if strings.TrimSpace(p.StripePriceID) != "" {
			return false // catalog is authoritative once any active price exists
		}
	}
	return h.stripe.validPriceID(trimmed)
}

// validateCheckoutPlan binds all client-supplied checkout fields to one
// catalog row. A Stripe price alone is not sufficient: without this check a
// caller could submit a subscription price using mode=payment, which would
// make a recurring purchase look like a PAYG top-up when its webhook arrives.
//
// The legacy allowlist remains usable only while the catalog is completely
// empty. Once a catalog exists, it is the sole authority for a plan's price
// and commercial shape.
func (h *handler) validateCheckoutPlan(ctx context.Context, req stripeCheckoutRequest) bool {
	planID := strings.TrimSpace(req.PlanID)
	priceID := strings.TrimSpace(req.StripePriceID)
	mode := h.stripe.normalizeMode(req.Mode)
	if mode == "payment" && isCustomPaygoTopUp(planID, req.AmountMinor, req.Currency) {
		return priceID == ""
	}
	if planID == "" || priceID == "" {
		return false
	}

	plan, err := h.store.GetBillingPlan(ctx, planID)
	if err == nil {
		if !plan.Active || strings.TrimSpace(plan.StripePriceID) != priceID {
			return false
		}
		switch strings.ToLower(strings.TrimSpace(plan.Kind)) {
		case "paygo_topup":
			return mode == "payment" && isFixedPaygoTopUp(plan.PlanID, plan.PriceAmount, plan.PriceCurrency, plan.PriceUnit)
		case "subscription":
			return mode == "subscription"
		default:
			return false
		}
	}
	if !errors.Is(err, store.ErrBillingPlanNotFound) {
		slog.Warn("billing plan lookup failed during checkout validation", "err", err, "planID", planID)
		return false
	}

	// Bootstrap compatibility for installations upgraded before their plan
	// catalog was seeded. It deliberately does not infer PAYG eligibility:
	// a one-time payment must have an explicit paygo_topup catalog record
	// before it can ever credit a balance.
	plans, listErr := h.store.ListBillingPlans(ctx, false)
	if listErr != nil {
		slog.Warn("billing plan list failed during checkout validation", "err", listErr)
		return false
	}
	if len(plans) != 0 {
		return false
	}
	return mode == "subscription" && h.stripe.validPriceID(priceID)
}

func (c *stripeClient) normalizeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "payment":
		return "payment"
	default:
		return "subscription"
	}
}

func (c *stripeClient) buildFrontendURL(path string) string {
	base := c.frontendURL
	if base == "" {
		base = "https://console.svc.plus"
	}
	if path == "" {
		return base
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

func (c *stripeClient) checkoutURLs(sourcePath string) (string, string) {
	cancelPath := strings.TrimSpace(sourcePath)
	if cancelPath == "" || !strings.HasPrefix(cancelPath, "/") {
		cancelPath = "/prices"
	}

	successURL := c.buildFrontendURL("/panel/subscription?checkout=success&session_id={CHECKOUT_SESSION_ID}")
	if strings.Contains(cancelPath, "?") {
		cancelPath += "&checkout=cancelled"
	} else {
		cancelPath += "?checkout=cancelled"
	}
	return successURL, c.buildFrontendURL(cancelPath)
}

func (c *stripeClient) doForm(ctx context.Context, method, path string, form url.Values, out any, idempotencyKey ...string) error {
	if !c.enabled() {
		return errors.New("Stripe configuration is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL()+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.secretKey)
	req.Header.Set("Stripe-Version", stripeAPIVersion)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if len(idempotencyKey) > 0 && idempotencyKey[0] != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey[0])
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("stripe %s %s failed: %s", method, path, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

func (c *stripeClient) doJSON(ctx context.Context, method, path string, out any) error {
	if !c.enabled() {
		return errors.New("Stripe configuration is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL()+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.secretKey)
	req.Header.Set("Stripe-Version", stripeAPIVersion)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("stripe %s %s failed: %s", method, path, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

func (c *stripeClient) createCheckoutSession(ctx context.Context, user *store.User, req stripeCheckoutRequest) (*stripeSessionResponse, error) {
	mode := c.normalizeMode(req.Mode)
	successURL, cancelURL := c.checkoutURLs(req.SourcePath)
	form := url.Values{
		"mode":                    []string{mode},
		"success_url":             []string{successURL},
		"cancel_url":              []string{cancelURL},
		"customer_email":          []string{strings.TrimSpace(strings.ToLower(user.Email))},
		"line_items[0][quantity]": []string{"1"},
		"metadata[user_id]":       []string{strings.TrimSpace(user.ID)},
		"metadata[user_email]":    []string{strings.TrimSpace(strings.ToLower(user.Email))},
		"metadata[plan_id]":       []string{strings.TrimSpace(req.PlanID)},
		"metadata[product_slug]":  []string{strings.TrimSpace(req.ProductSlug)},
		"metadata[kind]":          []string{map[string]string{"payment": "paygo", "subscription": "subscription"}[mode]},
	}
	if isCustomPaygoTopUp(req.PlanID, req.AmountMinor, req.Currency) {
		currency := strings.ToLower(strings.TrimSpace(req.Currency))
		form.Set("line_items[0][price_data][currency]", currency)
		form.Set("line_items[0][price_data][unit_amount]", strconv.FormatInt(req.AmountMinor, 10))
		form.Set("line_items[0][price_data][product_data][name]", "XConnect balance top-up")
		form.Set("metadata[amount_minor]", strconv.FormatInt(req.AmountMinor, 10))
		form.Set("metadata[currency]", strings.ToUpper(currency))
	} else {
		form.Set("line_items[0][price]", strings.TrimSpace(req.StripePriceID))
	}
	clientReferenceID, err := c.signedClientReferenceID(user.ID)
	if err == nil {
		form.Set("client_reference_id", clientReferenceID)
	}
	if mode == "subscription" {
		expires := req.SessionExpiresAt
		if expires == 0 {
			expires = time.Now().Add(time.Hour).Unix()
		}
		form.Set("expires_at", strconv.FormatInt(expires, 10))
		form.Set("metadata[purchase_operation_key]", req.OperationKey)
		form.Set("subscription_data[metadata][purchase_operation_key]", req.OperationKey)
		form.Set("subscription_data[metadata][user_id]", strings.TrimSpace(user.ID))
		form.Set("subscription_data[metadata][user_email]", strings.TrimSpace(strings.ToLower(user.Email)))
		form.Set("subscription_data[metadata][plan_id]", strings.TrimSpace(req.PlanID))
		form.Set("subscription_data[metadata][product_slug]", strings.TrimSpace(req.ProductSlug))
		form.Set("subscription_data[metadata][kind]", "subscription")
	} else {
		form.Set("payment_intent_data[metadata][user_id]", strings.TrimSpace(user.ID))
		form.Set("payment_intent_data[metadata][user_email]", strings.TrimSpace(strings.ToLower(user.Email)))
		form.Set("payment_intent_data[metadata][plan_id]", strings.TrimSpace(req.PlanID))
		form.Set("payment_intent_data[metadata][product_slug]", strings.TrimSpace(req.ProductSlug))
		form.Set("payment_intent_data[metadata][kind]", "paygo")
	}

	var session stripeSessionResponse
	if err := c.doForm(ctx, http.MethodPost, "/checkout/sessions", form, &session, req.OperationKey); err != nil {
		return nil, err
	}
	return &session, nil
}

func (c *stripeClient) listCustomersByEmail(ctx context.Context, email string) ([]stripeCustomer, error) {
	var payload struct {
		Data []stripeCustomer `json:"data"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/customers?email="+url.QueryEscape(strings.TrimSpace(email))+"&limit=1", &payload); err != nil {
		return nil, err
	}
	return payload.Data, nil
}

func (c *stripeClient) createPortalSession(ctx context.Context, customerID, returnURL string) (*stripeSessionResponse, error) {
	if !strings.HasPrefix(c.portalConfigurationID, "bpc_") {
		return nil, errors.New("reviewed read-only billing portal configuration is required")
	}
	var configuration struct {
		ID       string `json:"id"`
		Active   bool   `json:"active"`
		Features struct {
			SubscriptionUpdate struct {
				Enabled *bool `json:"enabled"`
			} `json:"subscription_update"`
			SubscriptionCancel struct {
				Enabled *bool `json:"enabled"`
			} `json:"subscription_cancel"`
			PaymentMethodUpdate struct {
				Enabled *bool `json:"enabled"`
			} `json:"payment_method_update"`
			InvoiceHistory struct {
				Enabled *bool `json:"enabled"`
			} `json:"invoice_history"`
			CustomerUpdate struct {
				Enabled *bool `json:"enabled"`
			} `json:"customer_update"`
		} `json:"features"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/billing_portal/configurations/"+url.PathEscape(c.portalConfigurationID), &configuration); err != nil {
		return nil, err
	}
	f := configuration.Features
	if configuration.ID != c.portalConfigurationID || !configuration.Active || f.SubscriptionUpdate.Enabled == nil || *f.SubscriptionUpdate.Enabled || f.SubscriptionCancel.Enabled == nil || *f.SubscriptionCancel.Enabled || f.CustomerUpdate.Enabled == nil || *f.CustomerUpdate.Enabled || f.InvoiceHistory.Enabled == nil || !*f.InvoiceHistory.Enabled || f.PaymentMethodUpdate.Enabled == nil || !*f.PaymentMethodUpdate.Enabled {
		return nil, errors.New("billing portal configuration permits unreviewed operations or lacks required features")
	}
	form := url.Values{
		"customer":      []string{strings.TrimSpace(customerID)},
		"return_url":    []string{returnURL},
		"configuration": {c.portalConfigurationID},
	}
	var session stripeSessionResponse
	if err := c.doForm(ctx, http.MethodPost, "/billing_portal/sessions", form, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

func (c *stripeClient) cancelSubscription(ctx context.Context, subscriptionID string) error {
	return c.doForm(ctx, http.MethodDelete, "/subscriptions/"+url.PathEscape(strings.TrimSpace(subscriptionID)), url.Values{}, nil)
}

func (c *stripeClient) scheduleSubscriptionCancellation(ctx context.Context, subscriptionID string) (*stripeSubscription, error) {
	form := url.Values{"cancel_at_period_end": []string{"true"}}
	var subscription stripeSubscription
	if err := c.doForm(ctx, http.MethodPost, "/subscriptions/"+url.PathEscape(strings.TrimSpace(subscriptionID)), form, &subscription); err != nil {
		return nil, err
	}
	return &subscription, nil
}

func (c *stripeClient) fetchSubscription(ctx context.Context, subscriptionID string) (*stripeSubscription, error) {
	var sub stripeSubscription
	if err := c.doJSON(ctx, http.MethodGet, "/subscriptions/"+url.PathEscape(strings.TrimSpace(subscriptionID)), &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

func (c *stripeClient) fetchInvoice(ctx context.Context, invoiceID string) (*stripeInvoice, error) {
	var invoice stripeInvoice
	query := url.Values{}
	query.Add("expand[]", "payments")
	if err := c.doJSON(ctx, http.MethodGet, "/invoices/"+url.PathEscape(strings.TrimSpace(invoiceID))+"?"+query.Encode(), &invoice); err != nil {
		return nil, err
	}
	return &invoice, nil
}

func invoiceSubscriptionID(invoice *stripeInvoice) string {
	return firstNonEmpty(customerIDFromAny(invoice.Subscription), customerIDFromAny(invoice.Parent.SubscriptionDetails.Subscription))
}

func (c *stripeClient) listPaidInvoices(ctx context.Context, customerID string) ([]stripeInvoice, error) {
	var all []stripeInvoice
	startingAfter := ""
	for page := 0; page < 100; page++ {
		query := url.Values{"customer": {strings.TrimSpace(customerID)}, "status": {"paid"}, "limit": {"100"}}
		query.Add("expand[]", "data.payments")
		if startingAfter != "" {
			query.Set("starting_after", startingAfter)
		}
		var result stripeInvoiceList
		if err := c.doJSON(ctx, http.MethodGet, "/invoices?"+query.Encode(), &result); err != nil {
			return nil, err
		}
		all = append(all, result.Data...)
		if !result.HasMore {
			return all, nil
		}
		if len(result.Data) == 0 {
			return nil, errors.New("Stripe invoice history page is empty but has_more is true")
		}
		startingAfter = result.Data[len(result.Data)-1].ID
	}
	return nil, errors.New("Stripe paid invoice history exceeds the bounded referral verification limit")
}

func (c *stripeClient) fetchCharge(ctx context.Context, chargeID string) (*stripeCharge, error) {
	var charge stripeCharge
	if err := c.doJSON(ctx, http.MethodGet, "/charges/"+url.PathEscape(strings.TrimSpace(chargeID)), &charge); err != nil {
		return nil, err
	}
	return &charge, nil
}

func (c *stripeClient) fetchPaymentIntentCharge(ctx context.Context, paymentIntentID string) (*stripeCharge, error) {
	var intent stripePaymentIntent
	query := url.Values{}
	query.Add("expand[]", "latest_charge")
	if err := c.doJSON(ctx, http.MethodGet, "/payment_intents/"+url.PathEscape(strings.TrimSpace(paymentIntentID))+"?"+query.Encode(), &intent); err != nil {
		return nil, err
	}
	chargeID := customerIDFromAny(intent.LatestCharge)
	if chargeID == "" {
		return nil, errors.New("Stripe payment intent has no latest charge")
	}
	if expanded, ok := intent.LatestCharge.(map[string]any); ok {
		body, err := json.Marshal(expanded)
		if err != nil {
			return nil, err
		}
		var charge stripeCharge
		if err = json.Unmarshal(body, &charge); err != nil {
			return nil, err
		}
		if charge.ID == "" {
			charge.ID = chargeID
		}
		return &charge, nil
	}
	return c.fetchCharge(ctx, chargeID)
}

func (c *stripeClient) baseURL() string {
	if base := strings.TrimRight(strings.TrimSpace(c.apiBaseURL), "/"); base != "" {
		return base
	}
	return stripeAPIBaseURL
}

func (c *stripeClient) refundPaymentIntent(ctx context.Context, paymentIntentID, idempotencyKey string, amount int64) (*stripeRefund, error) {
	if !c.enabled() {
		return nil, errors.New("Stripe configuration is unavailable")
	}
	form := url.Values{"payment_intent": {strings.TrimSpace(paymentIntentID)}, "amount": {strconv.FormatInt(amount, 10)}, "metadata[finance_operation_key]": {idempotencyKey}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL()+"/refunds", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.secretKey)
	req.Header.Set("Stripe-Version", stripeAPIVersion)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// A retry after Stripe accepted the refund but before Accounts persisted its
	// local cancellation must return the original refund, never create another.
	if key := strings.TrimSpace(idempotencyKey); key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("stripe refund failed: %s", strings.TrimSpace(string(body)))
	}
	var refund stripeRefund
	if err := json.Unmarshal(body, &refund); err != nil {
		return nil, err
	}
	return &refund, nil
}

func (c *stripeClient) verifyWebhook(payload []byte, signatureHeader string) bool {
	if c.webhookSecret == "" {
		return false
	}
	parts := strings.Split(signatureHeader, ",")
	var timestamp string
	var signatures []string
	for _, part := range parts {
		piece := strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(piece, "t="):
			timestamp = strings.TrimPrefix(piece, "t=")
		case strings.HasPrefix(piece, "v1="):
			signatures = append(signatures, strings.TrimPrefix(piece, "v1="))
		}
	}
	if timestamp == "" || len(signatures) == 0 {
		return false
	}
	timestampValue, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	signedAt := time.Unix(timestampValue, 0)
	now := time.Now()
	if now.Sub(signedAt) > stripeWebhookTimestampTolerance || signedAt.Sub(now) > stripeWebhookTimestampTolerance {
		return false
	}

	signedPayload := timestamp + "." + string(payload)
	mac := hmac.New(sha256.New, []byte(c.webhookSecret))
	_, _ = mac.Write([]byte(signedPayload))
	expected := hex.EncodeToString(mac.Sum(nil))
	for _, candidate := range signatures {
		if hmac.Equal([]byte(expected), []byte(candidate)) {
			return true
		}
	}
	return false
}

func epochToRFC3339(value int64) string {
	if value <= 0 {
		return ""
	}
	return time.Unix(value, 0).UTC().Format(time.RFC3339)
}

func customerIDFromAny(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		if id, ok := typed["id"].(string); ok {
			return strings.TrimSpace(id)
		}
	}
	return ""
}

func buildStripeMeta(base map[string]any, additions map[string]string) map[string]any {
	meta := make(map[string]any, len(base)+len(additions))
	for key, value := range base {
		meta[key] = value
	}
	for key, value := range additions {
		if strings.TrimSpace(value) != "" {
			meta[key] = strings.TrimSpace(value)
		}
	}
	return meta
}

func (h *handler) stripeCheckout(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}
	if h.isReadOnlyAccount(user) {
		respondError(c, http.StatusForbidden, "read_only_account", "demo account is read-only")
		return
	}
	if h.stripe == nil || !h.stripe.enabled() {
		respondError(c, http.StatusServiceUnavailable, "stripe_not_configured", "stripe is not configured")
		return
	}

	var req stripeCheckoutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}
	req.PlanID = strings.TrimSpace(req.PlanID)
	req.StripePriceID = strings.TrimSpace(req.StripePriceID)
	req.Currency = strings.ToUpper(strings.TrimSpace(req.Currency))
	req.ProductSlug = strings.TrimSpace(req.ProductSlug)
	req.SourcePath = strings.TrimSpace(req.SourcePath)
	req.Mode = h.stripe.normalizeMode(req.Mode)

	if req.ProductSlug == "" || !h.validateCheckoutPlan(c.Request.Context(), req) {
		respondError(c, http.StatusBadRequest, "invalid_billing_plan", "billing plan is invalid or unavailable")
		return
	}
	if modeErr := h.validateBillingModeCheckout(c.Request.Context(), user.ID, req); modeErr != nil {
		respondError(c, http.StatusConflict, "billing_mode_conflict", modeErr.Error())
		return
	}
	if req.Mode == "subscription" {
		h.executeSubscriptionPurchase(c, user, req)
		return
	}
	if pending, err := h.store.PendingSubscriptionPurchase(c.Request.Context(), user.ID); err != nil || pending != nil {
		respondError(c, http.StatusConflict, "purchase_pending", "Finish subscription payment reconciliation before changing billing mode")
		return
	}

	session, err := h.stripe.createCheckoutSession(c.Request.Context(), user, req)
	if err != nil {
		respondError(c, http.StatusBadGateway, "stripe_checkout_failed", "failed to create stripe checkout session")
		return
	}

	c.JSON(http.StatusOK, gin.H{"url": session.URL, "id": session.ID})
}

func (h *handler) stripePortal(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}
	if h.isReadOnlyAccount(user) {
		respondError(c, http.StatusForbidden, "read_only_account", "demo account is read-only")
		return
	}
	if h.stripe == nil || !h.stripe.enabled() {
		respondError(c, http.StatusServiceUnavailable, "stripe_not_configured", "stripe is not configured")
		return
	}

	var req stripePortalRequest
	_ = c.ShouldBindJSON(&req)
	returnURL := h.stripe.buildFrontendURL("/panel/subscription")
	if path := strings.TrimSpace(req.ReturnPath); path != "" && strings.HasPrefix(path, "/") {
		returnURL = h.stripe.buildFrontendURL(path)
	}

	customers, err := h.stripe.listCustomersByEmail(c.Request.Context(), user.Email)
	if err != nil || len(customers) == 0 {
		respondError(c, http.StatusNotFound, "stripe_customer_not_found", "stripe customer not found")
		return
	}

	session, err := h.stripe.createPortalSession(c.Request.Context(), customers[0].ID, returnURL)
	if err != nil {
		respondError(c, http.StatusBadGateway, "stripe_portal_failed", "failed to create stripe portal session")
		return
	}

	c.JSON(http.StatusOK, gin.H{"url": session.URL, "id": session.ID})
}

func (h *handler) stripeWebhook(c *gin.Context) {
	if h.stripe == nil || !h.stripe.enabled() {
		respondError(c, http.StatusServiceUnavailable, "stripe_not_configured", "stripe is not configured")
		return
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "failed to read request body")
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewBuffer(body))

	if !h.stripe.verifyWebhook(body, c.GetHeader("Stripe-Signature")) {
		respondError(c, http.StatusUnauthorized, "invalid_signature", "stripe signature verification failed")
		return
	}

	var event stripeEvent
	if err := json.Unmarshal(body, &event); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid stripe event payload")
		return
	}
	if strings.TrimSpace(event.ID) == "" || strings.TrimSpace(event.Type) == "" || event.Livemode == nil {
		respondError(c, http.StatusBadRequest, "invalid_stripe_event", "event ID, type and payment mode are required")
		return
	}
	if *event.Livemode != h.stripe.liveMode {
		respondError(c, http.StatusBadRequest, "stripe_mode_mismatch", "event payment mode does not match the configured Stripe mode")
		return
	}
	if strings.TrimSpace(event.Account) != "" {
		respondError(c, http.StatusBadRequest, "stripe_account_mismatch", "connected account events are not supported by this endpoint")
		return
	}

	// Persist the event before processing: replays of already-processed
	// events are acknowledged without side effects, and failures leave an
	// auditable record (billing P1).
	tracked := strings.TrimSpace(event.ID) != ""
	if tracked {
		alreadyProcessed, err := h.store.BeginStripeWebhookEvent(c.Request.Context(), &store.StripeWebhookEvent{
			EventID:   event.ID,
			EventType: event.Type,
			Payload:   json.RawMessage(body),
		})
		if err != nil {
			respondError(c, http.StatusInternalServerError, "stripe_event_persist_failed", "failed to record stripe event")
			return
		}
		if alreadyProcessed {
			// A previous release may have acknowledged a refund event before its
			// entitlement postcondition was repaired. Reconcile only derived
			// access state from already-settled, locally recorded refund facts.
			if err := h.reconcileProcessedStripeRefund(c.Request.Context(), event); err != nil {
				respondError(c, http.StatusBadGateway, "stripe_refund_reconcile_failed", "failed to reconcile refunded subscription access")
				return
			}
			c.JSON(http.StatusOK, gin.H{"received": true, "duplicate": true})
			return
		}
	}

	procErr := h.handleStripeEvent(c.Request.Context(), event)
	if tracked {
		if err := h.store.FinishStripeWebhookEvent(c.Request.Context(), event.ID, procErr); err != nil {
			slog.Warn("failed to finalize stripe webhook event record", "err", err, "eventID", event.ID)
			respondError(c, http.StatusServiceUnavailable, "stripe_event_finalize_failed", "failed to persist stripe event processing result")
			return
		}
	}
	if procErr != nil {
		respondError(c, http.StatusBadGateway, "stripe_webhook_failed", "failed to process stripe event")
		return
	}

	c.JSON(http.StatusOK, gin.H{"received": true})
}

func (h *handler) handleStripeEvent(ctx context.Context, event stripeEvent) error {
	switch event.Type {
	case "checkout.session.completed":
		var session stripeCheckoutSession
		if err := json.Unmarshal(event.Data.Object, &session); err != nil {
			return err
		}
		if session.Subscription != "" {
			sub, err := h.stripe.fetchSubscription(ctx, session.Subscription)
			if err != nil {
				return err
			}
			if strings.TrimSpace(sub.Metadata["user_id"]) == "" {
				if userID := h.stripe.userIDFromClientReference(session.ClientReferenceID); userID != "" {
					if sub.Metadata == nil {
						sub.Metadata = make(map[string]string)
					}
					sub.Metadata["user_id"] = userID
					sub.Metadata["kind"] = "subscription"
				}
			}
			if err := h.upsertStripeSubscription(ctx, sub, session.Customer); err != nil {
				return err
			}
			return h.syncSubscriptionEntitlements(ctx, sub, true)
		}

		userID := strings.TrimSpace(session.Metadata["user_id"])
		if userID == "" {
			userID = h.stripe.userIDFromClientReference(session.ClientReferenceID)
		}
		if userID == "" {
			return nil
		}
		sub := &store.Subscription{
			UserID:        userID,
			Provider:      "stripe",
			PaymentMethod: "stripe",
			Kind:          strings.TrimSpace(session.Metadata["kind"]),
			PlanID:        strings.TrimSpace(session.Metadata["plan_id"]),
			ExternalID:    firstNonEmpty(session.PaymentIntent, session.ID),
			Status:        firstNonEmpty(session.PaymentStatus, "active"),
			Meta: buildStripeMeta(nil, map[string]string{
				"price_id":     "",
				"customer_id":  session.Customer,
				"session_id":   session.ID,
				"product_slug": session.Metadata["product_slug"],
				"user_email":   session.Metadata["user_email"],
			}),
		}
		if err := h.store.UpsertSubscription(ctx, sub); err != nil {
			return err
		}
		return h.creditTopUpBalance(ctx, userID, &session)
	case "customer.subscription.created", "customer.subscription.updated":
		var subscription stripeSubscription
		if err := json.Unmarshal(event.Data.Object, &subscription); err != nil {
			return err
		}
		// Webhooks can arrive out of order. Re-read Stripe's current item and
		// invoice instead of replaying an obsolete plan from an older event.
		current, err := h.stripe.fetchSubscription(ctx, subscription.ID)
		if err != nil {
			return err
		}
		if current.ID != subscription.ID {
			return errors.New("subscription identity changed")
		}
		subscription = *current
		if err := h.upsertStripeSubscription(ctx, &subscription, customerIDFromAny(subscription.Customer)); err != nil {
			return err
		}
		return h.syncSubscriptionEntitlements(ctx, &subscription, event.Type == "customer.subscription.created")
	case "customer.subscription.deleted":
		var subscription stripeSubscription
		if err := json.Unmarshal(event.Data.Object, &subscription); err != nil {
			return err
		}
		if err := h.upsertStripeSubscription(ctx, &subscription, customerIDFromAny(subscription.Customer)); err != nil {
			return err
		}
		if userID := strings.TrimSpace(subscription.Metadata["user_id"]); userID != "" {
			if user, err := h.store.GetUserByID(ctx, userID); err == nil {
				if err := h.reconcileSubscriptionAccessAt(ctx, user, time.Now().UTC()); err != nil {
					return err
				}
			} else if !errors.Is(err, store.ErrUserNotFound) {
				return err
			}
			h.publishBillingEvent(ctx, &store.BillingEvent{
				Type: "subscription_deleted", UserID: userID,
				PlanID:  strings.TrimSpace(subscription.Metadata["plan_id"]),
				PriceID: subscriptionPriceID(&subscription), ExternalID: subscription.ID,
			})
		}
		return nil
	case "refund.created", "refund.updated":
		var refund stripeRefund
		if err := json.Unmarshal(event.Data.Object, &refund); err != nil {
			return err
		}
		return h.recordStripeRefund(ctx, &refund)
	case "charge.refunded":
		var charge stripeCharge
		if err := json.Unmarshal(event.Data.Object, &charge); err != nil {
			return err
		}
		invoiceID := customerIDFromAny(charge.Invoice)
		if invoiceID != "" && charge.AmountRefunded > 0 {
			if err := h.store.RecordReferralRisk(ctx, store.ReferralPaymentRisk{SourceKey: event.ID, ProviderInvoiceID: invoiceID, Type: "refund", Reason: "Stripe charge refund", OccurredAt: time.Now().UTC()}); err != nil {
				return err
			}
		}
		if !charge.Refunded || charge.Amount <= 0 || charge.AmountRefunded < charge.Amount {
			return nil // Partial refunds do not revoke the subscription entitlement.
		}
		for _, refund := range charge.Refunds.Data {
			if !strings.EqualFold(strings.TrimSpace(refund.Status), "succeeded") {
				return nil // Pending or failed refund states never revoke access.
			}
		}
		if h.stripe == nil {
			return nil
		}
		if invoiceID == "" {
			return h.reconcileProcessedStripeRefund(ctx, event)
		}
		invoice, err := h.stripe.fetchInvoice(ctx, invoiceID)
		if err != nil {
			return err
		}
		subscriptionID := invoiceSubscriptionID(invoice)
		invoice.Subscription = subscriptionID
		if subscriptionID == "" {
			return nil
		}
		subscription, err := h.stripe.fetchSubscription(ctx, subscriptionID)
		if err != nil {
			return err
		}
		userID := strings.TrimSpace(subscription.Metadata["user_id"])
		if userID == "" {
			return nil
		}
		subscription.Status = "refunded" // This branch has verified a completed full refund.
		if err := h.upsertStripeSubscription(ctx, subscription, customerIDFromAny(subscription.Customer)); err != nil {
			return err
		}
		if err := h.revokeRefundedSubscription(ctx, userID, subscriptionID); err != nil {
			return err
		}
		h.publishBillingEvent(ctx, &store.BillingEvent{Type: "subscription_refunded", UserID: userID, PlanID: subscription.Metadata["plan_id"], PriceID: subscriptionPriceID(subscription), ExternalID: subscriptionID})
		return nil
	case "charge.dispute.created", "charge.dispute.updated", "charge.dispute.closed":
		var dispute stripeDispute
		if err := json.Unmarshal(event.Data.Object, &dispute); err != nil {
			return err
		}
		chargeID := customerIDFromAny(dispute.Charge)
		if chargeID == "" || h.stripe == nil {
			return errors.New("Stripe dispute is missing its charge reference")
		}
		charge, err := h.stripe.fetchCharge(ctx, chargeID)
		if err != nil {
			return err
		}
		invoiceID := customerIDFromAny(charge.Invoice)
		if invoiceID == "" {
			return errors.New("Stripe dispute charge is missing its invoice reference")
		}
		occurred := time.Now().UTC()
		if dispute.Created > 0 {
			occurred = time.Unix(dispute.Created, 0).UTC()
		}
		return h.store.RecordReferralRisk(ctx, store.ReferralPaymentRisk{SourceKey: event.ID, ProviderInvoiceID: invoiceID, Type: "dispute", Reason: dispute.Reason, OccurredAt: occurred})
	case "invoice.paid", "invoice.payment_failed":
		var invoice stripeInvoice
		if err := json.Unmarshal(event.Data.Object, &invoice); err != nil {
			return err
		}
		subscriptionID := invoiceSubscriptionID(&invoice)
		invoice.Subscription = subscriptionID
		if subscriptionID == "" {
			return nil
		}
		sub, err := h.stripe.fetchSubscription(ctx, subscriptionID)
		if err != nil {
			return err
		}
		if err := h.upsertStripeSubscription(ctx, sub, customerIDFromAny(invoice.Customer)); err != nil {
			return err
		}
		userID := strings.TrimSpace(sub.Metadata["user_id"])
		if userID == "" {
			return nil
		}
		if event.Type == "invoice.payment_failed" {
			// Dunning step 1: mark arrears; time-based escalation to
			// throttled/suspended is billing-service's job (P1.5).
			if err := h.markAccountArrears(ctx, userID); err != nil {
				return err
			}
			h.publishBillingEvent(ctx, &store.BillingEvent{
				Type: "payment_failed", UserID: userID,
				PlanID:  strings.TrimSpace(sub.Metadata["plan_id"]),
				PriceID: subscriptionPriceID(sub), ExternalID: sub.ID,
			})
			return nil
		}
		// Persist the provider's settled subscription invoice/payment as local
		// financial facts for every supported SaaS subscription. The referral
		// helper below remains responsible only for referral eligibility/rewards.
		if !strings.EqualFold(invoice.Status, "paid") {
			return errors.New("invoice.paid event does not contain a paid invoice")
		}
		if invoice.AmountPaid > 0 && customerIDFromAny(invoice.PaymentIntent) == "" {
			verified, err := h.stripe.fetchInvoice(ctx, invoice.ID)
			if err != nil {
				return err
			}
			if verified.ID != invoice.ID || !strings.EqualFold(verified.Status, "paid") || customerIDFromAny(verified.Subscription) != subscriptionID || verified.AmountPaid != invoice.AmountPaid || !strings.EqualFold(verified.Currency, invoice.Currency) {
				return errors.New("Stripe invoice changed while resolving its payment")
			}
			invoice = *verified
		}
		if err := h.recordStripePaidInvoice(ctx, &invoice, sub, userID); err != nil {
			return err
		}
		if refunded, err := h.reconcileRefundedLatestInvoice(ctx, sub); err != nil {
			return err
		} else if refunded {
			return h.recordReferralPaidInvoice(ctx, &invoice, sub, userID)
		}
		if hasPendingUpdate(sub) || customerIDFromAny(sub.LatestInvoice) != invoice.ID {
			// A historical payment still qualifies independently of today's
			// subscription projection. Never reset current quota from this invoice.
			return h.recordReferralPaidInvoice(ctx, &invoice, sub, userID)
		}
		// invoice.paid renews the billing period: re-arm quota and clear
		// arrears from the plan the subscription is on.
		plan, err := h.resolveBillingPlan(ctx, subscriptionPriceID(sub), sub.Metadata["plan_id"])
		if err != nil {
			if errors.Is(err, store.ErrBillingPlanNotFound) {
				slog.Warn("stripe invoice.paid without matching billing plan", "userID", userID, "priceID", subscriptionPriceID(sub))
				return h.recordReferralPaidInvoice(ctx, &invoice, sub, userID)
			}
			return err
		}
		previous, previousErr := h.store.GetAccountBillingProfile(ctx, userID)
		if previousErr != nil && !errors.Is(previousErr, store.ErrUserNotFound) {
			return previousErr
		}
		upgraded := previous != nil && plan.IncludedQuotaBytes > previous.IncludedQuotaBytes
		periodStart, periodEnd := subscriptionQuotaPeriod(plan, sub, time.Now())
		activation := previous != nil && previous.PricingRuleVersion == "plan:FREE" && invoice.BillingReason == "subscription_create"
		if activation {
			if err := h.store.ActivateSubscriptionQuota(ctx, userID, previous.IncludedQuotaBytes, plan.IncludedQuotaBytes, periodStart, periodEnd); err != nil {
				return err
			}
		} else if upgraded {
			if err := h.applyPlanUpgradeQuota(ctx, userID, previous.IncludedQuotaBytes, plan.IncludedQuotaBytes, periodStart, periodEnd); err != nil {
				return err
			}
		}
		if err := h.applyPlanEntitlements(ctx, userID, plan); err != nil {
			return err
		}
		if !upgraded && !activation {
			if err := h.resetQuotaForPlanOnce(ctx, userID, plan, periodStart, periodEnd); err != nil {
				return err
			}
		}
		if err := h.restoreQuotaGroupForPlan(ctx, userID, plan); err != nil {
			return err
		}
		// The referral ledger performs extra Stripe history verification and can
		// fail transiently. Restore the paid subscriber's core entitlements first;
		// those operations are idempotent across webhook retries.
		if err := h.recordReferralPaidInvoice(ctx, &invoice, sub, userID); err != nil {
			return err
		}
		h.publishBillingEvent(ctx, &store.BillingEvent{
			Type: "invoice_paid", UserID: userID,
			PlanID: plan.PlanID, PriceID: subscriptionPriceID(sub), ExternalID: sub.ID,
		})
		return nil
	default:
		return nil
	}
}

// recordStripePaidInvoice imports only a settled Stripe invoice and its
// associated payment. Both writes are idempotent so a webhook retry can finish
// the payment write if the process fails after persisting the invoice.
func (h *handler) recordStripePaidInvoice(ctx context.Context, invoice *stripeInvoice, subscription *stripeSubscription, userID string) error {
	if invoice == nil || subscription == nil || strings.TrimSpace(invoice.ID) == "" ||
		!strings.EqualFold(strings.TrimSpace(invoice.Status), "paid") || invoice.PaidOutOfBand {
		return nil
	}

	paidAtUnix := invoice.StatusTransitions.PaidAt
	if paidAtUnix <= 0 {
		return errors.New("Stripe paid invoice is missing paid_at")
	}
	if strings.TrimSpace(customerIDFromAny(invoice.Subscription)) != strings.TrimSpace(subscription.ID) {
		return errors.New("Stripe invoice subscription does not match the processed subscription")
	}
	currency := strings.ToUpper(strings.TrimSpace(invoice.Currency))
	if len(currency) != 3 || currency[0] < 'A' || currency[0] > 'Z' ||
		currency[1] < 'A' || currency[1] > 'Z' || currency[2] < 'A' || currency[2] > 'Z' {
		return errors.New("Stripe invoice has invalid currency")
	}
	if invoice.AmountPaid < 0 {
		return errors.New("Stripe invoice has invalid paid amount")
	}

	paidAt := time.Unix(paidAtUnix, 0).UTC()
	issuedAt := paidAt
	if invoice.Created > 0 {
		issuedAt = time.Unix(invoice.Created, 0).UTC()
	}
	subscriptions, err := h.store.ListSubscriptionsByUser(ctx, userID)
	if err != nil {
		return err
	}
	var localSubscription *store.Subscription
	for i := range subscriptions {
		if strings.EqualFold(subscriptions[i].Provider, "stripe") && subscriptions[i].ExternalID == subscription.ID {
			localSubscription = &subscriptions[i]
			break
		}
	}
	if localSubscription == nil {
		return errors.New("local Stripe subscription is unavailable for paid invoice")
	}

	localInvoice := &store.FinanceInvoice{
		IdempotencyKey: "stripe-invoice:" + invoice.ID,
		AccountUUID:    userID, SubscriptionUUID: localSubscription.ID,
		Provider: "stripe", ProviderInvoiceID: invoice.ID,
		AmountMinor: invoice.AmountPaid, Currency: currency,
		Description: "Settled Stripe subscription invoice", IssuedAt: issuedAt,
	}
	if periodStart, periodEnd, ok := referralInvoicePeriod(invoice); ok {
		localInvoice.ServicePeriodStart, localInvoice.ServicePeriodEnd = &periodStart, &periodEnd
	}
	if _, err := h.store.CreateFinanceInvoice(ctx, localInvoice); err != nil {
		return err
	}

	// A zero-value invoice has no payment fact. A positive settled invoice must
	// identify the Stripe PaymentIntent; never substitute the invoice ID for it.
	if invoice.AmountPaid == 0 {
		return nil
	}
	paymentIntentID := customerIDFromAny(invoice.PaymentIntent)
	if paymentIntentID == "" {
		return errors.New("Stripe paid invoice is missing its payment intent")
	}
	localPayment := &store.FinancePayment{
		IdempotencyKey: "stripe-payment:" + invoice.ID,
		InvoiceID:      localInvoice.ID, AccountUUID: userID,
		Provider: "stripe", ProviderPaymentID: paymentIntentID,
		AmountMinor: invoice.AmountPaid, Currency: currency, PaidAt: paidAt,
	}
	if _, err := h.store.RecordFinancePayment(ctx, localPayment); err != nil {
		return err
	}
	return nil
}

// recordReferralPaidInvoice writes only signed Stripe facts for the explicitly
// scoped XConnect subscription product. Catalog price fields are never used as
// payment evidence or reward inputs.
func (h *handler) recordReferralPaidInvoice(ctx context.Context, invoice *stripeInvoice, subscription *stripeSubscription, userID string) error {
	if invoice == nil || subscription == nil || strings.TrimSpace(invoice.ID) == "" || invoice.AmountPaid <= 0 || !strings.EqualFold(strings.TrimSpace(invoice.Status), "paid") {
		return nil
	}
	if invoice.PaidOutOfBand {
		return nil
	}
	paymentID := customerIDFromAny(invoice.PaymentIntent)
	if paymentID == "" {
		return nil
	}
	// The current subscription metadata can change after this invoice was
	// issued (for example, a delayed monthly invoice webhook after an upgrade).
	// Resolve the plan from the immutable invoice line price snapshot instead.
	if invoice.StatusTransitions.PaidAt <= 0 || len(invoice.Lines.Data) == 0 || invoice.Lines.HasMore {
		verified, fetchErr := h.stripe.fetchInvoice(ctx, invoice.ID)
		if fetchErr != nil {
			return fetchErr
		}
		if !strings.EqualFold(verified.Status, "paid") || verified.PaidOutOfBand || verified.AmountPaid <= 0 || customerIDFromAny(verified.PaymentIntent) == "" || verified.StatusTransitions.PaidAt <= 0 {
			return errors.New("Stripe invoice lacks verified paid_at, payment intent, or paid status")
		}
		invoice = verified
		paymentID = customerIDFromAny(invoice.PaymentIntent)
	}
	planID, err := h.referralPlanFromInvoice(ctx, invoice)
	if err != nil {
		return err
	}
	if planID == "" {
		return nil
	}
	periodStart, periodEnd, ok := referralInvoicePeriod(invoice)
	if !ok {
		return errors.New("Stripe XConnect invoice lacks a consistent subscription service period")
	}
	productSlug := "xconnect"
	currency := strings.ToUpper(strings.TrimSpace(invoice.Currency))
	if !validReferralCurrency(currency) {
		return errors.New("Stripe invoice has invalid currency")
	}
	paidMinor, err := stripeAmountToCurrencyMinor(currency, invoice.AmountPaid)
	if err != nil {
		return err
	}
	paidAt := time.Unix(invoice.StatusTransitions.PaidAt, 0).UTC()
	subscriptions, err := h.store.ListSubscriptionsByUser(ctx, userID)
	if err != nil {
		return err
	}
	var subscriptionUUID string
	for _, local := range subscriptions {
		if local.Provider == "stripe" && local.ExternalID == subscription.ID {
			subscriptionUUID = local.ID
			break
		}
	}
	if subscriptionUUID == "" {
		return errors.New("local Stripe subscription is unavailable for verified invoice")
	}
	financeInvoice, err := h.verifiedReferralFinanceInvoice(ctx, invoice, userID, subscriptionUUID, planID, invoice.AmountPaid, periodStart, periodEnd)
	if err != nil {
		return err
	}
	financePayment := &store.FinancePayment{IdempotencyKey: "stripe-payment:" + invoice.ID, InvoiceID: financeInvoice.ID, AccountUUID: userID, Provider: "stripe", ProviderPaymentID: paymentID, AmountMinor: financeInvoice.AmountMinor, Currency: currency, PaidAt: paidAt}
	if _, err := h.store.RecordFinancePayment(ctx, financePayment); err != nil && !errors.Is(err, store.ErrFinanceInvoiceAlreadyPaid) {
		return err
	}
	campaign, err := h.referralCampaignAt(ctx, paidAt)
	if err != nil {
		return err
	}
	if campaign == nil || !campaign.Active(paidAt) {
		return nil
	}
	if isXConnectAnnualPlanID(planID) {
		if err := h.ensureReferrerPaidPeriod(ctx, userID, paidAt); err != nil {
			return err
		}
	}
	firstInvoice, err := h.stripe.isFirstPaidXConnectInvoice(ctx, h.store, customerIDFromAny(invoice.Customer), invoice.ID, paidAt)
	if err != nil {
		return err
	}
	if !firstInvoice {
		return nil
	}
	rewardAmount := paidMinor / 10 // Integer floor in ISO currency minor units.
	reward := &store.ReferralReward{ProviderInvoiceID: invoice.ID, InviteeUUID: userID, PlanID: planID, ProductSlug: productSlug, PaidMinor: paidMinor, RewardMinor: rewardAmount, Currency: currency, PaidAt: paidAt, EligibleAt: paidAt.Add(7 * 24 * time.Hour)}
	_, err = h.store.CreateReferralReward(ctx, reward)
	if errors.Is(err, store.ErrReferralNotFound) {
		return nil
	}
	return err
}

// verifiedReferralFinanceInvoice links immutable provider price evidence to an
// existing financial fact. created (issuance) and paid_at are distinct facts;
// proving referral eligibility must never rewrite or compare them as equals.
// Financial facts keep Stripe provider units; only the reward uses ISO units.
func (h *handler) verifiedReferralFinanceInvoice(ctx context.Context, source *stripeInvoice, userID, subscriptionID, planID string, providerAmount int64, periodStart, periodEnd time.Time) (*store.FinanceInvoice, error) {
	if source == nil || len(source.Lines.Data) == 0 || !isXConnectProPlanID(planID) || providerAmount <= 0 || providerAmount != source.AmountPaid || !periodStart.Before(periodEnd) {
		return nil, errors.New("verified Stripe invoice evidence is incomplete")
	}
	invoices, err := h.store.ListFinanceInvoices(ctx, userID, subscriptionID, 500)
	if err != nil {
		return nil, err
	}
	var invoice *store.FinanceInvoice
	for i := range invoices {
		if invoices[i].Provider == "stripe" && invoices[i].ProviderInvoiceID == source.ID {
			invoice = &invoices[i]
			break
		}
	}
	currency := strings.ToUpper(strings.TrimSpace(source.Currency))
	if invoice != nil {
		if invoice.AccountUUID != userID || invoice.SubscriptionUUID != subscriptionID || invoice.AmountMinor != providerAmount || !strings.EqualFold(invoice.Currency, currency) || (invoice.ServicePeriodStart != nil && !invoice.ServicePeriodStart.Equal(periodStart)) || (invoice.ServicePeriodEnd != nil && !invoice.ServicePeriodEnd.Equal(periodEnd)) {
			return nil, errors.New("verified Stripe invoice conflicts with immutable local financial facts")
		}
		if invoice.ServicePeriodStart == nil && invoice.ServicePeriodEnd == nil {
			invoice.ServicePeriodStart, invoice.ServicePeriodEnd = &periodStart, &periodEnd
			if _, err := h.store.CreateFinanceInvoice(ctx, invoice); err != nil {
				return nil, err
			}
		}
	} else {
		issuedAt := time.Unix(source.StatusTransitions.PaidAt, 0).UTC()
		if source.Created > 0 {
			issuedAt = time.Unix(source.Created, 0).UTC()
		}
		invoice = &store.FinanceInvoice{IdempotencyKey: "stripe-invoice:" + source.ID, AccountUUID: userID, SubscriptionUUID: subscriptionID, Provider: "stripe", ProviderInvoiceID: source.ID, AmountMinor: providerAmount, Currency: currency, Description: "Verified XConnect subscription invoice", IssuedAt: issuedAt, ServicePeriodStart: &periodStart, ServicePeriodEnd: &periodEnd}
		if _, err := h.store.CreateFinanceInvoice(ctx, invoice); err != nil {
			return nil, err
		}
	}
	// All immutable line prices were checked by referralPlanFromInvoice before
	// reaching here; record their common allowed plan and a provider price key.
	priceID := customerIDFromAny(source.Lines.Data[0].Price)
	if priceID == "" {
		priceID = customerIDFromAny(source.Lines.Data[0].Pricing.PriceDetails.Price)
	}
	if err := h.store.VerifyReferralInvoice(ctx, &store.ReferralInvoiceVerification{InvoiceID: invoice.ID, ProviderPriceID: priceID, PlanID: planID, ProductSlug: "xconnect"}); err != nil {
		return nil, err
	}
	return invoice, nil
}

// ensureReferrerPaidPeriod imports signed provider invoice/payment facts for
// legacy XConnect subscriptions that predate the local finance ledger. This
// keeps annual eligibility from silently excluding existing paid subscribers.
// It only imports invoices whose immutable price line proves the active period
// at the friend's payment time; current subscription metadata is not used as
// historical payment evidence.
func (h *handler) ensureReferrerPaidPeriod(ctx context.Context, inviteeUUID string, paidAt time.Time) error {
	referrerUUID, err := h.store.GetReferralReferrer(ctx, inviteeUUID)
	if errors.Is(err, store.ErrReferralNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	subscriptions, err := h.store.ListSubscriptionsByUser(ctx, referrerUUID)
	if err != nil {
		return err
	}
	for _, local := range subscriptions {
		if local.Provider != "stripe" || local.Kind != "subscription" {
			continue
		}
		customerID := strings.TrimSpace(subscriptionMetaString(local.Meta, "customer_id"))
		if customerID == "" {
			continue
		}
		invoices, listErr := h.stripe.listPaidInvoices(ctx, customerID)
		if listErr != nil {
			return listErr
		}
		for _, listed := range invoices {
			if customerIDFromAny(listed.Subscription) != local.ExternalID || listed.AmountPaid <= 0 || !strings.EqualFold(listed.Status, "paid") {
				continue
			}
			verified, fetchErr := h.stripe.fetchInvoice(ctx, listed.ID)
			if fetchErr != nil {
				return fetchErr
			}
			if verified.PaidOutOfBand || customerIDFromAny(verified.PaymentIntent) == "" || verified.StatusTransitions.PaidAt <= 0 || verified.AmountPaid <= 0 || !strings.EqualFold(verified.Status, "paid") {
				continue
			}
			invoicePaidAt := time.Unix(verified.StatusTransitions.PaidAt, 0).UTC()
			if invoicePaidAt.After(paidAt) {
				continue
			}
			planID, planErr := h.referralPlanFromInvoice(ctx, verified)
			if planErr != nil {
				return planErr
			}
			if planID == "" {
				continue
			}
			periodStart, periodEnd, periodOK := referralInvoicePeriod(verified)
			if !periodOK || paidAt.Before(periodStart) || !paidAt.Before(periodEnd) {
				continue
			}
			paymentIntentID := customerIDFromAny(verified.PaymentIntent)
			charge, chargeErr := h.stripe.fetchPaymentIntentCharge(ctx, paymentIntentID)
			if chargeErr != nil {
				return chargeErr
			}
			if !strings.EqualFold(charge.Status, "succeeded") {
				continue
			}
			if charge.Disputed {
				if err = h.store.RecordReferralRisk(ctx, store.ReferralPaymentRisk{SourceKey: "provider-history:dispute:" + charge.ID, ProviderInvoiceID: verified.ID, Type: "dispute", Reason: "Provider history shows disputed inviter payment", OccurredAt: paidAt}); err != nil {
					return err
				}
				continue
			}
			if charge.Refunded || charge.Amount > 0 && charge.AmountRefunded >= charge.Amount {
				if err = h.store.RecordReferralRisk(ctx, store.ReferralPaymentRisk{SourceKey: "provider-history:refund:" + charge.ID, ProviderInvoiceID: verified.ID, Type: "refund", Reason: "Provider history shows fully refunded inviter payment", OccurredAt: paidAt}); err != nil {
					return err
				}
				continue
			}
			currency := strings.ToUpper(strings.TrimSpace(verified.Currency))
			if !validReferralCurrency(currency) {
				return errors.New("Stripe inviter invoice has invalid currency")
			}
			_, amountErr := stripeAmountToCurrencyMinor(currency, verified.AmountPaid)
			if amountErr != nil {
				return amountErr
			}
			invoice, verifyErr := h.verifiedReferralFinanceInvoice(ctx, verified, referrerUUID, local.ID, planID, verified.AmountPaid, periodStart, periodEnd)
			if verifyErr != nil {
				return verifyErr
			}
			payment := &store.FinancePayment{IdempotencyKey: "stripe-payment:" + verified.ID, InvoiceID: invoice.ID, AccountUUID: referrerUUID, Provider: "stripe", ProviderPaymentID: paymentIntentID, AmountMinor: invoice.AmountMinor, Currency: currency, PaidAt: invoicePaidAt}
			if _, err = h.store.RecordFinancePayment(ctx, payment); err != nil && !errors.Is(err, store.ErrFinanceInvoiceAlreadyPaid) {
				return err
			}
		}
	}
	return nil
}

func subscriptionMetaString(meta map[string]any, key string) string {
	value, _ := meta[key].(string)
	return value
}

func referralInvoicePeriod(invoice *stripeInvoice) (time.Time, time.Time, bool) {
	if invoice == nil {
		return time.Time{}, time.Time{}, false
	}
	var start, end time.Time
	for _, line := range invoice.Lines.Data {
		if line.Period.Start <= 0 || line.Period.End <= line.Period.Start {
			return time.Time{}, time.Time{}, false
		}
		lineStart, lineEnd := time.Unix(line.Period.Start, 0).UTC(), time.Unix(line.Period.End, 0).UTC()
		if start.IsZero() {
			start, end = lineStart, lineEnd
		} else if !start.Equal(lineStart) || !end.Equal(lineEnd) {
			return time.Time{}, time.Time{}, false
		}
	}
	return start, end, !start.IsZero() && start.Before(end)
}

func (h *handler) referralPlanFromInvoice(ctx context.Context, invoice *stripeInvoice) (string, error) {
	if invoice == nil || len(invoice.Lines.Data) == 0 {
		return "", errors.New("Stripe invoice lacks immutable subscription price lines")
	}
	planID := ""
	for _, line := range invoice.Lines.Data {
		priceID := customerIDFromAny(line.Price)
		if priceID == "" {
			priceID = customerIDFromAny(line.Pricing.PriceDetails.Price)
		}
		if priceID == "" {
			return "", errors.New("Stripe invoice line lacks a verifiable price")
		}
		plan, err := h.store.GetBillingPlanByPriceID(ctx, priceID)
		if errors.Is(err, store.ErrBillingPlanNotFound) {
			// An unrecognized historical price cannot prove whether this was an
			// XConnect invoice, so leave the event retryable for manual catalog repair.
			return "", errors.New("Stripe invoice price is absent from the billing catalog")
		}
		if err != nil {
			return "", err
		}
		if plan.Kind != "subscription" || !isXConnectProPlanID(plan.PlanID) {
			return "", nil
		}
		wantUnit := "month"
		if isXConnectAnnualPlanID(plan.PlanID) {
			wantUnit = "year"
		}
		if !strings.EqualFold(strings.TrimSpace(plan.PriceUnit), wantUnit) {
			return "", errors.New("billing catalog plan interval conflicts with referral plan")
		}
		if planID != "" && planID != plan.PlanID {
			return "", errors.New("Stripe invoice contains conflicting XConnect plan lines")
		}
		planID = plan.PlanID
	}
	if invoice.Lines.HasMore {
		return "", errors.New("Stripe invoice has more price lines than the referral verifier can inspect")
	}
	return planID, nil
}

func (c *stripeClient) isFirstPaidXConnectInvoice(ctx context.Context, st store.Store, customerID, currentInvoiceID string, currentPaidAt time.Time) (bool, error) {
	if strings.TrimSpace(customerID) == "" {
		return false, errors.New("Stripe customer is missing for referral invoice history verification")
	}
	invoices, err := c.listPaidInvoices(ctx, customerID)
	if err != nil {
		return false, err
	}
	currentFound := false
	for _, candidate := range invoices {
		if candidate.ID == currentInvoiceID {
			currentFound = true
		}
		if candidate.AmountPaid <= 0 || !strings.EqualFold(candidate.Status, "paid") || customerIDFromAny(candidate.Subscription) == "" {
			continue
		}
		verified, err := c.fetchInvoice(ctx, candidate.ID)
		if err != nil {
			return false, err
		}
		if verified.AmountPaid <= 0 || !strings.EqualFold(verified.Status, "paid") || verified.PaidOutOfBand || customerIDFromAny(verified.PaymentIntent) == "" || verified.StatusTransitions.PaidAt <= 0 {
			// Out-of-band invoices and historical rows without a verified provider
			// collection fact do not count as the customer's first paid invoice.
			continue
		}
		paidAt := time.Unix(verified.StatusTransitions.PaidAt, 0).UTC()
		if candidate.ID == currentInvoiceID {
			paidAt = currentPaidAt
		}
		planID, err := (&handler{store: st}).referralPlanFromInvoice(ctx, verified)
		if err != nil {
			return false, err
		}
		if planID != "" && (paidAt.Before(currentPaidAt) || (paidAt.Equal(currentPaidAt) && candidate.ID < currentInvoiceID)) {
			return false, nil
		}
	}
	if !currentFound {
		return false, errors.New("verified paid invoice is not yet visible in Stripe customer invoice history")
	}
	return true, nil
}

func validReferralCurrency(currency string) bool {
	if len(currency) != 3 {
		return false
	}
	for _, r := range currency {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// Stripe retains a two-decimal API representation for ISK and UGX, while
// their ISO currency units have no fractional minor units. Normalize those
// invoice amounts to ISO minor units before calculating or exposing rewards.
func stripeAmountToCurrencyMinor(currency string, amount int64) (int64, error) {
	if currency == "ISK" || currency == "UGX" {
		if amount%100 != 0 {
			return 0, errors.New("Stripe invoice amount is not representable in ISO minor units")
		}
		return amount / 100, nil
	}
	if currency == "MGA" {
		if amount > int64(^uint64(0)>>1)/100 || amount < 0 {
			return 0, errors.New("Stripe MGA invoice amount overflows ISO minor units")
		}
		return amount * 100, nil
	}
	return amount, nil
}

// subscriptionPriceID extracts the first line-item price id.
func subscriptionPriceID(source *stripeSubscription) string {
	if source == nil || len(source.Items.Data) == 0 {
		return ""
	}
	return strings.TrimSpace(source.Items.Data[0].Price.ID)
}

// subscriptionPeriod reads the subscription's current billing period so
// resetQuotaForPlan can record when the grant resets. Falls back to a
// natural month when Stripe hasn't populated the period (defensive; every
// real subscription object carries it).
func subscriptionPeriod(source *stripeSubscription) (time.Time, time.Time) {
	if source != nil && len(source.Items.Data) == 1 {
		item := source.Items.Data[0]
		if item.CurrentPeriodStart > 0 && item.CurrentPeriodEnd > item.CurrentPeriodStart {
			return time.Unix(item.CurrentPeriodStart, 0).UTC(), time.Unix(item.CurrentPeriodEnd, 0).UTC()
		}
	}
	if source == nil || source.CurrentPeriodStart <= 0 || source.CurrentPeriodEnd <= source.CurrentPeriodStart {
		return naturalMonthPeriod(time.Now())
	}
	return time.Unix(source.CurrentPeriodStart, 0).UTC(), time.Unix(source.CurrentPeriodEnd, 0).UTC()
}

// syncSubscriptionEntitlements applies catalog entitlements for an active or
// trialing subscription; freshly created subscriptions also re-arm quota and
// supersede any live trial.
func (h *handler) syncSubscriptionEntitlements(ctx context.Context, source *stripeSubscription, created bool) error {
	if refunded, err := h.reconcileRefundedLatestInvoice(ctx, source); err != nil || refunded {
		return err
	}

	if source == nil {
		return nil
	}
	userID := strings.TrimSpace(source.Metadata["user_id"])
	if userID == "" {
		return nil
	}
	status := strings.ToLower(strings.TrimSpace(source.Status))
	if status != "active" && status != "trialing" {
		return nil
	}
	// A subscription update is not proof of payment. In particular, pending
	// upgrades must never grant their target allowance before settlement.
	freshActivation := created
	if h.stripe != nil && h.stripe.enabled() {
		if hasPendingUpdate(source) {
			return nil
		}
		invoiceID := customerIDFromAny(source.LatestInvoice)
		if invoiceID == "" {
			return errors.New("paid subscription invoice required")
		}
		invoice, err := h.stripe.fetchInvoice(ctx, invoiceID)
		if err != nil {
			return err
		}
		if invoice.Status != "paid" || invoiceSubscriptionID(invoice) != source.ID {
			return nil
		}
		freshActivation = invoice.BillingReason == "subscription_create"
	}
	plan, err := h.resolveBillingPlan(ctx, subscriptionPriceID(source), source.Metadata["plan_id"])
	if err != nil {
		if errors.Is(err, store.ErrBillingPlanNotFound) {
			slog.Warn("stripe subscription without matching billing plan", "userID", userID, "priceID", subscriptionPriceID(source))
			return nil
		}
		return err
	}
	previous, previousErr := h.store.GetAccountBillingProfile(ctx, userID)
	if previousErr != nil && !errors.Is(previousErr, store.ErrUserNotFound) {
		return previousErr
	}
	upgraded := previous != nil && plan.IncludedQuotaBytes > previous.IncludedQuotaBytes
	if freshActivation && previous != nil && previous.PricingRuleVersion == "plan:FREE" {
		periodStart, periodEnd := subscriptionQuotaPeriod(plan, source, time.Now())
		if err := h.store.ActivateSubscriptionQuota(ctx, userID, previous.IncludedQuotaBytes, plan.IncludedQuotaBytes, periodStart, periodEnd); err != nil {
			return err
		}
		if err := h.applyPlanEntitlements(ctx, userID, plan); err != nil {
			return err
		}
	} else if created {
		if err := h.applyPlanEntitlements(ctx, userID, plan); err != nil {
			return err
		}
		periodStart, periodEnd := subscriptionQuotaPeriod(plan, source, time.Now())
		if err := h.resetQuotaForPlanOnce(ctx, userID, plan, periodStart, periodEnd); err != nil {
			return err
		}
	} else if upgraded {
		periodStart, periodEnd := subscriptionQuotaPeriod(plan, source, time.Now())
		if err := h.applyPlanUpgradeQuota(ctx, userID, previous.IncludedQuotaBytes, plan.IncludedQuotaBytes, periodStart, periodEnd); err != nil {
			return err
		}
		if err := h.applyPlanEntitlements(ctx, userID, plan); err != nil {
			return err
		}
	} else if err := h.applyPlanEntitlements(ctx, userID, plan); err != nil {
		return err
	}
	if err := h.restoreQuotaGroupForPlan(ctx, userID, plan); err != nil {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(plan.Kind), "trial") {
		h.supersedeActiveTrials(ctx, userID)
	}
	eventType := "subscription_updated"
	if created {
		eventType = "subscription_activated"
	}
	h.publishBillingEvent(ctx, &store.BillingEvent{
		Type: eventType, UserID: userID,
		PlanID: plan.PlanID, PriceID: subscriptionPriceID(source), ExternalID: source.ID,
	})
	return nil
}

func (h *handler) upsertStripeSubscription(ctx context.Context, source *stripeSubscription, customerID string) error {
	if refunded, err := h.reconcileRefundedLatestInvoice(ctx, source); err != nil || refunded {
		return err
	}

	if source == nil {
		return nil
	}
	userID := strings.TrimSpace(source.Metadata["user_id"])
	if userID == "" {
		return nil
	}
	if source.Status == "active" && h.stripe != nil && h.stripe.enabled() {
		if hasPendingUpdate(source) {
			return nil
		}
		if customerIDFromAny(source.LatestInvoice) == "" {
			return nil
		}
		invoice, err := h.stripe.fetchInvoice(ctx, customerIDFromAny(source.LatestInvoice))
		if err != nil {
			return err
		}
		if invoice.Status != "paid" || invoiceSubscriptionID(invoice) != source.ID {
			return nil
		}
	}
	priceID := ""
	if len(source.Items.Data) > 0 {
		priceID = strings.TrimSpace(source.Items.Data[0].Price.ID)
	}
	kind := strings.TrimSpace(source.Metadata["kind"])
	if kind == "" {
		kind = "subscription"
	}
	status := strings.TrimSpace(source.Status)
	if status == "" {
		status = "active"
	}
	if strings.EqualFold(status, "canceled") {
		status = "cancelled"
	}
	meta := buildStripeMeta(nil, map[string]string{
		"price_id":          priceID,
		"customer_id":       firstNonEmpty(customerID, customerIDFromAny(source.Customer)),
		"product_slug":      source.Metadata["product_slug"],
		"user_email":        source.Metadata["user_email"],
		"startsAt":          epochToRFC3339(source.CurrentPeriodStart),
		"expiresAt":         epochToRFC3339(source.CurrentPeriodEnd),
		"cancelAtPeriodEnd": strconv.FormatBool(source.CancelAtPeriodEnd),
	})
	planID := strings.TrimSpace(source.Metadata["plan_id"])
	if plan, err := h.store.GetBillingPlanByPriceID(ctx, priceID); err == nil {
		planID = plan.PlanID // Pending updates cannot change Stripe metadata.
	} else if !errors.Is(err, store.ErrBillingPlanNotFound) {
		return err
	}
	start, end := subscriptionPeriod(source)
	if source.CurrentPeriodStart > 0 || (len(source.Items.Data) == 1 && source.Items.Data[0].CurrentPeriodStart > 0) {
		meta["startsAt"] = start.Format(time.RFC3339)
		meta["expiresAt"] = end.Format(time.RFC3339)
	}
	if source.CancelAtPeriodEnd && status != "cancelled" {
		status = "canceling"
	}
	subscription := &store.Subscription{
		UserID:        userID,
		Provider:      "stripe",
		PaymentMethod: "stripe",
		Kind:          kind,
		PlanID:        planID,
		ExternalID:    strings.TrimSpace(source.ID),
		Status:        status,
		Meta:          meta,
	}
	if status == "cancelled" {
		cancelledAt := time.Now().UTC()
		subscription.CancelledAt = &cancelledAt
	}
	return h.store.UpsertSubscription(ctx, subscription)
}

func ParseStripeAllowedPriceIDs(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			values = append(values, trimmed)
		}
	}
	return values
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func parseUnixString(value string) int64 {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0
	}
	number, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0
	}
	return number
}
