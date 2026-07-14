package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

const accountingDataSource = "postgresql"

type nodeHeartbeatRequest struct {
	NodeID            string  `json:"nodeId"`
	Region            string  `json:"region"`
	LineCode          string  `json:"lineCode"`
	PricingGroup      string  `json:"pricingGroup"`
	StatsEnabled      bool    `json:"statsEnabled"`
	XrayRevision      string  `json:"xrayRevision"`
	Healthy           bool    `json:"healthy"`
	LatencyMS         int     `json:"latencyMs"`
	ErrorRate         float64 `json:"errorRate"`
	ActiveConnections int     `json:"activeConnections"`
	HealthScore       float64 `json:"healthScore"`
	SampledAt         string  `json:"sampledAt"`
}

func parseOptionalTime(value string) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err == nil {
		return parsed.UTC(), nil
	}
	return time.Parse("2006-01-02T15:04", trimmed)
}

func (h *handler) accountUsageSummary(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}

	buckets, err := h.store.ListTrafficMinuteBucketsByAccount(c.Request.Context(), user.ID, time.Time{}, time.Time{})
	if err != nil {
		respondError(c, http.StatusInternalServerError, "usage_summary_unavailable", "failed to load usage summary")
		return
	}

	var totalBytes, uplinkBytes, downlinkBytes int64
	var lastBucketAt *time.Time
	for _, bucket := range buckets {
		totalBytes += bucket.TotalBytes
		uplinkBytes += bucket.UplinkBytes
		downlinkBytes += bucket.DownlinkBytes
		timestamp := bucket.BucketStart.UTC()
		if lastBucketAt == nil || timestamp.After(*lastBucketAt) {
			lastBucketAt = &timestamp
		}
	}

	currentBalance := 0.0
	remainingQuota := int64(0)
	suspendState := "active"
	throttleState := "normal"
	arrears := false
	var arrearsSince *time.Time
	var billingProfile *store.AccountBillingProfile
	if quota, err := h.store.GetAccountQuotaState(c.Request.Context(), user.ID); err == nil && quota != nil {
		currentBalance = quota.CurrentBalance
		remainingQuota = quota.RemainingIncludedQuota
		suspendState = quota.SuspendState
		throttleState = quota.ThrottleState
		arrears = quota.Arrears
		arrearsSince = quota.ArrearsSince
	}
	if profile, err := h.store.GetAccountBillingProfile(c.Request.Context(), user.ID); err == nil && profile != nil {
		billingProfile = profile
	}

	syncDelaySeconds := 0
	if lastBucketAt != nil {
		syncDelaySeconds = int(time.Since(*lastBucketAt).Seconds())
		if syncDelaySeconds < 0 {
			syncDelaySeconds = 0
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"accountUuid":            user.ID,
		"totalBytes":             totalBytes,
		"uplinkBytes":            uplinkBytes,
		"downlinkBytes":          downlinkBytes,
		"sourceOfTruth":          accountingDataSource,
		"currentBalance":         currentBalance,
		"remainingIncludedQuota": remainingQuota,
		"suspendState":           suspendState,
		"throttleState":          throttleState,
		"arrears":                arrears,
		"arrearsSince":           arrearsSince,
		"lastBucketAt":           lastBucketAt,
		"syncDelaySeconds":       syncDelaySeconds,
		"billingProfile":         billingProfile,
	})
}

func (h *handler) accountUsageBuckets(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}

	start, err := parseOptionalTime(c.Query("start"))
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_start", "start must be RFC3339")
		return
	}
	end, err := parseOptionalTime(c.Query("end"))
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_end", "end must be RFC3339")
		return
	}

	buckets, err := h.store.ListTrafficMinuteBucketsByAccount(c.Request.Context(), user.ID, start, end)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "usage_buckets_unavailable", "failed to load usage buckets")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"accountUuid":   user.ID,
		"buckets":       buckets,
		"sourceOfTruth": accountingDataSource,
	})
}

func (h *handler) accountBillingSummary(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}

	ledger, err := h.store.ListBillingLedgerByAccount(c.Request.Context(), user.ID, 20)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "billing_summary_unavailable", "failed to load billing summary")
		return
	}

	var quota *store.AccountQuotaState
	if snapshot, err := h.store.GetAccountQuotaState(c.Request.Context(), user.ID); err == nil {
		quota = snapshot
	}
	var billingProfile *store.AccountBillingProfile
	if profile, err := h.store.GetAccountBillingProfile(c.Request.Context(), user.ID); err == nil {
		billingProfile = profile
	}

	c.JSON(http.StatusOK, gin.H{
		"accountUuid":    user.ID,
		"quotaState":     quota,
		"billingProfile": billingProfile,
		"ledger":         ledger,
		"sourceOfTruth":  accountingDataSource,
	})
}

func (h *handler) accountPolicy(c *gin.Context) {
	user, ok := h.requireAuthenticatedUser(c)
	if !ok {
		return
	}

	policy, err := h.store.GetLatestAccountPolicySnapshot(c.Request.Context(), user.ID)
	if err != nil {
		respondError(c, http.StatusNotFound, "policy_not_found", "account policy snapshot is not available")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"accountUuid":        policy.AccountUUID,
		"policyVersion":      policy.PolicyVersion,
		"authState":          policy.AuthState,
		"rateProfile":        policy.RateProfile,
		"connProfile":        policy.ConnProfile,
		"eligibleNodeGroups": policy.EligibleNodeGroups,
		"preferredStrategy":  policy.PreferredStrategy,
		"degradeMode":        policy.DegradeMode,
		"expiresAt":          policy.ExpiresAt,
	})
}

