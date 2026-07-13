package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"account/api"
	"account/config"
	"account/internal/agentmode"
	"account/internal/agentserver"
	"account/internal/auth"
	"account/internal/mailer"
	"account/internal/model"
	"account/internal/service"
	"account/internal/store"
	"account/internal/xrayconfig"
)

var (
	configPath string
	logLevel   string
)

const (
	// SandboxEmail is the canonical email for the sandbox account.
	SandboxEmail = "sandbox@svc.plus"
	// ReviewEmail is the canonical email for the readonly App Review account.
	ReviewEmail = "review@svc.plus"
	// SharedXWorkmateBridgeServerURL is the managed bridge endpoint exposed to
	// tenant-shared xworkmate clients such as the App Review account.
	SharedXWorkmateBridgeServerURL = "https://xworkmate-bridge.svc.plus"
)

const (
	rootUsername             = "admin"
	rootBootstrapPasswordEnv = "ROOT_BOOTSTRAP_PASSWORD"
)

var defaultReviewPermissions = []string{
	"admin.settings.read",
	"admin.users.metrics.read",
	"admin.users.list.read",
	"admin.agents.status.read",
	"admin.blacklist.read",
}

type sharedXWorkmateBootstrapConfig struct {
	BridgeServerURL string
	BridgeAuthToken string
}

func resolveSharedXWorkmateBootstrapConfig() sharedXWorkmateBootstrapConfig {
	return sharedXWorkmateBootstrapConfig{
		BridgeServerURL: firstNonEmptyString(
			os.Getenv("XWORKMATE_BRIDGE_SERVER_URL"),
			os.Getenv("BRIDGE_SERVER_URL"),
			SharedXWorkmateBridgeServerURL,
		),
		BridgeAuthToken: firstNonEmptyString(
			os.Getenv("BRIDGE_AUTH_TOKEN"),
			os.Getenv("INTERNAL_SERVICE_TOKEN"),
		),
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func normalizeBridgeServerOrigin(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host}).String()
}

func managedSharedXWorkmateBridgeLocator() store.XWorkmateSecretLocator {
	locator := store.XWorkmateSecretLocator{
		ID:         strings.Join([]string{"managed", store.SharedXWorkmateTenantID, "", store.XWorkmateProfileScopeTenantShared, store.XWorkmateSecretLocatorTargetBridgeAuthToken}, "|"),
		Provider:   store.XWorkmateSecretLocatorProviderVault,
		SecretPath: fmt.Sprintf("xworkmate/tenants/%s/shared", store.SharedXWorkmateTenantID),
		SecretKey:  store.XWorkmateSecretLocatorTargetBridgeAuthToken,
		Target:     store.XWorkmateSecretLocatorTargetBridgeAuthToken,
		Required:   true,
	}
	store.NormalizeXWorkmateSecretLocator(&locator)
	return locator
}

func upsertXWorkmateSecretLocator(
	locators []store.XWorkmateSecretLocator,
	locator store.XWorkmateSecretLocator,
) []store.XWorkmateSecretLocator {
	next := make([]store.XWorkmateSecretLocator, 0, len(locators)+1)
	replaced := false
	for _, current := range locators {
		store.NormalizeXWorkmateSecretLocator(&current)
		if current.Target == locator.Target {
			next = append(next, locator)
			replaced = true
			continue
		}
		next = append(next, current)
	}
	if !replaced {
		next = append(next, locator)
	}
	return next
}

func ensureSharedReviewXWorkmateProfile(
	ctx context.Context,
	st store.Store,
	reviewCfg config.ReviewAccount,
	bootstrap sharedXWorkmateBootstrapConfig,
	writeSecret func(context.Context, store.XWorkmateSecretLocator, string) error,
	logger *slog.Logger,
) error {
	if !reviewCfg.Enabled {
		return nil
	}

	bridgeServerURL := strings.TrimSpace(bootstrap.BridgeServerURL)
	if bridgeServerURL == "" {
		return fmt.Errorf("shared xworkmate bridge server url is required")
	}
	parsedBridgeURL, err := url.Parse(bridgeServerURL)
	if err != nil || parsedBridgeURL.Scheme == "" || parsedBridgeURL.Host == "" {
		return fmt.Errorf("shared xworkmate bridge server url is invalid: %q", bridgeServerURL)
	}
	bridgeAuthToken := strings.TrimSpace(bootstrap.BridgeAuthToken)
	if bridgeAuthToken == "" {
		return fmt.Errorf("shared xworkmate bridge auth token is required")
	}
	if writeSecret == nil {
		return fmt.Errorf("xworkmate vault service is required for shared bridge bootstrap")
	}

	profile, err := st.GetXWorkmateProfile(
		ctx,
		store.SharedXWorkmateTenantID,
		"",
		store.XWorkmateProfileScopeTenantShared,
	)
	if err != nil && !errors.Is(err, store.ErrXWorkmateProfileNotFound) {
		return fmt.Errorf("load shared xworkmate profile: %w", err)
	}
	if errors.Is(err, store.ErrXWorkmateProfileNotFound) || profile == nil {
		profile = &store.XWorkmateProfile{
			TenantID: store.SharedXWorkmateTenantID,
			Scope:    store.XWorkmateProfileScopeTenantShared,
		}
	}

	locator := managedSharedXWorkmateBridgeLocator()
	profile.BridgeServerURL = bridgeServerURL
	profile.BridgeServerOrigin = normalizeBridgeServerOrigin(bridgeServerURL)
	profile.SecretLocators = upsertXWorkmateSecretLocator(
		profile.SecretLocators,
		locator,
	)

	if err := st.UpsertXWorkmateProfile(ctx, profile); err != nil {
		return fmt.Errorf("upsert shared xworkmate profile: %w", err)
	}
	if err := writeSecret(ctx, locator, bridgeAuthToken); err != nil {
		return fmt.Errorf("persist shared bridge auth token: %w", err)
	}
	if logger != nil {
		logger.Info(
			"shared xworkmate bridge contract ensured",
			"bridgeServerURL",
			bridgeServerURL,
			"profileScope",
			store.XWorkmateProfileScopeTenantShared,
		)
	}
	return nil
}

