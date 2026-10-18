package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

var referralCampaignBeijing = time.FixedZone("Asia/Shanghai", 8*60*60)
var referralCampaignStart = time.Date(2026, 10, 13, 0, 0, 0, 0, referralCampaignBeijing)
var referralCampaignEndExclusive = time.Date(2027, 1, 1, 0, 0, 0, 0, referralCampaignBeijing)

func referralCampaignActive(at time.Time) bool {
	return !at.Before(referralCampaignStart) && at.Before(referralCampaignEndExclusive)
}

func randomReferralCode() (string, error) {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return strings.ToUpper(hex.EncodeToString(raw)), nil
}

func (h *handler) getReferralOverview(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}
	code, err := h.ensureReferralCode(c, user.ID)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "referral_code_unavailable", "failed to load referral code")
		return
	}
	now := time.Now().UTC()
	summary, err := h.store.GetReferralSummary(c.Request.Context(), user.ID, now)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "referrals_unavailable", "failed to load referral summary")
		return
	}
	rewards, err := h.store.ListReferralRewards(c.Request.Context(), user.ID, 201)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "referrals_unavailable", "failed to load referral records")
		return
	}
	settlements, err := h.store.ListReferralSettlements(c.Request.Context(), user.ID, 101)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "referrals_unavailable", "failed to load settlement records")
		return
	}
	extensions, err := h.store.ListSubscriptionExtensions(c.Request.Context(), user.ID, 101)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "referrals_unavailable", "failed to load subscription extension records")
		return
	}
	referrals := make([]gin.H, 0, len(rewards))
	for _, reward := range rewards {
		status := reward.Status
		if status == "pending" && !reward.EligibleAt.After(now) {
			status = "eligible"
		}
		referrals = append(referrals, gin.H{"id": reward.ReferralID, "rewardId": reward.ID, "createdAt": reward.ReferralCreatedAt, "paidAt": reward.PaidAt, "invitee": maskReferralEmail(reward.InviteeEmail), "planType": map[bool]string{true: "year", false: "month"}[isXConnectAnnualPlanID(reward.PlanID)], "paidMinor": reward.PaidMinor, "currency": strings.TrimSpace(reward.Currency), "rewardMinor": reward.RewardMinor, "status": status, "eligibleAt": reward.EligibleAt})
	}
	rows := summary.CashByCurrency
	sort.Slice(rows, func(i, j int) bool { return rows[i].Currency < rows[j].Currency })
	extRows := make([]gin.H, 0, len(extensions))
	for _, extension := range extensions {
		extStatus := extension.Status
		if extStatus == "applied" {
			if effectiveEnd, err := h.store.ReferralExtensionEnd(c.Request.Context(), extension.SubscriptionUUID); err == nil && effectiveEnd.After(extension.EffectiveUntil) {
				extStatus = "applied"
			}
		}
		extRows = append(extRows, gin.H{"id": extension.ID, "rewardId": extension.RewardID, "referralId": extension.ReferralID, "days": extension.Days, "effectiveFrom": extension.EffectiveFrom, "effectiveUntil": extension.EffectiveUntil, "status": extStatus, "createdAt": extension.CreatedAt})
	}
	settlementRows := make([]gin.H, 0, len(settlements))
	for _, s := range settlements {
		settlementRows = append(settlementRows, gin.H{"id": s.ID, "amountMinor": s.AmountMinor, "currency": strings.TrimSpace(s.Currency), "status": s.Status, "requestedAt": s.RequestedAt, "settledAt": s.CompletedAt})
	}
	referralsMore := len(referrals) > 200
	settlementsMore := len(settlementRows) > 100
	extensionsMore := len(extRows) > 100
	if referralsMore {
		referrals = referrals[:200]
	}
	if settlementsMore {
		settlementRows = settlementRows[:100]
	}
	if extensionsMore {
		extRows = extRows[:100]
	}
	c.JSON(http.StatusOK, gin.H{"code": code.Code, "inviteUrl": "https://xworktech.com/referral?code=" + code.Code, "campaign": gin.H{"startsAt": referralCampaignStart.Format(time.RFC3339), "endsAt": referralCampaignEndExclusive.Add(-time.Second).Format(time.RFC3339), "cashbackBps": 1000, "annualBonusDays": 30}, "stats": summary, "referrals": referrals, "settlements": settlementRows, "extensions": extRows, "historyHasMore": gin.H{"referrals": referralsMore, "settlements": settlementsMore, "extensions": extensionsMore}})
}

