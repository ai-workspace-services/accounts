package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4/database"
	"github.com/jackc/pgx/v5"
)

const controlledHistoryTable = "accounts_controlled_migration_history"
const MaxControlledLockTimeout = 5 * time.Minute
const MaxControlledStatementTimeout = 30 * time.Minute
const advisoryLockIDSalt uint32 = 1486364155

var migrationFilename = regexp.MustCompile(`^(\d+)_([a-zA-Z0-9][a-zA-Z0-9_.-]*)\.up\.sql$`)

// ControlledOptions identifies one reviewed, forward-only migration.
type ControlledOptions struct {
	ExpectedVersion  uint
	TargetVersion    uint
	SHA256           string
	LockTimeout      time.Duration
	StatementTimeout time.Duration
}

// ControlledMigrate applies exactly one embedded migration atomically. SQL
// comes from the checked-in embedded filesystem; callers cannot supply SQL or
// a filesystem path.
func ControlledMigrate(ctx context.Context, dsn string, files fs.FS, opts ControlledOptions) error {
	if strings.TrimSpace(dsn) == "" || files == nil {
		return errors.New("controlled migration requires a database connection and embedded migrations")
	}
	if opts.TargetVersion <= opts.ExpectedVersion {
		return errors.New("target version must be greater than expected version")
	}
	if len(opts.SHA256) != sha256.Size*2 {
		return errors.New("expected SHA-256 must be 64 hexadecimal characters")
	}
	wantHash, err := hex.DecodeString(opts.SHA256)
	if err != nil {
		return errors.New("expected SHA-256 must be 64 hexadecimal characters")
	}
	if opts.LockTimeout <= 0 || opts.LockTimeout > MaxControlledLockTimeout || opts.StatementTimeout <= 0 || opts.StatementTimeout > MaxControlledStatementTimeout {
		return errors.New("lock timeout must be at most 5 minutes and statement timeout at most 30 minutes")
	}
	steps, err := migrationSteps(files)
	if err != nil {
		return err
	}
	stepIndex := -1
	for i, step := range steps {
		if step.version == opts.TargetVersion {
			stepIndex = i
			break
		}
	}
	if stepIndex < 0 {
		return errors.New("target must be the immediate next checked-in migration after the expected version")
	}
	if (stepIndex == 0 && opts.ExpectedVersion != 0) || (stepIndex > 0 && steps[stepIndex-1].version != opts.ExpectedVersion) {
		return errors.New("target must be the immediate next checked-in migration after the expected version")
	}
	raw, err := fs.ReadFile(files, steps[stepIndex].name)
	if err != nil {
		return errors.New("unable to read checked-in migration")
	}
	actualHash := sha256.Sum256(raw)
	if !equalBytes(actualHash[:], wantHash) {
		return errors.New("migration SHA-256 does not match the reviewed checksum")
	}
	statements, err := splitMigrationSQL(string(raw))
	if err != nil {
		return errors.New("checked-in migration has unsupported SQL transaction control")
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return errors.New("unable to connect to migration database")
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var databaseName, schemaName string
	if err := conn.QueryRow(ctx, "SELECT current_database(), current_schema()").Scan(&databaseName, &schemaName); err != nil || databaseName == "" || schemaName == "" {
		return errors.New("unable to determine migration database schema")
	}
	lockID, _ := database.GenerateAdvisoryLockId(databaseName, schemaName, "schema_migrations")
	var lockIDInt64 int64
	if _, err := fmt.Sscan(lockID, &lockIDInt64); err != nil {
		return errors.New("unable to prepare migration lock")
	}
	if _, err := conn.Exec(ctx, "SELECT set_config('statement_timeout', $1, false)", opts.LockTimeout.String()); err != nil {
		return errors.New("unable to configure migration lock timeout")
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockIDInt64); err != nil {
		return errors.New("unable to acquire migration lock before timeout")
	}
	defer func() { _, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", lockIDInt64) }()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return errors.New("unable to start migration transaction")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('statement_timeout', $1, true), set_config('lock_timeout', $2, true)", opts.StatementTimeout.String(), opts.LockTimeout.String()); err != nil {
		return errors.New("unable to configure migration timeouts")
	}
	quotedSchema := pgx.Identifier{schemaName}.Sanitize()
	var relation *string
	if err := tx.QueryRow(ctx, "SELECT to_regclass($1)::text", pgx.Identifier{schemaName, "schema_migrations"}.Sanitize()).Scan(&relation); err != nil {
		return errors.New("unable to inspect migration version state")
	}
	if relation == nil {
		if opts.ExpectedVersion != 0 {
			return errors.New("database version does not match the expected starting version")
		}
		if _, err := tx.Exec(ctx, "CREATE TABLE "+quotedSchema+".schema_migrations (version bigint NOT NULL, dirty boolean NOT NULL)"); err != nil {
			return errors.New("unable to initialize migration version state")
		}
		if _, err := tx.Exec(ctx, "INSERT INTO "+quotedSchema+".schema_migrations (version, dirty) VALUES (0, false)"); err != nil {
			return errors.New("unable to initialize migration version state")
		}
	}
	if _, err := tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+quotedSchema+"."+controlledHistoryTable+" (from_version bigint NOT NULL, to_version bigint PRIMARY KEY, sql_sha256 text NOT NULL CHECK (sql_sha256 ~ '^[0-9a-f]{64}$'), applied_at timestamptz NOT NULL DEFAULT now())"); err != nil {
		return errors.New("unable to prepare controlled migration history")
	}
	var rowCount int
	var current int64
	var dirty bool
	if err := tx.QueryRow(ctx, "SELECT count(*), COALESCE(min(version), 0), COALESCE(bool_or(dirty), false) FROM "+quotedSchema+".schema_migrations").Scan(&rowCount, &current, &dirty); err != nil {
		return errors.New("unable to read migration version state")
	}
	if rowCount != 1 || current < 0 {
		return errors.New("database migration version state is invalid")
	}
	if dirty {
		return errors.New("database migration state is dirty; automatic repair is prohibited")
	}
	if current == int64(opts.TargetVersion) {
		var fromVersion uint64
		var storedHash string
		if err := tx.QueryRow(ctx, "SELECT from_version, sql_sha256 FROM "+quotedSchema+"."+controlledHistoryTable+" WHERE to_version = $1", opts.TargetVersion).Scan(&fromVersion, &storedHash); err != nil || fromVersion != uint64(opts.ExpectedVersion) || storedHash != strings.ToLower(opts.SHA256) {
			return errors.New("target version is already applied without matching controlled migration history")
		}
		if err := tx.Commit(ctx); err != nil {
			return errors.New("unable to finish idempotent migration replay")
		}
		return nil
	}
	if current != int64(opts.ExpectedVersion) {
		return errors.New("database version does not match the expected starting version")
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return errors.New("controlled migration SQL failed; transaction rolled back")
		}
	}
	if _, err := tx.Exec(ctx, "UPDATE "+quotedSchema+".schema_migrations SET version = $1, dirty = false", opts.TargetVersion); err != nil {
		return errors.New("unable to update migration version state")
	}
	if _, err := tx.Exec(ctx, "INSERT INTO "+quotedSchema+"."+controlledHistoryTable+" (from_version, to_version, sql_sha256) VALUES ($1, $2, $3)", opts.ExpectedVersion, opts.TargetVersion, strings.ToLower(opts.SHA256)); err != nil {
		return errors.New("unable to persist controlled migration checksum")
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.New("controlled migration commit failed")
	}
	return nil
}