func (h *handler) adminTrafficNodes(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminAgentsStatus); !ok {
		return
	}

	nodes, err := h.store.ListLatestNodeHealthSnapshots(c.Request.Context())
	if err != nil {
		respondError(c, http.StatusInternalServerError, "node_health_unavailable", "failed to load node health snapshots")
		return
	}

	c.JSON(http.StatusOK, gin.H{"nodes": nodes})
}

func (h *handler) adminTrafficAccount(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminUsersListRead); !ok {
		return
	}

	accountUUID := strings.TrimSpace(c.Param("uuid"))
	if accountUUID == "" {
		respondError(c, http.StatusBadRequest, "account_uuid_required", "account uuid is required")
		return
	}

	buckets, err := h.store.ListTrafficMinuteBucketsByAccount(c.Request.Context(), accountUUID, time.Time{}, time.Time{})
	if err != nil {
		respondError(c, http.StatusInternalServerError, "account_traffic_unavailable", "failed to load account traffic")
		return
	}
	ledger, err := h.store.ListBillingLedgerByAccount(c.Request.Context(), accountUUID, 20)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "account_billing_unavailable", "failed to load account billing")
		return
	}
	policy, _ := h.store.GetLatestAccountPolicySnapshot(c.Request.Context(), accountUUID)
	quota, _ := h.store.GetAccountQuotaState(c.Request.Context(), accountUUID)
	billingProfile, _ := h.store.GetAccountBillingProfile(c.Request.Context(), accountUUID)

	c.JSON(http.StatusOK, gin.H{
		"accountUuid":    accountUUID,
		"buckets":        buckets,
		"ledger":         ledger,
		"policy":         policy,
		"quotaState":     quota,
		"billingProfile": billingProfile,
	})
}

func (h *handler) adminCollectorStatus(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminAgentsStatus); !ok {
		return
	}

	checkpoints, err := h.store.ListTrafficStatCheckpoints(c.Request.Context())
	if err != nil {
		respondError(c, http.StatusInternalServerError, "collector_status_unavailable", "failed to load collector checkpoints")
		return
	}
	buckets, err := h.store.ListTrafficMinuteBuckets(c.Request.Context())
	if err != nil {
		respondError(c, http.StatusInternalServerError, "collector_status_unavailable", "failed to load collector buckets")
		return
	}

	var latestCheckpointAt *time.Time
	for _, checkpoint := range checkpoints {
		timestamp := checkpoint.LastSeenAt.UTC()
		if latestCheckpointAt == nil || timestamp.After(*latestCheckpointAt) {
			latestCheckpointAt = &timestamp
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"checkpointCount":    len(checkpoints),
		"minuteBucketCount":  len(buckets),
		"latestCheckpointAt": latestCheckpointAt,
	})
}

func (h *handler) adminSchedulerStatus(c *gin.Context) {
	if _, ok := h.requireAdminPermission(c, permissionAdminAgentsStatus); !ok {
		return
	}

	limit := 20
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 200 {
			limit = parsed
		}
	}

	decisions, err := h.store.ListRecentSchedulerDecisions(c.Request.Context(), limit)
	if err != nil {
		respondError(c, http.StatusInternalServerError, "scheduler_status_unavailable", "failed to load scheduler decisions")
		return
	}

	c.JSON(http.StatusOK, gin.H{"decisions": decisions})
}

func (h *handler) internalAccountPolicy(c *gin.Context) {
	accountUUID := strings.TrimSpace(c.Param("accountUUID"))
	if accountUUID == "" {
		respondError(c, http.StatusBadRequest, "account_uuid_required", "account uuid is required")
		return
	}

	policy, err := h.store.GetLatestAccountPolicySnapshot(c.Request.Context(), accountUUID)
	if err != nil {
		respondError(c, http.StatusNotFound, "policy_not_found", "account policy snapshot is not available")
		return
	}

	c.JSON(http.StatusOK, policy)
}

func (h *handler) internalNodeHeartbeat(c *gin.Context) {
	var req nodeHeartbeatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_request", "invalid heartbeat payload")
		return
	}

	nodeID := strings.TrimSpace(req.NodeID)
	if nodeID == "" {
		respondError(c, http.StatusBadRequest, "node_id_required", "node id is required")
		return
	}

	sampledAt, err := parseOptionalTime(req.SampledAt)
	if err != nil {
		respondError(c, http.StatusBadRequest, "invalid_sampled_at", "sampledAt must be RFC3339")
		return
	}
	if sampledAt.IsZero() {
		sampledAt = time.Now().UTC()
	}

	if err := h.store.UpsertNodeHealthSnapshot(c.Request.Context(), &store.NodeHealthSnapshot{
		NodeID:            nodeID,
		Region:            strings.TrimSpace(req.Region),
		LineCode:          strings.TrimSpace(req.LineCode),
		PricingGroup:      strings.TrimSpace(req.PricingGroup),
		StatsEnabled:      req.StatsEnabled,
		XrayRevision:      strings.TrimSpace(req.XrayRevision),
		Healthy:           req.Healthy,
		LatencyMS:         req.LatencyMS,
		ErrorRate:         req.ErrorRate,
		ActiveConnections: req.ActiveConnections,
		HealthScore:       req.HealthScore,
		SampledAt:         sampledAt,
	}); err != nil {
		respondError(c, http.StatusInternalServerError, "heartbeat_persist_failed", "failed to persist node heartbeat")
		return
	}

	c.Status(http.StatusNoContent)
}
