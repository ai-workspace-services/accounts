package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

const (
	maxAdminPlanGroupBatch = 500
	adminPlanPreviewTTL    = 10 * time.Minute
)

type adminPlanGroupUpdate struct {
	UserID     string          `json:"userId"`
	PlanID     string          `json:"planId,omitempty"`
	Groups     *[]string       `json:"groups,omitempty"`
	ValidFrom  json.RawMessage `json:"validFrom,omitempty"`
	ValidUntil json.RawMessage `json:"validUntil,omitempty"`
}

type adminPlanGroupRequest struct {
	Mode         string                 `json:"mode"`
	RequestID    string                 `json:"requestId"`
	PreviewToken string                 `json:"previewToken"`
	Reason       string                 `json:"reason"`
	Updates      []adminPlanGroupUpdate `json:"updates"`
}

type adminPlanGroupPreviewRow struct {
	UserID                     string   `json:"userId"`
	PlanID                     string   `json:"planId"`
	Before                     gin.H    `json:"before"`
	After                      gin.H    `json:"after"`
	Groups                     []string `json:"groups"`
	UsedBytesPreserved         int64    `json:"usedBytesPreserved"`
	RemainingIncludedQuota     int64    `json:"remainingIncludedQuota"`
	ConfigurationSyncWillPause bool     `json:"configurationSyncWillPause"`
}

func (h *handler) adminAssignPlanGroup(c *gin.Context) {
	userID := strings.TrimSpace(c.Param("userId"))
	if userID == "" {
		respondError(c, http.StatusBadRequest, "userId_required", "userId is required")
		return
	}
	h.adminChangePlanGroups(c, []string{userID})
}

func (h *handler) adminAssignPlanGroupsBatch(c *gin.Context) {
	h.adminChangePlanGroups(c, nil)
}