func ensureReviewUser(ctx context.Context, st store.Store, cfg config.ReviewAccount, logger *slog.Logger) error {
	email := strings.ToLower(strings.TrimSpace(cfg.Email))
	if email == "" {
		email = ReviewEmail
	}
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		name = "Review"
	}
	groups := cfg.Groups
	if len(groups) == 0 {
		groups = []string{"User", "Beta", "Review", "ReadOnly Role"}
	}
	permissions := cfg.Permissions
	if len(permissions) == 0 {
		permissions = defaultReviewPermissions
	}

	reviewUser, err := st.GetUserByEmail(ctx, email)
	if err != nil && !errors.Is(err, store.ErrUserNotFound) {
		return fmt.Errorf("lookup review user: %w", err)
	}

	if !cfg.Enabled {
		if reviewUser != nil && reviewUser.Active {
			reviewUser.Active = false
			if err := st.UpdateUser(ctx, reviewUser); err != nil {
				return fmt.Errorf("disable review user: %w", err)
			}
			if logger != nil {
				logger.Info("review account disabled", "email", email)
			}
		}
		return nil
	}

	password := strings.TrimSpace(cfg.Password)
	if password == "" {
		return fmt.Errorf("review account %q enabled without password", email)
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash review password: %w", err)
	}

	if reviewUser == nil {
		user := &store.User{
			Name:          name,
			Email:         email,
			EmailVerified: true,
			PasswordHash:  string(hashed),
			Level:         store.LevelUser,
			Role:          store.RoleReadOnly,
			Groups:        groups,
			Permissions:   permissions,
			Active:        true,
		}
		if err := st.CreateUser(ctx, user); err != nil {
			return fmt.Errorf("create review user: %w", err)
		}
		if logger != nil {
			logger.Info("review account ensured", "email", email, "created", true)
		}
		return nil
	}

	reviewUser.Name = name
	reviewUser.Email = email
	reviewUser.EmailVerified = true
	reviewUser.PasswordHash = string(hashed)
	reviewUser.Role = store.RoleReadOnly
	reviewUser.Level = store.LevelUser
	reviewUser.Groups = groups
	reviewUser.Permissions = permissions
	reviewUser.Active = true
	reviewUser.MFATOTPSecret = ""
	reviewUser.MFAEnabled = false
	reviewUser.MFASecretIssuedAt = time.Time{}
	reviewUser.MFAConfirmedAt = time.Time{}
	if err := st.UpdateUser(ctx, reviewUser); err != nil {
		return fmt.Errorf("update review user: %w", err)
	}
	if logger != nil {
		logger.Info("review account ensured", "email", email, "created", false)
	}
	return nil
}

type mailerAdapter struct {
	sender mailer.Sender
}

func (m mailerAdapter) Send(ctx context.Context, msg api.EmailMessage) error {
	if m.sender == nil {
		return nil
	}
	mail := mailer.Message{
		To:        append([]string(nil), msg.To...),
		Subject:   msg.Subject,
		PlainBody: msg.PlainBody,
		HTMLBody:  msg.HTMLBody,
	}
	return m.sender.Send(ctx, mail)
}

type metricsAdapter struct {
	st store.Store
}

func (a *metricsAdapter) ListUsers(ctx context.Context) ([]service.UserRecord, error) {
	users, err := a.st.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	records := make([]service.UserRecord, 0, len(users))
	for _, u := range users {
		records = append(records, service.UserRecord{
			ID:        u.ID,
			CreatedAt: u.CreatedAt,
			Active:    u.Active,
		})
	}
	return records, nil
}

func (a *metricsAdapter) FetchSubscriptionStates(ctx context.Context, userIDs []string) (map[string]service.SubscriptionState, error) {
	states := make(map[string]service.SubscriptionState)
	for _, userID := range userIDs {
		subs, err := a.st.ListSubscriptionsByUser(ctx, userID)
		if err != nil {
			continue
		}
		active := false
		var expiresAt *time.Time
		for _, sub := range subs {
			if strings.ToLower(sub.Status) == "active" {
				active = true
				if t, ok := sub.Meta["expiresAt"].(time.Time); ok {
					if expiresAt == nil || t.After(*expiresAt) {
						expiresAt = &t
					}
				}
			}
		}
		states[userID] = service.SubscriptionState{
			Active:    active,
			ExpiresAt: expiresAt,
		}
	}
	return states, nil
}

func ensureSandboxUser(ctx context.Context, st store.Store, logger *slog.Logger) error {
	sandboxUser, err := st.GetUserByEmail(ctx, SandboxEmail)
	if err != nil && !errors.Is(err, store.ErrUserNotFound) {
		return fmt.Errorf("lookup sandbox user: %w", err)
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte("Sandbox123!"), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash sandbox password: %w", err)
	}

	expiresAt := time.Now().UTC().Add(time.Hour)
	if sandboxUser == nil {
		user := &store.User{
			Name:               "Sandbox",
			Email:              SandboxEmail,
			EmailVerified:      true,
			PasswordHash:       string(hashed),
			Level:              store.LevelUser,
			Role:               store.RoleReadOnly,
			Groups:             []string{"User", "Sandbox", "ReadOnly Role"},
			Permissions:        []string{},
			Active:             true,
			ProxyUUID:          uuid.NewString(),
			ProxyUUIDExpiresAt: &expiresAt,
		}
		if err := st.CreateUser(ctx, user); err != nil {
			return fmt.Errorf("create sandbox user: %w", err)
		}
		if logger != nil {
			logger.Info("sandbox experience user created", "email", SandboxEmail)
		}
	} else {
		// Ensure sandbox user is active and has properties aligned with experience mode
		sandboxUser.Name = "Sandbox"
		sandboxUser.Active = true
		sandboxUser.Role = store.RoleReadOnly
		if !containsCaseInsensitive(sandboxUser.Groups, "Sandbox") {
			sandboxUser.Groups = append(sandboxUser.Groups, "Sandbox")
		}
		if !containsCaseInsensitive(sandboxUser.Groups, "ReadOnly Role") {
			sandboxUser.Groups = append(sandboxUser.Groups, "ReadOnly Role")
		}

		if sandboxUser.ProxyUUID == "" {
			sandboxUser.ProxyUUID = uuid.NewString()
		}
		if sandboxUser.ProxyUUIDExpiresAt == nil {
			sandboxUser.ProxyUUIDExpiresAt = &expiresAt
		}

		if err := st.UpdateUser(ctx, sandboxUser); err != nil {
			return fmt.Errorf("update sandbox user: %w", err)
		}
		if logger != nil {
			logger.Info("sandbox experience user ensured", "email", SandboxEmail)
		}
	}
	return nil
}

