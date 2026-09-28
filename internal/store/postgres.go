package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/XSAM/otelsql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Config describes how to construct a Store implementation.
type Config struct {
	Driver                  string
	DSN                     string
	MaxOpenConns            int
	MaxIdleConns            int
	ConnMaxLifetime         time.Duration
	ConnMaxIdleTime         time.Duration
	AllowSuperAdminCounting bool
}

// New creates a Store implementation based on the provided configuration.
func New(ctx context.Context, cfg Config) (Store, func(context.Context) error, error) {
	driver := strings.ToLower(strings.TrimSpace(cfg.Driver))
	if driver == "" || driver == "memory" {
		ms := newMemoryStore(cfg.AllowSuperAdminCounting)
		return ms, func(context.Context) error { return nil }, nil
	}

	switch driver {
	case "postgres", "postgresql", "pgx":
		if strings.TrimSpace(cfg.DSN) == "" {
			return nil, nil, errors.New("store dsn is required for postgres driver")
		}

		db, err := otelsql.Open("pgx", cfg.DSN)
		if err != nil {
			return nil, nil, err
		}

		if cfg.MaxOpenConns > 0 {
			db.SetMaxOpenConns(cfg.MaxOpenConns)
		}
		if cfg.MaxIdleConns >= 0 {
			db.SetMaxIdleConns(cfg.MaxIdleConns)
		}
		if cfg.ConnMaxLifetime > 0 {
			db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
		}
		if cfg.ConnMaxIdleTime > 0 {
			db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
		}

		if err := db.PingContext(ctx); err != nil {
			db.Close()
			return nil, nil, err
		}

		cleanup := func(context.Context) error {
			return db.Close()
		}

		return &postgresStore{db: db, allowSuperAdminCounting: cfg.AllowSuperAdminCounting}, cleanup, nil
	default:
		return nil, nil, fmt.Errorf("unsupported store driver %q", cfg.Driver)
	}
}

type schemaCapabilities struct {
	hasMFATOTPSecret          bool
	hasMFAEnabled             bool
	hasMFASecretIssuedAt      bool
	hasMFAConfirmedAt         bool
	hasCreatedAt              bool
	hasUpdatedAt              bool
	hasLevel                  bool
	hasRole                   bool
	hasGroups                 bool
	hasPermissions            bool
	hasActive                 bool
	hasProxyUUID              bool
	hasProxyUUIDExpiresAt     bool
	hasSubscriptionValidFrom  bool
	hasSubscriptionValidUntil bool
	hasLastActiveAt           bool
	hasArchivedAt             bool
}

func (c schemaCapabilities) supportsMFA() bool {
	return c.hasMFATOTPSecret && c.hasMFAEnabled && c.hasMFASecretIssuedAt && c.hasMFAConfirmedAt
}

type postgresStore struct {
	db                      *sql.DB
	allowSuperAdminCounting bool
	billingEventsEnabled    atomic.Bool

	capsMu     sync.RWMutex
	caps       schemaCapabilities
	capsLoaded bool
}

// Ping verifies the business store's own connection pool. Accounts keeps the
// business store and admin-settings GORM handle separate, so readiness checks
// both pools before traffic is routed to this process.
func (s *postgresStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *postgresStore) CreateUser(ctx context.Context, user *User) error {
	normalizedEmail := strings.ToLower(strings.TrimSpace(user.Email))
	normalizedName := strings.TrimSpace(user.Name)
	if normalizedName == "" {
		return ErrInvalidName
	}

	caps, err := s.capabilities(ctx)
	if err != nil {
		return err
	}

	normalizeUserRoleFields(user)
	if strings.TrimSpace(user.ID) == "" {
		user.ID = uuid.NewString()
	}
	// proxy_uuid is the legacy network credential. Existing values are preserved
	// and new values use UUIDv7; it must never be derived from users.uuid.
	if strings.TrimSpace(user.ProxyUUID) == "" {
		credentialID, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("generate proxy credential uuid: %w", err)
		}
		user.ProxyUUID = credentialID.String()
	}

	var (
		verifiedAt any
	)
	if user.EmailVerified {
		verifiedAt = time.Now().UTC()
	}

	if normalizedEmail != "" {
		const emailExistsQuery = "SELECT EXISTS(SELECT 1 FROM users WHERE lower(email) = $1)"
		emailExists, err := s.userExists(ctx, emailExistsQuery, normalizedEmail)
		if err != nil {
			return err
		}
		if emailExists {
			return ErrEmailExists
		}
	}

	const nameExistsQuery = "SELECT EXISTS(SELECT 1 FROM users WHERE lower(username) = lower($1))"
	nameExists, err := s.userExists(ctx, nameExistsQuery, normalizedName)
	if err != nil {
		return err
	}
	if nameExists {
		return ErrNameExists
	}

	columns := []string{"uuid", "username", "password", "email", "email_verified_at"}
	placeholders := []string{"$1", "$2", "$3", "$4", "$5"}
	args := []any{user.ID, normalizedName, user.PasswordHash, normalizedEmail, verifiedAt}

	idx := len(args) + 1

	if caps.hasLevel {
		columns = append(columns, "level")
		placeholders = append(placeholders, fmt.Sprintf("$%d", idx))
		args = append(args, user.Level)
		idx++
	}
	if caps.hasRole {
		columns = append(columns, "role")
		placeholders = append(placeholders, fmt.Sprintf("$%d", idx))
		args = append(args, user.Role)
		idx++
	}
	if caps.hasGroups {
		encoded, err := encodeStringSlice(user.Groups)
		if err != nil {
			return err
		}
		columns = append(columns, "groups")
		placeholders = append(placeholders, fmt.Sprintf("$%d", idx))
		args = append(args, encoded)
		idx++
	}
	if caps.hasPermissions {
		encoded, err := encodeStringSlice(user.Permissions)
		if err != nil {
			return err
		}
		columns = append(columns, "permissions")
		placeholders = append(placeholders, fmt.Sprintf("$%d", idx))
		args = append(args, encoded)
		idx++
	}

	if caps.hasActive {
		columns = append(columns, "active")
		placeholders = append(placeholders, fmt.Sprintf("$%d", idx))
		args = append(args, user.Active)
		idx++
	}
	if caps.hasProxyUUID && user.ProxyUUID != "" {
		columns = append(columns, "proxy_uuid")
		placeholders = append(placeholders, fmt.Sprintf("$%d", idx))
		args = append(args, user.ProxyUUID)
		idx++
	}
	if caps.hasProxyUUIDExpiresAt {
		columns = append(columns, "proxy_uuid_expires_at")
		placeholders = append(placeholders, fmt.Sprintf("$%d", idx))
		args = append(args, user.ProxyUUIDExpiresAt)
		idx++
	}
	if caps.hasSubscriptionValidFrom {
		columns = append(columns, "subscription_valid_from")
		placeholders = append(placeholders, fmt.Sprintf("$%d", idx))
		args = append(args, user.SubscriptionValidFrom)
		idx++
	}
	if caps.hasSubscriptionValidUntil {
		columns = append(columns, "subscription_valid_until")
		placeholders = append(placeholders, fmt.Sprintf("$%d", idx))
		args = append(args, user.SubscriptionValidUntil)
		idx++
	}
	if caps.hasLastActiveAt {
		columns = append(columns, "last_active_at")
		placeholders = append(placeholders, fmt.Sprintf("$%d", idx))
		args = append(args, user.LastActiveAt)
		idx++
	}
	if caps.hasArchivedAt {
		columns = append(columns, "archived_at")
		placeholders = append(placeholders, fmt.Sprintf("$%d", idx))
		args = append(args, user.ArchivedAt)
		idx++
	}

	query := fmt.Sprintf(`INSERT INTO users (%s)
      VALUES (%s)
      RETURNING uuid, coalesce(created_at, now()), coalesce(updated_at, now()), (email_verified_at IS NOT NULL)`, strings.Join(columns, ", "), strings.Join(placeholders, ", "))

	var idValue any
	var createdAt time.Time
	var updatedAt time.Time
	var emailVerified sql.NullBool
	err = s.db.QueryRowContext(ctx, query, args...).Scan(&idValue, &createdAt, &updatedAt, &emailVerified)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			if pgErr.Code == "23505" { // unique_violation
				switch {
				case strings.Contains(pgErr.ConstraintName, "email"):
					return ErrEmailExists
				case strings.Contains(pgErr.ConstraintName, "name") || strings.Contains(pgErr.ConstraintName, "username"):
					return ErrNameExists
				}
			}
		}
		return err
	}

	identifier, err := formatIdentifier(idValue)
	if err != nil {
		return err
	}

	user.ID = identifier
	user.Name = normalizedName
	user.Email = normalizedEmail
	user.CreatedAt = createdAt.UTC()
	user.UpdatedAt = updatedAt.UTC()
	user.EmailVerified = emailVerified.Bool
	return nil
}

