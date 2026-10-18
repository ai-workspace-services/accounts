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
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

const stripeAPIBaseURL = "https://api.stripe.com/v1"

// Stripe signs webhook payloads with a timestamp. Keep the same bounded
// tolerance used by Stripe's official SDKs so an old captured request cannot
// be replayed outside the event-id deduplication window.
const stripeWebhookTimestampTolerance = 5 * time.Minute

type StripeConfig struct {
	SecretKey       string
	WebhookSecret   string
	AllowedPriceIDs []string
	FrontendURL     string
}

type stripeClient struct {
	secretKey      string
	webhookSecret  string
	frontendURL    string
	apiBaseURL     string
	allowedPriceID map[string]struct{}
	httpClient     *http.Client
}

type stripeCheckoutRequest struct {
	PlanID        string `json:"planId"`
	StripePriceID string `json:"stripePriceId"`
	Mode          string `json:"mode"`
	ProductSlug   string `json:"productSlug"`
	SourcePath    string `json:"sourcePath"`
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
	Items              struct {
		Data []struct {
			Price struct {
				ID string `json:"id"`
			} `json:"price"`
		} `json:"data"`
	} `json:"items"`
}

type stripeEvent struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
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
	// AmountTotal is in the currency's smallest unit (cents/分), which is how
	// Stripe reports every amount. It has to be divided by 100 before it means
	// anything in balance terms.
	AmountTotal int64             `json:"amount_total"`
	Currency    string            `json:"currency"`
	Metadata    map[string]string `json:"metadata"`
}