func startSandboxUUIDRotator(ctx context.Context, st store.Store, logger *slog.Logger) {
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				user, err := st.GetUserByEmail(context.Background(), SandboxEmail)
				if err != nil {
					if logger != nil {
						logger.Warn("sandbox uuid rotation skipped: lookup failed", "err", err)
					}
					continue
				}
				if user == nil {
					if err := ensureSandboxUser(context.Background(), st, logger); err != nil && logger != nil {
						logger.Warn("sandbox uuid rotation failed to recreate user", "err", err)
					}
					continue
				}

				expiresAt := time.Now().UTC().Add(time.Hour)
				user.ProxyUUID = uuid.NewString()
				user.ProxyUUIDExpiresAt = &expiresAt
				if err := st.UpdateUser(context.Background(), user); err != nil {
					if logger != nil {
						logger.Warn("sandbox uuid rotation failed", "err", err)
					}
					continue
				}
				if logger != nil {
					logger.Info("sandbox uuid rotated", "userID", user.ID, "expiresAt", expiresAt)
				}
			}
		}
	}()
}

func ensureRootUser(ctx context.Context, st store.Store, logger *slog.Logger) error {
	users, err := st.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("list users for root check: %w", err)
	}

	var rootUser *store.User
	for i := range users {
		user := users[i]
		if strings.EqualFold(strings.TrimSpace(user.Email), store.RootAdminEmail) {
			candidate := user
			rootUser = &candidate
			break
		}
	}

	if rootUser == nil {
		bootstrapPassword := strings.TrimSpace(os.Getenv(rootBootstrapPasswordEnv))
		if bootstrapPassword == "" {
			return fmt.Errorf("root account %q missing: set %s to bootstrap it", store.RootAdminEmail, rootBootstrapPasswordEnv)
		}

		hashed, err := bcrypt.GenerateFromPassword([]byte(bootstrapPassword), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("hash root bootstrap password: %w", err)
		}

		root := &store.User{
			Name:          rootUsername,
			Email:         store.RootAdminEmail,
			PasswordHash:  string(hashed),
			EmailVerified: true,
			Role:          store.RoleRoot,
			Level:         store.LevelAdmin,
			Groups:        []string{"Admin"},
			Permissions:   []string{"*"},
			Active:        true,
		}
		if err := st.CreateUser(ctx, root); err != nil {
			return fmt.Errorf("create root user: %w", err)
		}
		rootUser = root
		if logger != nil {
			logger.Warn("root account bootstrapped from environment variable", "email", store.RootAdminEmail)
		}
	}

	if rootUser != nil {
		updatedRoot := *rootUser
		if enforceRootProfile(&updatedRoot) {
			if err := st.UpdateUser(ctx, &updatedRoot); err != nil {
				return fmt.Errorf("enforce root profile: %w", err)
			}
			rootUser = &updatedRoot
			if logger != nil {
				logger.Info("root profile normalized", "email", store.RootAdminEmail, "userID", rootUser.ID)
			}
		}
	}

	for i := range users {
		user := users[i]
		if rootUser != nil && user.ID == rootUser.ID {
			continue
		}
		if !store.IsAdminRole(user.Role) {
			continue
		}

		updated := user
		updated.Role = store.RoleOperator
		updated.Level = store.LevelOperator
		updated.Permissions = dropPermission(updated.Permissions, "*")
		updated.Groups = dropGroup(updated.Groups, "Admin")
		if len(updated.Groups) == 0 {
			updated.Groups = []string{"Operator"}
		}

		if err := st.UpdateUser(ctx, &updated); err != nil {
			return fmt.Errorf("demote legacy root/admin user %q: %w", user.Email, err)
		}
		if logger != nil {
			logger.Warn("demoted legacy root/admin account to operator", "userID", updated.ID, "email", updated.Email)
		}
	}

	return nil
}

func enforceRootProfile(user *store.User) bool {
	if user == nil {
		return false
	}

	changed := false
	if !strings.EqualFold(strings.TrimSpace(user.Email), store.RootAdminEmail) {
		user.Email = store.RootAdminEmail
		changed = true
	}
	if strings.ToLower(strings.TrimSpace(user.Role)) != store.RoleRoot {
		user.Role = store.RoleRoot
		changed = true
	}
	if user.Level != store.LevelAdmin {
		user.Level = store.LevelAdmin
		changed = true
	}
	if !user.Active {
		user.Active = true
		changed = true
	}
	if !user.EmailVerified {
		user.EmailVerified = true
		changed = true
	}
	if !containsCaseInsensitive(user.Groups, "Admin") {
		user.Groups = append(user.Groups, "Admin")
		changed = true
	}
	if !containsExactValue(user.Permissions, "*") {
		user.Permissions = append(user.Permissions, "*")
		changed = true
	}
	return changed
}

func dropPermission(values []string, permission string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == permission {
			continue
		}
		result = append(result, value)
	}
	return result
}

func dropGroup(values []string, group string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), group) {
			continue
		}
		result = append(result, value)
	}
	return result
}

func containsCaseInsensitive(values []string, target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), target) {
			return true
		}
	}
	return false
}

func containsExactValue(values []string, target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}
	for _, value := range values {
		if strings.TrimSpace(value) == target {
			return true
		}
	}
	return false
}