func (s *postgresStore) userExists(ctx context.Context, query string, arg any) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, query, arg).Scan(&exists)
	if err != nil {
		if isDatabaseEmptyError(err) {
			return false, nil
		}
		return false, err
	}
	return exists, nil
}

func isDatabaseEmptyError(err error) bool {
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code == "42P01" { // undefined_table
			return true
		}
	}
	return false
}

func (s *postgresStore) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	normalized := strings.ToLower(strings.TrimSpace(email))
	if normalized == "" {
		return nil, ErrUserNotFound
	}

	caps, err := s.capabilities(ctx)
	if err != nil {
		return nil, err
	}

	query := s.selectUserQuery(caps, "WHERE lower(email) = $1 LIMIT 1")

	row := s.db.QueryRowContext(ctx, query, normalized)
	return scanUser(row)
}

func (s *postgresStore) GetUserByName(ctx context.Context, name string) (*User, error) {
	normalized := strings.TrimSpace(name)
	if normalized == "" {
		return nil, ErrUserNotFound
	}

	caps, err := s.capabilities(ctx)
	if err != nil {
		return nil, err
	}

	query := s.selectUserQuery(caps, "WHERE lower(username) = lower($1) LIMIT 1")

	row := s.db.QueryRowContext(ctx, query, normalized)
	return scanUser(row)
}

func (s *postgresStore) GetUserByID(ctx context.Context, id string) (*User, error) {
	caps, err := s.capabilities(ctx)
	if err != nil {
		return nil, err
	}

	query := s.selectUserQuery(caps, "WHERE uuid = $1")

	row := s.db.QueryRowContext(ctx, query, id)
	return scanUser(row)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanUser(row rowScanner) (*User, error) {
	var (
		idValue                any
		username               sql.NullString
		email                  sql.NullString
		emailVerified          sql.NullBool
		password               sql.NullString
		mfaSecret              sql.NullString
		mfaEnabled             sql.NullBool
		mfaSecretIssued        sql.NullTime
		mfaConfirmed           sql.NullTime
		createdAt              time.Time
		updatedAt              time.Time
		levelValue             sql.NullInt64
		roleValue              sql.NullString
		groupsRaw              []byte
		permissionsRaw         []byte
		activeValue            sql.NullBool
		proxyUUID              sql.NullString
		proxyExpiresAt         sql.NullTime
		subscriptionValidFrom  sql.NullTime
		subscriptionValidUntil sql.NullTime
		lastActiveAt           sql.NullTime
		archivedAt             sql.NullTime
	)

	if err := row.Scan(&idValue, &username, &email, &emailVerified, &password, &mfaSecret, &mfaEnabled, &mfaSecretIssued, &mfaConfirmed, &createdAt, &updatedAt, &levelValue, &roleValue, &groupsRaw, &permissionsRaw, &activeValue, &proxyUUID, &proxyExpiresAt, &subscriptionValidFrom, &subscriptionValidUntil, &lastActiveAt, &archivedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}

	identifier, err := formatIdentifier(idValue)
	if err != nil {
		return nil, err
	}

	user := &User{
		ID:                identifier,
		Name:              strings.TrimSpace(username.String),
		Email:             strings.ToLower(strings.TrimSpace(email.String)),
		EmailVerified:     emailVerified.Bool,
		PasswordHash:      password.String,
		MFATOTPSecret:     strings.TrimSpace(mfaSecret.String),
		MFAEnabled:        mfaEnabled.Bool,
		MFASecretIssuedAt: toUTCTime(mfaSecretIssued),
		MFAConfirmedAt:    toUTCTime(mfaConfirmed),
		CreatedAt:         createdAt.UTC(),
		UpdatedAt:         updatedAt.UTC(),
	}
	if levelValue.Valid {
		user.Level = int(levelValue.Int64)
	}
	user.Role = strings.TrimSpace(roleValue.String)
	user.Groups = decodeStringSlice(groupsRaw)
	user.Permissions = decodeStringSlice(permissionsRaw)
	user.Active = activeValue.Valid && activeValue.Bool
	user.ProxyUUID = strings.TrimSpace(proxyUUID.String)
	if proxyExpiresAt.Valid {
		t := proxyExpiresAt.Time.UTC()
		user.ProxyUUIDExpiresAt = &t
	}
	if subscriptionValidFrom.Valid {
		t := subscriptionValidFrom.Time.UTC()
		user.SubscriptionValidFrom = &t
	}
	if subscriptionValidUntil.Valid {
		t := subscriptionValidUntil.Time.UTC()
		user.SubscriptionValidUntil = &t
	}
	if lastActiveAt.Valid {
		t := lastActiveAt.Time.UTC()
		user.LastActiveAt = &t
	}
	if archivedAt.Valid {
		t := archivedAt.Time.UTC()
		user.ArchivedAt = &t
	}
	normalizeUserRoleFields(user)
	return user, nil
}

func (s *postgresStore) UpdateUser(ctx context.Context, user *User) error {
	normalizedName := strings.TrimSpace(user.Name)
	if normalizedName == "" {
		return ErrInvalidName
	}

	normalizedEmail := strings.ToLower(strings.TrimSpace(user.Email))
	if strings.TrimSpace(user.ProxyUUID) == "" {
		credentialID, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("generate proxy credential uuid: %w", err)
		}
		user.ProxyUUID = credentialID.String()
	}

	caps, err := s.capabilities(ctx)
	if err != nil {
		return err
	}

	var issuedAt any
	if !user.MFASecretIssuedAt.IsZero() {
		issuedAt = user.MFASecretIssuedAt.UTC()
	}
	var confirmedAt any
	if !user.MFAConfirmedAt.IsZero() {
		confirmedAt = user.MFAConfirmedAt.UTC()
	}

	builder := strings.Builder{}
	builder.WriteString("UPDATE users SET username = $1, email = $2, password = $3")

	if user.EmailVerified {
		builder.WriteString(", email_verified_at = COALESCE(email_verified_at, now())")
	} else {
		builder.WriteString(", email_verified_at = NULL")
	}

	normalizeUserRoleFields(user)

	args := []any{normalizedName, normalizedEmail, user.PasswordHash}
	idx := 4

	if caps.hasMFATOTPSecret {
		builder.WriteString(fmt.Sprintf(", mfa_totp_secret = $%d", idx))
		args = append(args, nullForEmpty(user.MFATOTPSecret))
		idx++
	} else if strings.TrimSpace(user.MFATOTPSecret) != "" {
		return ErrMFANotSupported
	}

	if caps.hasMFAEnabled {
		builder.WriteString(fmt.Sprintf(", mfa_enabled = $%d", idx))
		args = append(args, user.MFAEnabled)
		idx++
	} else if user.MFAEnabled {
		return ErrMFANotSupported
	}

	if caps.hasMFASecretIssuedAt {
		builder.WriteString(fmt.Sprintf(", mfa_secret_issued_at = $%d", idx))
		args = append(args, issuedAt)
		idx++
	} else if !user.MFASecretIssuedAt.IsZero() {
		return ErrMFANotSupported
	}

	if caps.hasMFAConfirmedAt {
		builder.WriteString(fmt.Sprintf(", mfa_confirmed_at = $%d", idx))
		args = append(args, confirmedAt)
		idx++
	} else if !user.MFAConfirmedAt.IsZero() {
		return ErrMFANotSupported
	}

	if caps.hasUpdatedAt {
		builder.WriteString(", updated_at = now()")
	}

	if caps.hasLevel {
		builder.WriteString(fmt.Sprintf(", level = $%d", idx))
		args = append(args, user.Level)
		idx++
	}

	if caps.hasRole {
		builder.WriteString(fmt.Sprintf(", role = $%d", idx))
		args = append(args, user.Role)
		idx++
	}

	if caps.hasGroups {
		encoded, err := encodeStringSlice(user.Groups)
		if err != nil {
			return err
		}
		builder.WriteString(fmt.Sprintf(", groups = $%d", idx))
		args = append(args, encoded)
		idx++
	}

	if caps.hasPermissions {
		encoded, err := encodeStringSlice(user.Permissions)
		if err != nil {
			return err
		}
		builder.WriteString(fmt.Sprintf(", permissions = $%d", idx))
		args = append(args, encoded)
		idx++
	}

	if caps.hasActive {
		builder.WriteString(fmt.Sprintf(", active = $%d", idx))
		args = append(args, user.Active)
		idx++
	}
	if caps.hasProxyUUID && user.ProxyUUID != "" {
		builder.WriteString(fmt.Sprintf(", proxy_uuid = $%d", idx))
		args = append(args, user.ProxyUUID)
		idx++
	}
	if caps.hasProxyUUIDExpiresAt {
		builder.WriteString(fmt.Sprintf(", proxy_uuid_expires_at = $%d", idx))
		args = append(args, user.ProxyUUIDExpiresAt)
		idx++
	}
	if caps.hasSubscriptionValidFrom {
		builder.WriteString(fmt.Sprintf(", subscription_valid_from = $%d", idx))
		args = append(args, user.SubscriptionValidFrom)
		idx++
	}
	if caps.hasSubscriptionValidUntil {
		builder.WriteString(fmt.Sprintf(", subscription_valid_until = $%d", idx))
		args = append(args, user.SubscriptionValidUntil)
		idx++
	}
	if caps.hasLastActiveAt {
		builder.WriteString(fmt.Sprintf(", last_active_at = $%d", idx))
		args = append(args, user.LastActiveAt)
		idx++
	}
	if caps.hasArchivedAt {
		builder.WriteString(fmt.Sprintf(", archived_at = $%d", idx))
		args = append(args, user.ArchivedAt)
		idx++
	}

	builder.WriteString(fmt.Sprintf(" WHERE uuid = $%d RETURNING ", idx))
	args = append(args, user.ID)
	idx++

	if caps.hasCreatedAt {
		builder.WriteString("coalesce(created_at, now())")
	} else {
		builder.WriteString("now()")
	}

	if caps.hasUpdatedAt {
		builder.WriteString(", coalesce(updated_at, now())")
	} else {
		builder.WriteString(", now()")
	}

	query := builder.String()

	var createdAt time.Time
	var updatedAt time.Time
	err = s.db.QueryRowContext(ctx, query, args...).Scan(&createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			if pgErr.Code == "23505" {
				switch {
				case strings.Contains(pgErr.ConstraintName, "email"):
					return ErrEmailExists
				case strings.Contains(pgErr.ConstraintName, "name") || strings.Contains(pgErr.ConstraintName, "username"):
					return ErrNameExists
				}
			}
		}
		return err
	}

	user.Name = normalizedName
	user.Email = normalizedEmail
	user.CreatedAt = createdAt.UTC()
	user.UpdatedAt = updatedAt.UTC()
	return nil
}

