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
	PayURL          string
	AllowedPriceIDs []string
	FrontendURL     string
}

type stripeClient struct {
	secretKey      string
	webhookSecret  string
	payURL         string
	frontendURL    string
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
	ID            string `json:"id"`
	Customer      any    `json:"customer"`
	Subscription  any    `json:"subscription"`
	Status        string `json:"status"`
	PaymentIntent any    `json:"payment_intent"`
}

func newStripeClient(cfg StripeConfig) *stripeClient {
	secretKey := strings.TrimSpace(cfg.SecretKey)
	payURL := strings.TrimSpace(cfg.PayURL)
	if secretKey == "" && payURL == "" {
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
		payURL:         payURL,
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

// paymentLinkURL decorates the configured Payment Link with Stripe's
// documented URL parameters. Payment Link configuration remains in Stripe
// Dashboard, where the account can enable all eligible payment methods; this
// helper only carries the authenticated account reference and email.
func (c *stripeClient) paymentLinkURL(user *store.User) (string, error) {
	if c == nil || strings.TrimSpace(c.payURL) == "" {
		return "", errors.New("stripe payment link is not configured")
	}
	if user == nil || !validStripeClientReferenceID(user.ID) {
		return "", errors.New("user id is not a valid stripe client reference")
	}
	parsed, err := url.Parse(c.payURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", errors.New("stripe payment link must be an https URL")
	}
	query := parsed.Query()
	clientReferenceID, err := c.signedClientReferenceID(user.ID)
	if err != nil {
		return "", err
	}
	query.Set("client_reference_id", clientReferenceID)
	if email := strings.TrimSpace(strings.ToLower(user.Email)); email != "" {
		query.Set("prefilled_email", email)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
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
	req, err := http.NewRequestWithContext(ctx, method, stripeAPIBaseURL+path, nil)
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

func (c *stripeClient) fetchSubscription(ctx context.Context, subscriptionID string) (*stripeSubscription, error) {
	var sub stripeSubscription
	if err := c.doJSON(ctx, http.MethodGet, "/subscriptions/"+url.PathEscape(strings.TrimSpace(subscriptionID)), &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

func (c *stripeClient) fetchInvoice(ctx context.Context, invoiceID string) (*stripeInvoice, error) {
	var invoice stripeInvoice
	if err := c.doJSON(ctx, http.MethodGet, "/invoices/"+url.PathEscape(strings.TrimSpace(invoiceID)), &invoice); err != nil {
		return nil, err
	}
	return &invoice, nil
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

// stripePay redirects an authenticated user to the configured Stripe Payment
// Link. Payment Links support multiple payment methods configured in Stripe
// Dashboard, while client_reference_id lets the webhook reconcile the
// resulting Checkout Session back to this account.
func (h *handler) stripePay(c *gin.Context) {
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
	if h.stripe == nil || strings.TrimSpace(h.stripe.payURL) == "" {
		respondError(c, http.StatusServiceUnavailable, "stripe_pay_link_not_configured", "stripe payment link is not configured")
		return
	}

	link, err := h.stripe.paymentLinkURL(user)
	if err != nil {
		respondError(c, http.StatusServiceUnavailable, "stripe_pay_link_not_configured", "stripe payment link is not configured")
		return
	}
	c.Redirect(http.StatusSeeOther, link)
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
			if err := h.downgradeToFreePlan(ctx, userID); err != nil {
				return err
			}
			h.publishBillingEvent(ctx, &store.BillingEvent{
				Type: "subscription_deleted", UserID: userID,
				PlanID:  strings.TrimSpace(subscription.Metadata["plan_id"]),
				PriceID: subscriptionPriceID(&subscription), ExternalID: subscription.ID,
			})
		}
		return nil
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
		if err := h.applyPlanEntitlements(ctx, userID, plan); err != nil {
			return err
		}
		periodStart, periodEnd := subscriptionQuotaPeriod(plan, sub, time.Now())
		if err := h.resetQuotaForPlan(ctx, userID, plan, periodStart, periodEnd); err != nil {
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
	if err := h.applyPlanEntitlements(ctx, userID, plan); err != nil {
		return err
	}
	if created {
		periodStart, periodEnd := subscriptionQuotaPeriod(plan, source, time.Now())
		if err := h.resetQuotaForPlan(ctx, userID, plan, periodStart, periodEnd); err != nil {
			return err
		}
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
		"price_id":     priceID,
		"customer_id":  firstNonEmpty(customerID, customerIDFromAny(source.Customer)),
		"product_slug": source.Metadata["product_slug"],
		"user_email":   source.Metadata["user_email"],
		"startsAt":     epochToRFC3339(source.CurrentPeriodStart),
		"expiresAt":    epochToRFC3339(source.CurrentPeriodEnd),
	})
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
	if status == "cancelled" || source.CancelAtPeriodEnd {
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
