package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

// Operations console endpoints (ops P0). Everything here is an operator
// acting on somebody else's account, so every write records an audit entry
// with a mandatory reason and the before/after values.
//
// These deliberately reuse the entitlement helpers the Stripe webhook path
// already uses (applyPlanEntitlements / resetQuotaForPlan), so a manual grant
// and an automatic one converge on identical state instead of drifting.

type adminAssignPlanRequest struct {
	PlanID string `json:"planId"`
	Reason string `json:"reason"`
}

type adminAdjustQuotaRequest struct {
	// RemainingIncludedQuota is the absolute value to set, in bytes. Absolute
	// rather than a delta on purpose: an operator reading "20 GB" off the
	// screen and typing "20 GB" should end at 20 GB regardless of what raced
	// with them.
	RemainingIncludedQuota int64  `json:"remainingIncludedQuota"`
	Reason                 string `json:"reason"`
}

type adminAdjustBalanceRequest struct {
	// Delta is signed: positive credits, negative debits. A delta rather than
	// an absolute so concurrent adjustments add up instead of overwriting one
	// another, which is also what makes the ledger entry meaningful.
	Delta  float64 `json:"delta"`
	Reason string  `json:"reason"`
}

type adminGrantTrialRequest struct {
	// PlanID defaults to the catalog trial plan when omitted.
	PlanID string `json:"planId"`
	// Days defaults to the plan's own trial_days, then to 7.
	Days   int    `json:"days"`
	Reason string `json:"reason"`
}

// resolveTargetUser loads the account an operator is acting on and rejects
// the root account, matching the protection already applied to role and
// group changes.
func (h *handler) resolveTargetUser(c *gin.Context) (*store.User, bool) {
	accountUUID := strings.TrimSpace(c.Param("accountUUID"))
	if accountUUID == "" {
		respondError(c, http.StatusBadRequest, "account_uuid_required", "account uuid is required")
		return nil, false
	}
	user, err := h.store.GetUserByID(c.Request.Context(), accountUUID)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			respondError(c, http.StatusNotFound, "user_not_found", "user not found")
			return nil, false
		}
		respondError(c, http.StatusInternalServerError, "user_lookup_failed", "failed to load user")
		return nil, false
	}
	if h.isRootAccount(user) {
		respondError(c, http.StatusForbidden, "root_protected", "root account cannot be modified")
		return nil, false
	}
	return user, true
}

// adminGetBillingAccount returns everything the single-account operations
// view needs in one round trip: profile, quota state, subscriptions and the
// most recent ledger entries.
func (h *handler) adminGetBillingAccount(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminUsersListRead); !ok {
		return
	}
	accountUUID := strings.TrimSpace(c.Param("accountUUID"))
	if accountUUID == "" {
		respondError(c, http.StatusBadRequest, "account_uuid_required", "account uuid is required")
		return
	}

	ctx := c.Request.Context()
	user, err := h.store.GetUserByID(ctx, accountUUID)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			respondError(c, http.StatusNotFound, "user_not_found", "user not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "user_lookup_failed", "failed to load user")
		return
	}

	// The billing rows are all optional: an account that has never been
	// through entitlement sync legitimately has none of them, and that is
	// exactly the case operators need to see and fix.
	var profile *store.AccountBillingProfile
	if loaded, err := h.store.GetAccountBillingProfile(ctx, accountUUID); err == nil {
		profile = loaded
	}
	var quota *store.AccountQuotaState
	if loaded, err := h.store.GetAccountQuotaState(ctx, accountUUID); err == nil {
		quota = loaded
	}
	subscriptions, err := h.store.ListSubscriptionsByUser(ctx, accountUUID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "subscriptions_unavailable", "failed to load subscriptions")
		return
	}
	sanitizedSubs := make([]gin.H, 0, len(subscriptions))
	for i := range subscriptions {
		sanitizedSubs = append(sanitizedSubs, sanitizeSubscription(&subscriptions[i]))
	}
	ledger, err := h.store.ListBillingLedgerByAccount(ctx, accountUUID, 20)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "ledger_unavailable", "failed to load ledger")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"user":           sanitizeUser(user, nil),
		"billingProfile": profile,
		"quotaState":     quota,
		"subscriptions":  sanitizedSubs,
		"ledger":         ledger,
	})
}