func (s *postgresStore) CreateAdminPlanGroupPreview(ctx context.Context, preview AdminPlanGroupPreview) ([]AdminPlanGroupChange, error) {
	if preview.TokenHash == "" || preview.RequestID == "" || len(preview.Changes) == 0 || !preview.ExpiresAt.After(time.Now()) {
		return nil, errors.New("invalid admin plan group preview")
	}
	caps, err := s.capabilities(ctx)
	if err != nil {
		return nil, err
	}
	if !caps.hasGroups || !caps.hasSubscriptionValidFrom || !caps.hasSubscriptionValidUntil {
		return nil, errors.New("admin plan group updates are not supported by the current user schema")
	}
	changes := cloneAdminPlanGroupChanges(preview.Changes)
	sort.Slice(changes, func(i, j int) bool { return changes[i].UserID < changes[j].UserID })
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	seen := make(map[string]struct{}, len(changes))
	for i := range changes {
		change := &changes[i]
		if _, duplicate := seen[change.UserID]; duplicate {
			return nil, errors.New("duplicate user in admin plan group preview")
		}
		seen[change.UserID] = struct{}{}
		var groupsRaw []byte
		var validFrom, validUntil sql.NullTime
		var role string
		var level int
		roleExpr, levelExpr := "'user'", fmt.Sprintf("%d", LevelUser)
		if caps.hasRole {
			roleExpr = "coalesce(role, 'user')"
		}
		if caps.hasLevel {
			levelExpr = fmt.Sprintf("coalesce(level, %d)", LevelUser)
		}
		query := fmt.Sprintf("SELECT groups, subscription_valid_from, subscription_valid_until, %s, %s FROM public.users WHERE uuid = $1 FOR UPDATE", roleExpr, levelExpr)
		err := tx.QueryRowContext(ctx, query, change.UserID).Scan(&groupsRaw, &validFrom, &validUntil, &role, &level)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		if err != nil {
			return nil, err
		}
		currentGroups := decodeStringSlice(groupsRaw)
		var currentFrom, currentUntil *time.Time
		if validFrom.Valid {
			value := validFrom.Time.UTC()
			currentFrom = &value
		}
		if validUntil.Valid {
			value := validUntil.Time.UTC()
			currentUntil = &value
		}
		if IsRootRole(role) {
			return nil, ErrUserProtected
		}
		if !equalStoreStrings(currentGroups, change.ExpectedGroups) || !equalStoreTimes(currentFrom, change.ExpectedValidFrom) || !equalStoreTimes(currentUntil, change.ExpectedValidUntil) {
			return nil, ErrAdminPlanGroupStale
		}
		change.ExpectedGroups = currentGroups
		change.ExpectedValidFrom, change.ExpectedValidUntil = currentFrom, currentUntil
		var profileUpdated sql.NullTime
		err = tx.QueryRowContext(ctx, `SELECT included_quota_bytes, updated_at FROM public.account_billing_profiles WHERE account_uuid = $1 FOR UPDATE`, change.UserID).Scan(&change.ExpectedProfileIncludedQuota, &profileUpdated)
		if errors.Is(err, sql.ErrNoRows) {
			change.ProfileExisted = false
		} else if err != nil {
			return nil, err
		} else {
			change.ProfileExisted = true
			value := profileUpdated.Time.UTC()
			change.ExpectedProfileUpdatedAt = &value
		}
		var periodStart, periodEnd, quotaUpdated sql.NullTime
		err = tx.QueryRowContext(ctx, `SELECT remaining_included_quota, period_start, period_end, updated_at FROM public.account_quota_states WHERE account_uuid = $1 FOR UPDATE`, change.UserID).Scan(&change.ExpectedQuotaRemaining, &periodStart, &periodEnd, &quotaUpdated)
		if errors.Is(err, sql.ErrNoRows) {
			change.QuotaExisted = false
		} else if err != nil {
			return nil, err
		} else {
			change.QuotaExisted = true
			value := quotaUpdated.Time.UTC()
			change.ExpectedQuotaUpdatedAt = &value
			if periodStart.Valid {
				value := periodStart.Time.UTC()
				change.ExpectedPeriodStart = &value
			}
			if periodEnd.Valid {
				value := periodEnd.Time.UTC()
				change.ExpectedPeriodEnd = &value
			}
		}
		if change.SetEntitlement {
			start := adminPlanGroupPeriodStart(change.ExpectedPeriodStart, time.Now().UTC())
			var usage int64
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(total_bytes), 0) FROM public.traffic_minute_buckets WHERE account_uuid = $1 AND bucket_start >= $2 AND ($3::timestamptz IS NULL OR bucket_start < $3)`, change.UserID, start, change.ExpectedPeriodEnd).Scan(&usage); err != nil {
				return nil, err
			}
			if usage < 0 {
				usage = 0
			}
			change.ExpectedUsageBytes = usage
			used := change.ExpectedProfileIncludedQuota - change.ExpectedQuotaRemaining
			if used < 0 {
				used = 0
			}
			if usage > used {
				used = usage
			}
			change.UsedBytesPreserved = used
			change.RemainingAfter = change.IncludedQuotaBytes - used
			if change.RemainingAfter < 0 {
				change.RemainingAfter = 0
			}
		}
	}
	details, err := json.Marshal(map[string]any{"request_id": preview.RequestID, "reason": preview.Reason, "preview_token_hash": preview.TokenHash,
		"expires_at": preview.ExpiresAt.UTC(), "changes": changes})
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO public.audit_logs (uuid, action, actor_uuid, details, created_at) VALUES ($1, $2, $3, $4, $5)`,
		uuid.NewString(), AuditActionPlanGroupPreview, preview.ActorUUID, details, time.Now().UTC()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return changes, nil
}

func (s *postgresStore) ApplyAdminPlanGroupBatch(ctx context.Context, batch AdminPlanGroupBatch) ([]AdminPlanGroupChange, bool, error) {
	if strings.TrimSpace(batch.RequestID) == "" || strings.TrimSpace(batch.PreviewTokenHash) == "" {
		return nil, false, errors.New("request id and preview token are required")
	}
	caps, err := s.capabilities(ctx)
	if err != nil {
		return nil, false, err
	}
	if !caps.hasGroups || !caps.hasSubscriptionValidFrom || !caps.hasSubscriptionValidUntil {
		return nil, false, errors.New("admin plan group updates are not supported by the current user schema")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "admin-plan-group:"+batch.RequestID); err != nil {
		return nil, false, err
	}
	var priorActor string
	var priorRaw []byte
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(actor_uuid::text, ''), details FROM public.audit_logs WHERE action = $1 AND details->>'request_id' = $2 ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, AuditActionPlanGroupUpdate, batch.RequestID).Scan(&priorActor, &priorRaw)
	if err == nil {
		var prior map[string]any
		if err := json.Unmarshal(priorRaw, &prior); err != nil {
			return nil, false, err
		}
		if priorActor != batch.ActorUUID || prior["reason"] != batch.Reason || prior["preview_token_hash"] != batch.PreviewTokenHash {
			return nil, false, ErrAdminPlanGroupReplay
		}
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return nil, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	var previewActor string
	var previewRaw []byte
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(actor_uuid::text, ''), details FROM public.audit_logs WHERE action = $1 AND details->>'request_id' = $2 AND details->>'preview_token_hash' = $3 ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, AuditActionPlanGroupPreview, batch.RequestID, batch.PreviewTokenHash).Scan(&previewActor, &previewRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrAdminPlanGroupPreview
	}
	if err != nil {
		return nil, false, err
	}
	var storedPreview struct {
		Reason    string                 `json:"reason"`
		TokenHash string                 `json:"preview_token_hash"`
		ExpiresAt time.Time              `json:"expires_at"`
		Changes   []AdminPlanGroupChange `json:"changes"`
	}
	if err := json.Unmarshal(previewRaw, &storedPreview); err != nil {
		return nil, false, err
	}
	if previewActor != batch.ActorUUID || storedPreview.Reason != batch.Reason || storedPreview.TokenHash != batch.PreviewTokenHash || !storedPreview.ExpiresAt.After(time.Now().UTC()) {
		return nil, false, ErrAdminPlanGroupPreview
	}
	var consumed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.audit_logs WHERE action = $1 AND details->>'preview_token_hash' = $2)`, AuditActionPlanGroupPreviewUsed, batch.PreviewTokenHash).Scan(&consumed); err != nil {
		return nil, false, err
	}
	if consumed {
		return nil, false, ErrAdminPlanGroupPreview
	}
	changes := storedPreview.Changes
	sort.Slice(changes, func(i, j int) bool { return changes[i].UserID < changes[j].UserID })
	if len(batch.ExpectedUserIDs) > 0 {
		expected := append([]string(nil), batch.ExpectedUserIDs...)
		sort.Strings(expected)
		if len(expected) != len(changes) {
			return nil, false, ErrAdminPlanGroupPreview
		}
		for i := range expected {
			if expected[i] != changes[i].UserID {
				return nil, false, ErrAdminPlanGroupPreview
			}
		}
	}
	type currentState struct {
		change                     AdminPlanGroupChange
		groups                     []string
		validFrom, validUntil      *time.Time
		profile                    AccountBillingProfile
		quota                      AccountQuotaState
		profileExists, quotaExists bool
	}
	states := make([]currentState, 0, len(changes))
	seen := make(map[string]struct{}, len(changes))
	for _, change := range changes {
		if _, ok := seen[change.UserID]; ok {
			return nil, false, errors.New("duplicate user in admin plan group preview")
		}
		seen[change.UserID] = struct{}{}
		var groupsRaw []byte
		var validFrom, validUntil sql.NullTime
		var role string
		var level int
		roleExpr, levelExpr := "'user'", fmt.Sprintf("%d", LevelUser)
		if caps.hasRole {
			roleExpr = "coalesce(role, 'user')"
		}
		if caps.hasLevel {
			levelExpr = fmt.Sprintf("coalesce(level, %d)", LevelUser)
		}
		query := fmt.Sprintf("SELECT groups, subscription_valid_from, subscription_valid_until, %s, %s FROM public.users WHERE uuid = $1 FOR UPDATE", roleExpr, levelExpr)
		err := tx.QueryRowContext(ctx, query, change.UserID).Scan(&groupsRaw, &validFrom, &validUntil, &role, &level)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, ErrUserNotFound
		}
		if err != nil {
			return nil, false, err
		}
		if IsRootRole(role) {
			return nil, false, ErrUserProtected
		}
		state := currentState{change: change, groups: decodeStringSlice(groupsRaw)}
		if validFrom.Valid {
			value := validFrom.Time.UTC()
			state.validFrom = &value
		}
		if validUntil.Valid {
			value := validUntil.Time.UTC()
			state.validUntil = &value
		}
		if !equalStoreStrings(state.groups, change.ExpectedGroups) || !equalStoreTimes(state.validFrom, change.ExpectedValidFrom) || !equalStoreTimes(state.validUntil, change.ExpectedValidUntil) {
			return nil, false, ErrAdminPlanGroupStale
		}
		var pUpdated sql.NullTime
		err = tx.QueryRowContext(ctx, `SELECT package_name, included_quota_bytes, base_price_per_byte, region_multiplier, line_multiplier, peak_multiplier, offpeak_multiplier, pricing_rule_version, updated_at FROM public.account_billing_profiles WHERE account_uuid = $1 FOR UPDATE`, change.UserID).Scan(&state.profile.PackageName, &state.profile.IncludedQuotaBytes, &state.profile.BasePricePerByte, &state.profile.RegionMultiplier, &state.profile.LineMultiplier, &state.profile.PeakMultiplier, &state.profile.OffPeakMultiplier, &state.profile.PricingRuleVersion, &pUpdated)
		if errors.Is(err, sql.ErrNoRows) {
			state.profileExists = false
		} else if err != nil {
			return nil, false, err
		} else {
			state.profileExists = true
			state.profile.AccountUUID = change.UserID
			state.profile.UpdatedAt = pUpdated.Time.UTC()
		}
		var qUpdated, qStart, qEnd sql.NullTime
		err = tx.QueryRowContext(ctx, `SELECT remaining_included_quota, period_start, period_end, updated_at FROM public.account_quota_states WHERE account_uuid = $1 FOR UPDATE`, change.UserID).Scan(&state.quota.RemainingIncludedQuota, &qStart, &qEnd, &qUpdated)
		if errors.Is(err, sql.ErrNoRows) {
			state.quotaExists = false
		} else if err != nil {
			return nil, false, err
		} else {
			state.quotaExists = true
			state.quota.AccountUUID = change.UserID
			state.quota.UpdatedAt = qUpdated.Time.UTC()
			if qStart.Valid {
				value := qStart.Time.UTC()
				state.quota.PeriodStart = &value
			}
			if qEnd.Valid {
				value := qEnd.Time.UTC()
				state.quota.PeriodEnd = &value
			}
		}
		if change.SetEntitlement && (state.profileExists != change.ProfileExisted || state.quotaExists != change.QuotaExisted || state.profile.IncludedQuotaBytes != change.ExpectedProfileIncludedQuota || state.quota.RemainingIncludedQuota != change.ExpectedQuotaRemaining ||
			!equalStoreTimes(nullableTimePointer(pUpdated), change.ExpectedProfileUpdatedAt) || !equalStoreTimes(nullableTimePointer(qUpdated), change.ExpectedQuotaUpdatedAt) || !equalStoreTimes(state.quota.PeriodStart, change.ExpectedPeriodStart) || !equalStoreTimes(state.quota.PeriodEnd, change.ExpectedPeriodEnd)) {
			return nil, false, ErrAdminPlanGroupStale
		}
		if change.SetEntitlement {
			start := adminPlanGroupPeriodStart(state.quota.PeriodStart, time.Now().UTC())
			var usage int64
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(total_bytes), 0) FROM public.traffic_minute_buckets WHERE account_uuid = $1 AND bucket_start >= $2 AND ($3::timestamptz IS NULL OR bucket_start < $3)`, change.UserID, start, state.quota.PeriodEnd).Scan(&usage); err != nil {
				return nil, false, err
			}
			if usage != change.ExpectedUsageBytes {
				return nil, false, ErrAdminPlanGroupStale
			}
		}
		states = append(states, state)
	}
	now := time.Now().UTC()
	for _, state := range states {
		change := state.change
		groups, err := encodeStringSlice(change.Groups)
		if err != nil {
			return nil, false, err
		}
		validFrom, validUntil := state.validFrom, state.validUntil
		if change.SetValidFrom {
			validFrom = change.ValidFrom
		}
		if change.SetValidUntil {
			validUntil = change.ValidUntil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE public.users SET groups = $1, subscription_valid_from = $2, subscription_valid_until = $3, updated_at = $4 WHERE uuid = $5`, groups, validFrom, validUntil, now, change.UserID); err != nil {
			return nil, false, err
		}
		used := int64(0)
		remaining := int64(0)
		if change.SetEntitlement {
			used = state.profile.IncludedQuotaBytes - state.quota.RemainingIncludedQuota
			if used < 0 {
				used = 0
			}
			if change.ExpectedUsageBytes > used {
				used = change.ExpectedUsageBytes
			}
			remaining = change.IncludedQuotaBytes - used
			if remaining < 0 {
				remaining = 0
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO public.account_billing_profiles (account_uuid, package_name, included_quota_bytes, base_price_per_byte, region_multiplier, line_multiplier, peak_multiplier, offpeak_multiplier, pricing_rule_version) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (account_uuid) DO UPDATE SET package_name=EXCLUDED.package_name, included_quota_bytes=EXCLUDED.included_quota_bytes, region_multiplier=EXCLUDED.region_multiplier, line_multiplier=EXCLUDED.line_multiplier, peak_multiplier=EXCLUDED.peak_multiplier, offpeak_multiplier=EXCLUDED.offpeak_multiplier, pricing_rule_version=EXCLUDED.pricing_rule_version, updated_at=now()`, change.UserID, change.PackageName, change.IncludedQuotaBytes, state.profile.BasePricePerByte, change.RegionMultiplier, change.LineMultiplier, change.PeakMultiplier, change.OffPeakMultiplier, change.PricingRuleVersion); err != nil {
				return nil, false, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO public.account_quota_states (account_uuid, remaining_included_quota, effective_at) VALUES ($1,$2,$3) ON CONFLICT (account_uuid) DO UPDATE SET remaining_included_quota=EXCLUDED.remaining_included_quota, effective_at=EXCLUDED.effective_at, updated_at=now()`, change.UserID, remaining, now); err != nil {
				return nil, false, err
			}
		}
		before, _ := json.Marshal(map[string]any{"groups": state.groups, "valid_from": state.validFrom, "valid_until": state.validUntil, "included_quota_bytes": state.profile.IncludedQuotaBytes, "remaining_included_quota": state.quota.RemainingIncludedQuota})
		after, _ := json.Marshal(map[string]any{"groups": change.Groups, "valid_from": validFrom, "valid_until": validUntil, "plan_id": change.PlanID, "included_quota_bytes": change.IncludedQuotaBytes, "used_bytes_preserved": used, "remaining_included_quota": remaining, "configuration_sync_paused": change.IncludedQuotaBytes > 0 && remaining == 0})
		details, _ := json.Marshal(map[string]any{"target_uuid": change.UserID, "reason": batch.Reason, "request_id": batch.RequestID, "preview_token_hash": batch.PreviewTokenHash, "before": json.RawMessage(before), "after": json.RawMessage(after)})
		if _, err := tx.ExecContext(ctx, `INSERT INTO public.audit_logs (uuid, action, actor_uuid, details, created_at) VALUES ($1,$2,$3,$4,$5)`, uuid.NewString(), AuditActionPlanGroupUpdate, batch.ActorUUID, details, now); err != nil {
			return nil, false, err
		}
	}
	usedDetails, _ := json.Marshal(map[string]any{"request_id": batch.RequestID, "preview_token_hash": batch.PreviewTokenHash})
	if _, err := tx.ExecContext(ctx, `INSERT INTO public.audit_logs (uuid, action, actor_uuid, details, created_at) VALUES ($1,$2,$3,$4,$5)`, uuid.NewString(), AuditActionPlanGroupPreviewUsed, batch.ActorUUID, usedDetails, now); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return changes, false, nil
}

func adminPlanGroupPeriodStart(start *time.Time, now time.Time) time.Time {
	if start != nil {
		return start.UTC()
	}
	return time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
}

func nullableTimePointer(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	result := value.Time.UTC()
	return &result
}

func (s *postgresStore) CountSuperAdmins(ctx context.Context) (int, error) {
	if !s.allowSuperAdminCounting {
		return 0, ErrSuperAdminCountingDisabled
	}
	caps, err := s.capabilities(ctx)
	if err != nil {
		return 0, err
	}

	roleClauses := make([]string, 0, 2)
	if caps.hasRole {
		roleClauses = append(roleClauses, "lower(role) IN ('root','admin')")
	}
	if caps.hasLevel {
		roleClauses = append(roleClauses, fmt.Sprintf("level = %d", LevelAdmin))
	}
	if len(roleClauses) == 0 {
		return 0, errors.New("postgres store schema does not expose role or level columns")
	}

	conditions := []string{fmt.Sprintf("(%s)", strings.Join(roleClauses, " OR "))}

	if caps.hasGroups {
		conditions = append(conditions, "groups @> '[\"Admin\"]'::jsonb")
	}
	if caps.hasPermissions {
		conditions = append(conditions, "permissions @> '[\"*\"]'::jsonb")
	}

	query := fmt.Sprintf("SELECT COUNT(*) FROM users WHERE %s", strings.Join(conditions, " AND "))

	var count int
	if err := s.db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// UpsertSubscription creates or updates a subscription row.
func (s *postgresStore) UpsertSubscription(ctx context.Context, subscription *Subscription) error {
	if subscription == nil {
		return errors.New("subscription is required")
	}

	normalizedUserID := strings.TrimSpace(subscription.UserID)
	if normalizedUserID == "" {
		return ErrUserNotFound
	}

	externalID := strings.TrimSpace(subscription.ExternalID)
	if externalID == "" {
		return errors.New("external id is required")
	}
	if strings.TrimSpace(subscription.PaymentMethod) == "" {
		subscription.PaymentMethod = strings.TrimSpace(subscription.Provider)
	}
	subscription.PaymentQRCode = strings.TrimSpace(subscription.PaymentQRCode)

	encodedMeta, err := json.Marshal(subscription.Meta)
	if err != nil {
		return err
	}

	var cancelledAt any
	if subscription.CancelledAt != nil {
		cancelledAt = subscription.CancelledAt.UTC()
	}

	const query = `INSERT INTO subscriptions (user_uuid, provider, payment_method, kind, plan_id, external_id, status, payment_qr, meta, cancelled_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, COALESCE($9, '{}'::jsonb), $10)
ON CONFLICT (user_uuid, external_id) DO UPDATE SET
  provider = EXCLUDED.provider,
  payment_method = EXCLUDED.payment_method,
  kind = EXCLUDED.kind,
  plan_id = EXCLUDED.plan_id,
  status = EXCLUDED.status,
  payment_qr = EXCLUDED.payment_qr,
  meta = EXCLUDED.meta,
  cancelled_at = EXCLUDED.cancelled_at,
  updated_at = now()
RETURNING uuid, created_at, updated_at, cancelled_at`

	var (
		idValue   any
		createdAt time.Time
		updatedAt time.Time
		cancelled sql.NullTime
	)

	err = s.db.QueryRowContext(
		ctx,
		query,
		normalizedUserID,
		strings.TrimSpace(subscription.Provider),
		strings.TrimSpace(subscription.PaymentMethod),
		strings.TrimSpace(subscription.Kind),
		strings.TrimSpace(subscription.PlanID),
		externalID,
		strings.TrimSpace(subscription.Status),
		strings.TrimSpace(subscription.PaymentQRCode),
		encodedMeta,
		cancelledAt,
	).Scan(&idValue, &createdAt, &updatedAt, &cancelled)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSubscriptionNotFound
		}
		return err
	}

	identifier, err := formatIdentifier(idValue)
	if err != nil {
		return err
	}

	subscription.ID = identifier
	subscription.UserID = normalizedUserID
	subscription.ExternalID = externalID
	subscription.CreatedAt = createdAt.UTC()
	subscription.UpdatedAt = updatedAt.UTC()
	subscription.Meta, _ = decodeSubscriptionMeta(encodedMeta)
	if cancelled.Valid {
		subscription.CancelledAt = &cancelled.Time
	}

	return nil
}

// ListSubscriptionsByUser returns all subscriptions for a user ordered by recency.
func (s *postgresStore) ListSubscriptionsByUser(ctx context.Context, userID string) ([]Subscription, error) {
	normalizedUserID := strings.TrimSpace(userID)
	if normalizedUserID == "" {
		return nil, ErrUserNotFound
	}

	const query = `SELECT uuid, user_uuid, provider, payment_method, kind, plan_id, external_id, status, payment_qr, meta, created_at, updated_at, cancelled_at
FROM subscriptions WHERE user_uuid = $1 ORDER BY created_at DESC`

	rows, err := s.db.QueryContext(ctx, query, normalizedUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var subs []Subscription
	for rows.Next() {
		var (
			idValue       any
			provider      string
			paymentMethod string
			kind          string
			planID        sql.NullString
			externalID    string
			status        string
			paymentQR     sql.NullString
			metaBytes     []byte
			createdAt     time.Time
			updatedAt     time.Time
			cancelled     sql.NullTime
		)
		if err := rows.Scan(&idValue, &normalizedUserID, &provider, &paymentMethod, &kind, &planID, &externalID, &status, &paymentQR, &metaBytes, &createdAt, &updatedAt, &cancelled); err != nil {
			return nil, err
		}

		identifier, err := formatIdentifier(idValue)
		if err != nil {
			return nil, err
		}

		meta, err := decodeSubscriptionMeta(metaBytes)
		if err != nil {
			return nil, err
		}

		sub := Subscription{
			ID:            identifier,
			UserID:        userID,
			Provider:      provider,
			PaymentMethod: paymentMethod,
			PaymentQRCode: paymentQR.String,
			Kind:          kind,
			PlanID:        planID.String,
			ExternalID:    externalID,
			Status:        status,
			Meta:          meta,
			CreatedAt:     createdAt.UTC(),
			UpdatedAt:     updatedAt.UTC(),
		}
		if cancelled.Valid {
			sub.CancelledAt = &cancelled.Time
		}

		subs = append(subs, sub)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return subs, nil
}

// CancelSubscription marks the subscription as cancelled.
func (s *postgresStore) CancelSubscription(ctx context.Context, userID, externalID string, cancelledAt time.Time) (*Subscription, error) {
	normalizedUserID := strings.TrimSpace(userID)
	if normalizedUserID == "" {
		return nil, ErrUserNotFound
	}

	key := strings.TrimSpace(externalID)
	if key == "" {
		return nil, ErrSubscriptionNotFound
	}

	const query = `UPDATE subscriptions
SET status = 'cancelled', cancelled_at = $3, updated_at = now()
WHERE user_uuid = $1 AND external_id = $2
RETURNING uuid, provider, payment_method, kind, plan_id, status, payment_qr, meta, created_at, updated_at, cancelled_at`

	var (
		idValue       any
		provider      string
		paymentMethod string
		kind          string
		planID        sql.NullString
		status        string
		paymentQR     sql.NullString
		metaBytes     []byte
		createdAt     time.Time
		updatedAt     time.Time
		cancelled     sql.NullTime
	)

	err := s.db.QueryRowContext(ctx, query, normalizedUserID, key, cancelledAt.UTC()).Scan(
		&idValue,
		&provider,
		&paymentMethod,
		&kind,
		&planID,
		&status,
		&paymentQR,
		&metaBytes,
		&createdAt,
		&updatedAt,
		&cancelled,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSubscriptionNotFound
		}
		return nil, err
	}

	identifier, err := formatIdentifier(idValue)
	if err != nil {
		return nil, err
	}

	meta, err := decodeSubscriptionMeta(metaBytes)
	if err != nil {
		return nil, err
	}

	sub := &Subscription{
		ID:            identifier,
		UserID:        normalizedUserID,
		Provider:      provider,
		PaymentMethod: paymentMethod,
		PaymentQRCode: paymentQR.String,
		Kind:          kind,
		PlanID:        planID.String,
		ExternalID:    key,
		Status:        status,
		Meta:          meta,
		CreatedAt:     createdAt.UTC(),
		UpdatedAt:     updatedAt.UTC(),
	}
	if cancelled.Valid {
		sub.CancelledAt = &cancelled.Time
	}

	return sub, nil
}

func decodeSubscriptionMeta(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, err
	}
	if meta == nil {
		meta = map[string]any{}
	}
	return meta, nil
}

func nullForEmpty(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func toUTCTime(value sql.NullTime) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return value.Time.UTC()
}

func formatIdentifier(value any) (string, error) {
	switch v := value.(type) {
	case nil:
		return "", errors.New("user id is nil")
	case string:
		return v, nil
	case []byte:
		return string(v), nil
	case [16]byte:
		id := uuid.UUID(v)
		return id.String(), nil
	case *[16]byte:
		if v == nil {
			return "", errors.New("user id is nil")
		}
		id := uuid.UUID(*v)
		return id.String(), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case int32:
		return strconv.FormatInt(int64(v), 10), nil
	case int:
		return strconv.FormatInt(int64(v), 10), nil
	case uint64:
		return strconv.FormatUint(v, 10), nil
	case uint32:
		return strconv.FormatUint(uint64(v), 10), nil
	case pgtype.UUID:
		if !v.Valid {
			return "", errors.New("user id is nil")
		}
		return v.String(), nil
	case *pgtype.UUID:
		if v == nil || !v.Valid {
			return "", errors.New("user id is nil")
		}
		return v.String(), nil
	case fmt.Stringer:
		return v.String(), nil
	default:
		return "", fmt.Errorf("unsupported identifier type %T", value)
	}
}

func (s *postgresStore) capabilities(ctx context.Context) (schemaCapabilities, error) {
	s.capsMu.RLock()
	if s.capsLoaded {
		caps := s.caps
		s.capsMu.RUnlock()
		return caps, nil
	}
	s.capsMu.RUnlock()

	s.capsMu.Lock()
	defer s.capsMu.Unlock()
	if s.capsLoaded {
		return s.caps, nil
	}

	query := `SELECT
  EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'users'
      AND table_schema = ANY (current_schemas(false))
      AND column_name = 'mfa_totp_secret'
  ) AS has_mfa_totp_secret,
  EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'users'
      AND table_schema = ANY (current_schemas(false))
      AND column_name = 'mfa_enabled'
  ) AS has_mfa_enabled,
  EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'users'
      AND table_schema = ANY (current_schemas(false))
      AND column_name = 'mfa_secret_issued_at'
  ) AS has_mfa_secret_issued_at,
  EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'users'
      AND table_schema = ANY (current_schemas(false))
      AND column_name = 'mfa_confirmed_at'
  ) AS has_mfa_confirmed_at,
  EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'users'
      AND table_schema = ANY (current_schemas(false))
      AND column_name = 'created_at'
  ) AS has_created_at,
  EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'users'
      AND table_schema = ANY (current_schemas(false))
      AND column_name = 'updated_at'
  ) AS has_updated_at,
  EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'users'
      AND table_schema = ANY (current_schemas(false))
      AND column_name = 'level'
  ) AS has_level,
  EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'users'
      AND table_schema = ANY (current_schemas(false))
      AND column_name = 'role'
  ) AS has_role,
  EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'users'
      AND table_schema = ANY (current_schemas(false))
      AND column_name = 'groups'
  ) AS has_groups,
  EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'users'
      AND table_schema = ANY (current_schemas(false))
      AND column_name = 'permissions'
  ) AS has_permissions,
  EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'users'
      AND table_schema = ANY (current_schemas(false))
      AND column_name = 'active'
  ) AS has_active,
  EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'users'
      AND table_schema = ANY (current_schemas(false))
      AND column_name = 'proxy_uuid'
  ) AS has_proxy_uuid,
  EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'users'
      AND table_schema = ANY (current_schemas(false))
      AND column_name = 'proxy_uuid_expires_at'
  ) AS has_proxy_uuid_expires_at,
  EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'users'
      AND table_schema = ANY (current_schemas(false))
      AND column_name = 'subscription_valid_from'
  ) AS has_subscription_valid_from,
  EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'users'
      AND table_schema = ANY (current_schemas(false))
      AND column_name = 'subscription_valid_until'
  ) AS has_subscription_valid_until,
  EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'users'
      AND table_schema = ANY (current_schemas(false))
      AND column_name = 'last_active_at'
  ) AS has_last_active_at,
  EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'users'
      AND table_schema = ANY (current_schemas(false))
      AND column_name = 'archived_at'
  ) AS has_archived_at`

	row := s.db.QueryRowContext(ctx, query)
	var caps schemaCapabilities
	if err := row.Scan(
		&caps.hasMFATOTPSecret,
		&caps.hasMFAEnabled,
		&caps.hasMFASecretIssuedAt,
		&caps.hasMFAConfirmedAt,
		&caps.hasCreatedAt,
		&caps.hasUpdatedAt,
		&caps.hasLevel,
		&caps.hasRole,
		&caps.hasGroups,
		&caps.hasPermissions,
		&caps.hasActive,
		&caps.hasProxyUUID,
		&caps.hasProxyUUIDExpiresAt,
		&caps.hasSubscriptionValidFrom,
		&caps.hasSubscriptionValidUntil,
		&caps.hasLastActiveAt,
		&caps.hasArchivedAt,
	); err != nil {
		return schemaCapabilities{}, err
	}

	s.caps = caps
	s.capsLoaded = true
	return caps, nil
}

func (s *postgresStore) selectUserQuery(caps schemaCapabilities, whereClause string) string {
	secretExpr := "NULL::text"
	if caps.hasMFATOTPSecret {
		secretExpr = "mfa_totp_secret"
	}

	enabledExpr := "false"
	if caps.hasMFAEnabled {
		enabledExpr = "coalesce(mfa_enabled, false)"
	}

	issuedExpr := "NULL::timestamptz"
	if caps.hasMFASecretIssuedAt {
		issuedExpr = "mfa_secret_issued_at"
	}

	confirmedExpr := "NULL::timestamptz"
	if caps.hasMFAConfirmedAt {
		confirmedExpr = "mfa_confirmed_at"
	}

	createdExpr := "now()"
	if caps.hasCreatedAt {
		createdExpr = "coalesce(created_at, now())"
	}

	updatedExpr := "now()"
	if caps.hasUpdatedAt {
		updatedExpr = "coalesce(updated_at, now())"
	}

	levelExpr := fmt.Sprintf("%d", LevelUser)
	if caps.hasLevel {
		levelExpr = fmt.Sprintf("coalesce(level, %d)", LevelUser)
	}

	roleExpr := fmt.Sprintf("'%s'", RoleUser)
	if caps.hasRole {
		roleExpr = fmt.Sprintf("coalesce(role, '%s')", RoleUser)
	}

	groupsExpr := "'[]'::jsonb"
	if caps.hasGroups {
		groupsExpr = "coalesce(groups, '[]'::jsonb)"
	}

	permissionsExpr := "'[]'::jsonb"
	if caps.hasPermissions {
		permissionsExpr = "coalesce(permissions, '[]'::jsonb)"
	}

	activeExpr := "true"
	if caps.hasActive {
		activeExpr = "coalesce(active, true)"
	}

	proxyUUIDExpr := "NULL::uuid"
	if caps.hasProxyUUID {
		proxyUUIDExpr = "proxy_uuid"
	}

	proxyExpiresAtExpr := "NULL::timestamptz"
	if caps.hasProxyUUIDExpiresAt {
		proxyExpiresAtExpr = "proxy_uuid_expires_at"
	}

	subscriptionValidFromExpr := "NULL::timestamptz"
	if caps.hasSubscriptionValidFrom {
		subscriptionValidFromExpr = "subscription_valid_from"
	}

	subscriptionValidUntilExpr := "NULL::timestamptz"
	if caps.hasSubscriptionValidUntil {
		subscriptionValidUntilExpr = "subscription_valid_until"
	}

	lastActiveAtExpr := "NULL::timestamptz"
	if caps.hasLastActiveAt {
		lastActiveAtExpr = "last_active_at"
	}

	archivedAtExpr := "NULL::timestamptz"
	if caps.hasArchivedAt {
		archivedAtExpr = "archived_at"
	}

	return fmt.Sprintf(`SELECT uuid, username, email, (email_verified_at IS NOT NULL) AS email_verified, password, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s FROM users %s`,
		secretExpr, enabledExpr, issuedExpr, confirmedExpr, createdExpr, updatedExpr, levelExpr, roleExpr, groupsExpr, permissionsExpr, activeExpr, proxyUUIDExpr, proxyExpiresAtExpr, subscriptionValidFromExpr, subscriptionValidUntilExpr, lastActiveAtExpr, archivedAtExpr, whereClause)
}

func encodeStringSlice(values []string) ([]byte, error) {
	normalized := normalizeStringSlice(values)
	if len(normalized) == 0 {
		return []byte("[]"), nil
	}
	return json.Marshal(normalized)
}

func decodeStringSlice(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil
	}
	return normalizeStringSlice(values)
}
func (s *postgresStore) CreateIdentity(ctx context.Context, identity *Identity) error {
	if identity == nil {
		return errors.New("identity is required")
	}

	normalizedUserID := strings.TrimSpace(identity.UserID)
	if normalizedUserID == "" {
		return ErrUserNotFound
	}

	provider := strings.TrimSpace(identity.Provider)
	externalID := strings.TrimSpace(identity.ExternalID)
	if provider == "" || externalID == "" {
		return errors.New("provider and external_id are required")
	}

	const query = `INSERT INTO identities (user_uuid, provider, external_id)
