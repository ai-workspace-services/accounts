package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"account/internal/store"
	"github.com/gin-gonic/gin"
)

func (h *handler) referralCampaignAt(ctx context.Context, at time.Time) (*store.ReferralCampaign, error) {
	st, ok := h.store.(store.ReferralCampaignStore)
	if !ok {
		return nil, errors.New("referral campaign configuration is unavailable")
	}
	return st.GetReferralCampaign(ctx, at)
}

func (h *handler) publicReferralCampaign(c *gin.Context) {
	now := time.Now().UTC()
	campaign, err := h.referralCampaignAt(c.Request.Context(), now)
	if err != nil || campaign == nil {
		respondError(c, http.StatusServiceUnavailable, "campaign_unavailable", "referral campaign configuration is unavailable")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"campaign": gin.H{
		"revision": campaign.Revision, "enabled": campaign.Enabled, "active": campaign.Active(now),
		"startsAt": campaign.StartsAt, "endsAt": campaign.EndsAt, "cashbackBps": 1000, "annualBonusDays": 30,
	}})
}

func (h *handler) adminGetReferralCampaign(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminBillingMoneyWrite); !ok {
		return
	}
	campaign, err := h.referralCampaignAt(c.Request.Context(), time.Now().UTC())
	if err != nil {
		respondError(c, http.StatusServiceUnavailable, "campaign_unavailable", "referral campaign configuration is unavailable")
		return
	}
	c.JSON(http.StatusOK, gin.H{"campaign": campaign})
}

func (h *handler) adminConfigureReferralCampaign(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminBillingMoneyWrite)
	if !ok {
		return
	}
	// A complete desired configuration avoids ambiguous PATCH semantics:
	// enabled is mandatory, and null dates explicitly remove a time boundary.
	var req struct {
		Enabled  *bool      `json:"enabled"`
		StartsAt *time.Time `json:"startsAt"`
		EndsAt   *time.Time `json:"endsAt"`
		Reason   string     `json:"reason"`
	}
	limitReferralRequestBody(c, 16*1024)
	if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil {
		status := http.StatusBadRequest
		if referralBodyTooLarge(err) {
			status = http.StatusRequestEntityTooLarge
		}
		respondError(c, status, "invalid_campaign", "enabled and reason are required; dates must use RFC3339 or null")
		return
	}
	h.persistReferralCampaign(c, actor, &store.ReferralCampaign{Enabled: *req.Enabled, StartsAt: req.StartsAt, EndsAt: req.EndsAt, Reason: req.Reason})
}

func (h *handler) adminOpenReferralCampaign(c *gin.Context)  { h.adminToggleReferralCampaign(c, true) }
func (h *handler) adminCloseReferralCampaign(c *gin.Context) { h.adminToggleReferralCampaign(c, false) }

func (h *handler) adminToggleReferralCampaign(c *gin.Context, enabled bool) {
	actor, ok := h.requireAdminPermission(c, permissionAdminBillingMoneyWrite)
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	limitReferralRequestBody(c, 16*1024)
	if err := c.ShouldBindJSON(&req); err != nil {
		status := http.StatusBadRequest
		if referralBodyTooLarge(err) {
			status = http.StatusRequestEntityTooLarge
		}
		respondError(c, status, "invalid_campaign", "an audit reason is required")
		return
	}
	current, err := h.referralCampaignAt(c.Request.Context(), time.Now().UTC())
	if err != nil || current == nil {
		respondError(c, http.StatusServiceUnavailable, "campaign_unavailable", "referral campaign configuration is unavailable")
		return
	}
	// Operational open/close preserves the previously defined schedule.
	h.persistReferralCampaign(c, actor, &store.ReferralCampaign{Enabled: enabled, StartsAt: current.StartsAt, EndsAt: current.EndsAt, Reason: req.Reason})
}

func (h *handler) persistReferralCampaign(c *gin.Context, actor *store.User, input *store.ReferralCampaign) {
	input.ActorUUID = actor.ID
	st, ok := h.store.(store.ReferralCampaignStore)
	if !ok {
		respondError(c, http.StatusServiceUnavailable, "campaign_unavailable", "referral campaign configuration is unavailable")
		return
	}
	campaign, err := st.ConfigureReferralCampaign(c.Request.Context(), input)
	if err != nil {
		if errors.Is(err, store.ErrReferralCampaignInvalid) {
			respondError(c, http.StatusBadRequest, "invalid_campaign", "invalid campaign dates or audit reason")
			return
		}
		respondError(c, http.StatusServiceUnavailable, "campaign_unavailable", "referral campaign configuration could not be saved")
		return
	}
	// The immutable revision itself retains actor and reason atomically. The
	// shared audit log is supplemental, and a failure must be reported honestly.
	if err := h.recordAudit(c.Request.Context(), actor.ID, "referral.campaign.configure", map[string]any{"revision": campaign.Revision, "enabled": campaign.Enabled, "reason": campaign.Reason}); err != nil {
		respondError(c, http.StatusInternalServerError, "audit_write_failed", "campaign revision was saved but supplemental audit logging failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"campaign": campaign})
}
