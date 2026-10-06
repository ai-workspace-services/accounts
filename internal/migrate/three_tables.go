package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	accountschema "account/sql"
	"github.com/google/uuid"
)

// ColumnDefinition records the actual source catalog, not the exporter's schema.
type ColumnDefinition struct {
	Name      string  `yaml:"name"`
	Type      string  `yaml:"type"`
	Nullable  bool    `yaml:"nullable"`
	Default   *string `yaml:"default"`
	Generated bool    `yaml:"generated"`
}

type ThreeTableSnapshot struct {
	SourceIdentity string                        `yaml:"sourceIdentity"`
	Columns        map[string][]ColumnDefinition `yaml:"columns"`
	Rows           map[string][]string           `yaml:"rows"`
}

var accountTables = []string{"users", "identities", "sessions"}

// Deliberately explicit: a new service column must be reviewed before transport.
var transportColumns = map[string]string{
	"users":      "uuid username password email role level groups permissions created_at updated_at version origin_node mfa_totp_secret mfa_enabled mfa_secret_issued_at mfa_confirmed_at email_verified_at email_verified active proxy_uuid proxy_uuid_expires_at subscription_valid_from subscription_valid_until last_active_at archived_at",
	"identities": "uuid provider external_id user_uuid created_at updated_at version origin_node",
	"sessions":   "uuid token expires_at user_uuid created_at updated_at version origin_node",
}

func catalogColumns(ctx context.Context, tx *sql.Tx, table string) ([]ColumnDefinition, error) {
	rows, err := tx.QueryContext(ctx, `SELECT a.attname, format_type(a.atttypid,a.atttypmod), NOT a.attnotnull,
pg_get_expr(d.adbin,d.adrelid), a.attgenerated <> ''
FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace
LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
WHERE n.nspname='public' AND c.relname=$1 AND c.relkind='r' AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum`, table)
	if err != nil {
		return nil, errors.New("cannot read three-table column catalog")
	}
	defer rows.Close()
	var columns []ColumnDefinition
	for rows.Next() {
		var c ColumnDefinition
		var def sql.NullString
		if err := rows.Scan(&c.Name, &c.Type, &c.Nullable, &def, &c.Generated); err != nil {
			return nil, errors.New("cannot decode column catalog")
		}
		if def.Valid {
			c.Default = &def.String
		}
		columns = append(columns, c)
	}
	if rows.Err() != nil || len(columns) == 0 {
		return nil, fmt.Errorf("missing or unreadable public.%s", table)
	}
	return columns, nil
}