func (h *handler) adminChangePlanGroups(c *gin.Context, pathUserIDs []string) {
	actor, ok := h.requireAdminPermission(c, permissionAdminSettingsWrite)
	if !ok {
		return
	}
	var req adminPlanGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}
	reason, ok := requireReason(c, req.Reason)
	if !ok {
		return
	}
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" || len(requestID) > 128 {
		respondError(c, http.StatusBadRequest, "request_id_required", "requestId must be 1 to 128 characters")
		return
	}
	if req.Mode != "preview" && req.Mode != "apply" {
		respondError(c, http.StatusBadRequest, "invalid_mode", "mode must be preview or apply")
		return
	}
	if req.Mode == "apply" {
		if strings.TrimSpace(req.PreviewToken) == "" {
			respondError(c, http.StatusBadRequest, "preview_required", "previewToken from a current preview is required")
			return
		}
		expectedIDs := pathUserIDs
		if len(req.Updates) > 0 && len(pathUserIDs) == 0 {
			expectedIDs = make([]string, 0, len(req.Updates))
			for _, update := range req.Updates {
				expectedIDs = append(expectedIDs, strings.TrimSpace(update.UserID))
			}
		}
		h.applyAdminPlanGroupPreview(c, actor.ID, reason, requestID, strings.TrimSpace(req.PreviewToken), expectedIDs)
		return
	}
	if len(pathUserIDs) > 0 {
		if len(req.Updates) != 1 {
			respondError(c, http.StatusBadRequest, "invalid_request", "single-account preview must contain one update")
			return
		}
		req.Updates[0].UserID = pathUserIDs[0]
	} else if len(req.Updates) == 0 || len(req.Updates) > maxAdminPlanGroupBatch {
		respondError(c, http.StatusBadRequest, "invalid_request", "updates must contain between 1 and 500 users")
		return
	}

	ctx := c.Request.Context()
	changes := make([]store.AdminPlanGroupChange, 0, len(req.Updates))
	seen := make(map[string]struct{}, len(req.Updates))
	for _, update := range req.Updates {
		userID := strings.TrimSpace(update.UserID)
		if userID == "" {
			respondError(c, http.StatusBadRequest, "userId_required", "userId is required")
			return
		}
		if _, duplicate := seen[userID]; duplicate {
			respondError(c, http.StatusBadRequest, "duplicate_user", "each user may appear only once")
			return
		}
		seen[userID] = struct{}{}
		user, err := h.store.GetUserByID(ctx, userID)
		if err != nil {
			if errors.Is(err, store.ErrUserNotFound) {
				respondError(c, http.StatusNotFound, "user_not_found", "user not found")
			} else {
				respondError(c, http.StatusInternalServerError, "user_lookup_failed", "failed to load user")
			}
			return
		}
		if h.isRootAccount(user) {
			respondError(c, http.StatusForbidden, "root_protected", "root account plan cannot be modified")
			return
		}

		planID := strings.ToUpper(strings.TrimSpace(update.PlanID))
		if planID == "" {
			if update.Groups == nil {
				respondError(c, http.StatusBadRequest, "plan_required", "planId is required")
				return
			}
			// The legacy segment editor may still update labels, but it cannot
			// change or remove the managed entitlement group.
			currentPlanGroup := store.MonthlyQuotaGroup(user)
			for _, requested := range *update.Groups {
				if isMonthlyQuotaGroup(requested) && requested != currentPlanGroup {
					respondError(c, http.StatusBadRequest, "plan_group_requires_plan_id", "managed quota groups must be changed through planId")
					return
				}
			}
			groups := normalizeGroups(*update.Groups)
			if currentPlanGroup != "" {
				groups = replaceMonthlyQuotaGroup(groups, currentPlanGroup)
			}
			profile, err := h.store.GetAccountBillingProfile(ctx, user.ID)
			if err != nil && !errors.Is(err, store.ErrUserNotFound) {
				respondError(c, http.StatusInternalServerError, "billing_profile_unavailable", "failed to load account plan")
				return
			}
			change := store.AdminPlanGroupChange{UserID: user.ID, PlanID: existingPlanID(profile), Groups: groups}
			if profile != nil {
				change.PackageName, change.IncludedQuotaBytes = profile.PackageName, profile.IncludedQuotaBytes
				change.RegionMultiplier, change.LineMultiplier = profile.RegionMultiplier, profile.LineMultiplier
				change.PeakMultiplier, change.OffPeakMultiplier = profile.PeakMultiplier, profile.OffPeakMultiplier
				change.PricingRuleVersion = profile.PricingRuleVersion
			}
			if err := setPlanValidity(&change, user, update); err != nil {
				respondError(c, http.StatusBadRequest, "invalid_validity", err.Error())
				return
			}
			changes = append(changes, change)
			continue
		}

		group, err := planIDToQuotaGroup(planID)
		if err != nil {
			respondError(c, http.StatusBadRequest, "invalid_plan", err.Error())
			return
		}
		plan, err := h.store.GetBillingPlan(ctx, planID)
		if err != nil || plan == nil || !plan.Active {
			if errors.Is(err, store.ErrBillingPlanNotFound) || (err == nil && plan != nil && !plan.Active) {
				respondError(c, http.StatusBadRequest, "plan_unavailable", "selected plan is not active in the billing catalog")
			} else {
				respondError(c, http.StatusInternalServerError, "plan_lookup_failed", "failed to load selected plan")
			}
			return
		}
		change := store.AdminPlanGroupChange{
			UserID: user.ID, PlanID: plan.PlanID, SetEntitlement: true, Groups: replaceMonthlyQuotaGroup(user.Groups, group),
			PackageName: plan.PackageName, IncludedQuotaBytes: plan.IncludedQuotaBytes,
			RegionMultiplier: plan.Multiplier("region"), LineMultiplier: plan.Multiplier("line"),
			PeakMultiplier: plan.Multiplier("peak"), OffPeakMultiplier: plan.Multiplier("offpeak"),
			PricingRuleVersion: fmt.Sprintf("plan:%s", plan.PlanID),
		}
		if err := setPlanValidity(&change, user, update); err != nil {
			respondError(c, http.StatusBadRequest, "invalid_validity", err.Error())
			return
		}
		changes = append(changes, change)
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].UserID < changes[j].UserID })
	previewToken, tokenHash, err := generateAdminPreviewToken()
	if err != nil {
		respondError(c, http.StatusInternalServerError, "preview_failed", "failed to create plan preview")
		return
	}
	expiresAt := time.Now().UTC().Add(adminPlanPreviewTTL)
	previewChanges, err := h.store.CreateAdminPlanGroupPreview(ctx, store.AdminPlanGroupPreview{
		ActorUUID: actor.ID, Reason: reason, RequestID: requestID, TokenHash: tokenHash,
		ExpiresAt: expiresAt, Changes: changes,
	})
	if err != nil {
		h.respondAdminPlanGroupStoreError(c, err)
		return
	}
	rows := make([]adminPlanGroupPreviewRow, 0, len(previewChanges))
	for _, change := range previewChanges {
		beforeFrom, beforeUntil := change.ExpectedValidFrom, change.ExpectedValidUntil
		desiredFrom, desiredUntil := beforeFrom, beforeUntil
		if change.SetValidFrom {
			desiredFrom = change.ValidFrom
		}
		if change.SetValidUntil {
			desiredUntil = change.ValidUntil
		}
		rows = append(rows, adminPlanGroupPreviewRow{
			UserID: change.UserID, PlanID: change.PlanID, Groups: change.Groups,
			Before: gin.H{"groups": change.ExpectedGroups, "validFrom": adminPlanDateString(beforeFrom), "validUntil": adminPlanDateString(beforeUntil),
				"includedQuotaBytes": change.ExpectedProfileIncludedQuota, "remainingIncludedQuota": change.ExpectedQuotaRemaining},
			After: gin.H{"groups": change.Groups, "validFrom": adminPlanDateString(desiredFrom), "validUntil": adminPlanDateString(desiredUntil),
				"includedQuotaBytes": change.IncludedQuotaBytes},
			UsedBytesPreserved: change.UsedBytesPreserved, RemainingIncludedQuota: change.RemainingAfter,
			ConfigurationSyncWillPause: change.IncludedQuotaBytes > 0 && change.RemainingAfter == 0,
		})
	}
	c.JSON(http.StatusOK, gin.H{"mode": "preview", "requestId": requestID, "previewToken": previewToken,
		"expiresAt": expiresAt, "changes": rows})
}

