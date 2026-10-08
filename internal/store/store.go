package store

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const MaxUserListPageSize = 500

// User represents an account within the account service domain.
type User struct {
	ID                string
	Name              string
	Email             string
	Level             int
	Role              string
	Groups            []string
	Permissions       []string
	EmailVerified     bool
	PasswordHash      string
	MFATOTPSecret     string
	MFAEnabled        bool
	MFASecretIssuedAt time.Time
	MFAConfirmedAt    time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Active            bool
	// ProxyUUID is the legacy Xray client identifier. During the credential
	// migration it is copied verbatim into bridge_credentials.credential_uuid;
	// users.ID is never used as a network credential.
	ProxyUUID              string
	ProxyUUIDExpiresAt     *time.Time
	SubscriptionValidFrom  *time.Time
	SubscriptionValidUntil *time.Time
	LastActiveAt           *time.Time
	ArchivedAt             *time.Time
}

// Monthly quota groups are managed by the admin console. Membership changes
// only affect proxy/config access; the user record is never removed.
const (
	MonthlyFreeQuotaLimitGroup     = "segment:quota:free-5gb"
	MonthlyPlusQuotaLimitGroup     = "segment:quota:plus-20gb"
	MonthlyUnlimitedBetaQuotaGroup = "segment:quota:unlimited-beta"
)

var monthlyQuotaGroups = [...]string{
	MonthlyUnlimitedBetaQuotaGroup,
	MonthlyPlusQuotaLimitGroup,
	MonthlyFreeQuotaLimitGroup,
}

func MonthlyQuotaGroup(user *User) string {
	if user == nil {
		return ""
	}
	for _, group := range user.Groups {
		for _, known := range monthlyQuotaGroups {
			if strings.TrimSpace(group) == known {
				return known
			}
		}
	}
	return ""
}

func IsMonthlyFreeQuotaLimitMember(user *User) bool {
	return MonthlyQuotaGroup(user) == MonthlyFreeQuotaLimitGroup
}

func IsMonthlyQuotaLimitMember(user *User) bool {
	group := MonthlyQuotaGroup(user)
	return group == MonthlyFreeQuotaLimitGroup || group == MonthlyPlusQuotaLimitGroup
}

// Subscription represents a recurring or usage-based billing relationship.
type Subscription struct {
	ID            string
	UserID        string
	Provider      string
	PaymentMethod string
	PaymentQRCode string
	Kind          string
	PlanID        string
	ExternalID    string
	Status        string
	Meta          map[string]any
	CreatedAt     time.Time
	UpdatedAt     time.Time
	CancelledAt   *time.Time
}