// ExportAccountsOnly reads all three tables from one read-only MVCC snapshot.
// There is no email filter: filtering could hide orphans and partial baselines.
func (e *Exporter) ExportAccountsOnly(ctx context.Context, dsn string) (*AccountDump, error) {
	db, err := openDB(ctx, dsn)
	if err != nil {
		return nil, errors.New("cannot connect to accounts-only source")
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, errors.New("cannot start read-only source snapshot")
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL row_security = off`); err != nil {
		return nil, errors.New("cannot enforce complete source visibility")
	}
	snapshot := &ThreeTableSnapshot{Columns: map[string][]ColumnDefinition{}, Rows: map[string][]string{}}
	snapshot.SourceIdentity, err = databaseIdentity(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, table := range accountTables {
		cols, err := catalogColumns(ctx, tx, table)
		if err != nil {
			return nil, err
		}
		snapshot.Columns[table] = cols
		rows, err := tx.QueryContext(ctx, `SELECT to_jsonb(t)::text FROM public.`+table+` t ORDER BY uuid`)
		if err != nil {
			return nil, fmt.Errorf("cannot read public.%s", table)
		}
		snapshot.Rows[table] = []string{}
		for rows.Next() {
			var row string
			if rows.Scan(&row) != nil {
				rows.Close()
				return nil, fmt.Errorf("cannot decode public.%s", table)
			}
			snapshot.Rows[table] = append(snapshot.Rows[table], row)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("incomplete public.%s export", table)
		}
	}
	if _, err := validateThreeTableRows(snapshot); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, errors.New("source snapshot failed")
	}
	return &AccountDump{Metadata: &SnapshotMetadata{Version: "accounts-only-v1", SchemaHash: accountschema.Hash(), ExportedAt: time.Now().UTC()}, ThreeTables: snapshot}, nil
}

type rawRow map[string]json.RawMessage

func rowString(row rawRow, key string) string {
	var s string
	_ = json.Unmarshal(row[key], &s)
	return s
}

func validateThreeTableRows(s *ThreeTableSnapshot) (map[string][]rawRow, error) {
	if s == nil || len(s.Columns) != 3 || len(s.Rows) != 3 {
		return nil, errors.New("accounts-only snapshot must contain exactly three tables and their catalogs")
	}
	parsed := map[string][]rawRow{}
	users := map[string]bool{}
	for _, table := range accountTables {
		cols, ok := s.Columns[table]
		if !ok || len(cols) == 0 {
			return nil, fmt.Errorf("missing %s catalog", table)
		}
		rows, ok := s.Rows[table]
		if !ok {
			return nil, fmt.Errorf("missing %s rows (empty must be explicit)", table)
		}
		known := map[string]bool{}
		for _, c := range cols {
			if known[c.Name] || !strings.Contains(" "+transportColumns[table]+" ", " "+c.Name+" ") {
				return nil, fmt.Errorf("unmapped or duplicate column %s.%s", table, c.Name)
			}
			known[c.Name] = true
		}
		required := map[string][]string{"users": {"uuid", "username", "password", "email", "role", "level", "groups", "permissions", "active", "proxy_uuid"}, "identities": {"uuid", "provider", "external_id", "user_uuid"}, "sessions": {"uuid", "token", "expires_at", "user_uuid"}}
		for _, key := range required[table] {
			if !known[key] {
				return nil, fmt.Errorf("missing semantic column %s.%s; explicit mapping review required", table, key)
			}
		}
		ids, unique := map[string]bool{}, map[string]bool{}
		for index, text := range rows {
			var row rawRow
			if json.Unmarshal([]byte(text), &row) != nil || len(row) != len(known) {
				return nil, fmt.Errorf("invalid %s row %d or column coverage", table, index)
			}
			for key := range row {
				if !known[key] {
					return nil, fmt.Errorf("unmapped %s row column", table)
				}
			}
			for key := range known {
				if _, ok := row[key]; !ok {
					return nil, fmt.Errorf("missing %s row column", table)
				}
			}
			id := rowString(row, "uuid")
			u, err := uuid.Parse(id)
			if err != nil || u.String() != id || ids[id] {
				return nil, fmt.Errorf("invalid or duplicate %s UUID at row %d", table, index)
			}
			ids[id] = true
			var natural string
			switch table {
			case "users":
				users[id] = true
				for _, key := range []string{"username", "email"} {
					v := strings.ToLower(rowString(row, key))
					if v != "" {
						k := key + ":" + v
						if unique[k] {
							return nil, fmt.Errorf("duplicate users %s at row %d", key, index)
						}
						unique[k] = true
					}
				}
				if _, err := uuid.Parse(rowString(row, "proxy_uuid")); err != nil {
					return nil, fmt.Errorf("invalid users proxy UUID at row %d", index)
				}
				if v, ok := row["email_verified"]; ok {
					var verified bool
					if json.Unmarshal(v, &verified) != nil {
						return nil, errors.New("invalid email verification flag")
					}
					at, exists := row["email_verified_at"]
					if verified != (exists && string(at) != "null") {
						return nil, errors.New("email verification cannot be preserved without a consistent email_verified_at")
					}
				}
			case "identities":
				natural = rowString(row, "provider") + "\x00" + rowString(row, "external_id")
			case "sessions":
				natural = rowString(row, "token")
				if natural == "" {
					return nil, fmt.Errorf("invalid session token at row %d", index)
				}
			}
			if table != "users" {
				if !users[rowString(row, "user_uuid")] {
					return nil, fmt.Errorf("orphan %s FK at row %d", table, index)
				}
				if unique[natural] {
					return nil, fmt.Errorf("duplicate %s natural key at row %d", table, index)
				}
				unique[natural] = true
			}
			parsed[table] = append(parsed[table], row)
		}
	}
	return parsed, nil
}

func importThreeTables(ctx context.Context, dsn string, dump *AccountDump, opts ImportOptions) (*ImportReport, error) {
	if !strings.HasPrefix(opts.TargetDatabase, "accounts_rebuild_") || len(opts.TargetDatabase) <= len("accounts_rebuild_") {
		return nil, errors.New("accounts-only requires explicit independent --target-database accounts_rebuild_<id>")
	}
	if opts.Merge || opts.MergeStrategy != "" || opts.RegenerateUserUUIDs || opts.SkipSessions || opts.PreserveExistingUsers || len(opts.Allowlist) > 0 {
		return nil, errors.New("accounts-only forbids merge, rekey, skip-sessions and partial allowlists")
	}
	if dump == nil || dump.Metadata == nil || dump.Metadata.Version != "accounts-only-v1" || dump.Metadata.SchemaHash != accountschema.Hash() || len(dump.Users)+len(dump.Identities)+len(dump.Sessions) > 0 {
		return nil, errors.New("invalid accounts-only format or release schema binding")
	}
	parsed, err := validateThreeTableRows(dump.ThreeTables)
	if err != nil {
		return nil, err
	}
	db, err := openDB(ctx, dsn)
	if err != nil {
		return nil, errors.New("cannot connect to accounts-only target")
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, errors.New("cannot start target transaction")
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `LOCK TABLE public.users,public.identities,public.sessions IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return nil, errors.New("cannot lock accounts-only target")
	}
	var actual string
	if tx.QueryRowContext(ctx, `SELECT current_database()`).Scan(&actual) != nil || actual != opts.TargetDatabase {
		return nil, errors.New("independent target database identity mismatch")
	}
	identity, err := databaseIdentity(ctx, tx)
	if err != nil {
		return nil, err
	}
	if dump.ThreeTables.SourceIdentity == "" || identity == dump.ThreeTables.SourceIdentity {
		return nil, errors.New("missing source identity or source equals target")
	}
	report := &ImportReport{}
	counts := map[string]int{}
	anyRows := false
	for _, table := range accountTables {
		var countsValue int
		if tx.QueryRowContext(ctx, `SELECT count(*) FROM public.`+table).Scan(&countsValue) != nil {
			return nil, fmt.Errorf("cannot count target %s", table)
		}
		counts[table] = countsValue
		anyRows = anyRows || countsValue > 0
	}
	if anyRows {
		for _, table := range accountTables {
			if counts[table] != len(parsed[table]) {
				return nil, fmt.Errorf("partial or unrelated target %s; refusing repair/merge", table)
			}
		}
	}
	for _, table := range accountTables {
		target, err := catalogColumns(ctx, tx, table)
		if err != nil {
			return nil, err
		}
		targetByName := map[string]ColumnDefinition{}
		for _, c := range target {
			targetByName[c.Name] = c
		}
		for _, c := range dump.ThreeTables.Columns[table] {
			t, ok := targetByName[c.Name]
			if !ok {
				return nil, fmt.Errorf("unmapped target column %s.%s", table, c.Name)
			}
			if c.Type != t.Type {
				return nil, fmt.Errorf("type mapping review required for %s.%s", table, c.Name)
			}
		}
		for _, c := range target {
			if !strings.Contains(" "+transportColumns[table]+" ", " "+c.Name+" ") {
				return nil, fmt.Errorf("unreviewed target column %s.%s", table, c.Name)
			}
		}
		count := counts[table]
		if count != 0 && count != len(parsed[table]) {
			return nil, fmt.Errorf("partial or unrelated target %s; refusing repair/merge", table)
		}
		for index, row := range parsed[table] {
			keys := make([]string, 0, len(row))
			for key := range row {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			columns, values, predicates := []string{}, []string{}, []string{}
			args := []any{}
			for _, key := range keys {
				c := targetByName[key]
				if c.Generated {
					continue
				}
				if string(row[key]) == "null" && !c.Nullable {
					return nil, fmt.Errorf("explicit NULL cannot use default: %s.%s row %d", table, key, index)
				}
				var arg any
				if string(row[key]) != "null" {
					var text string
					if json.Unmarshal(row[key], &text) == nil {
						arg = text
					} else {
						arg = string(row[key])
					}
				}
				args = append(args, arg)
				expr := fmt.Sprintf("$%d::%s", len(args), c.Type)
				columns = append(columns, `"`+key+`"`)
				values = append(values, expr)
				predicates = append(predicates, `"`+key+`" IS NOT DISTINCT FROM `+expr)
			}
			for _, c := range target {
				if _, present := row[c.Name]; !present && !c.Nullable && c.Default == nil && !c.Generated {
					return nil, fmt.Errorf("missing required target column %s.%s", table, c.Name)
				}
			}
			// Read-only casts validate actual PostgreSQL types without executing
			// INSERT, UPDATE or DELETE, including in dry-run mode.
			var castsValid bool
			checks := make([]string, len(values))
			for j, value := range values {
				checks[j] = "(" + value + " IS NULL OR " + value + " IS NOT NULL)"
			}
			if tx.QueryRowContext(ctx, "SELECT "+strings.Join(checks, " AND "), args...).Scan(&castsValid) != nil {
				return nil, fmt.Errorf("invalid PostgreSQL field type in %s row %d (details suppressed)", table, index)
			}
			if count > 0 {
				var same bool
				if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.`+table+` WHERE `+strings.Join(predicates, " AND ")+`)`, args...).Scan(&same) != nil {
					return nil, fmt.Errorf("cannot compare %s row %d", table, index)
				}
				if !same {
					return nil, fmt.Errorf("conflicting %s row %d; no overwrite permitted", table, index)
				}
				if table == "users" {
					report.UsersSkipped++
				}
				continue
			}
			if !opts.DryRun {
				if _, err := tx.ExecContext(ctx, `INSERT INTO public.`+table+` (`+strings.Join(columns, ",")+`) VALUES (`+strings.Join(values, ",")+`)`, args...); err != nil {
					return nil, fmt.Errorf("INSERT rejected for %s row %d; transaction rolled back (details suppressed)", table, index)
				}
			}
			switch table {
			case "users":
				report.UsersInserted++
			case "identities":
				report.IdentitiesInserted++
			case "sessions":
				report.SessionsInserted++
			}
		}
	}
	if opts.DryRun {
		return report, nil
	}
	if tx.Commit() != nil {
		return nil, errors.New("accounts-only commit failed; verify target before retry")
	}
	return report, nil
}

func databaseIdentity(ctx context.Context, tx *sql.Tx) (string, error) {
	var identity string
	if tx.QueryRowContext(ctx, `SELECT current_database() || ':' || coalesce(inet_server_addr()::text,'local-socket') || ':' || coalesce(inet_server_port()::text,'local-socket')`).Scan(&identity) != nil {
		return "", errors.New("database identity query failed")
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(identity))), nil
}
