package store

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// User represents an account within the account service domain.
type User struct {
	ID                 string
	Name               string
	Email              string
	Level              int
	Role               string
	Groups             []string
	Permissions        []string
	EmailVerified      bool
	PasswordHash       string
	MFATOTPSecret      string
	MFAEnabled         bool
	MFASecretIssuedAt  time.Time
	MFAConfirmedAt     time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
	Active             bool
	ProxyUUID          string
	ProxyUUIDExpiresAt *time.Time
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

type TrafficMinuteBucket struct {
	BucketStart    time.Time
	NodeID         string
	AccountUUID    string
	Region         string
	LineCode       string
	UplinkBytes    int64
	DownlinkBytes  int64
	TotalBytes     int64
	Multiplier     float64
	RatingStatus   string
	SourceRevision string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type BillingLedgerEntry struct {
	ID                 string
	AccountUUID        string
	BucketStart        time.Time
	BucketEnd          time.Time
	EntryType          string
	RatedBytes         int64
	AmountDelta        float64
	BalanceAfter       float64
	PricingRuleVersion string
	CreatedAt          time.Time
}

type AccountQuotaState struct {
	AccountUUID            string
	RemainingIncludedQuota int64
	CurrentBalance         float64
	Arrears                bool
	ThrottleState          string
	SuspendState           string
	LastRatedBucketAt      *time.Time
	EffectiveAt            time.Time
	UpdatedAt              time.Time
}

type AccountBillingProfile struct {
	AccountUUID        string
	PackageName        string
	IncludedQuotaBytes int64
	BasePricePerByte   float64
	RegionMultiplier   float64
	LineMultiplier     float64
	PeakMultiplier     float64
	OffPeakMultiplier  float64
	PricingRuleVersion string
	CreatedAt          time.Time
	UpdatedAt          time.Time
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

	UpsertSubscription(ctx context.Context, subscription *Subscription) error
	ListSubscriptionsByUser(ctx context.Context, userID string) ([]Subscription, error)
	CancelSubscription(ctx context.Context, userID, externalID string, cancelledAt time.Time) (*Subscription, error)
	CreateIdentity(ctx context.Context, identity *Identity) error
	ListUsers(ctx context.Context) ([]User, error)
	DeleteUser(ctx context.Context, id string) error

	// Email Blacklist
	AddToBlacklist(ctx context.Context, email string) error
	RemoveFromBlacklist(ctx context.Context, email string) error
	IsBlacklisted(ctx context.Context, email string) (bool, error)
	ListBlacklist(ctx context.Context) ([]string, error)

	// Session management
	CreateSession(ctx context.Context, token, userID string, expiresAt time.Time) error
	GetSession(ctx context.Context, token string) (string, time.Time, error)
	DeleteSession(ctx context.Context, token string) error

	// Agent management
	UpsertAgent(ctx context.Context, agent *Agent) error
	GetAgent(ctx context.Context, id string) (*Agent, error)
	ListAgents(ctx context.Context) ([]*Agent, error)
	DeleteAgent(ctx context.Context, id string) error
	DeleteStaleAgents(ctx context.Context, staleThreshold time.Duration) (int, error)

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
	UpsertAccountBillingProfile(ctx context.Context, profile *AccountBillingProfile) error
	GetAccountBillingProfile(ctx context.Context, accountUUID string) (*AccountBillingProfile, error)
	UpsertAccountPolicySnapshot(ctx context.Context, snapshot *AccountPolicySnapshot) error
	GetLatestAccountPolicySnapshot(ctx context.Context, accountUUID string) (*AccountPolicySnapshot, error)
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
	ErrEmailExists                = errors.New("email already exists")
	ErrNameExists                 = errors.New("name already exists")
	ErrInvalidName                = errors.New("invalid user name")
	ErrUserNotFound               = errors.New("user not found")
	ErrMFANotSupported            = errors.New("mfa is not supported by the current store schema")
	ErrSuperAdminCountingDisabled = errors.New("super administrator counting is disabled")
	ErrSubscriptionNotFound       = errors.New("subscription not found")
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
	sessions                map[string]*sessionRecord
	tenants                 map[string]*Tenant
	tenantDomains           map[string]*TenantDomain
	tenantMemberships       map[string]map[string]*TenantMembership
	xworkmateProfiles       map[string]*XWorkmateProfile
	trafficStatCheckpoints  map[string]*TrafficStatCheckpoint
	trafficMinuteBuckets    map[string]*TrafficMinuteBucket
	billingLedgerEntries    map[string]*BillingLedgerEntry
	accountQuotaStates      map[string]*AccountQuotaState
	accountBillingProfiles  map[string]*AccountBillingProfile
	accountPolicySnapshots  map[string]*AccountPolicySnapshot
	nodeHealthSnapshots     map[string]*NodeHealthSnapshot
	schedulerDecisions      map[string]*SchedulerDecision
}

type sessionRecord struct {
	UserID    string
	ExpiresAt time.Time
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
		sessions:                make(map[string]*sessionRecord),
		tenants:                 make(map[string]*Tenant),
		tenantDomains:           make(map[string]*TenantDomain),
		tenantMemberships:       make(map[string]map[string]*TenantMembership),
		xworkmateProfiles:       make(map[string]*XWorkmateProfile),
		trafficStatCheckpoints:  make(map[string]*TrafficStatCheckpoint),
		trafficMinuteBuckets:    make(map[string]*TrafficMinuteBucket),
		billingLedgerEntries:    make(map[string]*BillingLedgerEntry),
		accountQuotaStates:      make(map[string]*AccountQuotaState),
		accountBillingProfiles:  make(map[string]*AccountBillingProfile),
		accountPolicySnapshots:  make(map[string]*AccountPolicySnapshot),
		nodeHealthSnapshots:     make(map[string]*NodeHealthSnapshot),
		schedulerDecisions:      make(map[string]*SchedulerDecision),
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
	stored.Active = true
	stored.ProxyUUID = uuid.NewString()
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
	updated.ProxyUUID = user.ProxyUUID
	updated.ProxyUUIDExpiresAt = user.ProxyUUIDExpiresAt
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

const (
	// RootAdminEmail is the canonical email for the single root account.
	RootAdminEmail = "admin@svc.plus"
)

const (
	// LevelAdmin is the numeric level for administrator accounts.
	LevelAdmin = 0
	// LevelOperator is the numeric level for operator accounts.
	LevelOperator = 10
	// LevelUser is the numeric level for standard user accounts.
	LevelUser = 20
)

const (
	// RoleRoot identifies the single root administrator account.
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
	normalizeUserRoleFields(&clone)
	return &clone
}

func assignUser(dst, src *User) {
	*dst = *src
	dst.Groups = cloneStringSlice(src.Groups)
	dst.Permissions = cloneStringSlice(src.Permissions)
	normalizeUserRoleFields(dst)
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

func (s *memoryStore) DeleteUser(ctx context.Context, id string) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	user, ok := s.byID[id]
	if !ok {
		return nil
	}
	delete(s.byID, id)
	delete(s.byEmail, strings.ToLower(user.Email))
	delete(s.byName, strings.ToLower(user.Name))
	return nil
}

func (s *memoryStore) AddToBlacklist(ctx context.Context, email string) error {
	return nil
}

func (s *memoryStore) RemoveFromBlacklist(ctx context.Context, email string) error {
	return nil
}

func (s *memoryStore) IsBlacklisted(ctx context.Context, email string) (bool, error) {
	return false, nil
}

func (s *memoryStore) ListBlacklist(ctx context.Context) ([]string, error) {
	return []string{}, nil
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
