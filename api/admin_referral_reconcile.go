package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Administrators preview explicit invoices first, then explicitly apply the
// same bounded selection. Underlying finance/reward facts remain idempotent.
func (h *handler) adminReconcileReferralInvoices(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminBillingMoneyWrite)
	if !ok {
		return
	}
	var req struct {
		AccountUUID string   `json:"accountUuid"`
		InvoiceIDs  []string `json:"invoiceIds"`
		Apply       bool     `json:"apply"`
		Reason      string   `json:"reason"`
	}
	limitReferralRequestBody(c, 16*1024)
	if err := c.ShouldBindJSON(&req); err != nil {
		status := http.StatusBadRequest
		if referralBodyTooLarge(err) {
			status = http.StatusRequestEntityTooLarge
		}
		respondError(c, status, "invalid_request", "invalid reconciliation request")
		return
	}
	if strings.TrimSpace(req.AccountUUID) == "" || len(req.InvoiceIDs) == 0 || len(req.InvoiceIDs) > maxReferralReconcileInvoices || (req.Apply && (strings.TrimSpace(req.Reason) == "" || len(req.Reason) > 500)) {
		respondError(c, http.StatusBadRequest, "invalid_request", "account and 1 to 50 unique invoice IDs are required; applying requires an audit reason")
		return
	}
	seen := make(map[string]bool, len(req.InvoiceIDs))
	for _, id := range req.InvoiceIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			respondError(c, http.StatusBadRequest, "invalid_request", "invoice IDs must be nonempty and unique")
			return
		}
		seen[id] = true
	}
	items, reconcileErr := h.reconcileReferralPaidInvoices(c.Request.Context(), ReferralReconcileRequest{AccountUUID: req.AccountUUID, InvoiceIDs: req.InvoiceIDs, Apply: req.Apply})
	if req.Apply {
		// Also retain partial progress if a later provider request fails.
		if err := h.recordAudit(c.Request.Context(), actor.ID, "referral.invoices.reconcile", map[string]any{"account_uuid": req.AccountUUID, "invoice_ids": req.InvoiceIDs, "reason": strings.TrimSpace(req.Reason), "completed": reconcileErr == nil, "items": items}); err != nil {
			respondError(c, http.StatusInternalServerError, "audit_write_failed", "reconciliation may have applied; audit logging failed, review facts before retry")
			return
		}
	}
	if reconcileErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "referral_reconciliation_failed", "items": items, "apply": req.Apply})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "apply": req.Apply})
}