type stripeInvoice struct {
	ID                string `json:"id"`
	Customer          any    `json:"customer"`
	Subscription      any    `json:"subscription"`
	Status            string `json:"status"`
	PaymentIntent     any    `json:"payment_intent"`
	AmountPaid        int64  `json:"amount_paid"`
	PaidOutOfBand     bool   `json:"paid_out_of_band"`
	Currency          string `json:"currency"`
	Created           int64  `json:"created"`
	BillingReason     string `json:"billing_reason"`
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

	allowed := make(map[string]struct{}, len(cfg.AllowedPriceIDs))
	for _, priceID := range cfg.AllowedPriceIDs {
		trimmed := strings.TrimSpace(priceID)
		if trimmed != "" {
			allowed[trimmed] = struct{}{}
		}
	}

	return &stripeClient{
		secretKey:      secretKey,
		webhookSecret:  strings.TrimSpace(cfg.WebhookSecret),
		frontendURL:    strings.TrimRight(strings.TrimSpace(cfg.FrontendURL), "/"),
		allowedPriceID: allowed,
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
}

func (c *stripeClient) enabled() bool {
	return c != nil && c.secretKey != ""
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
			return mode == "payment"
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

func (c *stripeClient) doForm(ctx context.Context, method, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, stripeAPIBaseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
	baseURL := strings.TrimRight(strings.TrimSpace(c.apiBaseURL), "/")
	if baseURL == "" {
		baseURL = stripeAPIBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.secretKey)
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
		"line_items[0][price]":    []string{strings.TrimSpace(req.StripePriceID)},
		"line_items[0][quantity]": []string{"1"},
		"metadata[user_id]":       []string{strings.TrimSpace(user.ID)},
		"metadata[user_email]":    []string{strings.TrimSpace(strings.ToLower(user.Email))},
		"metadata[plan_id]":       []string{strings.TrimSpace(req.PlanID)},
		"metadata[product_slug]":  []string{strings.TrimSpace(req.ProductSlug)},
		"metadata[kind]":          []string{map[string]string{"payment": "paygo", "subscription": "subscription"}[mode]},
	}
	clientReferenceID, err := c.signedClientReferenceID(user.ID)
	if err == nil {
		form.Set("client_reference_id", clientReferenceID)
	}
	if mode == "subscription" {
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
	if err := c.doForm(ctx, http.MethodPost, "/checkout/sessions", form, &session); err != nil {
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
	form := url.Values{
		"customer":   []string{strings.TrimSpace(customerID)},
		"return_url": []string{returnURL},
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
	query.Add("expand[]", "lines.data.price")
	if err := c.doJSON(ctx, http.MethodGet, "/invoices/"+url.PathEscape(strings.TrimSpace(invoiceID))+"?"+query.Encode(), &invoice); err != nil {
		return nil, err
	}
	return &invoice, nil
}

func (c *stripeClient) listPaidInvoices(ctx context.Context, customerID string) ([]stripeInvoice, error) {
	var all []stripeInvoice
	startingAfter := ""
	for page := 0; page < 100; page++ {
		query := url.Values{"customer": {strings.TrimSpace(customerID)}, "status": {"paid"}, "limit": {"100"}}
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

func (c *stripeClient) refundPaymentIntent(ctx context.Context, paymentIntentID, idempotencyKey string) error {
	form := url.Values{"payment_intent": []string{strings.TrimSpace(paymentIntentID)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, stripeAPIBaseURL+"/refunds", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.secretKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// A retry after Stripe accepted the refund but before Accounts persisted its
	// local cancellation must return the original refund, never create another.
	if key := strings.TrimSpace(idempotencyKey); key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
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
		return fmt.Errorf("stripe refund failed: %s", strings.TrimSpace(string(body)))
	}
	return nil
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
	if !user.MFAEnabled {
		respondError(c, http.StatusForbidden, "mfa_required", "multi-factor authentication is required before starting a payment")
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
	req.ProductSlug = strings.TrimSpace(req.ProductSlug)
	req.SourcePath = strings.TrimSpace(req.SourcePath)
	req.Mode = h.stripe.normalizeMode(req.Mode)

	if req.ProductSlug == "" || !h.validateCheckoutPlan(c.Request.Context(), req) {
		respondError(c, http.StatusBadRequest, "invalid_billing_plan", "billing plan is invalid or unavailable")
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
	if !user.MFAEnabled {
		respondError(c, http.StatusForbidden, "mfa_required", "multi-factor authentication is required before managing billing")
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
			c.JSON(http.StatusOK, gin.H{"received": true, "duplicate": true})
			return
		}
	}

	procErr := h.handleStripeEvent(c.Request.Context(), event)
	if tracked {
		if err := h.store.FinishStripeWebhookEvent(c.Request.Context(), event.ID, procErr); err != nil {
			slog.Warn("failed to finalize stripe webhook event record", "err", err, "eventID", event.ID)
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
		if invoiceID == "" || h.stripe == nil {
			return nil
		}
		invoice, err := h.stripe.fetchInvoice(ctx, invoiceID)
		if err != nil {
			return err
		}
		subscriptionID := customerIDFromAny(invoice.Subscription)
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
		subscriptionID := customerIDFromAny(invoice.Subscription)
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
		if err := h.recordStripePaidInvoice(ctx, &invoice, sub, userID); err != nil {
			return err
		}
		// invoice.paid renews the billing period: re-arm quota and clear
		// arrears from the plan the subscription is on.
		plan, err := h.resolveBillingPlan(ctx, subscriptionPriceID(sub), sub.Metadata["plan_id"])
		if err != nil {
			if errors.Is(err, store.ErrBillingPlanNotFound) {
				slog.Warn("stripe invoice.paid without matching billing plan", "userID", userID, "priceID", subscriptionPriceID(sub))
				return nil
			}
			return err
		}
		previous, previousErr := h.store.GetAccountBillingProfile(ctx, userID)
		if previousErr != nil && !errors.Is(previousErr, store.ErrUserNotFound) {
			return previousErr
		}
		upgraded := previous != nil && plan.IncludedQuotaBytes > previous.IncludedQuotaBytes
		periodStart, periodEnd := subscriptionQuotaPeriod(plan, sub, time.Now())
		if upgraded {
			if err := h.applyPlanUpgradeQuota(ctx, userID, previous.IncludedQuotaBytes, plan.IncludedQuotaBytes, periodStart, periodEnd); err != nil {
				return err
			}
		}
		if err := h.applyPlanEntitlements(ctx, userID, plan); err != nil {
			return err
		}
		if !upgraded {
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
	financeInvoices, err := h.store.ListFinanceInvoices(ctx, userID, subscriptionUUID, 500)
	if err != nil {
		return err
	}
	var financeInvoice *store.FinanceInvoice
	for i := range financeInvoices {
		if financeInvoices[i].Provider == "stripe" && financeInvoices[i].ProviderInvoiceID == invoice.ID {
			financeInvoice = &financeInvoices[i]
			break
		}
	}
	if financeInvoice == nil {
		financeInvoice = &store.FinanceInvoice{IdempotencyKey: "stripe-invoice:" + invoice.ID, AccountUUID: userID, SubscriptionUUID: subscriptionUUID, Provider: "stripe", ProviderInvoiceID: invoice.ID, AmountMinor: paidMinor, Currency: currency, Description: "Verified XConnect subscription invoice", IssuedAt: paidAt, ServicePeriodStart: &periodStart, ServicePeriodEnd: &periodEnd}
		if _, err := h.store.CreateFinanceInvoice(ctx, financeInvoice); err != nil {
			return err
		}
	}
	financePayment := &store.FinancePayment{IdempotencyKey: "stripe-payment:" + invoice.ID, InvoiceID: financeInvoice.ID, AccountUUID: userID, Provider: "stripe", ProviderPaymentID: paymentID, AmountMinor: paidMinor, Currency: currency, PaidAt: paidAt}
	if _, err := h.store.RecordFinancePayment(ctx, financePayment); err != nil && !errors.Is(err, store.ErrFinanceInvoiceAlreadyPaid) {
		return err
	}
	if !referralCampaignActive(paidAt) {
		return nil
	}
	if planID == "PRO-YEARLY" {
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
			amount, amountErr := stripeAmountToCurrencyMinor(currency, verified.AmountPaid)
			if amountErr != nil {
				return amountErr
			}
			invoice := &store.FinanceInvoice{IdempotencyKey: "stripe-invoice:" + verified.ID, AccountUUID: referrerUUID, SubscriptionUUID: local.ID, Provider: "stripe", ProviderInvoiceID: verified.ID, AmountMinor: amount, Currency: currency, Description: "Verified XConnect inviter subscription invoice", IssuedAt: invoicePaidAt, ServicePeriodStart: &periodStart, ServicePeriodEnd: &periodEnd}
			if _, err = h.store.CreateFinanceInvoice(ctx, invoice); err != nil {
				return err
			}
			payment := &store.FinancePayment{IdempotencyKey: "stripe-payment:" + verified.ID, InvoiceID: invoice.ID, AccountUUID: referrerUUID, Provider: "stripe", ProviderPaymentID: paymentIntentID, AmountMinor: amount, Currency: currency, PaidAt: invoicePaidAt}
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
		if plan.Kind != "subscription" || (plan.PlanID != "PRO-MONTHLY" && plan.PlanID != "PRO-YEARLY") {
			return "", nil
		}
		wantUnit := "month"
		if plan.PlanID == "PRO-YEARLY" {
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
	if source == nil || source.CurrentPeriodStart <= 0 || source.CurrentPeriodEnd <= source.CurrentPeriodStart {
		return naturalMonthPeriod(time.Now())
	}
	return time.Unix(source.CurrentPeriodStart, 0).UTC(), time.Unix(source.CurrentPeriodEnd, 0).UTC()
}

// syncSubscriptionEntitlements applies catalog entitlements for an active or
// trialing subscription; freshly created subscriptions also re-arm quota and
// supersede any live trial.
func (h *handler) syncSubscriptionEntitlements(ctx context.Context, source *stripeSubscription, created bool) error {
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
	if created {
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
	if source == nil {
		return nil
	}
	userID := strings.TrimSpace(source.Metadata["user_id"])
	if userID == "" {
		return nil
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
	if source.CancelAtPeriodEnd && status != "cancelled" {
		status = "canceling"
	}
	subscription := &store.Subscription{
		UserID:        userID,
		Provider:      "stripe",
		PaymentMethod: "stripe",
		Kind:          kind,
		PlanID:        strings.TrimSpace(source.Metadata["plan_id"]),
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