// Identity represents a mapping between a user and a third-party authentication provider.
type Identity struct {
	ID         string
	UserID     string
	Provider   string
	ExternalID string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Agent represents a registered agent instance with health tracking.
type Agent struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Groups        []string   `json:"groups"`
	Healthy       bool       `json:"healthy"`
	LastHeartbeat *time.Time `json:"lastHeartbeat,omitempty"`
	ClientsCount  int        `json:"clientsCount"`
	SyncRevision  string     `json:"syncRevision,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
}

// OverlayDevice is a user-owned WireGuard device registered through the
// resilient overlay control plane.
type OverlayDevice struct {
	ID                 string     `json:"id"`
	UserID             string     `json:"userId"`
	NetworkID          string     `json:"networkId"`
	Name               string     `json:"name"`
	Platform           string     `json:"platform"`
	Hostname           string     `json:"hostname"`
	WireGuardPublicKey string     `json:"wireguardPublicKey"`
	WireGuardAddress   string     `json:"wireguardAddress"`
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
	LastSeenAt         *time.Time `json:"lastSeenAt,omitempty"`
}

// OverlayNode is a gateway/relay/exit-node that can terminate the
// WireGuard-over-VLESS data path for overlay clients.
type OverlayNode struct {
	ID                 string     `json:"id"`
	NetworkID          string     `json:"networkId"`
	Name               string     `json:"name"`
	Role               string     `json:"role"`
	Region             string     `json:"region"`
	WireGuardPublicKey string     `json:"wireguardPublicKey"`
	WireGuardAddress   string     `json:"wireguardAddress"`
	EndpointHost       string     `json:"endpointHost"`
	EndpointPort       int        `json:"endpointPort"`
	TransportType      string     `json:"transportType"`
	TransportSecurity  string     `json:"transportSecurity"`
	TransportPath      string     `json:"transportPath"`
	TransportMode      string     `json:"transportMode"`
	TransportUUID      string     `json:"transportUuid"`
	Healthy            bool       `json:"healthy"`
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
	LastHeartbeat      *time.Time `json:"lastHeartbeat,omitempty"`
}

// OverlayConfigAck records the latest config revision applied by a device.
type OverlayConfigAck struct {
	DeviceID   string    `json:"deviceId"`
	UserID     string    `json:"userId"`
	NetworkID  string    `json:"networkId"`
	Revision   string    `json:"revision"`
	Digest     string    `json:"digest"`
	AppliedAt  time.Time `json:"appliedAt"`
	ReceivedAt time.Time `json:"receivedAt"`
}

const (
	RatingStatusPending = "pending"
	RatingStatusRated   = "rated"
)

type TrafficStatCheckpoint struct {
	NodeID            string
	AccountUUID       string
	LastUplinkTotal   int64
	LastDownlinkTotal int64
	LastSeenAt        time.Time
	XrayRevision      string
	ResetEpoch        int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// 下面四个结构体是直接被 /api/account/* 序列化出去的响应体, 不只是内部模型。
// 没有 tag 时 encoding/json 用 Go 字段名原样输出(RatedBytes、CurrentBalance
// ...), 而这些接口的其余字段都由 gin.H 显式写成小驼峰, 前端也照小驼峰读。
// 结果是 quotaState / billingProfile / ledger / buckets 里每个字段前端都读成
// undefined —— ledger 为空时无人察觉, 一旦真有账目, Portal 就在
// entry.ratedBytes.toLocaleString() 上整页崩掉。
type TrafficMinuteBucket struct {
	BucketStart    time.Time `json:"bucketStart"`
	NodeID         string    `json:"nodeId"`
	AccountUUID    string    `json:"accountUuid"`
	Region         string    `json:"region"`
	LineCode       string    `json:"lineCode"`
	UplinkBytes    int64     `json:"uplinkBytes"`
	DownlinkBytes  int64     `json:"downlinkBytes"`
	TotalBytes     int64     `json:"totalBytes"`
	Multiplier     float64   `json:"multiplier"`
	RatingStatus   string    `json:"ratingStatus"`
	SourceRevision string    `json:"sourceRevision"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type BillingLedgerEntry struct {
	ID                 string    `json:"id"`
	AccountUUID        string    `json:"accountUuid"`
	BucketStart        time.Time `json:"bucketStart"`
	BucketEnd          time.Time `json:"bucketEnd"`
	EntryType          string    `json:"entryType"`
	RatedBytes         int64     `json:"ratedBytes"`
	AmountDelta        float64   `json:"amountDelta"`
	BalanceAfter       float64   `json:"balanceAfter"`
	PricingRuleVersion string    `json:"pricingRuleVersion"`
	CreatedAt          time.Time `json:"createdAt"`
}

// FinanceInvoice, FinancePayment, and FinanceRefund are immutable local
// financial facts. Amounts use the currency's minor unit (for example cents).
type FinanceInvoice struct {
	ID                string
	IdempotencyKey    string
	AccountUUID       string
	SubscriptionUUID  string
	Provider          string
	ProviderInvoiceID string
	AmountMinor       int64
	Currency          string
	Description       string
	IssuedAt          time.Time
	DueAt             *time.Time
	CreatedAt         time.Time
}

type FinancePayment struct {
	ID                string
	IdempotencyKey    string
	InvoiceID         string
	AccountUUID       string
	Provider          string
	ProviderPaymentID string
	AmountMinor       int64
	Currency          string
	PaidAt            time.Time
	CreatedAt         time.Time
}

type FinanceRefund struct {
	ID               string
	IdempotencyKey   string
	PaymentID        string
	Provider         string
	ProviderRefundID string
	AmountMinor      int64
	Currency         string
	Reason           string
	RefundedAt       time.Time
	CreatedAt        time.Time
}

// FinanceOperation is the durable retry/reconciliation projection. Every
// start and result is also captured in an append-only FinanceOperationEvent.
type FinanceOperation struct {
	ID                  string
	IdempotencyKey      string
	OperationType       string
	TargetType          string
	TargetID            string
	Provider            string
	ProviderOperationID string
	Status              string
	AttemptCount        int
	NextAttemptAt       *time.Time
	LastError           string
	Request             json.RawMessage
	Response            json.RawMessage
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type FinanceOperationEvent struct {
	ID                  string
	OperationID         string
	Attempt             int
	EventType           string
	Status              string
	ProviderOperationID string
	Payload             json.RawMessage
	Error               string
	OccurredAt          time.Time
}

const (
	FinanceOperationPending                = "pending"
	FinanceOperationInProgress             = "in_progress"
	FinanceOperationSucceeded              = "succeeded"
	FinanceOperationFailed                 = "failed"
	FinanceOperationReconciliationRequired = "reconcile_needed"
)

var (
	ErrFinanceRecordNotFound        = errors.New("finance record not found")
	ErrFinanceIdempotencyConflict   = errors.New("finance idempotency key conflicts with existing record")
	ErrFinanceInvoiceAlreadyPaid    = errors.New("finance invoice already has a settled payment")
	ErrFinanceRefundExceedsPayment  = errors.New("refund amount exceeds remaining payment amount")
	ErrFinanceOperationInProgress   = errors.New("finance operation is already in progress")
	ErrFinanceOperationNotRetryable = errors.New("finance operation is not retryable")
)

// AuditLog is one operator-initiated change. Reads are never audited — only
// writes — so the table stays proportional to operator activity rather than
// to traffic.
type AuditLog struct {
	UUID      string         `json:"uuid"`
	Action    string         `json:"action"`
	ActorUUID string         `json:"actorUuid"`
	Details   map[string]any `json:"details"`
	CreatedAt time.Time      `json:"createdAt"`
}

// AuditLogFilter narrows an audit query. ActionPrefix matches on the
// `<domain>.<object>.<verb>` convention, so "billing." returns every billing
// change and "billing.balance." only the balance ones.
type AuditLogFilter struct {
	ActionPrefix string
	ActorUUID    string
	TargetUUID   string
	Limit        int
	Offset       int
}

// AdminPlanGroupChange is a compare-and-set update for an account's plan
// group and optional subscription validity dates. The expected fields come
// from the operator's preview and prevent stale previews from overwriting a
// newer administrator change.
type AdminPlanGroupChange struct {
	UserID                       string
	PlanID                       string
	SetEntitlement               bool
	ExpectedGroups               []string
	Groups                       []string
	ExpectedValidFrom            *time.Time
	ExpectedValidUntil           *time.Time
	SetValidFrom                 bool
	ValidFrom                    *time.Time
	SetValidUntil                bool
	ValidUntil                   *time.Time
	PackageName                  string
	IncludedQuotaBytes           int64
	RegionMultiplier             float64
	LineMultiplier               float64
	PeakMultiplier               float64
	OffPeakMultiplier            float64
	PricingRuleVersion           string
	ExpectedProfileIncludedQuota int64
	ExpectedQuotaRemaining       int64
	ExpectedUsageBytes           int64
	ExpectedPeriodStart          *time.Time
	ExpectedPeriodEnd            *time.Time
	ExpectedProfileUpdatedAt     *time.Time
	ExpectedQuotaUpdatedAt       *time.Time
	ProfileExisted               bool
	QuotaExisted                 bool
	UsedBytesPreserved           int64
	RemainingAfter               int64
}

// AdminPlanGroupBatch is the complete, audited admin operation. RequestID is
// the idempotency key and PreviewToken binds the apply to the reviewed state.
type AdminPlanGroupBatch struct {
	ActorUUID        string
	Reason           string
	RequestID        string
	PreviewTokenHash string
	ExpectedUserIDs  []string
	Changes          []AdminPlanGroupChange
}

type AdminPlanGroupPreview struct {
	ActorUUID string
	Reason    string
	RequestID string
	TokenHash string
	ExpiresAt time.Time
	Changes   []AdminPlanGroupChange
}

// Audit action names. Kept as constants so a typo cannot silently create a
// second, unqueryable action stream.
const (
	AuditActionPlanUpsert            = "billing.plan.upsert"
	AuditActionPlanDelete            = "billing.plan.delete"
	AuditActionQuotaAdjust           = "billing.quota.adjust"
	AuditActionBalanceAdjust         = "billing.balance.adjust"
	AuditActionEntitlementGrant      = "billing.entitlement.grant"
	AuditActionTrialGrant            = "billing.trial.grant"
	AuditActionArrearsClear          = "billing.arrears.clear"
	AuditActionSubscriptionCancel    = "billing.subscription.cancel"
	AuditActionUserArchive           = "account.user.archive"
	AuditActionSegmentUpdate         = "account.segment.update"
	AuditActionPlanGroupUpdate       = "account.plan_group.update"
	AuditActionPlanGroupPreview      = "account.plan_group.preview"
	AuditActionPlanGroupPreviewUsed  = "account.plan_group.preview.used"
	AuditActionRoleUpdate            = "account.role.update"
	AuditActionOverlayOwnerReconcile = "overlay.gateway.owner_reconcile"
)

type AccountQuotaState struct {
	AccountUUID            string  `json:"accountUuid"`
	RemainingIncludedQuota int64   `json:"remainingIncludedQuota"`
	CurrentBalance         float64 `json:"currentBalance"`
	Arrears                bool    `json:"arrears"`
	// ArrearsSince marks when Arrears last flipped false->true; cleared back
	// to nil whenever Arrears clears. billing-service's SuspendSyncer reads
	// this to decide when a prolonged arrears episode should suspend access.
	ArrearsSince  *time.Time `json:"arrearsSince"`
	ThrottleState string     `json:"throttleState"`
	SuspendState  string     `json:"suspendState"`
	// ProxyAccessState is the operator-controlled VLESS gate.  It is kept
	// separate from SuspendState, which is owned by billing dunning, so an
	// operator can pause or resume proxy traffic without changing login or
	// accidentally clearing an arrears suspension.
	ProxyAccessState  string     `json:"proxyAccessState"`
	LastRatedBucketAt *time.Time `json:"lastRatedBucketAt"`
	// PeriodStart/PeriodEnd bound the current quota grant (the billing
	// period RemainingIncludedQuota was reset for). Written by entitlement
	// sync on grant/reset; nil until the first reset writes them.
	PeriodStart *time.Time `json:"periodStart"`
	PeriodEnd   *time.Time `json:"periodEnd"`
	EffectiveAt time.Time  `json:"effectiveAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

type AccountBillingProfile struct {
	AccountUUID        string    `json:"accountUuid"`
	PackageName        string    `json:"packageName"`
	IncludedQuotaBytes int64     `json:"includedQuotaBytes"`
	BasePricePerByte   float64   `json:"basePricePerByte"`
	RegionMultiplier   float64   `json:"regionMultiplier"`
	LineMultiplier     float64   `json:"lineMultiplier"`
	PeakMultiplier     float64   `json:"peakMultiplier"`
	OffPeakMultiplier  float64   `json:"offPeakMultiplier"`
	PricingRuleVersion string    `json:"pricingRuleVersion"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

type AccountPolicySnapshot struct {
	AccountUUID        string
	PolicyVersion      string
	AuthState          string
	RateProfile        string
	ConnProfile        string
	EligibleNodeGroups []string
	PreferredStrategy  string
	DegradeMode        string
	ExpiresAt          time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type NodeHealthSnapshot struct {
	NodeID            string
	Region            string
	LineCode          string
	PricingGroup      string
	StatsEnabled      bool
	XrayRevision      string
	Healthy           bool
	LatencyMS         int
	ErrorRate         float64
	ActiveConnections int
	HealthScore       float64
	SampledAt         time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type SchedulerDecision struct {
	ID          string
	AccountUUID string
	NodeGroup   string
	Strategy    string
	Decision    string
	GeneratedAt time.Time
	CreatedAt   time.Time
}

// Store provides persistence operations for users.
type Store interface {
	CreateUser(ctx context.Context, user *User) error
	GetUserByEmail(ctx context.Context, email string) (*User, error)
	GetUserByID(ctx context.Context, id string) (*User, error)
	GetUserByName(ctx context.Context, name string) (*User, error)
	UpdateUser(ctx context.Context, user *User) error
	CreateAdminPlanGroupPreview(ctx context.Context, preview AdminPlanGroupPreview) ([]AdminPlanGroupChange, error)
	ApplyAdminPlanGroupBatch(ctx context.Context, batch AdminPlanGroupBatch) (changes []AdminPlanGroupChange, replayed bool, err error)

	UpsertSubscription(ctx context.Context, subscription *Subscription) error
	ListSubscriptionsByUser(ctx context.Context, userID string) ([]Subscription, error)
	CancelSubscription(ctx context.Context, userID, externalID string, cancelledAt time.Time) (*Subscription, error)
	CreateIdentity(ctx context.Context, identity *Identity) error
	ListUsers(ctx context.Context) ([]User, error)
	ListUsersPage(ctx context.Context, afterID string, limit int) ([]User, error)
	// DeleteUser archives an account and atomically records the operator audit
	// and lifecycle transition. Implementations must reject unsupported schemas.
	DeleteUser(ctx context.Context, id string, audit *AuditLog, requestID string) error

	// Email Blacklist
	AddToBlacklist(ctx context.Context, email string) error
	RemoveFromBlacklist(ctx context.Context, email string) error
	IsBlacklisted(ctx context.Context, email string) (bool, error)
	ListBlacklist(ctx context.Context) ([]string, error)

	// Session management
	CreateSession(ctx context.Context, token, userID string, expiresAt time.Time) error
	GetSession(ctx context.Context, token string) (string, time.Time, error)
	DeleteSession(ctx context.Context, token string) error

	CreatePasswordRecoveryChallenge(ctx context.Context, challenge *PasswordRecoveryChallenge) error
	CreatePasswordRecoveryCodeChallenge(ctx context.Context, challenge *PasswordRecoveryChallenge, cooldown time.Duration) error
	GetPasswordRecoveryChallengeByTokenHash(ctx context.Context, tokenHash string) (*PasswordRecoveryChallenge, error)
	GetLatestPasswordRecoveryCode(ctx context.Context, email string) (*PasswordRecoveryChallenge, error)
	RecordPasswordRecoveryFailure(ctx context.Context, challengeID string, now time.Time, maxAttempts int, lockout time.Duration) (time.Time, error)
	InvalidatePasswordRecoveryChallenge(ctx context.Context, challengeID string, now time.Time) error
	CompletePasswordRecovery(ctx context.Context, challengeID, passwordHash string, now time.Time) error
	ReplaceMFARecoveryCodes(ctx context.Context, userID string, codes []MFARecoveryCode) error
	ListMFARecoveryCodes(ctx context.Context, userID string, now time.Time) ([]MFARecoveryCode, error)
	RecordMFARecoveryCodeFailure(ctx context.Context, userID string, now time.Time, maxAttempts int, lockout time.Duration) (time.Time, error)
	RevokeMFARecoveryCodes(ctx context.Context, userID string, now time.Time) error
	CompleteMFAPasswordReset(ctx context.Context, userID, recoveryCodeID, passwordHash string, now time.Time) error

	// OAuth exchange codes are short-lived, single-use credentials. They must
	// live in the same durable store as sessions so callback and exchange
	// requests can land on different service instances safely.
	CreateOAuthExchangeCode(ctx context.Context, code, sessionToken string, sessionExpiresAt, expiresAt time.Time) error
	ConsumeOAuthExchangeCode(ctx context.Context, code string) (sessionToken string, sessionExpiresAt time.Time, ok bool, err error)

	// Agent management
	UpsertAgent(ctx context.Context, agent *Agent) error
	GetAgent(ctx context.Context, id string) (*Agent, error)
	ListAgents(ctx context.Context) ([]*Agent, error)
	DeleteAgent(ctx context.Context, id string) error
	DeleteStaleAgents(ctx context.Context, staleThreshold time.Duration) (int, error)

	UpsertOverlayDevice(ctx context.Context, device *OverlayDevice) error
	GetOverlayDevice(ctx context.Context, userID, deviceID string) (*OverlayDevice, error)
	ListOverlayDevicesByUser(ctx context.Context, userID string) ([]OverlayDevice, error)
	ListOverlayDevicesByNetwork(ctx context.Context, networkID string) ([]OverlayDevice, error)
	UpsertOverlayNode(ctx context.Context, node *OverlayNode) error
	ListOverlayNodes(ctx context.Context, networkID string) ([]OverlayNode, error)
	UpsertOverlayConfigAck(ctx context.Context, ack *OverlayConfigAck) error

	UpsertTrafficStatCheckpoint(ctx context.Context, checkpoint *TrafficStatCheckpoint) error
	GetTrafficStatCheckpoint(ctx context.Context, nodeID, accountUUID string) (*TrafficStatCheckpoint, error)
	ListTrafficStatCheckpoints(ctx context.Context) ([]TrafficStatCheckpoint, error)
	UpsertTrafficMinuteBucket(ctx context.Context, bucket *TrafficMinuteBucket) error
	ListTrafficMinuteBucketsByAccount(ctx context.Context, accountUUID string, start, end time.Time) ([]TrafficMinuteBucket, error)
	ListTrafficMinuteBuckets(ctx context.Context) ([]TrafficMinuteBucket, error)
	InsertBillingLedgerEntry(ctx context.Context, entry *BillingLedgerEntry) error
	ListBillingLedgerByAccount(ctx context.Context, accountUUID string, limit int) ([]BillingLedgerEntry, error)
	UpsertAccountQuotaState(ctx context.Context, state *AccountQuotaState) error
	GetAccountQuotaState(ctx context.Context, accountUUID string) (*AccountQuotaState, error)
	// ListSuspendedAccountUUIDs returns the set of accounts currently
	// suspend_state='suspended', so agent/xray sync endpoints can drop them
	// in one batched lookup instead of a per-user quota-state query.
	ListSuspendedAccountUUIDs(ctx context.Context) (map[string]bool, error)
	// ListProxyBlockedAccountUUIDs contains billing-suspended and operator-paused
	// accounts. Quota exhaustion is statistical only until enforcement has been
	// verified in UAT; it must not withhold otherwise authorized Xray clients.
	ListProxyBlockedAccountUUIDs(ctx context.Context) (map[string]bool, error)
	UpsertAccountBillingProfile(ctx context.Context, profile *AccountBillingProfile) error
	GetAccountBillingProfile(ctx context.Context, accountUUID string) (*AccountBillingProfile, error)
	UpsertAccountPolicySnapshot(ctx context.Context, snapshot *AccountPolicySnapshot) error
	GetLatestAccountPolicySnapshot(ctx context.Context, accountUUID string) (*AccountPolicySnapshot, error)

	ListBillingPlans(ctx context.Context, includeInactive bool) ([]BillingPlan, error)
	GetBillingPlan(ctx context.Context, planID string) (*BillingPlan, error)
	GetBillingPlanByPriceID(ctx context.Context, stripePriceID string) (*BillingPlan, error)
	UpsertBillingPlan(ctx context.Context, plan *BillingPlan) error
	DeleteBillingPlan(ctx context.Context, planID string) error
	CreateFinanceInvoice(ctx context.Context, invoice *FinanceInvoice) (inserted bool, err error)
	GetFinanceInvoice(ctx context.Context, id string) (*FinanceInvoice, error)
	ListFinanceInvoices(ctx context.Context, accountUUID, subscriptionUUID string, limit int) ([]FinanceInvoice, error)
	// Each invoice accepts exactly one full-amount settled payment; distinct-key repeats fail.
	RecordFinancePayment(ctx context.Context, payment *FinancePayment) (inserted bool, err error)
	ListFinancePayments(ctx context.Context, accountUUID string, limit int) ([]FinancePayment, error)
	RecordFinanceRefund(ctx context.Context, refund *FinanceRefund) (inserted bool, err error)
	ListFinanceRefunds(ctx context.Context, accountUUID string, limit int) ([]FinanceRefund, error)
	// Refund operations use operation_type="refund", target_type="payment",
	// target_id=local payment UUID, and request JSON amount_minor/currency.
	// Their outstanding amount is reserved until failed or materialized as a refund fact.
	BeginFinanceOperation(ctx context.Context, operation *FinanceOperation) (claimed bool, err error)
	FinishFinanceOperation(ctx context.Context, operationID, status, providerOperationID string, response json.RawMessage, operationErr error, nextAttemptAt *time.Time) error
	GetFinanceOperation(ctx context.Context, idempotencyKey string) (*FinanceOperation, error)
	ListFinanceOperationsForReconciliation(ctx context.Context, limit int) ([]FinanceOperation, error)
	ListFinanceOperationEvents(ctx context.Context, operationID string) ([]FinanceOperationEvent, error)
	// BeginStripeWebhookEvent records an inbound event before processing and
	// reports whether it was already processed (idempotent replay guard).
	BeginStripeWebhookEvent(ctx context.Context, event *StripeWebhookEvent) (alreadyProcessed bool, err error)
	FinishStripeWebhookEvent(ctx context.Context, eventID string, procErr error) error
	// EnsureBillingEventQueue prepares the PGMQ billing_events queue and
	// reports whether publishing is enabled (extension present). Publishing
	// is best-effort and silently no-ops when disabled.
	EnsureBillingEventQueue(ctx context.Context) (bool, error)
	PublishBillingEvent(ctx context.Context, event *BillingEvent) error

	// InsertAuditLog records one operator-initiated change. Every admin write
	// that alters entitlements, quota, balance or pricing must produce one.
	InsertAuditLog(ctx context.Context, entry *AuditLog) error
	// ListAuditLogs returns the most recent entries first, optionally
	// narrowed by action prefix, actor or target account.
	ListAuditLogs(ctx context.Context, filter AuditLogFilter) ([]AuditLog, error)

	UpsertNodeHealthSnapshot(ctx context.Context, snapshot *NodeHealthSnapshot) error
	ListLatestNodeHealthSnapshots(ctx context.Context) ([]NodeHealthSnapshot, error)
	InsertSchedulerDecision(ctx context.Context, decision *SchedulerDecision) error
	ListRecentSchedulerDecisions(ctx context.Context, limit int) ([]SchedulerDecision, error)

	EnsureTenant(ctx context.Context, tenant *Tenant) error
	EnsureTenantDomain(ctx context.Context, domain *TenantDomain) error
	UpsertTenantMembership(ctx context.Context, membership *TenantMembership) error
	ResolveTenantByHost(ctx context.Context, host string) (*Tenant, *TenantDomain, error)
	ListTenantMembershipsByUser(ctx context.Context, userID string) ([]TenantMembership, error)
	GetTenantMembership(ctx context.Context, tenantID, userID string) (*TenantMembership, error)
	GetXWorkmateProfile(ctx context.Context, tenantID, userID, scope string) (*XWorkmateProfile, error)
	UpsertXWorkmateProfile(ctx context.Context, profile *XWorkmateProfile) error
}

// Domain level errors returned by the store implementation.
var (
	ErrEmailExists  = errors.New("email already exists")
	ErrNameExists   = errors.New("name already exists")
	ErrInvalidName  = errors.New("invalid user name")
	ErrUserNotFound = errors.New("user not found")
	// ErrUserArchiveUnsupported is returned when the backing schema cannot
	// atomically persist lifecycle state and its audit events.
	ErrUserArchiveUnsupported     = errors.New("atomic user archive is not supported by the current schema")
	ErrUserProtected              = errors.New("user is protected from archive")
	ErrUserAlreadyArchived        = errors.New("user is already archived")
	ErrUserArchiveReplayConflict  = errors.New("user archive request key was reused with different input")
	ErrAdminPlanGroupStale        = errors.New("admin plan group preview is stale")
	ErrAdminPlanGroupReplay       = errors.New("admin plan group request key was reused with different input")
	ErrAdminPlanGroupPreview      = errors.New("admin plan group preview is missing, expired, or already used")
	ErrMFANotSupported            = errors.New("mfa is not supported by the current store schema")
	ErrSuperAdminCountingDisabled = errors.New("super administrator counting is disabled")
	ErrSubscriptionNotFound       = errors.New("subscription not found")
	ErrPasswordRecoveryInvalid    = errors.New("password recovery challenge is invalid, expired, or consumed")
	ErrPasswordRecoveryCooldown   = errors.New("password recovery code cooldown is active")
)

// memoryStore provides an in-memory implementation of Store. It is suitable for
// unit tests and local development where a persistent database is not yet
// configured.
type memoryStore struct {
	mu                      sync.RWMutex
	allowSuperAdminCounting bool
	byID                    map[string]*User
	byEmail                 map[string]*User
	byName                  map[string]*User
	subscriptions           map[string]map[string]*Subscription
	identities              map[string]*Identity
	agents                  map[string]*Agent
	overlayDevices          map[string]*OverlayDevice
	overlayNodes            map[string]*OverlayNode
	overlayConfigAcks       map[string]*OverlayConfigAck
	sessions                map[string]*sessionRecord
	passwordRecovery        map[string]*PasswordRecoveryChallenge
	mfaRecoveryCodes        map[string]*MFARecoveryCode
	oauthExchangeCodes      map[string]*oauthExchangeRecord
	tenants                 map[string]*Tenant
	tenantDomains           map[string]*TenantDomain
	tenantMemberships       map[string]map[string]*TenantMembership
	xworkmateProfiles       map[string]*XWorkmateProfile
	trafficStatCheckpoints  map[string]*TrafficStatCheckpoint
	trafficMinuteBuckets    map[string]*TrafficMinuteBucket
	billingLedgerEntries    map[string]*BillingLedgerEntry
	auditLogs               []*AuditLog
	adminPlanGroupPreviews  map[string]AdminPlanGroupPreview
	adminPlanGroupConsumed  map[string]string
	accountQuotaStates      map[string]*AccountQuotaState
	accountBillingProfiles  map[string]*AccountBillingProfile
	accountPolicySnapshots  map[string]*AccountPolicySnapshot
	nodeHealthSnapshots     map[string]*NodeHealthSnapshot
	schedulerDecisions      map[string]*SchedulerDecision
	blacklistedEmails       map[string]bool
	billingPlans            map[string]*BillingPlan
	stripeWebhookEvents     map[string]*StripeWebhookEvent
	billingEvents           []BillingEvent
	financeInvoices         map[string]*FinanceInvoice
	financeInvoiceKeys      map[string]string
	financePayments         map[string]*FinancePayment
	financePaymentKeys      map[string]string
	financeRefunds          map[string]*FinanceRefund
	financeRefundKeys       map[string]string
	financeOperations       map[string]*FinanceOperation
	financeOperationKeys    map[string]string
	financeOperationEvents  map[string][]FinanceOperationEvent
}

type sessionRecord struct {
	UserID    string
	ExpiresAt time.Time
}

// PasswordRecoveryChallenge stores only a one-way representation of a reset
// credential. SecretHash is SHA-256 for high-entropy tokens and bcrypt for
// six-digit email codes.
type PasswordRecoveryChallenge struct {
	ID             string
	UserID         string
	Email          string
	Kind           string
	SecretHash     string
	ExpiresAt      time.Time
	FailedAttempts int
	LockedUntil    time.Time
	CreatedAt      time.Time
	ConsumedAt     time.Time
	InvalidatedAt  time.Time
}

// MFARecoveryCode stores only a password-hash representation of a one-time
// MFA recovery code. Raw codes are returned only when a batch is created.
type MFARecoveryCode struct {
	ID             string
	UserID         string
	BatchID        string
	CodeHash       string
	ExpiresAt      time.Time
	FailedAttempts int
	LockedUntil    time.Time
	CreatedAt      time.Time
	ConsumedAt     time.Time
	RevokedAt      time.Time
}

var ErrMFARecoveryCodeInvalid = errors.New("MFA recovery code is invalid, expired, consumed, or revoked")

type oauthExchangeRecord struct {
	SessionToken     string
	SessionExpiresAt time.Time
	ExpiresAt        time.Time
}

var ErrSessionNotFound = errors.New("session not found")

// NewMemoryStore creates a new in-memory store implementation with super
// administrator counting disabled by default to avoid accidental exposure of
// privileged metadata in environments where the caller has not explicitly
// opted-in.
func NewMemoryStore() Store {
	return newMemoryStore(false)
}

// NewMemoryStoreWithSuperAdminCounting creates a new in-memory store with
// explicit permission to count super administrators. This is primarily used by
// internal tooling that needs to enforce singleton guarantees.
func NewMemoryStoreWithSuperAdminCounting() Store {
	return newMemoryStore(true)
}

func newMemoryStore(allowSuperAdminCounting bool) Store {
	return &memoryStore{
		allowSuperAdminCounting: allowSuperAdminCounting,
		byID:                    make(map[string]*User),
		byEmail:                 make(map[string]*User),
		byName:                  make(map[string]*User),
		subscriptions:           make(map[string]map[string]*Subscription),
		identities:              make(map[string]*Identity),
		agents:                  make(map[string]*Agent),
		overlayDevices:          make(map[string]*OverlayDevice),
		overlayNodes:            make(map[string]*OverlayNode),
		overlayConfigAcks:       make(map[string]*OverlayConfigAck),
		sessions:                make(map[string]*sessionRecord),
		passwordRecovery:        make(map[string]*PasswordRecoveryChallenge),
		mfaRecoveryCodes:        make(map[string]*MFARecoveryCode),
		oauthExchangeCodes:      make(map[string]*oauthExchangeRecord),
		tenants:                 make(map[string]*Tenant),
		tenantDomains:           make(map[string]*TenantDomain),
		tenantMemberships:       make(map[string]map[string]*TenantMembership),
		xworkmateProfiles:       make(map[string]*XWorkmateProfile),
		trafficStatCheckpoints:  make(map[string]*TrafficStatCheckpoint),
		trafficMinuteBuckets:    make(map[string]*TrafficMinuteBucket),
		billingLedgerEntries:    make(map[string]*BillingLedgerEntry),
		auditLogs:               make([]*AuditLog, 0),
		adminPlanGroupPreviews:  make(map[string]AdminPlanGroupPreview),
		adminPlanGroupConsumed:  make(map[string]string),
		accountQuotaStates:      make(map[string]*AccountQuotaState),
		accountBillingProfiles:  make(map[string]*AccountBillingProfile),
		accountPolicySnapshots:  make(map[string]*AccountPolicySnapshot),
		nodeHealthSnapshots:     make(map[string]*NodeHealthSnapshot),
		schedulerDecisions:      make(map[string]*SchedulerDecision),
		blacklistedEmails:       make(map[string]bool),
		billingPlans:            make(map[string]*BillingPlan),
		stripeWebhookEvents:     make(map[string]*StripeWebhookEvent),
		financeInvoices:         make(map[string]*FinanceInvoice),
		financeInvoiceKeys:      make(map[string]string),
		financePayments:         make(map[string]*FinancePayment),
		financePaymentKeys:      make(map[string]string),
		financeRefunds:          make(map[string]*FinanceRefund),
		financeRefundKeys:       make(map[string]string),
		financeOperations:       make(map[string]*FinanceOperation),
		financeOperationKeys:    make(map[string]string),
		financeOperationEvents:  make(map[string][]FinanceOperationEvent),
	}
}

// CreateUser persists a user in the in-memory store.
func (s *memoryStore) CreateUser(ctx context.Context, user *User) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	loweredEmail := strings.ToLower(strings.TrimSpace(user.Email))
	normalizedName := strings.TrimSpace(user.Name)

	if normalizedName == "" {
		return ErrInvalidName
	}

	normalizeUserRoleFields(user)

	if _, exists := s.byEmail[loweredEmail]; exists {
		return ErrEmailExists
	}
	if _, exists := s.byName[strings.ToLower(normalizedName)]; exists {
		return ErrNameExists
	}
	userCopy := *user
	if userCopy.ID == "" {
		userCopy.ID = uuid.NewString()
	}
	if userCopy.CreatedAt.IsZero() {
		now := time.Now().UTC()
		userCopy.CreatedAt = now
		if userCopy.UpdatedAt.IsZero() {
			userCopy.UpdatedAt = now
		}
	}
	if userCopy.UpdatedAt.IsZero() {
		userCopy.UpdatedAt = time.Now().UTC()
	}
	userCopy.Email = loweredEmail
	userCopy.Name = normalizedName
	stored := userCopy
	normalizeUserRoleFields(&stored)
	stored.Groups = cloneStringSlice(stored.Groups)
	stored.Permissions = cloneStringSlice(stored.Permissions)
	// Diverges from postgresStore.CreateUser, which writes User.Active
	// verbatim. Tests that read an account back through this store therefore
	// cannot observe a caller that forgot to set Active; assert on the value
	// passed to CreateUser instead (see TestRegisterCreatesActiveAccount).
	stored.Active = true
	if strings.TrimSpace(stored.ProxyUUID) == "" {
		credentialID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		stored.ProxyUUID = credentialID.String()
	}
	s.byID[userCopy.ID] = &stored
	if loweredEmail != "" {
		s.byEmail[loweredEmail] = &stored
	}
	s.byName[strings.ToLower(normalizedName)] = &stored
	assignUser(user, &stored)
	return nil
}

// GetUserByEmail fetches a user by email, returning ErrUserNotFound when the
// user does not exist.
func (s *memoryStore) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()
	user, ok := s.byEmail[strings.ToLower(email)]
	if !ok {
		return nil, ErrUserNotFound
	}
	return cloneUser(user), nil
}

// GetUserByID fetches a user by unique identifier, returning ErrUserNotFound
// when absent.
func (s *memoryStore) GetUserByID(ctx context.Context, id string) (*User, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()
	user, ok := s.byID[id]
	if !ok {
		return nil, ErrUserNotFound
	}
	return cloneUser(user), nil
}

// GetUserByName fetches a user by case-insensitive username, returning
// ErrUserNotFound when absent.
func (s *memoryStore) GetUserByName(ctx context.Context, name string) (*User, error) {
	_ = ctx
	normalized := strings.ToLower(strings.TrimSpace(name))
	if normalized == "" {
		return nil, ErrUserNotFound
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	user, ok := s.byName[normalized]
	if !ok {
		return nil, ErrUserNotFound
	}

	return cloneUser(user), nil
}

// UpdateUser replaces the persisted user representation in memory.
func (s *memoryStore) UpdateUser(ctx context.Context, user *User) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.byID[user.ID]
	if !ok {
		return ErrUserNotFound
	}

	normalizedName := strings.TrimSpace(user.Name)
	loweredEmail := strings.ToLower(strings.TrimSpace(user.Email))

	if normalizedName == "" {
		return ErrInvalidName
	}

	// Re-index username if it changed.
	oldNameKey := strings.ToLower(existing.Name)
	newNameKey := strings.ToLower(normalizedName)
	if oldNameKey != newNameKey {
		if _, exists := s.byName[newNameKey]; exists {
			return ErrNameExists
		}
		delete(s.byName, oldNameKey)
	}

	// Re-index email if it changed.
	oldEmailKey := strings.ToLower(existing.Email)
	if oldEmailKey != loweredEmail {
		if loweredEmail != "" {
			if _, exists := s.byEmail[loweredEmail]; exists {
				return ErrEmailExists
			}
		}
		if oldEmailKey != "" {
			delete(s.byEmail, oldEmailKey)
		}
	}

	updated := *existing
	updated.Name = normalizedName
	updated.Email = loweredEmail
	updated.EmailVerified = user.EmailVerified
	updated.PasswordHash = user.PasswordHash
	updated.MFATOTPSecret = user.MFATOTPSecret
	updated.MFAEnabled = user.MFAEnabled
	updated.MFASecretIssuedAt = user.MFASecretIssuedAt
	updated.MFAConfirmedAt = user.MFAConfirmedAt
	updated.Level = user.Level
	updated.Role = user.Role
	updated.Groups = cloneStringSlice(user.Groups)
	updated.Permissions = cloneStringSlice(user.Permissions)
	updated.Active = user.Active
	updated.ProxyUUID = strings.TrimSpace(user.ProxyUUID)
	if updated.ProxyUUID == "" {
		credentialID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		updated.ProxyUUID = credentialID.String()
	}
	updated.ProxyUUIDExpiresAt = user.ProxyUUIDExpiresAt
	updated.SubscriptionValidFrom = cloneTimePointer(user.SubscriptionValidFrom)
	updated.SubscriptionValidUntil = cloneTimePointer(user.SubscriptionValidUntil)
	updated.LastActiveAt = cloneTimePointer(user.LastActiveAt)
	updated.ArchivedAt = cloneTimePointer(user.ArchivedAt)
	normalizeUserRoleFields(&updated)
	if user.CreatedAt.IsZero() {
		updated.CreatedAt = existing.CreatedAt
	} else {
		updated.CreatedAt = user.CreatedAt
	}
	if user.UpdatedAt.IsZero() {
		updated.UpdatedAt = time.Now().UTC()
	} else {
		updated.UpdatedAt = user.UpdatedAt
	}

	s.byID[user.ID] = &updated
	s.byName[newNameKey] = &updated
	if loweredEmail != "" {
		s.byEmail[loweredEmail] = &updated
	}

	assignUser(user, &updated)
	return nil
}

func (s *memoryStore) CreateAdminPlanGroupPreview(ctx context.Context, preview AdminPlanGroupPreview) ([]AdminPlanGroupChange, error) {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	if preview.TokenHash == "" || preview.RequestID == "" || len(preview.Changes) == 0 || !preview.ExpiresAt.After(time.Now()) {
		return nil, errors.New("invalid admin plan group preview")
	}
	if _, exists := s.adminPlanGroupPreviews[preview.TokenHash]; exists {
		return nil, errors.New("duplicate admin plan group preview token")
	}
	preview.Changes = cloneAdminPlanGroupChanges(preview.Changes)
	seen := make(map[string]struct{}, len(preview.Changes))
	for i := range preview.Changes {
		change := &preview.Changes[i]
		if _, duplicate := seen[change.UserID]; duplicate {
			return nil, errors.New("duplicate user in admin plan group preview")
		}
		seen[change.UserID] = struct{}{}
		user, ok := s.byID[change.UserID]
		if !ok {
			return nil, ErrUserNotFound
		}
		if IsRootRole(user.Role) {
			return nil, ErrUserProtected
		}
		if !equalStoreStrings(user.Groups, change.ExpectedGroups) || !equalStoreTimes(user.SubscriptionValidFrom, change.ExpectedValidFrom) || !equalStoreTimes(user.SubscriptionValidUntil, change.ExpectedValidUntil) {
			return nil, ErrAdminPlanGroupStale
		}
		profile := s.accountBillingProfiles[user.ID]
		quota := s.accountQuotaStates[user.ID]
		change.ProfileExisted = profile != nil
		change.QuotaExisted = quota != nil
		if profile != nil {
			change.ExpectedProfileIncludedQuota = profile.IncludedQuotaBytes
			updated := profile.UpdatedAt.UTC()
			change.ExpectedProfileUpdatedAt = &updated
		}
		if quota != nil {
			change.ExpectedQuotaRemaining = quota.RemainingIncludedQuota
			change.ExpectedPeriodStart = cloneTimePointer(quota.PeriodStart)
			change.ExpectedPeriodEnd = cloneTimePointer(quota.PeriodEnd)
			updated := quota.UpdatedAt.UTC()
			change.ExpectedQuotaUpdatedAt = &updated
		}
		if change.SetEntitlement {
			used := change.ExpectedProfileIncludedQuota - change.ExpectedQuotaRemaining
			if used < 0 {
				used = 0
			}
			change.ExpectedUsageBytes = adminPlanGroupBucketUsage(s.trafficMinuteBuckets, user.ID, quota)
			if change.ExpectedUsageBytes > used {
				used = change.ExpectedUsageBytes
			}
			change.UsedBytesPreserved = used
			change.RemainingAfter = change.IncludedQuotaBytes - used
			if change.RemainingAfter < 0 {
				change.RemainingAfter = 0
			}
		}
	}
	s.adminPlanGroupPreviews[preview.TokenHash] = preview
	s.auditLogs = append(s.auditLogs, &AuditLog{
		UUID: uuid.NewString(), Action: AuditActionPlanGroupPreview, ActorUUID: preview.ActorUUID,
		Details:   map[string]any{"request_id": preview.RequestID, "reason": preview.Reason, "expires_at": preview.ExpiresAt.UTC()},
		CreatedAt: time.Now().UTC(),
	})
	return cloneAdminPlanGroupChanges(preview.Changes), nil
}

func (s *memoryStore) ApplyAdminPlanGroupBatch(ctx context.Context, batch AdminPlanGroupBatch) ([]AdminPlanGroupChange, bool, error) {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.auditLogs {
		if existing.Action != AuditActionPlanGroupUpdate || existing.Details["request_id"] != batch.RequestID {
			continue
		}
		if existing.ActorUUID != batch.ActorUUID || existing.Details["reason"] != batch.Reason || existing.Details["preview_token_hash"] != batch.PreviewTokenHash {
			return nil, false, ErrAdminPlanGroupReplay
		}
		return nil, true, nil
	}
	preview, exists := s.adminPlanGroupPreviews[batch.PreviewTokenHash]
	if !exists || preview.ExpiresAt.Before(time.Now()) || preview.ActorUUID != batch.ActorUUID ||
		preview.RequestID != batch.RequestID || preview.Reason != batch.Reason ||
		s.adminPlanGroupConsumed[batch.PreviewTokenHash] != "" {
		return nil, false, ErrAdminPlanGroupPreview
	}
	changes := cloneAdminPlanGroupChanges(preview.Changes)
	if len(batch.ExpectedUserIDs) > 0 {
		expected := append([]string(nil), batch.ExpectedUserIDs...)
		sort.Strings(expected)
		if len(expected) != len(changes) {
			return nil, false, ErrAdminPlanGroupPreview
		}
		actual := make([]string, len(changes))
		for i := range changes {
			actual[i] = changes[i].UserID
		}
		sort.Strings(actual)
		for i := range expected {
			if expected[i] != actual[i] {
				return nil, false, ErrAdminPlanGroupPreview
			}
		}
	}
	prepared := make([]preparedAdminPlanGroupChange, 0, len(changes))
	seen := make(map[string]struct{}, len(changes))
	for _, change := range changes {
		if _, duplicate := seen[change.UserID]; duplicate {
			return nil, false, errors.New("duplicate user in admin plan group batch")
		}
		seen[change.UserID] = struct{}{}
		user, ok := s.byID[change.UserID]
		if !ok {
			return nil, false, ErrUserNotFound
		}
		if IsRootRole(user.Role) {
			return nil, false, ErrUserProtected
		}
		if !equalStoreStrings(user.Groups, change.ExpectedGroups) ||
			!equalStoreTimes(user.SubscriptionValidFrom, change.ExpectedValidFrom) ||
			!equalStoreTimes(user.SubscriptionValidUntil, change.ExpectedValidUntil) {
			return nil, false, ErrAdminPlanGroupStale
		}
		profile := cloneBillingProfile(s.accountBillingProfiles[user.ID])
		quota := cloneQuotaState(s.accountQuotaStates[user.ID])
		if change.SetEntitlement && ((profile != nil) != change.ProfileExisted || (quota != nil) != change.QuotaExisted) {
			return nil, false, ErrAdminPlanGroupStale
		}
		if profile == nil {
			profile = &AccountBillingProfile{AccountUUID: user.ID, RegionMultiplier: 1, LineMultiplier: 1, PeakMultiplier: 1, OffPeakMultiplier: 1}
		}
		if quota == nil {
			quota = &AccountQuotaState{AccountUUID: user.ID, ThrottleState: "normal", SuspendState: "active", ProxyAccessState: "active"}
		}
		if change.SetEntitlement && (profile.IncludedQuotaBytes != change.ExpectedProfileIncludedQuota || quota.RemainingIncludedQuota != change.ExpectedQuotaRemaining ||
			!equalStoreTimes(profileUpdatedAt(s.accountBillingProfiles[user.ID]), change.ExpectedProfileUpdatedAt) ||
			!equalStoreTimes(quotaUpdatedAt(s.accountQuotaStates[user.ID]), change.ExpectedQuotaUpdatedAt) ||
			!equalStoreTimes(quota.PeriodStart, change.ExpectedPeriodStart) || !equalStoreTimes(quota.PeriodEnd, change.ExpectedPeriodEnd)) {
			return nil, false, ErrAdminPlanGroupStale
		}
		used := profile.IncludedQuotaBytes - quota.RemainingIncludedQuota
		if used < 0 {
			used = 0
		}
		// Buckets are authoritative when they include over-cap traffic that the
		// remaining-quota counter can no longer represent.
		bucketUsage := adminPlanGroupBucketUsage(s.trafficMinuteBuckets, user.ID, quota)
		if change.SetEntitlement && bucketUsage != change.ExpectedUsageBytes {
			return nil, false, ErrAdminPlanGroupStale
		}
		if bucketUsage > used {
			used = bucketUsage
		}
		if change.SetEntitlement {
			profile.AccountUUID = user.ID
			profile.PackageName = change.PackageName
			profile.IncludedQuotaBytes = change.IncludedQuotaBytes
			profile.RegionMultiplier = change.RegionMultiplier
			profile.LineMultiplier = change.LineMultiplier
			profile.PeakMultiplier = change.PeakMultiplier
			profile.OffPeakMultiplier = change.OffPeakMultiplier
			profile.PricingRuleVersion = change.PricingRuleVersion
			quota.RemainingIncludedQuota = change.IncludedQuotaBytes - used
			if quota.RemainingIncludedQuota < 0 {
				quota.RemainingIncludedQuota = 0
			}
			quota.EffectiveAt = time.Now().UTC()
		}
		prepared = append(prepared, preparedAdminPlanGroupChange{change: change, user: user, profile: profile, quota: quota, used: used})
	}

	now := time.Now().UTC()
	for _, item := range prepared {
		item.user.Groups = cloneStringSlice(normalizeStringSlice(item.change.Groups))
		if item.change.SetValidFrom {
			item.user.SubscriptionValidFrom = cloneTimePointer(item.change.ValidFrom)
		}
		if item.change.SetValidUntil {
			item.user.SubscriptionValidUntil = cloneTimePointer(item.change.ValidUntil)
		}
		item.user.UpdatedAt = now
		if item.change.SetEntitlement {
			item.profile.UpdatedAt = now
			item.quota.UpdatedAt = now
			s.accountBillingProfiles[item.user.ID] = item.profile
			s.accountQuotaStates[item.user.ID] = item.quota
		}
		before := map[string]any{"groups": item.change.ExpectedGroups, "valid_from": item.change.ExpectedValidFrom, "valid_until": item.change.ExpectedValidUntil,
			"included_quota_bytes": item.change.ExpectedProfileIncludedQuota, "remaining_included_quota": item.change.ExpectedQuotaRemaining}
		after := map[string]any{"groups": item.change.Groups, "valid_from": item.user.SubscriptionValidFrom, "valid_until": item.user.SubscriptionValidUntil,
			"plan_id": item.change.PlanID, "included_quota_bytes": item.change.IncludedQuotaBytes, "remaining_included_quota": item.quota.RemainingIncludedQuota,
			"used_bytes_preserved": item.used, "configuration_sync_paused": item.change.SetEntitlement && item.change.IncludedQuotaBytes > 0 && item.quota.RemainingIncludedQuota == 0}
		s.auditLogs = append(s.auditLogs, &AuditLog{
			UUID: uuid.NewString(), Action: AuditActionPlanGroupUpdate, ActorUUID: batch.ActorUUID,
			Details: map[string]any{"target_uuid": item.user.ID, "reason": batch.Reason, "request_id": batch.RequestID,
				"preview_token_hash": batch.PreviewTokenHash, "before": before, "after": after}, CreatedAt: now,
		})
	}
	s.adminPlanGroupConsumed[batch.PreviewTokenHash] = batch.RequestID
	s.auditLogs = append(s.auditLogs, &AuditLog{
		UUID: uuid.NewString(), Action: AuditActionPlanGroupPreviewUsed, ActorUUID: batch.ActorUUID,
		Details: map[string]any{"request_id": batch.RequestID, "preview_token_hash": batch.PreviewTokenHash}, CreatedAt: now,
	})
	return changes, false, nil
}

type preparedAdminPlanGroupChange struct {
	change  AdminPlanGroupChange
	user    *User
	profile *AccountBillingProfile
	quota   *AccountQuotaState
	used    int64
}

func quotaPeriodStart(quota *AccountQuotaState, now time.Time) time.Time {
	if quota != nil && quota.PeriodStart != nil {
		return quota.PeriodStart.UTC()
	}
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func adminPlanGroupBucketUsage(buckets map[string]*TrafficMinuteBucket, accountID string, quota *AccountQuotaState) int64 {
	start := quotaPeriodStart(quota, time.Now().UTC())
	var total int64
	for _, bucket := range buckets {
		if bucket.AccountUUID == accountID && !bucket.BucketStart.Before(start) && (quota == nil || quota.PeriodEnd == nil || bucket.BucketStart.Before(*quota.PeriodEnd)) && bucket.TotalBytes > 0 {
			total += bucket.TotalBytes
		}
	}
	return total
}

func profileUpdatedAt(profile *AccountBillingProfile) *time.Time {
	if profile == nil || profile.UpdatedAt.IsZero() {
		return nil
	}
	value := profile.UpdatedAt.UTC()
	return &value
}

func quotaUpdatedAt(quota *AccountQuotaState) *time.Time {
	if quota == nil || quota.UpdatedAt.IsZero() {
		return nil
	}
	value := quota.UpdatedAt.UTC()
	return &value
}

func cloneAdminPlanGroupChanges(changes []AdminPlanGroupChange) []AdminPlanGroupChange {
	cloned := make([]AdminPlanGroupChange, len(changes))
	for i, change := range changes {
		cloned[i] = change
		cloned[i].ExpectedGroups = cloneStringSlice(change.ExpectedGroups)
		cloned[i].Groups = cloneStringSlice(change.Groups)
		cloned[i].ExpectedValidFrom = cloneTimePointer(change.ExpectedValidFrom)
		cloned[i].ExpectedValidUntil = cloneTimePointer(change.ExpectedValidUntil)
		cloned[i].ValidFrom = cloneTimePointer(change.ValidFrom)
		cloned[i].ValidUntil = cloneTimePointer(change.ValidUntil)
	}
	return cloned
}

func equalStoreStrings(a, b []string) bool {
	a = normalizeStringSlice(a)
	b = normalizeStringSlice(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalStoreTimes(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// UpsertSubscription creates or updates a subscription for a user.
func (s *memoryStore) UpsertSubscription(ctx context.Context, subscription *Subscription) error {
	_ = ctx
	if subscription == nil {
		return errors.New("subscription is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	userID := strings.TrimSpace(subscription.UserID)
	if userID == "" {
		return ErrUserNotFound
	}

	if _, ok := s.byID[userID]; !ok {
		return ErrUserNotFound
	}

	userSubs, ok := s.subscriptions[userID]
	if !ok {
		userSubs = make(map[string]*Subscription)
		s.subscriptions[userID] = userSubs
	}

	key := strings.TrimSpace(subscription.ExternalID)
	if key == "" {
		return errors.New("external id is required")
	}
	if strings.TrimSpace(subscription.PaymentMethod) == "" {
		subscription.PaymentMethod = strings.TrimSpace(subscription.Provider)
	}
	subscription.PaymentQRCode = strings.TrimSpace(subscription.PaymentQRCode)

	now := time.Now().UTC()
	stored, exists := userSubs[key]
	if !exists {
		stored = &Subscription{ID: uuid.NewString(), UserID: userID, ExternalID: key, CreatedAt: now}
		userSubs[key] = stored
	}

	stored.Provider = strings.TrimSpace(subscription.Provider)
	stored.PaymentMethod = strings.TrimSpace(subscription.PaymentMethod)
	stored.PaymentQRCode = strings.TrimSpace(subscription.PaymentQRCode)
	stored.Kind = strings.TrimSpace(subscription.Kind)
	stored.PlanID = strings.TrimSpace(subscription.PlanID)
	stored.Status = strings.TrimSpace(subscription.Status)
	stored.Meta = cloneSubscriptionMeta(subscription.Meta)
	stored.UpdatedAt = now
	if subscription.CancelledAt != nil {
		cancelled := subscription.CancelledAt.UTC()
		stored.CancelledAt = &cancelled
	}

	assignSubscription(subscription, stored)
	return nil
}

// ListSubscriptionsByUser returns subscriptions associated with a user.
func (s *memoryStore) ListSubscriptionsByUser(ctx context.Context, userID string) ([]Subscription, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	normalized := strings.TrimSpace(userID)
	if normalized == "" {
		return nil, ErrUserNotFound
	}

	subs := s.subscriptions[normalized]
	if len(subs) == 0 {
		return []Subscription{}, nil
	}

	result := make([]Subscription, 0, len(subs))
	for _, sub := range subs {
		result = append(result, *cloneSubscription(sub))
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result, nil
}

// CancelSubscription marks a subscription as cancelled.
func (s *memoryStore) CancelSubscription(ctx context.Context, userID, externalID string, cancelledAt time.Time) (*Subscription, error) {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()

	normalizedUserID := strings.TrimSpace(userID)
	if normalizedUserID == "" {
		return nil, ErrUserNotFound
	}

	subs := s.subscriptions[normalizedUserID]
	if subs == nil {
		return nil, ErrSubscriptionNotFound
	}

	key := strings.TrimSpace(externalID)
	existing, ok := subs[key]
	if !ok {
		return nil, ErrSubscriptionNotFound
	}

	cancelled := cancelledAt.UTC()
	existing.Status = "cancelled"
	existing.CancelledAt = &cancelled
	existing.UpdatedAt = time.Now().UTC()

	return cloneSubscription(existing), nil
}

// CountSuperAdmins returns the number of users configured as super administrators.
func (s *memoryStore) CountSuperAdmins(ctx context.Context) (int, error) {
	_ = ctx
	if !s.allowSuperAdminCounting {
		return 0, ErrSuperAdminCountingDisabled
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	count := 0
	for _, user := range s.byID {
		if isSuperAdmin(user) {
			count++
		}
	}
	return count, nil
}

const ()

const (
	// LevelAdmin is the numeric level for administrator accounts.
	LevelAdmin = 0
	// LevelOperator is the numeric level for operator accounts.
	LevelOperator = 10
	// LevelUser is the numeric level for standard user accounts.
	LevelUser = 20
)

const (
	// RoleRoot identifies a root administrator account. More than one root is
	// permitted so recovery does not depend on a singleton account.
	RoleRoot = "root"
	// RoleAdmin identifies legacy administrator accounts from earlier versions.
	RoleAdmin = "admin"
	// RoleOperator identifies operator accounts.
	RoleOperator = "operator"
	// RoleUser identifies standard user accounts.
	RoleUser = "user"
	// RoleReadOnly identifies read-only accounts.
	RoleReadOnly = "readonly"
)

var (
	roleToLevel = map[string]int{
		RoleRoot:     LevelAdmin,
		RoleAdmin:    LevelAdmin,
		RoleOperator: LevelOperator,
		RoleUser:     LevelUser,
		RoleReadOnly: LevelUser,
	}
	levelToRole = map[int]string{
		LevelAdmin:    RoleRoot,
		LevelOperator: RoleOperator,
		LevelUser:     RoleUser,
	}
)

// IsRootRole reports whether a role should be treated as root-equivalent.
func IsRootRole(role string) bool {
	normalized := strings.ToLower(strings.TrimSpace(role))
	return normalized == RoleRoot
}

// IsAdminRole reports whether a role is admin-like (root or legacy admin).
func IsAdminRole(role string) bool {
	normalized := strings.ToLower(strings.TrimSpace(role))
	return normalized == RoleRoot || normalized == RoleAdmin
}

// IsOperatorRole reports whether a role is operator.
func IsOperatorRole(role string) bool {
	return strings.ToLower(strings.TrimSpace(role)) == RoleOperator
}

func normalizeUserRoleFields(user *User) {
	if user == nil {
		return
	}

	normalizedRole := strings.ToLower(strings.TrimSpace(user.Role))
	if level, ok := roleToLevel[normalizedRole]; ok {
		user.Role = normalizedRole
		user.Level = level
	} else if role, ok := levelToRole[user.Level]; ok {
		user.Role = role
	} else {
		user.Role = RoleUser
		user.Level = LevelUser
	}

	user.Groups = normalizeStringSlice(user.Groups)
	user.Permissions = normalizeStringSlice(user.Permissions)
}

func normalizeStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func cloneStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	clone := make([]string, len(values))
	copy(clone, values)
	return clone
}

func cloneSubscription(sub *Subscription) *Subscription {
	if sub == nil {
		return nil
	}
	clone := *sub
	clone.Meta = cloneSubscriptionMeta(sub.Meta)
	if sub.CancelledAt != nil {
		cancelled := sub.CancelledAt.UTC()
		clone.CancelledAt = &cancelled
	}
	return &clone
}

func cloneSubscriptionMeta(meta map[string]any) map[string]any {
	if len(meta) == 0 {
		return map[string]any{}
	}
	clone := make(map[string]any, len(meta))
	for key, value := range meta {
		clone[key] = value
	}
	return clone
}

func cloneUser(user *User) *User {
	if user == nil {
		return nil
	}
	clone := *user
	clone.Groups = cloneStringSlice(user.Groups)
	clone.Permissions = cloneStringSlice(user.Permissions)
	clone.SubscriptionValidFrom = cloneTimePointer(user.SubscriptionValidFrom)
	clone.SubscriptionValidUntil = cloneTimePointer(user.SubscriptionValidUntil)
	clone.LastActiveAt = cloneTimePointer(user.LastActiveAt)
	clone.ArchivedAt = cloneTimePointer(user.ArchivedAt)
	normalizeUserRoleFields(&clone)
	return &clone
}

func assignUser(dst, src *User) {
	*dst = *src
	dst.Groups = cloneStringSlice(src.Groups)
	dst.Permissions = cloneStringSlice(src.Permissions)
	dst.SubscriptionValidFrom = cloneTimePointer(src.SubscriptionValidFrom)
	dst.SubscriptionValidUntil = cloneTimePointer(src.SubscriptionValidUntil)
	dst.LastActiveAt = cloneTimePointer(src.LastActiveAt)
	dst.ArchivedAt = cloneTimePointer(src.ArchivedAt)
	normalizeUserRoleFields(dst)
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := value.UTC()
	return &cloned
}

func assignSubscription(dst, src *Subscription) {
	*dst = *src
	dst.Meta = cloneSubscriptionMeta(src.Meta)
	if src.CancelledAt != nil {
		cancelled := src.CancelledAt.UTC()
		dst.CancelledAt = &cancelled
	}
}

func isSuperAdmin(user *User) bool {
	if user == nil {
		return false
	}
	if !IsAdminRole(user.Role) && user.Level != LevelAdmin {
		return false
	}

	hasWildcard := false
	for _, permission := range user.Permissions {
		if strings.TrimSpace(permission) == "*" {
			hasWildcard = true
			break
		}
	}
	if !hasWildcard {
		return false
	}

	for _, group := range user.Groups {
		if strings.EqualFold(strings.TrimSpace(group), "Admin") {
			return true
		}
	}

	return false
}

// CreateIdentity persists an identity record in the in-memory store.
func (s *memoryStore) CreateIdentity(ctx context.Context, identity *Identity) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()

	if identity.ID == "" {
		identity.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	if identity.CreatedAt.IsZero() {
		identity.CreatedAt = now
	}
	if identity.UpdatedAt.IsZero() {
		identity.UpdatedAt = now
	}

	key := identity.Provider + ":" + identity.ExternalID
	if _, exists := s.identities[key]; exists {
		return errors.New("identity already exists")
	}

	stored := *identity
	s.identities[key] = &stored
	return nil
}

// ListUsers returns all users in the in-memory store.
func (s *memoryStore) ListUsers(ctx context.Context) ([]User, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]User, 0, len(s.byID))
	for _, user := range s.byID {
		result = append(result, *cloneUser(user))
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})

	return result, nil
}

func (s *memoryStore) ListUsersPage(ctx context.Context, afterID string, limit int) ([]User, error) {
	_ = ctx
	if limit <= 0 || limit > MaxUserListPageSize {
		return nil, errors.New("user page limit is out of range")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.byID))
	for id := range s.byID {
		if id > afterID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) > limit {
		ids = ids[:limit]
	}
	users := make([]User, 0, len(ids))
	for _, id := range ids {
		users = append(users, *cloneUser(s.byID[id]))
	}
	return users, nil
}

func (s *memoryStore) DeleteUser(ctx context.Context, id string, audit *AuditLog, requestID string) error {
	_ = ctx
	if err := validateUserArchiveAudit(id, audit); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	requestID = strings.TrimSpace(requestID)
	if requestID != "" {
		for _, existing := range s.auditLogs {
			if existing.Action != AuditActionUserArchive ||
				existing.Details["target_uuid"] != id || existing.Details["request_id"] != requestID {
				continue
			}
			if existing.ActorUUID != audit.ActorUUID || existing.Details["reason"] != audit.Details["reason"] {
				return ErrUserArchiveReplayConflict
			}
			*audit = *cloneAuditLog(existing)
			return nil
		}
	}
	user, ok := s.byID[id]
	if !ok {
		return ErrUserNotFound
	}
	if user.ArchivedAt != nil {
		return ErrUserAlreadyArchived
	}
	if IsAdminRole(user.Role) || user.Level == LevelAdmin ||
		MonthlyQuotaGroup(user) == MonthlyPlusQuotaLimitGroup ||
		MonthlyQuotaGroup(user) == MonthlyUnlimitedBetaQuotaGroup {
		return ErrUserProtected
	}
	for _, subscription := range s.subscriptions[id] {
		if subscription.Status == "active" || subscription.Status == "trialing" || subscription.Status == "past_due" {
			return ErrUserProtected
		}
	}
	now := time.Now().UTC()
	transitionID := uuid.NewString()
	user.Active = false
	user.ArchivedAt = &now
	user.UpdatedAt = now
	if strings.TrimSpace(audit.UUID) == "" {
		audit.UUID = uuid.NewString()
	}
	audit.CreatedAt = now
	if requestID != "" {
		audit.Details["request_id"] = requestID
	}
	audit.Details["transition_id"] = transitionID
	audit.Details["occurred_at"] = now
	s.auditLogs = append(s.auditLogs, cloneAuditLog(audit))
	return nil
}

func validateUserArchiveAudit(userID string, audit *AuditLog) error {
	if audit == nil || strings.TrimSpace(audit.Action) != AuditActionUserArchive || strings.TrimSpace(audit.ActorUUID) == "" {
		return errors.New("a user archive audit entry with an actor is required")
	}
	if audit.Details == nil {
		return errors.New("user archive audit details are required")
	}
	target, _ := audit.Details["target_uuid"].(string)
	reason, _ := audit.Details["reason"].(string)
	if strings.TrimSpace(target) != strings.TrimSpace(userID) || strings.TrimSpace(reason) == "" {
		return errors.New("user archive audit target and reason are required")
	}
	return nil
}

func (s *memoryStore) AddToBlacklist(ctx context.Context, email string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blacklistedEmails[strings.ToLower(email)] = true
	return nil
}

func (s *memoryStore) RemoveFromBlacklist(ctx context.Context, email string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.blacklistedEmails, strings.ToLower(email))
	return nil
}

func (s *memoryStore) IsBlacklisted(ctx context.Context, email string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.blacklistedEmails[strings.ToLower(email)], nil
}

func (s *memoryStore) ListBlacklist(ctx context.Context) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	emails := make([]string, 0, len(s.blacklistedEmails))
	for email := range s.blacklistedEmails {
		emails = append(emails, email)
	}
	return emails, nil
}

func (s *memoryStore) UpsertAgent(ctx context.Context, agent *Agent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	existing, exists := s.agents[agent.ID]
	if !exists {
		existing = &Agent{
			ID:        agent.ID,
			CreatedAt: now,
		}
		s.agents[agent.ID] = existing
	}

	existing.Name = agent.Name
	existing.Groups = cloneStringSlice(agent.Groups)
	existing.Healthy = agent.Healthy
	existing.LastHeartbeat = agent.LastHeartbeat
	existing.ClientsCount = agent.ClientsCount
	existing.SyncRevision = agent.SyncRevision
	existing.UpdatedAt = now

	*agent = *existing
	return nil
}

func (s *memoryStore) GetAgent(ctx context.Context, id string) (*Agent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	agent, ok := s.agents[id]
	if !ok {
		return nil, errors.New("agent not found")
	}
	clone := *agent
	clone.Groups = cloneStringSlice(agent.Groups)
	return &clone, nil
}

func (s *memoryStore) ListAgents(ctx context.Context) ([]*Agent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*Agent, 0, len(s.agents))
	for _, agent := range s.agents {
		clone := *agent
		clone.Groups = cloneStringSlice(agent.Groups)
		result = append(result, &clone)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].ID < result[j].ID
	})
	return result, nil
}

func (s *memoryStore) DeleteAgent(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.agents, id)
	return nil
}