func (h *handler) ensureReferralCode(c *gin.Context, userUUID string) (*store.ReferralCode, error) {
	for i := 0; i < 4; i++ {
		candidate, err := randomReferralCode()
		if err != nil {
			return nil, err
		}
		code, err := h.store.EnsureReferralCode(c.Request.Context(), userUUID, candidate)
		if err == nil {
			return code, nil
		}
		if !errors.Is(err, store.ErrReferralAlreadyAttributed) {
			return nil, err
		}
	}
	return nil, errors.New("could not allocate referral code")
}

func maskReferralEmail(email string) string {
	email = strings.TrimSpace(email)
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		return "invited user"
	}
	local := []rune(parts[0])
	if len(local) <= 1 {
		return "*@***"
	}
	return string(local[:1]) + strings.Repeat("*", min(6, len(local)-1)) + "@***"
}

func limitReferralRequestBody(c *gin.Context, maxBytes int64) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
}
func referralBodyTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

func validReferralCodeFormat(code string) bool {
	if len(code) < 8 || len(code) > 16 {
		return false
	}
	for _, r := range code {
		if !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

type referralSettlementRequest struct {
	Currency string `json:"currency"`
}

func (h *handler) requestReferralSettlement(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}
	var req referralSettlementRequest
	limitReferralRequestBody(c, 16*1024)
	if err := c.ShouldBindJSON(&req); err != nil {
		if referralBodyTooLarge(err) {
			respondError(c, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 16 KiB")
			return
		}
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}
	currency := strings.ToUpper(strings.TrimSpace(req.Currency))
	if !validReferralCurrency(currency) {
		respondError(c, http.StatusBadRequest, "invalid_currency", "currency must be an ISO 4217 code")
		return
	}
	settlement, err := h.store.RequestReferralSettlement(c.Request.Context(), user.ID, currency, time.Now().UTC())
	if err != nil {
		if errors.Is(err, store.ErrReferralSettlementUnavailable) {
			respondError(c, http.StatusUnprocessableEntity, "no_settlement_balance", "no eligible balance is available for settlement")
			return
		}
		respondError(c, http.StatusInternalServerError, "settlement_request_failed", "failed to request settlement")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"settlement": gin.H{"id": settlement.ID, "amountMinor": settlement.AmountMinor, "currency": settlement.Currency, "status": settlement.Status, "requestedAt": settlement.RequestedAt}})
}

type adminReferralSettlementCompletion struct {
	PayoutReference string `json:"payoutReference"`
	Reason          string `json:"reason"`
}

func (h *handler) adminCompleteReferralSettlement(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminBillingMoneyWrite)
	if !ok {
		return
	}
	var req adminReferralSettlementCompletion
	limitReferralRequestBody(c, 16*1024)
	if err := c.ShouldBindJSON(&req); err != nil {
		if referralBodyTooLarge(err) {
			respondError(c, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 16 KiB")
			return
		}
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}
	req.PayoutReference = strings.TrimSpace(req.PayoutReference)
	req.Reason = strings.TrimSpace(req.Reason)
	if req.PayoutReference == "" || req.Reason == "" || len(req.PayoutReference) > 200 || len(req.Reason) > 500 {
		respondError(c, http.StatusBadRequest, "payout_reference_required", "payoutReference and reason are required")
		return
	}
	settlement, err := h.store.CompleteReferralSettlement(c.Request.Context(), c.Param("settlementId"), actor.ID, req.PayoutReference, time.Now().UTC())
	if err != nil {
		if errors.Is(err, store.ErrReferralSettlementRisk) {
			respondError(c, http.StatusConflict, "payment_risk_detected", "settlement contains a refunded or disputed invoice")
			return
		}
		respondError(c, http.StatusUnprocessableEntity, "settlement_unavailable", "settlement cannot be completed")
		return
	}
	details := auditDetails(settlement.ReferrerUUID, req.Reason, map[string]any{"status": "pending_review"}, map[string]any{"status": "paid"})
	details["settlement_id"] = settlement.ID
	details["amount_minor"] = settlement.AmountMinor
	details["currency"] = settlement.Currency
	if err := h.recordAudit(c.Request.Context(), actor.ID, "referral.settlement.complete", details); err != nil {
		respondError(c, http.StatusInternalServerError, "audit_write_failed", "settlement was recorded but audit logging failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"settlement": settlement})
}

// Reconcile records a real off-platform payout after a risk event blocked
// completion. Risk holds keep every settlement item reserved until staff
// confirms payment or rejects the request as unpaid.
func (h *handler) adminReconcileReferralSettlement(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminBillingMoneyWrite)
	if !ok {
		return
	}
	var req adminReferralSettlementCompletion
	limitReferralRequestBody(c, 16*1024)
	if err := c.ShouldBindJSON(&req); err != nil {
		if referralBodyTooLarge(err) {
			respondError(c, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 16 KiB")
			return
		}
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}
	req.PayoutReference = strings.TrimSpace(req.PayoutReference)
	req.Reason = strings.TrimSpace(req.Reason)
	if req.PayoutReference == "" || req.Reason == "" || len(req.PayoutReference) > 200 || len(req.Reason) > 500 {
		respondError(c, http.StatusBadRequest, "payout_reference_required", "payoutReference and reason are required")
		return
	}
	settlement, err := h.store.ReconcileReferralSettlement(c.Request.Context(), c.Param("settlementId"), actor.ID, req.PayoutReference, req.Reason, time.Now().UTC())
	if err != nil {
		respondError(c, http.StatusUnprocessableEntity, "settlement_unavailable", "only a risk-rejected settlement with an already completed payment can be reconciled")
		return
	}
	details := auditDetails(settlement.ReferrerUUID, req.Reason, map[string]any{"status": "rejected"}, map[string]any{"status": "paid", "reconciliation": true})
	details["settlement_id"] = settlement.ID
	details["amount_minor"] = settlement.AmountMinor
	details["currency"] = settlement.Currency
	details["payout_reference"] = settlement.PayoutReference
	if err := h.recordAudit(c.Request.Context(), actor.ID, "referral.settlement.reconcile_paid", details); err != nil {
		respondError(c, http.StatusInternalServerError, "audit_write_failed", "payment fact was recorded but audit logging failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"settlement": settlement})
}

func (h *handler) adminRejectReferralSettlement(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminBillingMoneyWrite)
	if !ok {
		return
	}
	var req adminReferralReversal
	limitReferralRequestBody(c, 16*1024)
	if err := c.ShouldBindJSON(&req); err != nil {
		if referralBodyTooLarge(err) {
			respondError(c, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 16 KiB")
			return
		}
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" || len(req.Reason) > 500 {
		respondError(c, http.StatusBadRequest, "reason_required", "reason is required")
		return
	}
	settlement, err := h.store.RejectReferralSettlement(c.Request.Context(), c.Param("settlementId"), actor.ID, req.Reason, time.Now().UTC())
	if err != nil {
		respondError(c, http.StatusUnprocessableEntity, "settlement_unavailable", "only an unpaid pending settlement can be rejected")
		return
	}
	details := auditDetails(settlement.ReferrerUUID, req.Reason, map[string]any{"status": "pending_review"}, map[string]any{"status": "rejected", "unpaid": true})
	details["settlement_id"] = settlement.ID
	details["amount_minor"] = settlement.AmountMinor
	details["currency"] = settlement.Currency
	if err := h.recordAudit(c.Request.Context(), actor.ID, "referral.settlement.reject_unpaid", details); err != nil {
		respondError(c, http.StatusInternalServerError, "audit_write_failed", "settlement was rejected but audit logging failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"settlement": settlement})
}

type adminReferralReversal struct {
	Reason string `json:"reason"`
}

func (h *handler) adminReverseReferralReward(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminBillingMoneyWrite)
	if !ok {
		return
	}
	var req adminReferralReversal
	limitReferralRequestBody(c, 16*1024)
	if err := c.ShouldBindJSON(&req); err != nil {
		if referralBodyTooLarge(err) {
			respondError(c, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 16 KiB")
			return
		}
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid request payload")
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" || len(req.Reason) > 500 {
		respondError(c, http.StatusBadRequest, "reason_required", "reason is required")
		return
	}
	if err := h.store.ReverseReferralReward(c.Request.Context(), c.Param("rewardId"), actor.ID, "admin:"+actor.ID+":"+c.Param("rewardId"), req.Reason, time.Now().UTC()); err != nil {
		respondError(c, http.StatusUnprocessableEntity, "reward_reverse_failed", "reward could not be reversed")
		return
	}
	details := auditDetails("", req.Reason, nil, map[string]any{"reward_id": c.Param("rewardId"), "status": "reversed"})
	if err := h.recordAudit(c.Request.Context(), actor.ID, "referral.reward.reverse", details); err != nil {
		respondError(c, http.StatusInternalServerError, "audit_write_failed", "reward was reversed but audit logging failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"reversed": true})
}