func applyRBACSchema(ctx context.Context, db *gorm.DB, driver string) error {
	if db == nil {
		return errors.New("database is nil")
	}

	normalized := strings.ToLower(strings.TrimSpace(driver))
	if normalized != "postgres" && normalized != "postgresql" && normalized != "pgx" {
		return nil
	}

	statements := []string{
		`CREATE TABLE IF NOT EXISTS public.users (
  uuid UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  username TEXT NOT NULL UNIQUE,
  email TEXT NOT NULL UNIQUE,
  email_verified BOOLEAN NOT NULL DEFAULT FALSE,
  password TEXT NOT NULL,
  mfa_totp_secret TEXT,
  mfa_enabled BOOLEAN NOT NULL DEFAULT FALSE,
  mfa_secret_issued_at TIMESTAMPTZ,
  mfa_confirmed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  level INTEGER NOT NULL DEFAULT 20,
  role TEXT NOT NULL DEFAULT 'user',
  groups JSONB NOT NULL DEFAULT '[]'::jsonb,
  permissions JSONB NOT NULL DEFAULT '[]'::jsonb,
  active BOOLEAN NOT NULL DEFAULT TRUE,
  proxy_uuid UUID NOT NULL DEFAULT gen_random_uuid(),
  proxy_uuid_expires_at TIMESTAMPTZ
)`,
		`ALTER TABLE public.users ADD COLUMN IF NOT EXISTS proxy_uuid UUID NOT NULL DEFAULT gen_random_uuid()`,
		`ALTER TABLE public.users ADD COLUMN IF NOT EXISTS proxy_uuid_expires_at TIMESTAMPTZ`,
		`ALTER TABLE public.users ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT now()`,
		`ALTER TABLE public.users ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now()`,
		`CREATE TABLE IF NOT EXISTS public.email_blacklist (
  email TEXT PRIMARY KEY,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE TABLE IF NOT EXISTS public.agents (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  groups JSONB NOT NULL DEFAULT '[]'::jsonb,
  healthy BOOLEAN NOT NULL DEFAULT FALSE,
  last_heartbeat TIMESTAMPTZ,
  clients_count INTEGER NOT NULL DEFAULT 0,
  sync_revision TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE TABLE IF NOT EXISTS public.sessions (
  token TEXT PRIMARY KEY,
  user_uuid UUID NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE TABLE IF NOT EXISTS public.rbac_roles (
  role_key TEXT PRIMARY KEY,
  description TEXT NOT NULL DEFAULT '',
  priority INTEGER NOT NULL DEFAULT 100,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE TABLE IF NOT EXISTS public.rbac_permissions (
  permission_key TEXT PRIMARY KEY,
  description TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,
		`CREATE TABLE IF NOT EXISTS public.rbac_role_permissions (
  role_key TEXT NOT NULL REFERENCES public.rbac_roles(role_key) ON DELETE CASCADE,
  permission_key TEXT NOT NULL REFERENCES public.rbac_permissions(permission_key) ON DELETE CASCADE,
  enabled BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (role_key, permission_key)
)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS users_single_root_role_uk ON public.users ((lower(role))) WHERE lower(role) = 'root'`,
		`DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_constraint
    WHERE conname = 'users_root_email_ck'
  ) THEN
    ALTER TABLE public.users
      ADD CONSTRAINT users_root_email_ck
      CHECK (lower(role) <> 'root' OR lower(email) = 'admin@svc.plus');
  END IF;
END
$$`,
	}

	for _, stmt := range statements {
		if err := db.WithContext(ctx).Exec(stmt).Error; err != nil {
			return err
		}
	}

	seedStatements := []string{
		`INSERT INTO public.rbac_roles (role_key, description, priority)
VALUES
  ('root', 'single root account', 0),
  ('operator', 'operation role with configurable permissions', 10),
  ('user', 'standard subscription user', 20),
  ('readonly', 'read-only experience account', 30)
ON CONFLICT (role_key) DO NOTHING`,
		`INSERT INTO public.rbac_permissions (permission_key, description)
VALUES
  ('admin.settings.read', 'read admin matrix settings'),
  ('admin.settings.write', 'update admin matrix settings'),
  ('admin.users.metrics.read', 'read user metrics'),
  ('admin.users.list.read', 'read user list'),
  ('admin.agents.status.read', 'read agent status'),
  ('admin.users.pause.write', 'pause users'),
  ('admin.users.resume.write', 'resume users'),
  ('admin.users.delete.write', 'delete users'),
  ('admin.users.renew_uuid.write', 'renew user proxy uuid'),
  ('admin.users.role.write', 'update/reset user role'),
  ('admin.blacklist.read', 'read blacklist'),
  ('admin.blacklist.write', 'update blacklist')
ON CONFLICT (permission_key) DO NOTHING`,
		`INSERT INTO public.rbac_role_permissions (role_key, permission_key, enabled)
SELECT 'operator', permission_key, true
FROM public.rbac_permissions
ON CONFLICT (role_key, permission_key) DO NOTHING`,
	}

	for _, stmt := range seedStatements {
		if err := db.WithContext(ctx).Exec(stmt).Error; err != nil {
			return err
		}
	}

	return nil
}

func runServer(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg == nil {
		return errors.New("config is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}

	storeCfg := store.Config{
		Driver:       cfg.Store.Driver,
		DSN:          cfg.Store.DSN,
		MaxOpenConns: cfg.Store.MaxOpenConns,
		MaxIdleConns: cfg.Store.MaxIdleConns,
	}

	// Initialize business store with retries to account for sidecar startup
	var st store.Store
	var cleanup func(context.Context) error
	var err error
	for i := 0; i < 15; i++ {
		st, cleanup, err = store.New(ctx, storeCfg)
		if err == nil {
			break
		}
		if storeCfg.Driver == "" || storeCfg.Driver == "memory" {
			return err
		}
		slog.Warn("retrying business store connection...", "attempt", i+1, "err", err)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		return fmt.Errorf("business store connection failed after sidecar wait: %w", err)
	}
	defer func() {
		if cleanup == nil {
			return
		}
		if err := cleanup(context.Background()); err != nil {
			logger.Error("failed to close store", "err", err)
		}
	}()

	gormDB, gormCleanup, err := openAdminSettingsDB(cfg.Store)
	if err != nil {
		return err
	}
	defer func() {
		if gormCleanup != nil {
			if err := gormCleanup(context.Background()); err != nil {
				logger.Error("failed to close admin settings db", "err", err)
			}
		}
	}()
	service.SetDB(gormDB)

	if err := ensureRootUser(ctx, st, logger); err != nil {
		return err
	}

	if err := ensureSandboxUser(ctx, st, logger); err != nil {
		logger.Warn("failed to ensure sandbox user", "err", err)
	}
	startSandboxUUIDRotator(ctx, st, logger)
	if err := ensureReviewUser(ctx, st, cfg.ReviewAccount, logger); err != nil {
		logger.Warn("failed to ensure review user", "err", err)
	}
	if err := st.EnsureTenant(ctx, &store.Tenant{
		ID:      store.SharedXWorkmateTenantID,
		Name:    store.SharedXWorkmateTenantName,
		Edition: store.SharedPublicTenantEdition,
	}); err != nil {
		return fmt.Errorf("ensure shared xworkmate tenant: %w", err)
	}
	if err := st.EnsureTenantDomain(ctx, &store.TenantDomain{
		TenantID:  store.SharedXWorkmateTenantID,
		Domain:    store.SharedXWorkmateDomain,
		Kind:      store.TenantDomainKindGenerated,
		IsPrimary: true,
		Status:    store.TenantDomainStatusVerified,
	}); err != nil {
		return fmt.Errorf("ensure shared xworkmate tenant domain: %w", err)
	}

	r := gin.New()
	corsConfig := buildCORSConfig(logger, cfg.Server, st)
	if corsConfig.AllowAllOrigins {
		logger.Info("configured cors", "allowAllOrigins", true)
	} else {
		logger.Info("configured cors", "allowedOrigins", cfg.Server.AllowedOrigins, "dynamicTenantDomains", true)
	}
	r.Use(cors.New(corsConfig))
	r.Use(gin.Recovery())
	r.Use(func(c *gin.Context) {
		start := time.Now()
		c.Next()
		logger.Info("request", "method", c.Request.Method, "path", c.FullPath(), "status", c.Writer.Status(), "latency", time.Since(start))
	})

	var emailSender api.EmailSender
	emailVerificationEnabled := true
	smtpHost := strings.TrimSpace(cfg.SMTP.Host)
	if smtpHost == "" {
		emailVerificationEnabled = false
	}
	if smtpHost != "" && isExampleDomain(smtpHost) {
		emailVerificationEnabled = false
		logger.Warn("smtp host is a placeholder; disabling email delivery", "host", smtpHost)
		smtpHost = ""
	}
	if smtpHost != "" {
		switch {
		case strings.TrimSpace(cfg.SMTP.Username) == "":
			emailVerificationEnabled = false
			logger.Warn("smtp username is missing; disabling email verification", "host", smtpHost)
		case strings.TrimSpace(cfg.SMTP.Password) == "":
			emailVerificationEnabled = false
			logger.Warn("smtp password is missing; disabling email verification", "host", smtpHost)
		case strings.TrimSpace(cfg.SMTP.From) == "":
			emailVerificationEnabled = false
			logger.Warn("smtp from address is missing; disabling email verification", "host", smtpHost)
		}
	}
	if smtpHost != "" {
		tlsMode := mailer.ParseTLSMode(cfg.SMTP.TLS.Mode)
		sender, err := mailer.New(mailer.Config{
			Host:               smtpHost,
			Port:               cfg.SMTP.Port,
			Username:           cfg.SMTP.Username,
			Password:           cfg.SMTP.Password,
			From:               cfg.SMTP.From,
			ReplyTo:            cfg.SMTP.ReplyTo,
			Timeout:            cfg.SMTP.Timeout,
			TLSMode:            tlsMode,
			InsecureSkipVerify: cfg.SMTP.TLS.InsecureSkipVerify,
		})
		if err != nil {
			return err
		}
		emailSender = mailerAdapter{sender: sender}
	}
	if emailSender == nil {
		emailVerificationEnabled = false
	}

	// Initialize TokenService for authentication
	var tokenService *auth.TokenService
	if cfg.Auth.Enable {
		accessExpiry := cfg.Auth.Token.AccessExpiry
		if accessExpiry <= 0 {
			accessExpiry = 1 * time.Hour
		}
		refreshExpiry := cfg.Auth.Token.RefreshExpiry
		if refreshExpiry <= 0 {
			refreshExpiry = 168 * time.Hour // 7 days
		}

		tokenService = auth.NewTokenService(auth.TokenConfig{
			PublicToken:   cfg.Auth.Token.PublicToken,
			RefreshSecret: cfg.Auth.Token.RefreshSecret,
			AccessSecret:  cfg.Auth.Token.AccessSecret,
			AccessExpiry:  accessExpiry,
			RefreshExpiry: refreshExpiry,
		})
		logger.Info("token service initialized", "auth_enabled", cfg.Auth.Enable)
	}

	if err := applyRBACSchema(ctx, gormDB, cfg.Store.Driver); err != nil {
		return fmt.Errorf("apply rbac schema: %w", err)
	}

	gormSource, err := xrayconfig.NewGormClientSource(gormDB)
	if err != nil {
		return err
	}

	var agentRegistry *agentserver.Registry
	if len(cfg.Agents.Credentials) > 0 {
		creds := make([]agentserver.Credential, 0, len(cfg.Agents.Credentials))
		for _, c := range cfg.Agents.Credentials {
			creds = append(creds, agentserver.Credential{
				ID:     c.ID,
				Name:   c.Name,
				Token:  c.Token,
				Groups: append([]string(nil), c.Groups...),
			})
		}
		agentRegistry, err = agentserver.NewRegistry(agentserver.Config{Credentials: creds})
		if err != nil {
			return err
		}
	} else if token := os.Getenv("INTERNAL_SERVICE_TOKEN"); token != "" {
		// Fallback: if no credentials configured but we have an internal token,
		// create a wildcard credential that accepts any agent presenting this token.
		// The actual agent ID will be extracted from the request (e.g., X-Agent-ID header).
		// This allows multiple agents to authenticate with the same shared token.
		agentRegistry, err = agentserver.NewRegistry(agentserver.Config{
			Credentials: []agentserver.Credential{{
				ID:     "*", // Wildcard: accept any agent ID
				Name:   "Internal Agents (Shared Token)",
				Token:  token,
				Groups: []string{"internal"},
			}},
		})
		if err != nil {
			return err
		}
	}

	if agentRegistry != nil {
		agentRegistry.SetStore(st)
		agentRegistry.SetLogger(logger.With("component", "agent-registry"))
		if err := agentRegistry.Load(ctx); err != nil {
			logger.Warn("failed to load agents from store", "err", err)
		} else {
			agents := agentRegistry.Agents()
			logger.Info("loaded agents from store", "count", len(agents))
		}

		// Start background sync task to keep in-memory registry updated from DB
		go func() {
			ticker := time.NewTicker(1 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := agentRegistry.Load(ctx); err != nil {
						logger.Warn("failed to reload agents from store", "err", err)
					} else {
						// logger.Debug("reloaded agents from store", "count", len(agentRegistry.Agents()))
					}
				}
			}
		}()

		// Start background cleanup task for stale agents (e.g., those that haven't heartbeated for 10 minutes)
		go runAgentCleanup(ctx, st, logger)
	}

	var stopXraySync func(context.Context) error
	if cfg.Xray.Sync.Enabled {
		syncInterval := cfg.Xray.Sync.Interval
		if syncInterval <= 0 {
			syncInterval = 5 * time.Minute
		}
		outputPath := strings.TrimSpace(cfg.Xray.Sync.OutputPath)
		if outputPath == "" {
			outputPath = "/usr/local/etc/xray/config.json"
		}
		syncer, err := xrayconfig.NewPeriodicSyncer(xrayconfig.PeriodicOptions{
			Logger:   logger.With("component", "xray-sync"),
			Interval: syncInterval,
			Source:   gormSource,
			Generators: []xrayconfig.Generator{
				{
					Definition: xrayconfig.XHTTPDefinition(),
					OutputPath: "/usr/local/etc/xray/config.json", // Match user's xhttp config path
					Domain:     cfg.Xray.Sync.Domain,
				},
				{
					Definition: xrayconfig.TCPDefinition(),
					OutputPath: "/usr/local/etc/xray/tcp-config.json", // Match user's tcp config path
					Domain:     cfg.Xray.Sync.Domain,
				},
			},
			ValidateCommand: cfg.Xray.Sync.ValidateCommand,
			RestartCommand:  cfg.Xray.Sync.RestartCommand,
		})
		if err != nil {
			return err
		}
		stop, err := syncer.Start(ctx)
		if err != nil {
			return err
		}
		logger.Info("xray periodic sync enabled", "interval", syncInterval, "output", outputPath)
		stopXraySync = stop
	}

	if stopXraySync != nil {
		defer func() {
			waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := stopXraySync(waitCtx); err != nil {
				logger.Warn("xray syncer shutdown", "err", err)
			}
		}()
	}

	xworkmateVaultService, err := api.NewXWorkmateVaultService(api.XWorkmateVaultConfig{
		Address:   strings.TrimSpace(os.Getenv("XWORKMATE_VAULT_ADDR")),
		Token:     strings.TrimSpace(os.Getenv("XWORKMATE_VAULT_TOKEN")),
		Namespace: strings.TrimSpace(os.Getenv("XWORKMATE_VAULT_NAMESPACE")),
		Mount:     strings.TrimSpace(os.Getenv("XWORKMATE_VAULT_MOUNT")),
	})
	if err != nil {
		return err
	}
	if err := ensureSharedReviewXWorkmateProfile(
		ctx,
		st,
		cfg.ReviewAccount,
		resolveSharedXWorkmateBootstrapConfig(),
		func(
			ctx context.Context,
			locator store.XWorkmateSecretLocator,
			value string,
		) error {
			if xworkmateVaultService == nil {
				return errors.New("xworkmate vault service is unavailable")
			}
			return xworkmateVaultService.WriteSecret(ctx, locator, value)
		},
		logger,
	); err != nil {
		logger.Warn("failed to ensure shared review xworkmate profile", "err", err)
	}

	options := []api.Option{
		api.WithStore(st),
		api.WithSessionTTL(cfg.Session.TTL),
		api.WithEmailSender(emailSender),
		api.WithEmailVerification(emailVerificationEnabled),
		api.WithTokenService(tokenService),
		api.WithOAuthFrontendURL(cfg.Auth.OAuth.FrontendURL),
		api.WithServerPublicURL(cfg.Server.PublicURL),
		api.WithStripeConfig(api.StripeConfig{
			SecretKey:       strings.TrimSpace(os.Getenv("STRIPE_SECRET_KEY")),
			WebhookSecret:   strings.TrimSpace(os.Getenv("STRIPE_WEBHOOK_SECRET")),
			AllowedPriceIDs: api.ParseStripeAllowedPriceIDs(os.Getenv("STRIPE_ALLOWED_PRICE_IDS")),
			FrontendURL:     strings.TrimSpace(cfg.Auth.OAuth.FrontendURL),
		}),
	}
	if xworkmateVaultService != nil {
		options = append(options, api.WithXWorkmateVaultService(xworkmateVaultService))
	}

	if agentRegistry != nil {
		options = append(options, api.WithAgentStatusReader(agentRegistry))
	}

	// Initialize User Metrics Service
	metricsSvc := &service.UserMetricsService{
		Users:         &metricsAdapter{st: st},
		Subscriptions: &metricsAdapter{st: st},
	}
	options = append(options, api.WithUserMetricsProvider(metricsSvc))

	// Initialize OAuth providers
	oauthProviders := make(map[string]auth.OAuthProvider)
	if cfg.Auth.Enable {
		if cfg.Auth.OAuth.GitHub.ClientID != "" {
			redirectURL := cfg.Auth.OAuth.GitHub.RedirectURL
			if redirectURL == "" {
				redirectURL = cfg.Auth.OAuth.RedirectURL
			}
			oauthProviders["github"] = auth.NewGitHubProvider(
				cfg.Auth.OAuth.GitHub.ClientID,
				cfg.Auth.OAuth.GitHub.ClientSecret,
				redirectURL,
			)
		}
		if cfg.Auth.OAuth.Google.ClientID != "" {
			redirectURL := cfg.Auth.OAuth.Google.RedirectURL
			if redirectURL == "" {
				redirectURL = cfg.Auth.OAuth.RedirectURL
			}
			oauthProviders["google"] = auth.NewGoogleProvider(
				cfg.Auth.OAuth.Google.ClientID,
				cfg.Auth.OAuth.Google.ClientSecret,
				redirectURL,
			)
		}
	}
	options = append(options, api.WithOAuthProviders(oauthProviders))
	options = append(options, api.WithAgentRegistry(agentRegistry))
	options = append(options, api.WithGormDB(gormDB))

	// Pre-load sandbox bindings from database into the registry
	if agentRegistry != nil {
		var sandboxBindings []model.SandboxBinding
		if err := gormDB.Find(&sandboxBindings).Error; err == nil {
			for _, b := range sandboxBindings {
				agentRegistry.SetSandboxAgent(b.AgentID, true)
			}
		}
	}

	api.RegisterRoutes(r, options...)

	addr := strings.TrimSpace(cfg.Server.Addr)
	if addr == "" {
		addr = ":8080"
	}

	tlsSettings := cfg.Server.TLS
	certFile := strings.TrimSpace(tlsSettings.CertFile)
	keyFile := strings.TrimSpace(tlsSettings.KeyFile)
	caFile := strings.TrimSpace(tlsSettings.CAFile)
	clientCAFile := strings.TrimSpace(tlsSettings.ClientCAFile)

	useTLS := tlsSettings.IsEnabled()

	var tlsConfig *tls.Config
	if useTLS {
		if certFile == "" || keyFile == "" {
			return fmt.Errorf("tls is enabled but certFile (%q) or keyFile (%q) is empty", certFile, keyFile)
		}

		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return fmt.Errorf("failed to load tls certificate: %w", err)
		}

		if caFile != "" {
			caPEM, err := os.ReadFile(caFile)
			if err != nil {
				return fmt.Errorf("failed to read ca file %q: %w", caFile, err)
			}

			var block *pem.Block
			existing := make(map[string]struct{}, len(cert.Certificate))
			for _, c := range cert.Certificate {
				existing[string(c)] = struct{}{}
			}

			for len(caPEM) > 0 {
				block, caPEM = pem.Decode(caPEM)
				if block == nil {
					break
				}
				if block.Type != "CERTIFICATE" || len(block.Bytes) == 0 {
					continue
				}
				if _, ok := existing[string(block.Bytes)]; ok {
					continue
				}
				cert.Certificate = append(cert.Certificate, block.Bytes)
			}

			if len(cert.Certificate) == 0 {
				return fmt.Errorf("ca file %q did not contain any certificates", caFile)
			}
		}

		tlsConfig = &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		}

		if clientCAFile != "" {
			caBytes, err := os.ReadFile(clientCAFile)
			if err != nil {
				return err
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(caBytes) {
				return errors.New("failed to parse client CA file")
			}
			tlsConfig.ClientCAs = pool
			tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
		}
	} else {
		if certFile != "" || keyFile != "" {
			logger.Info("TLS disabled; certificate paths will be ignored", "certFile", certFile, "keyFile", keyFile)
		}
		if clientCAFile != "" {
			logger.Warn("client CA configured but TLS is disabled; ignoring", "clientCAFile", clientCAFile)
		}
	}

	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	if useTLS {
		srv.TLSConfig = tlsConfig
	}

	logger.Info("starting account service", "addr", addr, "tls", useTLS)

	var listenCertFile, listenKeyFile string
	if useTLS {
		if tlsSettings.RedirectHTTP {
			go func() {
				redirectAddr := deriveRedirectAddr(addr)
				redirectSrv := &http.Server{
					Addr: redirectAddr,
					Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						host := r.Host
						if host == "" {
							host = redirectAddr
						}
						target := "https://" + host + r.URL.RequestURI()
						http.Redirect(w, r, target, http.StatusPermanentRedirect)
					}),
				}
				if err := redirectSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					logger.Error("http redirect listener exited", "err", err)
				}
			}()
		}

		if tlsConfig != nil && len(tlsConfig.Certificates) > 0 {
			listenCertFile = ""
			listenKeyFile = ""
		} else {
			listenCertFile = certFile
			listenKeyFile = keyFile
		}

		if err := srv.ListenAndServeTLS(listenCertFile, listenKeyFile); err != nil {
			if !errors.Is(err, http.ErrServerClosed) {
				logger.Error("account service shutdown", "err", err)
				return err
			}
		}
	} else {
		if err := srv.ListenAndServe(); err != nil {
			if !errors.Is(err, http.ErrServerClosed) {
				logger.Error("account service shutdown", "err", err)
				return err
			}
		}
	}
	return nil
}

func runServerAndAgent(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg == nil {
		return errors.New("config is nil")
	}

	agentCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	agentErrCh := make(chan error, 1)
	go func() {
		agentErrCh <- runAgent(agentCtx, cfg, logger)
	}()

	agentPending := true

	select {
	case err := <-agentErrCh:
		agentPending = false
		if err == nil {
			err = errors.New("agent exited unexpectedly")
		}
		return fmt.Errorf("agent startup failed: %w", err)
	default:
	}

	serverErr := runServer(ctx, cfg, logger)
	cancel()

	var agentErr error
	if agentPending {
		agentErr = <-agentErrCh
	}

	if serverErr != nil {
		return serverErr
	}
	if agentErr != nil {
		return agentErr
	}
	return nil
}

func runAgent(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	if cfg == nil {
		return errors.New("config is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if !cfg.Xray.Sync.Enabled {
		logger.Warn("xray sync is disabled in configuration; agent mode will still attempt to manage xray config")
	}
	options := agentmode.Options{
		Logger: logger.With("component", "agent"),
		Agent:  cfg.Agent,
		Xray:   cfg.Xray,
	}
	return agentmode.Run(ctx, options)
}

func extractBearerToken(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	header = strings.TrimPrefix(header, "Bearer ")
	return strings.TrimSpace(header)
}

func runAgentCleanup(ctx context.Context, st store.Store, logger *slog.Logger) {
	// Cleanup every 5 minutes
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	// Threshold for considering an agent stale: 10 minutes
	staleThreshold := 10 * time.Minute

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			count, err := st.DeleteStaleAgents(cleanupCtx, staleThreshold)
			cancel()

			if err != nil {
				logger.Warn("failed to cleanup stale agents", "err", err)
			} else if count > 0 {
				logger.Info("cleaned up stale agents", "count", count)
			}
		}
	}
}

var rootCmd = &cobra.Command{
	Use:   "xcontrol-account",
	Short: "Start the xcontrol account service",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		if logLevel != "" {
			cfg.Log.Level = logLevel
		}

		level := slog.LevelInfo
		switch strings.ToLower(strings.TrimSpace(cfg.Log.Level)) {
		case "debug":
			level = slog.LevelDebug
		case "warn", "warning":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		}

		logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
		slog.SetDefault(logger)

		ctx := context.Background()
		mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
		if mode == "" {
			mode = "server"
		}

		switch mode {
		case "server":
			return runServer(ctx, cfg, logger)
		case "agent":
			return runAgent(ctx, cfg, logger)
		case "server-agent", "all", "combined":
			return runServerAndAgent(ctx, cfg, logger)
		default:
			return fmt.Errorf("unsupported mode %q", cfg.Mode)
		}
	},
}

func openAdminSettingsDB(cfg config.Store) (*gorm.DB, func(context.Context) error, error) {
	driver := strings.ToLower(strings.TrimSpace(cfg.Driver))
	var (
		db  *gorm.DB
		err error
	)
	for i := 0; i < 15; i++ {
		switch driver {
		case "", "memory":
			db, err = gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
		case "postgres", "postgresql", "pgx":
			if strings.TrimSpace(cfg.DSN) == "" {
				return nil, nil, errors.New("admin settings database requires a dsn")
			}
			db, err = gorm.Open(postgres.Open(cfg.DSN), &gorm.Config{})
		default:
			return nil, nil, fmt.Errorf("unsupported admin settings driver %q", cfg.Driver)
		}

		if err == nil {
			break
		}
		if driver == "" || driver == "memory" {
			return nil, nil, err
		}
		slog.Warn("retrying admin settings db connection...", "attempt", i+1, "err", err)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("admin settings db connection failed after sidecar wait: %w", err)
	}

	if err := db.AutoMigrate(
		&model.AdminSetting{},
		&model.HomepageVideoSetting{},
		&model.SandboxBinding{},
		&model.Tenant{},
		&model.TenantDomain{},
		&model.TenantMembership{},
		&model.XWorkmateProfile{},
	); err != nil {
		return nil, nil, err
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, nil, err
	}
	if cfg.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	}

	cleanup := func(context.Context) error {
		return sqlDB.Close()
	}
	return db, cleanup, nil
}

func init() {
	rootCmd.Flags().StringVar(&configPath, "config", "", "path to xcontrol account configuration file")
	rootCmd.Flags().StringVar(&logLevel, "log-level", "", "log level (debug, info, warn, error)")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func isExampleDomain(host string) bool {
	normalized := strings.ToLower(strings.TrimSpace(host))
	if normalized == "" {
		return false
	}
	if h, _, ok := strings.Cut(normalized, ":"); ok {
		normalized = h
	}
	if normalized == "example.com" {
		return true
	}
	return strings.HasSuffix(normalized, ".example.com")
}

func buildCORSConfig(logger *slog.Logger, serverCfg config.Server, st store.Store) cors.Config {
	allowOrigins, allowAll := resolveAllowedOrigins(logger, serverCfg)

	cfg := cors.Config{
		AllowMethods: []string{
			http.MethodGet,
			http.MethodHead,
			http.MethodPost,
			http.MethodPut,
			http.MethodPatch,
			http.MethodDelete,
			http.MethodOptions,
		},
		AllowHeaders: []string{
			"Authorization",
			"Content-Type",
			"Accept",
			"Origin",
			"X-Requested-With",
			"Cookie",
		},
		ExposeHeaders: []string{
			"Content-Length",
		},
		MaxAge: 12 * time.Hour,
	}

	if allowAll {
		cfg.AllowAllOrigins = true
		cfg.AllowCredentials = false
	} else {
		cfg.AllowCredentials = true
		allowedOriginSet := make(map[string]struct{}, len(allowOrigins))
		for _, origin := range allowOrigins {
			allowedOriginSet[origin] = struct{}{}
		}
		cfg.AllowOriginFunc = func(origin string) bool {
			normalized, err := parseOrigin(origin)
			if err != nil {
				return false
			}
			if _, ok := allowedOriginSet[normalized]; ok {
				return true
			}

			parsed, err := url.Parse(normalized)
			if err != nil {
				return false
			}
			host := store.NormalizeHostname(parsed.Host)
			if store.IsSharedTenantHost(host) {
				return true
			}
			if st == nil {
				return false
			}
			_, _, err = st.ResolveTenantByHost(context.Background(), host)
			return err == nil
		}
	}

	return cfg
}

func resolveAllowedOrigins(logger *slog.Logger, serverCfg config.Server) ([]string, bool) {
	rawOrigins := serverCfg.AllowedOrigins
	seen := make(map[string]struct{}, len(rawOrigins))
	origins := make([]string, 0, len(rawOrigins))
	allowAll := false

	for _, origin := range rawOrigins {
		trimmed := strings.TrimSpace(origin)
		if trimmed == "" {
			continue
		}
		if trimmed == "*" {
			allowAll = true
			continue
		}

		normalized, err := parseOrigin(trimmed)
		if err != nil {
			logger.Warn("ignoring invalid cors origin", "origin", origin, "err", err)
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		origins = append(origins, normalized)
	}

	if allowAll {
		return nil, true
	}

	if len(origins) == 0 {
		publicURL := strings.TrimSpace(serverCfg.PublicURL)
		if publicURL != "" {
			normalized, err := parseOrigin(publicURL)
			if err != nil {
				logger.Warn("invalid server public url; falling back to defaults", "publicUrl", publicURL, "err", err)
			} else {
				origins = append(origins, normalized)
			}
		}
	}

	if len(origins) == 0 {
		origins = []string{
			"http://localhost:3001",
			"http://127.0.0.1:3001",
		}
	}

	return origins, false
}

func parseOrigin(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("origin is empty")
	}

	normalized := trimmed
	if !strings.Contains(normalized, "://") {
		normalized = "https://" + normalized
	}

	parsed, err := url.Parse(normalized)
	if err != nil {
		return "", err
	}

	scheme := strings.ToLower(strings.TrimSpace(parsed.Scheme))
	if scheme == "" {
		return "", fmt.Errorf("origin must include a scheme")
	}

	hostname := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	if hostname == "" {
		return "", fmt.Errorf("origin must include a host")
	}

	host := hostname
	if port := strings.TrimSpace(parsed.Port()); port != "" {
		host = net.JoinHostPort(hostname, port)
	}

	return scheme + "://" + host, nil
}

func deriveRedirectAddr(addr string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		trimmed := strings.TrimSpace(addr)
		if strings.HasPrefix(trimmed, ":") {
			port = strings.TrimPrefix(trimmed, ":")
			if port == "" || port == "443" {
				return ":80"
			}
			return ":" + port
		}
		return ":80"
	}
	if port == "" || port == "443" {
		port = "80"
	}
	return net.JoinHostPort(host, port)
}