VALUES ($1, $2, $3)
RETURNING uuid, created_at, updated_at`

	var (
		idValue   any
		createdAt time.Time
		updatedAt time.Time
	)

	err := s.db.QueryRowContext(ctx, query, normalizedUserID, provider, externalID).Scan(&idValue, &createdAt, &updatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			if pgErr.Code == "23505" { // unique_violation
				return errors.New("identity already exists")
			}
		}
		return err
	}

	identifier, err := formatIdentifier(idValue)
	if err != nil {
		return err
	}

	identity.ID = identifier
	identity.CreatedAt = createdAt.UTC()
	identity.UpdatedAt = updatedAt.UTC()

	return nil
}

// ListUsers returns all users from the postgres store.
func (s *postgresStore) ListUsers(ctx context.Context) ([]User, error) {
	caps, err := s.capabilities(ctx)
	if err != nil {
		return nil, err
	}

	query := s.selectUserQuery(caps, "ORDER BY created_at ASC")

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, *user)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return users, nil
}

func (s *postgresStore) ListUsersPage(ctx context.Context, afterID string, limit int) ([]User, error) {
	if limit <= 0 || limit > MaxUserListPageSize {
		return nil, errors.New("user page limit is out of range")
	}
	caps, err := s.capabilities(ctx)
	if err != nil {
		return nil, err
	}
	query := s.selectUserQuery(caps, "ORDER BY uuid ASC LIMIT $1")
	args := []any{limit}
	if strings.TrimSpace(afterID) != "" {
		query = s.selectUserQuery(caps, "WHERE uuid > $1 ORDER BY uuid ASC LIMIT $2")
		args = []any{strings.TrimSpace(afterID), limit}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := make([]User, 0, limit)
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, *user)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return users, nil
}

func (s *postgresStore) DeleteUser(ctx context.Context, id string, audit *AuditLog, requestID string) error {
	if err := validateUserArchiveAudit(id, audit); err != nil {
		return err
	}
	requestID = strings.TrimSpace(requestID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var schemaReady bool
	if err := tx.QueryRowContext(ctx, `
		SELECT
		  EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'active')
		  AND EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'archived_at')
		  AND EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'updated_at')
		  AND (SELECT COUNT(*) = 6 FROM information_schema.columns
		       WHERE table_schema = 'public' AND table_name = 'users'
		         AND column_name IN ('account_lifecycle_state', 'account_lifecycle_changed_at',
		           'account_lifecycle_actor_type', 'account_lifecycle_actor_ref',
		           'account_lifecycle_reason', 'account_lifecycle_transition_id'))
		  AND to_regclass('public.account_lifecycle_events') IS NOT NULL
		  AND to_regclass('public.audit_logs') IS NOT NULL
		  AND to_regclass('public.subscriptions') IS NOT NULL`).Scan(&schemaReady); err != nil {
		return err
	}
	if !schemaReady {
		return ErrUserArchiveUnsupported
	}

	var lifecycleState, role string
	var level int
	var groupsRaw []byte
	var archivedAt sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT account_lifecycle_state, role, level, groups, archived_at
		FROM public.users WHERE uuid = $1 FOR UPDATE`, id).
		Scan(&lifecycleState, &role, &level, &groupsRaw, &archivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUserNotFound
	}
	if err != nil {
		return err
	}
	if requestID != "" {
		var existingActor, existingReason, existingAuditActor string
		var existingTransitionID, existingAuditID string
		var eventMetadata, auditMetadata []byte
		var existingAt time.Time
		err := tx.QueryRowContext(ctx, `
			SELECT e.transition_id::text, e.actor_ref, e.reason, e.metadata,
			       a.uuid, COALESCE(a.actor_uuid::text, ''), a.details, a.created_at
			FROM public.account_lifecycle_events e
		JOIN public.audit_logs a
		  ON a.action = $3
		 AND a.details->>'transition_id' = e.transition_id::text
			WHERE e.user_uuid = $1 AND e.request_id = $2`,
			id, requestID, AuditActionUserArchive).
			Scan(&existingTransitionID, &existingActor, &existingReason, &eventMetadata,
				&existingAuditID, &existingAuditActor, &auditMetadata, &existingAt)
		if err == nil {
			if existingActor != audit.ActorUUID || existingAuditActor != audit.ActorUUID || existingReason != audit.Details["reason"] {
				return ErrUserArchiveReplayConflict
			}
			var details map[string]any
			if err := json.Unmarshal(auditMetadata, &details); err != nil {
				return fmt.Errorf("decode user archive replay audit: %w", err)
			}
			var eventDetails map[string]any
			if err := json.Unmarshal(eventMetadata, &eventDetails); err != nil {
				return fmt.Errorf("decode user archive replay event: %w", err)
			}
			if details["target_uuid"] != id || details["transition_id"] != existingTransitionID ||
				eventDetails["target_uuid"] != id || eventDetails["transition_id"] != existingTransitionID {
				return errors.New("user archive replay audit does not match its lifecycle event")
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			audit.UUID = existingAuditID
			audit.CreatedAt = existingAt
			audit.Details = details
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	if lifecycleState != "active" || archivedAt.Valid {
		return ErrUserAlreadyArchived
	}
	groups := decodeStringSlice(groupsRaw)
	if IsAdminRole(role) || level == LevelAdmin ||
		MonthlyQuotaGroup(&User{Groups: groups}) == MonthlyPlusQuotaLimitGroup ||
		MonthlyQuotaGroup(&User{Groups: groups}) == MonthlyUnlimitedBetaQuotaGroup {
		return ErrUserProtected
	}
	var paidSubscription bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM public.subscriptions
		  WHERE user_uuid = $1 AND status IN ('active', 'trialing', 'past_due')
		)`, id).Scan(&paidSubscription); err != nil {
		return err
	}
	if paidSubscription {
		return ErrUserProtected
	}

	now := time.Now().UTC()
	transitionID := uuid.NewString()
	auditEntry := cloneAuditLog(audit)
	if strings.TrimSpace(auditEntry.UUID) == "" {
		auditEntry.UUID = uuid.NewString()
	}
	requestID = strings.TrimSpace(requestID)
	var requestIDValue any
	if requestID != "" {
		requestIDValue = requestID
	}
	auditEntry.Details["transition_id"] = transitionID
	auditEntry.Details["request_id"] = requestID
	auditEntry.Details["occurred_at"] = now
	metadata, err := json.Marshal(auditEntry.Details)
	if err != nil {
		return fmt.Errorf("encode user archive metadata: %w", err)
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE public.users
		SET active = FALSE,
		    archived_at = COALESCE(archived_at, $1),
		    updated_at = $1,
		    account_lifecycle_state = 'archived',
		    account_lifecycle_changed_at = $1,
		    account_lifecycle_actor_type = 'admin',
		    account_lifecycle_actor_ref = $2,
		    account_lifecycle_reason = $3,
		    account_lifecycle_transition_id = $4
		WHERE uuid = $5 AND account_lifecycle_state = 'active'`,
		now, auditEntry.ActorUUID, auditEntry.Details["reason"], transitionID, id)
	if err != nil {
		return err
	}
	rowsUpdated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsUpdated != 1 {
		return ErrUserAlreadyArchived
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO public.account_lifecycle_events
		  (transition_id, user_uuid, from_state, to_state, actor_type, actor_ref, reason, request_id, metadata, occurred_at)
		VALUES ($1, $2, $3, 'archived', 'admin', $4, $5, $6, $7, $8)`,
		transitionID, id, lifecycleState, auditEntry.ActorUUID, auditEntry.Details["reason"], requestIDValue, metadata, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO public.audit_logs (uuid, action, actor_uuid, details, created_at)
		VALUES ($1, $2, $3, $4, $5)`,
		auditEntry.UUID, auditEntry.Action, auditEntry.ActorUUID, metadata, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	audit.UUID = auditEntry.UUID
	audit.CreatedAt = now
	audit.Details = auditEntry.Details
	return nil
}

func (s *postgresStore) AddToBlacklist(ctx context.Context, email string) error {
	const query = "INSERT INTO email_blacklist (email) VALUES ($1) ON CONFLICT (email) DO NOTHING"
	_, err := s.db.ExecContext(ctx, query, strings.ToLower(email))
	return err
}

func (s *postgresStore) RemoveFromBlacklist(ctx context.Context, email string) error {
	const query = "DELETE FROM email_blacklist WHERE email = $1"
	_, err := s.db.ExecContext(ctx, query, strings.ToLower(email))
	return err
}

func (s *postgresStore) IsBlacklisted(ctx context.Context, email string) (bool, error) {
	const query = "SELECT EXISTS(SELECT 1 FROM email_blacklist WHERE email = $1)"
	var exists bool
	err := s.db.QueryRowContext(ctx, query, strings.ToLower(email)).Scan(&exists)
	return exists, err
}

func (s *postgresStore) ListBlacklist(ctx context.Context) ([]string, error) {
	const query = "SELECT email FROM email_blacklist ORDER BY created_at DESC"
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var emails []string
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, err
		}
		emails = append(emails, email)
	}
	return emails, nil
}