type controlledStep struct {
	version uint
	name    string
}

func migrationSteps(files fs.FS) ([]controlledStep, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, errors.New("unable to list checked-in migrations")
	}
	steps := make([]controlledStep, 0, len(entries))
	seen := map[uint]bool{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		match := migrationFilename.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		v, err := strconv.ParseUint(match[1], 10, 32)
		if err != nil || seen[uint(v)] {
			return nil, errors.New("checked-in migration versions are invalid or duplicated")
		}
		seen[uint(v)] = true
		steps = append(steps, controlledStep{version: uint(v), name: entry.Name()})
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].version < steps[j].version })
	if len(steps) == 0 {
		return nil, errors.New("no checked-in migrations are available")
	}
	return steps, nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// splitMigrationSQL removes only a complete outer BEGIN/COMMIT wrapper and
// splits statements while respecting SQL strings, comments, identifiers, and
// dollar-quoted procedural bodies.
func splitMigrationSQL(sql string) ([]string, error) {
	stmts, err := sqlStatements(sql)
	if err != nil {
		return nil, err
	}
	if len(stmts) == 0 {
		return nil, errors.New("empty migration")
	}
	firstWord, firstRest := firstSQLWord(stmts[0])
	lastWord, lastRest := firstSQLWord(stmts[len(stmts)-1])
	wrapped := firstWord == "BEGIN" && firstRest == "" && lastWord == "COMMIT" && lastRest == ""
	if (firstWord == "BEGIN" && firstRest == "") != (lastWord == "COMMIT" && lastRest == "") {
		return nil, errors.New("incomplete outer transaction wrapper")
	}
	if wrapped {
		stmts = stmts[1 : len(stmts)-1]
	}
	for _, stmt := range stmts {
		word, _ := firstSQLWord(stmt)
		if word == "BEGIN" || word == "COMMIT" || word == "ROLLBACK" || word == "START" || word == "END" {
			return nil, errors.New("nested transaction control is unsupported")
		}
	}
	if len(stmts) == 0 {
		return nil, errors.New("empty migration body")
	}
	return stmts, nil
}

