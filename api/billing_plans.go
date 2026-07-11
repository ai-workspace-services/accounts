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
	PriceMultipliers   map[string]float64 `json:"priceMultipliers,omitempty"`
	Features           map[string]any     `json:"features,omitempty"`
	TrialDays          int                `json:"trialDays"`
	Active             bool               `json:"active"`
	SortOrder          int                `json:"sortOrder"`
}

func billingPlanToPayload(plan *store.BillingPlan) billingPlanPayload {
	return billingPlanPayload{
		PlanID:             plan.PlanID,
		StripePriceID:      plan.StripePriceID,
		DisplayName:        plan.DisplayName,
		Kind:               plan.Kind,
		IncludedQuotaBytes: plan.IncludedQuotaBytes,
		PackageName:        plan.PackageName,
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
	if _, ok := h.requireAdminPermission(c, permissionAdminSettingsWrite); !ok {
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
		PriceMultipliers:   req.PriceMultipliers,
		Features:           req.Features,
		TrialDays:          req.TrialDays,
		Active:             req.Active,
		SortOrder:          req.SortOrder,
	}
	if plan.PackageName == "" {
		plan.PackageName = "default"
	}
	if err := h.store.UpsertBillingPlan(c.Request.Context(), plan); err != nil {
		respondError(c, http.StatusInternalServerError, "billing_plan_save_failed", "failed to save billing plan")
		return
	}
	c.JSON(http.StatusOK, gin.H{"plan": billingPlanToPayload(plan)})
}

func (h *handler) adminDeleteBillingPlan(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminSettingsWrite); !ok {
		return
	}
	planID := strings.TrimSpace(c.Param("planId"))
	if planID == "" {
		respondError(c, http.StatusBadRequest, "plan_id_required", "plan id is required")
		return
	}
	if err := h.store.DeleteBillingPlan(c.Request.Context(), planID); err != nil {
		if errors.Is(err, store.ErrBillingPlanNotFound) {
			respondError(c, http.StatusNotFound, "billing_plan_not_found", "billing plan not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "billing_plan_delete_failed", "failed to delete billing plan")
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}