func (s *memoryStore) DeleteStaleAgents(ctx context.Context, staleThreshold time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().Add(-staleThreshold)
	count := 0
	for id, agent := range s.agents {
		if agent.LastHeartbeat == nil || agent.LastHeartbeat.Before(cutoff) {
			delete(s.agents, id)
			count++
		}
	}
	return count, nil
}

func (s *memoryStore) CreateSession(ctx context.Context, token, userID string, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[token] = &sessionRecord{
		UserID:    userID,
		ExpiresAt: expiresAt,
	}
	return nil
}

func (s *memoryStore) GetSession(ctx context.Context, token string) (string, time.Time, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[token]
	if !ok {
		return "", time.Time{}, ErrSessionNotFound
	}
	if time.Now().After(sess.ExpiresAt) {
		return "", time.Time{}, ErrSessionNotFound
	}
	return sess.UserID, sess.ExpiresAt, nil
}

func (s *memoryStore) DeleteSession(ctx context.Context, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
	return nil
}

func (s *memoryStore) CreateOAuthExchangeCode(ctx context.Context, code, sessionToken string, sessionExpiresAt, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.oauthExchangeCodes[strings.TrimSpace(code)] = &oauthExchangeRecord{
		SessionToken:     sessionToken,
		SessionExpiresAt: sessionExpiresAt,
		ExpiresAt:        expiresAt,
	}
	return nil
}

func (s *memoryStore) ConsumeOAuthExchangeCode(ctx context.Context, code string) (string, time.Time, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	normalized := strings.TrimSpace(code)
	record, ok := s.oauthExchangeCodes[normalized]
	if !ok {
		return "", time.Time{}, false, nil
	}
	delete(s.oauthExchangeCodes, normalized)
	if time.Now().After(record.ExpiresAt) {
		return "", time.Time{}, false, nil
	}
	return record.SessionToken, record.SessionExpiresAt, true, nil
}