func firstSQLWord(s string) (string, string) {
	for {
		s = strings.TrimLeft(s, " \r\n\t;")
		if strings.HasPrefix(s, "--") {
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				s = s[i+1:]
				continue
			}
			return "", ""
		}
		if strings.HasPrefix(s, "/*") {
			depth, i := 1, 2
			for i < len(s) && depth > 0 {
				if strings.HasPrefix(s[i:], "/*") {
					depth++
					i += 2
					continue
				}
				if strings.HasPrefix(s[i:], "*/") {
					depth--
					i += 2
					continue
				}
				i++
			}
			if depth != 0 {
				return "", ""
			}
			s = s[i:]
			continue
		}
		break
	}
	i := 0
	for i < len(s) && (s[i] >= 'a' && s[i] <= 'z' || s[i] >= 'A' && s[i] <= 'Z' || s[i] == '_') {
		i++
	}
	if i == 0 {
		return "", strings.TrimSpace(s)
	}
	return strings.ToUpper(s[:i]), strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s[i:]), ";"))
}

func sqlStatements(sql string) ([]string, error) {
	var out []string
	start := 0
	state := byte(0)
	blockDepth := 0
	dollar := ""
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if dollar != "" {
			if strings.HasPrefix(sql[i:], dollar) {
				i += len(dollar) - 1
				dollar = ""
			}
			continue
		}
		if state == 'e' {
			if c == '\\' {
				i++
				continue
			}
			if c == '\'' {
				if i+1 < len(sql) && sql[i+1] == '\'' {
					i++
					continue
				}
				state = 0
			}
			continue
		}
		if state == '\'' || state == '"' {
			if c == state {
				if i+1 < len(sql) && sql[i+1] == state {
					i++
					continue
				}
				state = 0
			}
			continue
		}
		if blockDepth > 0 {
			if i+1 < len(sql) && sql[i:i+2] == "/*" {
				blockDepth++
				i++
				continue
			}
			if i+1 < len(sql) && sql[i:i+2] == "*/" {
				blockDepth--
				i++
				continue
			}
			continue
		}
		if state == '-' {
			if c == '\n' {
				state = 0
			}
			continue
		}
		if i+1 < len(sql) && sql[i:i+2] == "--" {
			state = '-'
			i++
			continue
		}
		if i+1 < len(sql) && sql[i:i+2] == "/*" {
			blockDepth = 1
			i++
			continue
		}
		if c == '\'' {
			if i > 0 && (sql[i-1] == 'e' || sql[i-1] == 'E') && (i < 2 || !isSQLIdentifierByte(sql[i-2])) {
				state = 'e'
			} else {
				state = '\''
			}
			continue
		}
		if c == '"' {
			state = '"'
			continue
		}
		if c == '$' {
			j := i + 1
			for j < len(sql) && (sql[j] == '_' || sql[j] >= 'a' && sql[j] <= 'z' || sql[j] >= 'A' && sql[j] <= 'Z' || sql[j] >= '0' && sql[j] <= '9') {
				j++
			}
			if j < len(sql) && sql[j] == '$' {
				dollar = sql[i : j+1]
				i = j
				continue
			}
		}
		if c == ';' {
			part := strings.TrimSpace(sql[start : i+1])
			if strings.Trim(part, "; \r\n\t") != "" {
				out = append(out, part)
			}
			start = i + 1
		}
	}
	if state != 0 && state != '-' || blockDepth != 0 || dollar != "" {
		return nil, errors.New("unterminated SQL quote or comment")
	}
	if tail := strings.TrimSpace(sql[start:]); strings.Trim(tail, "; \r\n\t") != "" {
		if word, _ := firstSQLWord(tail); word != "" {
			return nil, errors.New("migration statement must end with a semicolon")
		}
	}
	return out, nil
}

func isSQLIdentifierByte(b byte) bool {
	return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '$'
}
