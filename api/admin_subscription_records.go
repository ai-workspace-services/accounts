package api

import (
	"net/http"
	"strings"
	"time"

	"account/internal/store"
	"github.com/gin-gonic/gin"
)

type adminSubscriptionRecord struct {
	UserID            string     `json:"userId"`
	SubscriptionType  string     `json:"subscriptionType"`
	MonthlyQuotaBytes int64      `json:"monthlyQuotaBytes"`
	AnnualQuotaBytes  int64      `json:"annualQuotaBytes"`
	FeeMinor          int64      `json:"feeMinor"`
	PaymentStatus     string     `json:"paymentStatus"`
	Note              string     `json:"note"`
	ValidFrom         *time.Time `json:"validFrom"`
	ValidUntil        *time.Time `json:"validUntil"`
}

func (h *handler) adminListSubscriptionRecords(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminSettingsWrite); !ok {
		return
	}
	records, err := h.store.ListSubscriptionRecords(c.Request.Context())
	if err != nil {
		respondError(c, http.StatusInternalServerError, "subscription_records_unavailable", "failed to load subscription records")
		return
	}
	out := make([]adminSubscriptionRecord, 0, len(records))
	for _, record := range records {
		out = append(out, adminSubscriptionRecord{UserID: record.UserID, SubscriptionType: record.SubscriptionType, MonthlyQuotaBytes: record.MonthlyQuotaBytes, AnnualQuotaBytes: record.AnnualQuotaBytes, FeeMinor: record.FeeMinor, PaymentStatus: record.PaymentStatus, Note: record.Note, ValidFrom: record.ValidFrom, ValidUntil: record.ValidUntil})
	}
	c.JSON(http.StatusOK, gin.H{"records": out})
}

func (h *handler) adminUpsertSubscriptionRecords(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminSettingsWrite); !ok {
		return
	}
	var req struct {
		Records []adminSubscriptionRecord `json:"records"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Records) == 0 || len(req.Records) > maxAdminPlanGroupBatch {
		respondError(c, http.StatusBadRequest, "invalid_request", "records must contain between 1 and 500 users")
		return
	}
	seen := make(map[string]struct{}, len(req.Records))
	records := make([]store.SubscriptionRecord, 0, len(req.Records))
	for _, input := range req.Records {
		id := strings.TrimSpace(input.UserID)
		if id == "" {
			respondError(c, http.StatusBadRequest, "userId_required", "userId is required")
			return
		}
		if _, ok := seen[id]; ok {
			respondError(c, http.StatusBadRequest, "duplicate_user", "each user may appear only once")
			return
		}
		seen[id] = struct{}{}
		if input.MonthlyQuotaBytes < 0 || input.AnnualQuotaBytes < 0 || input.FeeMinor < 0 {
			respondError(c, http.StatusBadRequest, "invalid_amount", "quota and fee values must be non-negative")
			return
		}
		if input.ValidFrom != nil && input.ValidUntil != nil && input.ValidUntil.Before(*input.ValidFrom) {
			respondError(c, http.StatusBadRequest, "invalid_validity", "validUntil must be on or after validFrom")
			return
		}
		records = append(records, store.SubscriptionRecord{UserID: id, SubscriptionType: strings.TrimSpace(input.SubscriptionType), MonthlyQuotaBytes: input.MonthlyQuotaBytes, AnnualQuotaBytes: input.AnnualQuotaBytes, FeeMinor: input.FeeMinor, PaymentStatus: strings.TrimSpace(input.PaymentStatus), Note: strings.TrimSpace(input.Note), ValidFrom: input.ValidFrom, ValidUntil: input.ValidUntil})
	}
	if err := h.store.UpsertSubscriptionRecords(c.Request.Context(), records); err != nil {
		respondError(c, http.StatusInternalServerError, "subscription_records_update_failed", "failed to persist subscription records")
		return
	}
	c.JSON(http.StatusOK, gin.H{"updated": len(records)})
}
