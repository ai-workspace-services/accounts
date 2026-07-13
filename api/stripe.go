package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

const stripeAPIBaseURL = "https://api.stripe.com/v1"

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
	Items              struct {
		Data []struct {
			Price struct {
				ID string `json:"id"`
			} `json:"price"`
		} `json:"data"`
	} `json:"items"`
}

type stripeEvent struct {
	Type string `json:"type"`
	Data struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

type stripeCheckoutSession struct {
	ID            string            `json:"id"`
	Mode          string            `json:"mode"`
	Subscription  string            `json:"subscription"`
	PaymentIntent string            `json:"payment_intent"`
	Customer      string            `json:"customer"`
	PaymentStatus string            `json:"payment_status"`
	Metadata      map[string]string `json:"metadata"`
}

type stripeInvoice struct {
	ID           string `json:"id"`
	Customer     any    `json:"customer"`
	Subscription any    `json:"subscription"`
	Status       string `json:"status"`
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
		"line_items[0][price]":    []string{strings.TrimSpace(req.StripePriceID)},
		"line_items[0][quantity]": []string{"1"},
		"metadata[user_id]":       []string{strings.TrimSpace(user.ID)},
		"metadata[user_email]":    []string{strings.TrimSpace(strings.ToLower(user.Email))},
		"metadata[plan_id]":       []string{strings.TrimSpace(req.PlanID)},
		"metadata[product_slug]":  []string{strings.TrimSpace(req.ProductSlug)},
		"metadata[kind]":          []string{map[string]string{"payment": "paygo", "subscription": "subscription"}[mode]},
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
	req.ProductSlug = strings.TrimSpace(req.ProductSlug)
	req.SourcePath = strings.TrimSpace(req.SourcePath)
	req.Mode = h.stripe.normalizeMode(req.Mode)

	if req.PlanID == "" || req.ProductSlug == "" || !h.stripe.validPriceID(req.StripePriceID) {
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

	if err := h.handleStripeEvent(c.Request.Context(), event); err != nil {
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
			return h.upsertStripeSubscription(ctx, sub, session.Customer)
		}

		userID := strings.TrimSpace(session.Metadata["user_id"])
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
		return h.store.UpsertSubscription(ctx, sub)
	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted":
		var subscription stripeSubscription
		if err := json.Unmarshal(event.Data.Object, &subscription); err != nil {
			return err
		}
		return h.upsertStripeSubscription(ctx, &subscription, customerIDFromAny(subscription.Customer))
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
		return h.upsertStripeSubscription(ctx, sub, customerIDFromAny(invoice.Customer))
	default:
		return nil
	}
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
