// Package dbruntime defines the application role during a database handover.
// It never authorizes a handover; Toolkit consumes independent data evidence.
package dbruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

const Primary = "primary"
const Standby = "standby"

type Config struct {
	Role              string
	BackgroundWriters bool
	ConnectionSHA256  string
	SchemaVersion     uint
}

// FromEnvironment returns no DSN override for the existing unmanaged path.
// Managed roles accept only DATABASE_URL; lingering Supabase aliases fail
// closed instead of silently selecting a different database.
func FromEnvironment() (Config, string, error) {
	role := strings.TrimSpace(os.Getenv("DATABASE_RUNTIME_ROLE"))
	background := strings.TrimSpace(os.Getenv("DATABASE_BACKGROUND_WRITERS"))
	identity := strings.TrimSpace(os.Getenv("DATABASE_IDENTITY_SHA256"))
	if role == "" {
		if background != "" || identity != "" {
			return Config{}, "", errors.New("database handover controls require DATABASE_RUNTIME_ROLE")
		}
		return Config{}, "", nil
	}
	if role != Primary && role != Standby {
		return Config{}, "", errors.New("DATABASE_RUNTIME_ROLE must be primary or standby")
	}
	if strings.TrimSpace(os.Getenv("SUPABASE_CONNECT_URI")) != "" || strings.TrimSpace(os.Getenv("SUPABASE_CONNECT_URL")) != "" {
		return Config{}, "", errors.New("managed database role requires clearing Supabase connection aliases")
	}
	if background != "true" && background != "false" {
		return Config{}, "", errors.New("managed database role requires explicit DATABASE_BACKGROUND_WRITERS=true or false")
	}
	if role == Standby && background != "false" {
		return Config{}, "", errors.New("standby cannot enable background writers")
	}
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	actual, err := ConnectionIdentity(dsn)
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(identity) || actual != identity {
		return Config{}, "", errors.New("managed database connection does not match the reviewed identity")
	}
	if role == Primary {
		parsed, _ := pgx.ParseConfig(dsn)
		if parsed.Database != "account" || (strings.HasSuffix(parsed.Host, ".supabase.com") || strings.HasSuffix(parsed.Host, ".supabase.co")) {
			return Config{}, "", errors.New("managed primary requires the native selfhost account database")
		}
	}
	return Config{Role: role, BackgroundWriters: background == "true", ConnectionSHA256: actual}, dsn, nil
}

// ConnectionIdentity matches migratectl's reviewed Host/Port/Database/Role
// JSON fingerprint. It contains no password; it describes configured routing,
// not physical database identity or business equality.
func ConnectionIdentity(dsn string) (string, error) {
	parsed, err := pgx.ParseConfig(dsn)
	if err != nil || strings.TrimSpace(dsn) == "" || parsed.Host == "" || parsed.Database == "" || parsed.User == "" || len(parsed.Fallbacks) != 0 {
		return "", errors.New("database connection contract is invalid or has alternate hosts")
	}
	identity := struct {
		Host     string
		Port     uint16
		Database string
		Role     string
	}{parsed.Host, parsed.Port, parsed.Database, parsed.User}
	raw, _ := json.Marshal(identity)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func (c Config) Managed() bool { return c.Role != "" }
func (c Config) MayRunBackgroundWriters() bool {
	return !c.Managed() || c.Role == Primary && c.BackgroundWriters
}

// Wrap exposes only safe probes in standby. No business handler, database
// pool or daemon is needed for that role, including GET routes with side effects.
func (c Config) Wrap(next http.Handler, image func() map[string]any) http.Handler {
	if !c.Managed() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probe := r.URL.Path == "/api/ping" || r.URL.Path == "/healthz" || r.URL.Path == "/readyz"
		w.Header().Set("X-Database-Runtime-Role", c.Role)
		if c.Role == Primary && (!probe || r.URL.Path == "/readyz") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Database-Runtime-Role", c.Role)
		if !probe || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unavailable","reason":"database_runtime_standby"}`))
			return
		}
		body := image()
		body["status"] = "ok"
		body["database_role"] = c.Role
		body["schema_management"] = "external"
		body["bootstrap_writes"] = false
		body["proxy_uuid_rotator"] = false
		body["background_writers"] = c.MayRunBackgroundWriters()
		body["business_requests_enabled"] = c.Role == Primary
		body["configured_database_sha256"] = c.ConnectionSHA256
		body["schema_version"] = c.SchemaVersion
		if c.Role == Standby && r.URL.Path == "/readyz" {
			body["status"] = "not_ready"
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		if r.Method != http.MethodHead {
			_ = json.NewEncoder(w).Encode(body)
		}
	})
}