// adminAssignPlan applies a catalog plan's entitlements to an account without
// going through Stripe. This is how custom/contract tiers get provisioned and
// how a mis-synced account gets corrected.
func (h *handler) adminAssignPlan(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminSettingsWrite)
	if !ok {
		return
	}
	user, ok := h.resolveTargetUser(c)
	if !ok {
		return
	}

	var req adminAssignPlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}
	reason, ok := requireReason(c, req.Reason)
	if !ok {
		return
	}
	planID := strings.TrimSpace(req.PlanID)
	if planID == "" {
		respondError(c, http.StatusBadRequest, "plan_id_required", "planId is required")
		return
	}

	ctx := c.Request.Context()
	plan, err := h.store.GetBillingPlan(ctx, planID)
	if err != nil {
		if errors.Is(err, store.ErrBillingPlanNotFound) {
			respondError(c, http.StatusNotFound, "plan_not_found", "billing plan not found")
			return
		}
		respondError(c, http.StatusInternalServerError, "plan_lookup_failed", "failed to load billing plan")
		return
	}

	before := map[string]any{}
	if existing, err := h.store.GetAccountBillingProfile(ctx, user.ID); err == nil && existing != nil {
		before["package_name"] = existing.PackageName
		before["included_quota_bytes"] = existing.IncludedQuotaBytes
		before["pricing_rule_version"] = existing.PricingRuleVersion
	}

	if err := h.applyPlanEntitlements(ctx, user.ID, plan); err != nil {
		respondError(c, http.StatusInternalServerError, "entitlement_apply_failed", "failed to apply plan entitlements")
		return
	}
	now := time.Now().UTC()
	periodStart, periodEnd := naturalMonthPeriod(now)
	if err := h.resetQuotaForPlan(ctx, user.ID, plan, periodStart, periodEnd); err != nil {
		respondError(c, http.StatusInternalServerError, "quota_reset_failed", "failed to reset quota for plan")
		return
	}

	after := map[string]any{
		"plan_id":              plan.PlanID,
		"package_name":         plan.PackageName,
		"included_quota_bytes": plan.IncludedQuotaBytes,
	}
	if err := h.recordAudit(ctx, actor.ID, store.AuditActionEntitlementGrant,
		auditDetails(user.ID, reason, before, after)); err != nil {
		respondError(c, http.StatusInternalServerError, "audit_write_failed",
			"entitlements were applied but the audit entry could not be written")
		return
	}

	h.publishBillingEvent(ctx, &store.BillingEvent{
		Type: "entitlement_granted", UserID: user.ID, PlanID: plan.PlanID,
	})

	c.JSON(http.StatusOK, gin.H{
		"accountUuid": user.ID,
		"planId":      plan.PlanID,
		"packageName": plan.PackageName,
	})
}

// adminAdjustQuota sets the remaining included quota outright. Used for
// goodwill top-ups and for correcting a bad rating run.
func (h *handler) adminAdjustQuota(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminSettingsWrite)
	if !ok {
		return
	}
	user, ok := h.resolveTargetUser(c)
	if !ok {
		return
	}

	var req adminAdjustQuotaRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}
	reason, ok := requireReason(c, req.Reason)
	if !ok {
		return
	}
	if req.RemainingIncludedQuota < 0 {
		respondError(c, http.StatusBadRequest, "invalid_quota", "remainingIncludedQuota must not be negative")
		return
	}

	ctx := c.Request.Context()
	state, err := h.store.GetAccountQuotaState(ctx, user.ID)
	if err != nil || state == nil {
		// No quota row yet is a normal state for accounts that predate
		// entitlement sync; create one rather than making the operator go
		// assign a plan first.
		state = &store.AccountQuotaState{AccountUUID: user.ID}
	}

	before := map[string]any{"remaining_included_quota": state.RemainingIncludedQuota}
	state.RemainingIncludedQuota = req.RemainingIncludedQuota
	state.EffectiveAt = time.Now().UTC()
	if err := h.store.UpsertAccountQuotaState(ctx, state); err != nil {
		respondError(c, http.StatusInternalServerError, "quota_save_failed", "failed to save quota state")
		return
	}

	after := map[string]any{"remaining_included_quota": state.RemainingIncludedQuota}
	if err := h.recordAudit(ctx, actor.ID, store.AuditActionQuotaAdjust,
		auditDetails(user.ID, reason, before, after)); err != nil {
		respondError(c, http.StatusInternalServerError, "audit_write_failed",
			"quota was adjusted but the audit entry could not be written")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"accountUuid":            user.ID,
		"remainingIncludedQuota": state.RemainingIncludedQuota,
	})
}

