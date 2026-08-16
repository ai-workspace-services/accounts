package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

// Billing plan catalog endpoints (billing P1). The public listing backs the
// console pricing page; the admin CRUD lets operators adjust plans without a
// deploy. Admin access reuses the settings permissions.

type billingPlanPayload struct {
	PlanID             string             `json:"planId"`
	StripePriceID      string             `json:"stripePriceId,omitempty"`
	DisplayName        string             `json:"displayName"`
	Kind               string             `json:"kind"`
	IncludedQuotaBytes int64              `json:"includedQuotaBytes"`
	PackageName        string             `json:"packageName"`
	PriceAmount        int64              `json:"priceAmount"`
	PriceCurrency      string             `json:"priceCurrency,omitempty"`
	PriceUnit          string             `json:"priceUnit,omitempty"`
	PriceMultipliers   map[string]float64 `json:"priceMultipliers,omitempty"`
	Features           map[string]any     `json:"features,omitempty"`
	TrialDays          int                `json:"trialDays"`
	Active             bool               `json:"active"`
	SortOrder          int                `json:"sortOrder"`
	// Reason is write-only: operators supply it when changing the catalog and
	// it is stored in the audit trail, never echoed back in listings.
	Reason string `json:"reason,omitempty"`
}

func billingPlanToPayload(plan *store.BillingPlan) billingPlanPayload {
	return billingPlanPayload{
		PlanID:             plan.PlanID,
		StripePriceID:      plan.StripePriceID,
		DisplayName:        plan.DisplayName,
		Kind:               plan.Kind,
		IncludedQuotaBytes: plan.IncludedQuotaBytes,
		PackageName:        plan.PackageName,
		PriceAmount:        plan.PriceAmount,
		PriceCurrency:      plan.PriceCurrency,
		PriceUnit:          plan.PriceUnit,
		PriceMultipliers:   plan.PriceMultipliers,
		Features:           plan.Features,
		TrialDays:          plan.TrialDays,
		Active:             plan.Active,
		SortOrder:          plan.SortOrder,
	}
}

// listPublicBillingPlans serves the active catalog for the pricing page.
func (h *handler) listPublicBillingPlans(c *gin.Context) {
	plans, err := h.store.ListBillingPlans(c.Request.Context(), false)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "billing_plans_unavailable", "failed to load billing plans")
		return
	}
	payload := make([]billingPlanPayload, 0, len(plans))
	for i := range plans {
		payload = append(payload, billingPlanToPayload(&plans[i]))
	}
	c.JSON(http.StatusOK, gin.H{"plans": payload})
}

func (h *handler) adminListBillingPlans(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminSettingsRead); !ok {
		return
	}
	plans, err := h.store.ListBillingPlans(c.Request.Context(), true)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "billing_plans_unavailable", "failed to load billing plans")
		return
	}
	payload := make([]billingPlanPayload, 0, len(plans))
	for i := range plans {
		payload = append(payload, billingPlanToPayload(&plans[i]))
	}
	c.JSON(http.StatusOK, gin.H{"plans": payload})
}