func (s *postgresStore) UpsertAgent(ctx context.Context, agent *Agent) error {
	groups, err := encodeStringSlice(agent.Groups)
	if err != nil {
		return err
	}

	const query = `
		INSERT INTO agents (id, name, groups, healthy, last_heartbeat, clients_count, sync_revision, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			groups = EXCLUDED.groups,
			healthy = EXCLUDED.healthy,
			last_heartbeat = EXCLUDED.last_heartbeat,
			clients_count = EXCLUDED.clients_count,
			sync_revision = EXCLUDED.sync_revision,
			updated_at = now()
		RETURNING created_at, updated_at`

	return s.db.QueryRowContext(ctx, query,
		agent.ID,
		agent.Name,
		groups,
		agent.Healthy,
		agent.LastHeartbeat,
		agent.ClientsCount,
		agent.SyncRevision,
	).Scan(&agent.CreatedAt, &agent.UpdatedAt)
}

func (s *postgresStore) GetAgent(ctx context.Context, id string) (*Agent, error) {
	const query = `SELECT id, name, groups, healthy, last_heartbeat, clients_count, sync_revision, created_at, updated_at FROM agents WHERE id = $1`
	var a Agent
	var groups []byte
	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&a.ID, &a.Name, &groups, &a.Healthy, &a.LastHeartbeat, &a.ClientsCount, &a.SyncRevision, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("agent not found")
		}
		return nil, err
	}
	a.Groups = decodeStringSlice(groups)
	return &a, nil
}

