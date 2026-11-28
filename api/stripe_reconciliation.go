package api

import (
	"bytes"
	"encoding/csv"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"account/internal/store"
	"github.com/gin-gonic/gin"
)

type billingReconciliationItem struct {
	InvoiceID              string     `json:"invoiceId"`
	ProviderInvoiceID      string     `json:"providerInvoiceId,omitempty"`
	ProviderPaymentID      string     `json:"providerPaymentId,omitempty"`
	AccountID              string     `json:"accountId"`
	AccountEmail           string     `json:"accountEmail,omitempty"`
	SubscriptionID         string     `json:"subscriptionId,omitempty"`
	ExternalSubscriptionID string     `json:"externalSubscriptionId,omitempty"`
	PlanID                 string     `json:"planId,omitempty"`
	PlanFamily             string     `json:"planFamily,omitempty"`
	StripeProfile          string     `json:"stripeProfile"`
	StripeAccountID        string     `json:"stripeAccountId,omitempty"`
	MerchantEntity         string     `json:"merchantEntity,omitempty"`
	BusinessLine           string     `json:"businessLine,omitempty"`
	ReconciliationTag      string     `json:"reconciliationTag,omitempty"`
	RegionScope            string     `json:"regionScope,omitempty"`
	Currency               string     `json:"currency"`
	AmountMinor            int64      `json:"amountMinor"`
	RefundedMinor          int64      `json:"refundedMinor"`
	NetMinor               int64      `json:"netMinor"`
	Status                 string     `json:"status"`
	IssuedAt               time.Time  `json:"issuedAt"`
	PaidAt                 *time.Time `json:"paidAt,omitempty"`
}

type billingReconciliationBucket struct {
	Count         int              `json:"count"`
	GrossByCcy    map[string]int64 `json:"grossMinorByCurrency"`
	RefundedByCcy map[string]int64 `json:"refundedMinorByCurrency"`
	NetByCcy      map[string]int64 `json:"netMinorByCurrency"`
}

func newReconciliationBucket() *billingReconciliationBucket {
	return &billingReconciliationBucket{
		GrossByCcy:    map[string]int64{},
		RefundedByCcy: map[string]int64{},
		NetByCcy:      map[string]int64{},
	}
}

func (b *billingReconciliationBucket) add(ccy string, gross, refunded, net int64) {
	b.Count++
	b.GrossByCcy[ccy] += gross
	b.RefundedByCcy[ccy] += refunded
	b.NetByCcy[ccy] += net
}