// adminAdjustBalance credits or debits the prepaid balance and writes a
// matching ledger entry.
//
// The ledger entry is not optional bookkeeping: balance has to stay equal to
// the sum of its ledger, otherwise reconciliation can no longer explain a
// difference — the failure mode that quietly ruins billing systems. So the
// ledger row is written first and the balance is derived from it.
func (h *handler) adminAdjustBalance(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminBillingMoneyWrite)
	if !ok {
		return
	}
	user, ok := h.resolveTargetUser(c)
	if !ok {
		return
	}

	var req adminAdjustBalanceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}
	reason, ok := requireReason(c, req.Reason)
	if !ok {
		return
	}
	if req.Delta == 0 {
		respondError(c, http.StatusBadRequest, "invalid_delta", "delta must be non-zero")
		return
	}

	ctx := c.Request.Context()
	state, err := h.store.GetAccountQuotaState(ctx, user.ID)
	if err != nil || state == nil {
		state = &store.AccountQuotaState{AccountUUID: user.ID}
	}

	before := map[string]any{"current_balance": state.CurrentBalance}
	newBalance := state.CurrentBalance + req.Delta

	now := time.Now().UTC()
	entry := &store.BillingLedgerEntry{
		AccountUUID:        user.ID,
		BucketStart:        now,
		BucketEnd:          now,
		EntryType:          "manual_adjustment",
		RatedBytes:         0,
		AmountDelta:        req.Delta,
		BalanceAfter:       newBalance,
		PricingRuleVersion: "manual",
	}
	if err := h.store.InsertBillingLedgerEntry(ctx, entry); err != nil {
		respondError(c, http.StatusInternalServerError, "ledger_write_failed", "failed to write ledger entry")
		return
	}

	state.CurrentBalance = newBalance
	state.EffectiveAt = now
	if err := h.store.UpsertAccountQuotaState(ctx, state); err != nil {
		respondError(c, http.StatusInternalServerError, "balance_save_failed", "failed to save balance")
		return
	}

	after := map[string]any{"current_balance": newBalance}
	details := auditDetails(user.ID, reason, before, after)
	details["delta"] = req.Delta
	details["ledger_entry_id"] = entry.ID
	if err := h.recordAudit(ctx, actor.ID, store.AuditActionBalanceAdjust, details); err != nil {
		respondError(c, http.StatusInternalServerError, "audit_write_failed",
			"balance was adjusted but the audit entry could not be written")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"accountUuid":    user.ID,
		"currentBalance": newBalance,
		"ledgerEntryId":  entry.ID,
	})
}

// adminGrantTrial provisions trial entitlements for an existing account.
//
// provisionTrialEntitlements already does this for newly registered users but
// is hardcoded to TRIAL-7D and only runs at signup, which is why accounts
// created any other way (bootstrap, import, migration) end up with no
// entitlements at all. This is the same flow with the plan and duration made
// explicit.
func (h *handler) adminGrantTrial(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminSettingsWrite)
	if !ok {
		return
	}
	user, ok := h.resolveTargetUser(c)
	if !ok {
		return
	}

	var req adminGrantTrialRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}
	reason, ok := requireReason(c, req.Reason)
	if !ok {
		return
	}

	planID := strings.TrimSpace(req.PlanID)
	if planID == "" {
		planID = store.BillingPlanTrial7D
	}

	ctx := c.Request.Context()
	plan, err := h.store.GetBillingPlan(ctx, planID)
	if err != nil {
		if errors.Is(err, store.ErrBillingPlanNotFound) {
			respondError(c, http.StatusNotFound, "plan_not_found", "trial plan not found in the catalog")
			return
		}
		respondError(c, http.StatusInternalServerError, "plan_lookup_failed", "failed to load billing plan")
		return
	}

	days := req.Days
	if days <= 0 {
		days = plan.TrialDays
	}
	if days <= 0 {
		days = 7
	}

	if err := h.applyPlanEntitlements(ctx, user.ID, plan); err != nil {
		respondError(c, http.StatusInternalServerError, "entitlement_apply_failed", "failed to apply trial entitlements")
		return
	}
	now := time.Now().UTC()
	expiresAt := now.AddDate(0, 0, days)
	if err := h.resetQuotaForPlan(ctx, user.ID, plan, now, expiresAt); err != nil {
		respondError(c, http.StatusInternalServerError, "quota_reset_failed", "failed to reset quota for trial")
		return
	}

	after := map[string]any{
		"plan_id":              plan.PlanID,
		"included_quota_bytes": plan.IncludedQuotaBytes,
		"days":                 days,
		"expires_at":           expiresAt.Format(time.RFC3339),
	}
	if err := h.recordAudit(ctx, actor.ID, store.AuditActionTrialGrant,
		auditDetails(user.ID, reason, nil, after)); err != nil {
		respondError(c, http.StatusInternalServerError, "audit_write_failed",
			"trial was granted but the audit entry could not be written")
		return
	}

	h.publishBillingEvent(ctx, &store.BillingEvent{
		Type: "trial_provisioned", UserID: user.ID, PlanID: plan.PlanID,
	})

	c.JSON(http.StatusOK, gin.H{
		"accountUuid": user.ID,
		"planId":      plan.PlanID,
		"days":        days,
		"expiresAt":   expiresAt.Format(time.RFC3339),
	})
}