func (s *postgresStore) ListAgents(ctx context.Context) ([]*Agent, error) {
	const query = `SELECT id, name, groups, healthy, last_heartbeat, clients_count, sync_revision, created_at, updated_at FROM agents ORDER BY id ASC`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*Agent
	for rows.Next() {
		var a Agent
		var groups []byte
		if err := rows.Scan(
			&a.ID, &a.Name, &groups, &a.Healthy, &a.LastHeartbeat, &a.ClientsCount, &a.SyncRevision, &a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			return nil, err
		}
		a.Groups = decodeStringSlice(groups)
		results = append(results, &a)
	}
	return results, rows.Err()
}

func (s *postgresStore) DeleteAgent(ctx context.Context, id string) error {
	const query = `DELETE FROM agents WHERE id = $1`
	_, err := s.db.ExecContext(ctx, query, id)
	return err
}

func (s *postgresStore) DeleteStaleAgents(ctx context.Context, staleThreshold time.Duration) (int, error) {
	cutoff := time.Now().Add(-staleThreshold)
	const query = `DELETE FROM agents WHERE last_heartbeat < $1 OR last_heartbeat IS NULL`
	result, err := s.db.ExecContext(ctx, query, cutoff)
	if err != nil {
		return 0, err
	}
	count, _ := result.RowsAffected()
	return int(count), nil
}