func (h *handler) applyAdminPlanGroupPreview(c *gin.Context, actorID, reason, requestID, token string, expectedIDs []string) {
	tokenHash := hashAdminPreviewToken(token)
	changes, replayed, err := h.store.ApplyAdminPlanGroupBatch(c.Request.Context(), store.AdminPlanGroupBatch{
		ActorUUID: actorID, Reason: reason, RequestID: requestID, PreviewTokenHash: tokenHash, ExpectedUserIDs: expectedIDs,
	})
	if err != nil {
		h.respondAdminPlanGroupStoreError(c, err)
		return
	}
	rows := make([]adminPlanGroupPreviewRow, 0, len(changes))
	for _, change := range changes {
		rows = append(rows, adminPlanGroupPreviewRow{UserID: change.UserID, PlanID: change.PlanID, Groups: change.Groups,
			UsedBytesPreserved: change.UsedBytesPreserved, RemainingIncludedQuota: change.RemainingAfter,
			ConfigurationSyncWillPause: change.IncludedQuotaBytes > 0 && change.RemainingAfter == 0})
	}
	c.JSON(http.StatusOK, gin.H{"mode": "applied", "requestId": requestID, "replayed": replayed, "changes": rows})
}

func (h *handler) respondAdminPlanGroupStoreError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrAdminPlanGroupStale):
		respondError(c, http.StatusConflict, "preview_stale", "account usage or plan state changed after preview; preview again")
	case errors.Is(err, store.ErrAdminPlanGroupReplay):
		respondError(c, http.StatusConflict, "idempotency_conflict", "requestId was already used for a different operation")
	case errors.Is(err, store.ErrAdminPlanGroupPreview):
		respondError(c, http.StatusConflict, "preview_expired_or_used", "preview is expired, invalid, or already consumed")
	case errors.Is(err, store.ErrUserNotFound):
		respondError(c, http.StatusNotFound, "user_not_found", "user not found")
	case errors.Is(err, store.ErrUserProtected):
		respondError(c, http.StatusForbidden, "root_protected", "root account plan cannot be modified")
	default:
		respondError(c, http.StatusInternalServerError, "plan_update_failed", "failed to prepare or apply plan changes")
	}
}