func (h *handler) adminUpsertBillingPlan(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminBillingMoneyWrite)
	if !ok {
		return
	}
	planID := strings.TrimSpace(c.Param("planId"))
	if planID == "" {
		respondError(c, http.StatusBadRequest, "plan_id_required", "plan id is required")
		return
	}

	var req billingPlanPayload
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid billing plan payload")
		return
	}
	reason, ok := requireReason(c, req.Reason)
	if !ok {
		return
	}
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	switch kind {
	case "trial", "subscription", "paygo_topup":
	default:
		respondError(c, http.StatusBadRequest, "invalid_plan_kind", "kind must be trial, subscription or paygo_topup")
		return
	}
	if req.IncludedQuotaBytes < 0 || req.TrialDays < 0 {
		respondError(c, http.StatusBadRequest, "invalid_request", "quota and trial days must be non-negative")
		return
	}
	// The list price is what the storefront shows. A negative amount, or an
	// amount with no currency to read it in, would render as a nonsense price
	// next to a working buy button.
	if req.PriceAmount < 0 {
		respondError(c, http.StatusBadRequest, "invalid_price", "priceAmount must be non-negative")
		return
	}
	priceCurrency := strings.ToUpper(strings.TrimSpace(req.PriceCurrency))
	if req.PriceAmount > 0 && priceCurrency == "" {
		respondError(c, http.StatusBadRequest, "invalid_price", "priceCurrency is required when priceAmount is set")
		return
	}
	if priceCurrency != "" && len(priceCurrency) != 3 {
		respondError(c, http.StatusBadRequest, "invalid_price", "priceCurrency must be a 3-letter ISO 4217 code")
		return
	}
	priceUnit := strings.ToLower(strings.TrimSpace(req.PriceUnit))
	switch priceUnit {
	case "", "month", "year", "once", "gb":
	default:
		respondError(c, http.StatusBadRequest, "invalid_price", "priceUnit must be month, year, once or gb")
		return
	}
	if priceID := strings.TrimSpace(req.StripePriceID); priceID != "" && !strings.HasPrefix(priceID, "price_") {
		respondError(c, http.StatusBadRequest, "invalid_price_id", "stripePriceId must be a Stripe price id")
		return
	}

	plan := &store.BillingPlan{
		PlanID:             planID,
		StripePriceID:      strings.TrimSpace(req.StripePriceID),
		DisplayName:        strings.TrimSpace(req.DisplayName),
		Kind:               kind,
		IncludedQuotaBytes: req.IncludedQuotaBytes,
		PackageName:        strings.TrimSpace(req.PackageName),
		PriceAmount:        req.PriceAmount,
		PriceCurrency:      priceCurrency,
		PriceUnit:          priceUnit,
		PriceMultipliers:   req.PriceMultipliers,
		Features:           req.Features,
		TrialDays:          req.TrialDays,
		Active:             req.Active,
		SortOrder:          req.SortOrder,
	}
	if plan.PackageName == "" {
		plan.PackageName = "default"
	}
	// The public pricing page reads this catalog live, so an upsert here is a
	// price/packaging publish. Capture what it replaced before overwriting.
	var before map[string]any
	if existing, err := h.store.GetBillingPlan(c.Request.Context(), planID); err == nil && existing != nil {
		before = map[string]any{
			"display_name":         existing.DisplayName,
			"stripe_price_id":      existing.StripePriceID,
			"included_quota_bytes": existing.IncludedQuotaBytes,
			"package_name":         existing.PackageName,
			"price_amount":         existing.PriceAmount,
			"price_currency":       existing.PriceCurrency,
			"price_unit":           existing.PriceUnit,
			"active":               existing.Active,
			"sort_order":           existing.SortOrder,
		}
	}

	if err := h.store.UpsertBillingPlan(c.Request.Context(), plan); err != nil {
		respondError(c, http.StatusInternalServerError, "billing_plan_save_failed", "failed to save billing plan")
		return
	}

	after := map[string]any{
		"display_name":         plan.DisplayName,
		"stripe_price_id":      plan.StripePriceID,
		"included_quota_bytes": plan.IncludedQuotaBytes,
		"package_name":         plan.PackageName,
		"price_amount":         plan.PriceAmount,
		"price_currency":       plan.PriceCurrency,
		"price_unit":           plan.PriceUnit,
		"active":               plan.Active,
		"sort_order":           plan.SortOrder,
	}
	if err := h.recordAudit(c.Request.Context(), actor.ID, store.AuditActionPlanUpsert,
		auditDetails(planID, reason, before, after)); err != nil {
		respondError(c, http.StatusInternalServerError, "audit_write_failed",
			"the plan was saved but the audit entry could not be written")
		return
	}

	c.JSON(http.StatusOK, gin.H{"plan": billingPlanToPayload(plan)})
}

func (h *handler) adminDeleteBillingPlan(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminBillingMoneyWrite)
	if !ok {
		return
	}
	planID := strings.TrimSpace(c.Param("planId"))
	if planID == "" {
		respondError(c, http.StatusBadRequest, "plan_id_required", "plan id is required")
		return
	}
	reason, ok := requireReason(c, c.Query("reason"))
	if !ok {
		return
	}

	// Snapshot before deleting: existing subscriptions still reference this
	// plan for renewal and reconciliation, so what it contained has to survive
	// in the audit trail even though the row does not.
	var before map[string]any
	if existing, err := h.store.GetBillingPlan(c.Request.Context(), planID); err == nil && existing != nil {
		before = map[string]any{
			"display_name":         existing.DisplayName,
			"stripe_price_id":      existing.StripePriceID,
			"included_quota_bytes": existing.IncludedQuotaBytes,
			"package_name":         existing.PackageName,
			"price_amount":         existing.PriceAmount,
			"price_currency":       existing.PriceCurrency,
			"price_unit":           existing.PriceUnit,
			"active":               existing.Active,
		}
	}

	if err := h.store.DeleteBillingPlan(c.Request.Context(), planID); err != nil {
		if errors.Is(err, store.ErrBillingPlanNotFound) {
			respondError(c, http.StatusNotFound, "billing_plan_not_found", "billing plan not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "billing_plan_delete_failed", "failed to delete billing plan")
		return
	}

	if err := h.recordAudit(c.Request.Context(), actor.ID, store.AuditActionPlanDelete,
		auditDetails(planID, reason, before, nil)); err != nil {
		respondError(c, http.StatusInternalServerError, "audit_write_failed",
			"the plan was deleted but the audit entry could not be written")
		return
	}

	c.JSON(http.StatusOK, gin.H{"deleted": true})
}