func (s *postgresStore) CreateSession(ctx context.Context, token, userID string, expiresAt time.Time) error {
	const query = "INSERT INTO sessions (token, user_uuid, expires_at) VALUES ($1, $2, $3) ON CONFLICT (token) DO UPDATE SET user_uuid = EXCLUDED.user_uuid, expires_at = EXCLUDED.expires_at"
	_, err := s.db.ExecContext(ctx, query, token, userID, expiresAt.UTC())
	return err
}

func (s *postgresStore) GetSession(ctx context.Context, token string) (string, time.Time, error) {
	const query = "SELECT user_uuid, expires_at FROM sessions WHERE token = $1"
	var userID string
	var expiresAt time.Time
	err := s.db.QueryRowContext(ctx, query, token).Scan(&userID, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", time.Time{}, ErrSessionNotFound
		}
		return "", time.Time{}, err
	}
	if time.Now().After(expiresAt) {
		return "", time.Time{}, ErrSessionNotFound
	}
	return userID, expiresAt.UTC(), nil
}

func (s *postgresStore) DeleteSession(ctx context.Context, token string) error {
	const query = "DELETE FROM sessions WHERE token = $1"
	_, err := s.db.ExecContext(ctx, query, token)
	return err
}

func (s *postgresStore) CreateOAuthExchangeCode(ctx context.Context, code, sessionToken string, sessionExpiresAt, expiresAt time.Time) error {
	const query = `INSERT INTO oauth_exchange_codes (code, session_token, session_expires_at, expires_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (code) DO UPDATE SET
  session_token = EXCLUDED.session_token,
  session_expires_at = EXCLUDED.session_expires_at,
  expires_at = EXCLUDED.expires_at`
	_, err := s.db.ExecContext(ctx, query, strings.TrimSpace(code), sessionToken, sessionExpiresAt.UTC(), expiresAt.UTC())
	return err
}

func (s *postgresStore) ConsumeOAuthExchangeCode(ctx context.Context, code string) (string, time.Time, bool, error) {
	const query = `DELETE FROM oauth_exchange_codes
WHERE code = $1 AND expires_at > now()
RETURNING session_token, session_expires_at`
	var sessionToken string
	var sessionExpiresAt time.Time
	err := s.db.QueryRowContext(ctx, query, strings.TrimSpace(code)).Scan(&sessionToken, &sessionExpiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", time.Time{}, false, nil
		}
		return "", time.Time{}, false, err
	}
	return sessionToken, sessionExpiresAt.UTC(), true, nil
}