func (h *handler) adminBillingReconciliation(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminBillingLedger); !ok {
		return
	}

	ctx := c.Request.Context()
	profileFilter := normalizeStripeProfile(c.Query("stripeProfile"))
	if raw := strings.TrimSpace(c.Query("stripeProfile")); raw != "" && !validStripeProfile(raw) {
		respondError(c, http.StatusBadRequest, "invalid_stripe_profile", "stripeProfile must be lowercase alphanumeric, hyphen or underscore")
		return
	}
	tagFilter := strings.TrimSpace(c.Query("reconciliationTag"))
	businessLineFilter := strings.TrimSpace(c.Query("businessLine"))
	format := strings.ToLower(strings.TrimSpace(c.Query("format")))
	if format != "" && format != "json" && format != "csv" {
		respondError(c, http.StatusBadRequest, "invalid_format", "format must be json or csv")
		return
	}

	limit := 200
	if rawLimit := strings.TrimSpace(c.Query("limit")); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed <= 0 {
			respondError(c, http.StatusBadRequest, "invalid_limit", "limit must be a positive integer")
			return
		}
		if parsed > 1000 {
			parsed = 1000
		}
		limit = parsed
	}

	invoices, err := h.store.ListFinanceInvoices(ctx, "", "", limit)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "reconciliation_unavailable", "failed to query finance invoices")
		return
	}
	payments, err := h.store.ListFinancePayments(ctx, "", 0)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "reconciliation_unavailable", "failed to query finance payments")
		return
	}
	refunds, err := h.store.ListFinanceRefunds(ctx, "", 0)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "reconciliation_unavailable", "failed to query finance refunds")
		return
	}
	plans, err := h.store.ListBillingPlans(ctx, true)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "reconciliation_unavailable", "failed to load billing plans")
		return
	}

	paymentByInvoiceID := make(map[string]store.FinancePayment, len(payments))
	for _, p := range payments {
		paymentByInvoiceID[p.InvoiceID] = p
	}
	refundedByPaymentID := make(map[string]int64, len(refunds))
	for _, r := range refunds {
		refundedByPaymentID[r.PaymentID] += r.AmountMinor
	}
	planByID := make(map[string]*store.BillingPlan, len(plans))
	for i := range plans {
		planByID[plans[i].PlanID] = &plans[i]
	}

	usersCache := map[string]*store.User{}
	subsCache := map[string][]store.Subscription{}
	loadUser := func(accountUUID string) *store.User {
		if u, ok := usersCache[accountUUID]; ok {
			return u
		}
		u, uErr := h.store.GetUserByID(ctx, accountUUID)
		if uErr != nil && !errors.Is(uErr, store.ErrUserNotFound) {
			u = nil
		}
		usersCache[accountUUID] = u
		return u
	}
	loadSubs := func(accountUUID string) []store.Subscription {
		if s, ok := subsCache[accountUUID]; ok {
			return s
		}
		s, sErr := h.store.ListSubscriptionsByUser(ctx, accountUUID)
		if sErr != nil {
			s = nil
		}
		subsCache[accountUUID] = s
		return s
	}

	items := make([]billingReconciliationItem, 0, len(invoices))
	byProfile := map[string]*billingReconciliationBucket{}
	byTag := map[string]*billingReconciliationBucket{}

	for _, inv := range invoices {
		var matchedSub *store.Subscription
		for _, s := range loadSubs(inv.AccountUUID) {
			if s.ID == inv.SubscriptionUUID || (inv.SubscriptionUUID == "" && strings.EqualFold(s.Provider, inv.Provider)) {
				subCopy := s
				matchedSub = &subCopy
				break
			}
		}

		planID := ""
		extSubID := ""
		var subMeta map[string]any
		if matchedSub != nil {
			planID = matchedSub.PlanID
			extSubID = matchedSub.ExternalID
			subMeta = matchedSub.Meta
		}
		planObj := planByID[planID]
		profile := normalizeStripeProfile(subscriptionMetaString(subMeta, "stripe_profile"))
		if profile == "" && planObj != nil {
			profile = h.planStripeProfile(planObj)
		}
		if profile == "" {
			profile = h.defaultStripeProfile()
		}
		client, _ := h.stripeClientForProfile(profile)
		recMeta := h.planReconciliationMeta(planObj, client)

		accountID := firstNonEmpty(subscriptionMetaString(subMeta, "stripe_account_id"), recMeta.StripeAccountID)
		businessLine := firstNonEmpty(subscriptionMetaString(subMeta, "business_line"), recMeta.BusinessLine)
		reconciliationTag := firstNonEmpty(subscriptionMetaString(subMeta, "reconciliation_tag"), recMeta.ReconciliationTag)
		regionScope := firstNonEmpty(subscriptionMetaString(subMeta, "region_scope"), recMeta.RegionScope)
		merchantEntity := firstNonEmpty(subscriptionMetaString(subMeta, "merchant_entity"), recMeta.MerchantEntity)
		planFamily := firstNonEmpty(subscriptionMetaString(subMeta, "plan_family"), recMeta.PlanFamily)

		if profileFilter != "" && profile != profileFilter {
			continue
		}
		if tagFilter != "" && !strings.EqualFold(reconciliationTag, tagFilter) {
			continue
		}
		if businessLineFilter != "" && !strings.EqualFold(businessLine, businessLineFilter) {
			continue
		}

		var paymentID string
		var paidAt *time.Time
		var refundedMinor int64
		status := "issued"
		if payment, ok := paymentByInvoiceID[inv.ID]; ok {
			paymentID = payment.ProviderPaymentID
			t := payment.PaidAt
			paidAt = &t
			refundedMinor = refundedByPaymentID[payment.ID]
			status = "paid"
			if refundedMinor >= inv.AmountMinor && inv.AmountMinor > 0 {
				status = "refunded"
			} else if refundedMinor > 0 {
				status = "partial_refund"
			}
		} else if inv.AmountMinor == 0 {
			status = "zero_amount"
		}

		netMinor := inv.AmountMinor - refundedMinor
		if netMinor < 0 {
			netMinor = 0
		}

		var email string
		if u := loadUser(inv.AccountUUID); u != nil {
			email = u.Email
		}

		ccy := strings.ToUpper(strings.TrimSpace(inv.Currency))
		if ccy == "" {
			ccy = "USD"
		}

		item := billingReconciliationItem{
			InvoiceID:              inv.ID,
			ProviderInvoiceID:      inv.ProviderInvoiceID,
			ProviderPaymentID:      paymentID,
			AccountID:              inv.AccountUUID,
			AccountEmail:           email,
			SubscriptionID:         inv.SubscriptionUUID,
			ExternalSubscriptionID: extSubID,
			PlanID:                 planID,
			PlanFamily:             planFamily,
			StripeProfile:          profile,
			StripeAccountID:        accountID,
			MerchantEntity:         merchantEntity,
			BusinessLine:           businessLine,
			ReconciliationTag:      reconciliationTag,
			RegionScope:            regionScope,
			Currency:               ccy,
			AmountMinor:            inv.AmountMinor,
			RefundedMinor:          refundedMinor,
			NetMinor:               netMinor,
			Status:                 status,
			IssuedAt:               inv.IssuedAt,
			PaidAt:                 paidAt,
		}
		items = append(items, item)

		pb, ok := byProfile[profile]
		if !ok {
			pb = newReconciliationBucket()
			byProfile[profile] = pb
		}
		pb.add(ccy, inv.AmountMinor, refundedMinor, netMinor)

		tagKey := firstNonEmpty(reconciliationTag, "UNCLASSIFIED")
		tb, ok := byTag[tagKey]
		if !ok {
			tb = newReconciliationBucket()
			byTag[tagKey] = tb
		}
		tb.add(ccy, inv.AmountMinor, refundedMinor, netMinor)
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].IssuedAt.After(items[j].IssuedAt)
	})

	if format == "csv" {
		var buf bytes.Buffer
		buf.Write([]byte{0xEF, 0xBB, 0xBF})
		writer := csv.NewWriter(&buf)
		_ = writer.Write([]string{
			"invoice_id",
			"provider_invoice_id",
			"provider_payment_id",
			"account_id",
			"account_email",
			"subscription_id",
			"external_subscription_id",
			"plan_id",
			"plan_family",
			"stripe_profile",
			"stripe_account_id",
			"merchant_entity",
			"business_line",
			"reconciliation_tag",
			"region_scope",
			"currency",
			"amount_minor",
			"refunded_minor",
			"net_minor",
			"status",
			"issued_at",
			"paid_at",
		})
		for _, item := range items {
			paidStr := ""
			if item.PaidAt != nil {
				paidStr = item.PaidAt.UTC().Format(time.RFC3339)
			}
			_ = writer.Write([]string{
				item.InvoiceID,
				item.ProviderInvoiceID,
				item.ProviderPaymentID,
				item.AccountID,
				item.AccountEmail,
				item.SubscriptionID,
				item.ExternalSubscriptionID,
				item.PlanID,
				item.PlanFamily,
				item.StripeProfile,
				item.StripeAccountID,
				item.MerchantEntity,
				item.BusinessLine,
				item.ReconciliationTag,
				item.RegionScope,
				item.Currency,
				strconv.FormatInt(item.AmountMinor, 10),
				strconv.FormatInt(item.RefundedMinor, 10),
				strconv.FormatInt(item.NetMinor, 10),
				item.Status,
				item.IssuedAt.UTC().Format(time.RFC3339),
				paidStr,
			})
		}
		writer.Flush()
		filename := "stripe-reconciliation-all.csv"
		if profileFilter != "" {
			filename = "stripe-reconciliation-" + profileFilter + ".csv"
		}
		c.Header("Content-Type", "text/csv; charset=utf-8")
		c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
		c.Data(http.StatusOK, "text/csv; charset=utf-8", buf.Bytes())
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"items": items,
		"summary": gin.H{
			"totalCount": len(items),
			"byProfile":  byProfile,
			"byTag":      byTag,
		},
		"filters": gin.H{
			"stripeProfile":     profileFilter,
			"reconciliationTag": tagFilter,
			"businessLine":      businessLineFilter,
			"limit":             limit,
		},
	})
}