func setPlanValidity(change *store.AdminPlanGroupChange, user *store.User, update adminPlanGroupUpdate) error {
	validFrom, setValidFrom, err := parseAdminPlanDate(update.ValidFrom)
	if err != nil {
		return errors.New("validFrom must be null or use YYYY-MM-DD")
	}
	validUntil, setValidUntil, err := parseAdminPlanDate(update.ValidUntil)
	if err != nil {
		return errors.New("validUntil must be null or use YYYY-MM-DD")
	}
	desiredFrom, desiredUntil := user.SubscriptionValidFrom, user.SubscriptionValidUntil
	if setValidFrom {
		desiredFrom = validFrom
	}
	if setValidUntil {
		desiredUntil = validUntil
	}
	if desiredFrom != nil && desiredUntil != nil && desiredUntil.Before(*desiredFrom) {
		return errors.New("validUntil must be on or after validFrom")
	}
	change.ExpectedGroups = append([]string(nil), user.Groups...)
	change.ExpectedValidFrom, change.ExpectedValidUntil = cloneAdminTime(user.SubscriptionValidFrom), cloneAdminTime(user.SubscriptionValidUntil)
	change.SetValidFrom, change.ValidFrom, change.SetValidUntil, change.ValidUntil = setValidFrom, validFrom, setValidUntil, validUntil
	return nil
}

func generateAdminPreviewToken() (string, string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", "", err
	}
	token := hex.EncodeToString(random)
	return token, hashAdminPreviewToken(token), nil
}

func hashAdminPreviewToken(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}

func existingPlanID(profile *store.AccountBillingProfile) string {
	if profile == nil || !strings.HasPrefix(profile.PricingRuleVersion, "plan:") {
		return ""
	}
	return strings.TrimPrefix(profile.PricingRuleVersion, "plan:")
}

func isMonthlyQuotaGroup(group string) bool {
	switch strings.TrimSpace(group) {
	case store.MonthlyFreeQuotaLimitGroup, store.MonthlyPlusQuotaLimitGroup, store.MonthlyUnlimitedBetaQuotaGroup:
		return true
	default:
		return false
	}
}

func planIDToQuotaGroup(planID string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(planID)) {
	case store.BillingPlanFree:
		return store.MonthlyFreeQuotaLimitGroup, nil
	case store.BillingPlanPlus:
		return store.MonthlyPlusQuotaLimitGroup, nil
	case store.BillingPlanUnlimitedBeta:
		return store.MonthlyUnlimitedBetaQuotaGroup, nil
	default:
		return "", errors.New("planId must be FREE, PLUS or UNLIMITED-BETA")
	}
}

func replaceMonthlyQuotaGroup(existing []string, selected string) []string {
	groups := make([]string, 0, len(existing)+1)
	for _, group := range existing {
		if !isMonthlyQuotaGroup(group) {
			groups = append(groups, group)
		}
	}
	groups = append(groups, selected)
	return normalizeGroups(groups)
}

func parseAdminPlanDate(raw json.RawMessage) (*time.Time, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if string(raw) == "null" {
		return nil, true, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, true, err
	}
	parsed, err := time.Parse("2006-01-02", strings.TrimSpace(value))
	if err != nil {
		return nil, true, err
	}
	parsed = parsed.UTC()
	return &parsed, true, nil
}

func cloneAdminTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := value.UTC()
	return &cloned
}

func adminPlanDateString(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format("2006-01-02")
}
