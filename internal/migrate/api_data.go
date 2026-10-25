package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const MigrationAPIScope = "identity-billing-v1"
const MigrationAPIMaxBytes = 64 << 20

type MigrationAuthorization struct {
	SourceAudience string `json:"source_audience"`
	TargetAudience string `json:"target_audience"`
	JobID          string `json:"job_id"`
	SubjectSHA256  string `json:"subject_sha256"`
}

// This list is reviewed, not inferred from database contents. FK parents are
// checked by businessOrder; tokens, sessions and challenges are never included.
var migrationAPITables = strings.Fields(`users identities mfa_recovery_codes account_lifecycle_events
 subscriptions subscription_records subscription_lifecycle_states billing_plans
 account_billing_profiles account_quota_states account_policy_snapshots billing_ledger
 billing_source_sync_state traffic_minute_buckets traffic_stat_checkpoints stripe_webhook_events
 finance_invoices finance_payments finance_refunds finance_operations finance_operation_events
 referral_verified_invoices referral_campaign_revisions referral_reward_campaign_snapshots
 referral_codes referrals referral_rewards referral_reward_events referral_payment_risks
 referral_settlements referral_settlement_items subscription_extension_ledger
 network_teams network_team_members team_agent_enrollments
 audit_logs tenants tenant_domains tenant_memberships rbac_roles rbac_permissions rbac_role_permissions`)

type MigrationManifest struct {
	Chunks               []MigrationChunk            `json:"chunks,omitempty"`
	ToolSHA256           string                      `json:"tool_sha256"`
	Authorization        MigrationAuthorization      `json:"authorization"`
	Format               int                         `json:"format"`
	Scope                string                      `json:"scope"`
	SnapshotID           string                      `json:"snapshot_id"`
	SourceIdentitySHA256 string                      `json:"source_identity_sha256"`
	SourceSchemaSHA256   string                      `json:"source_schema_sha256"`
	SnapshotSHA256       string                      `json:"snapshot_sha256"`
	CreatedAt            time.Time                   `json:"created_at"`
	ExpiresAt            time.Time                   `json:"expires_at"`
	Tables               map[string]BusinessEquality `json:"tables"`
	Columns              map[string][]businessColumn `json:"columns"`
}

type MigrationSnapshot struct {
	Manifest MigrationManifest   `json:"manifest"`
	Rows     map[string][]rawRow `json:"rows"`
}

type MigrationPlan struct {
	ToolSHA256           string            `json:"tool_sha256"`
	PlanID               string            `json:"plan_id"`
	PlanSHA256           string            `json:"plan_sha256"`
	State                string            `json:"state"`
	Snapshot             MigrationManifest `json:"snapshot"`
	TargetIdentitySHA256 string            `json:"target_identity_sha256"`
	TargetSchemaSHA256   string            `json:"target_schema_sha256"`
	TargetBaselineSHA256 string            `json:"target_baseline_sha256"`
	InsertRows           int               `json:"insert_rows"`
	MatchingRows         int               `json:"matching_rows"`
	ConflictRows         int               `json:"conflict_rows"`
	CreatedAt            time.Time         `json:"created_at"`
	ExpiresAt            time.Time         `json:"expires_at"`
}

func migrationDigest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

var migrationExecutableOnce sync.Once
var migrationExecutableHash string

func migrationExecutableSHA256() string {
	migrationExecutableOnce.Do(func() {
		path, err := os.Executable()
		if err != nil {
			return
		}
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		hash := sha256.New()
		if _, err = io.Copy(hash, f); err == nil {
			migrationExecutableHash = hex.EncodeToString(hash.Sum(nil))
		}
	})
	return migrationExecutableHash
}

func migrationCatalog() (map[string]businessTable, []string, error) {
	all, _, err := fullBusinessContract()
	if err != nil {
		return nil, nil, err
	}
	tables := map[string]businessTable{}
	for _, name := range migrationAPITables {
		tables[name] = all[name]
	}
	order, err := businessOrder(tables)
	return tables, order, err
}

func migrationBegin(ctx context.Context, db *sql.DB, readonly bool) (*sql.Tx, error) {
	isolation := sql.LevelReadCommitted
	if readonly {
		isolation = sql.LevelRepeatableRead
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: isolation, ReadOnly: readonly})
	if err != nil {
		return nil, errors.New("migration transaction unavailable")
	}
	_, err = tx.ExecContext(ctx, `SET LOCAL search_path=pg_catalog,public; SET LOCAL timezone='UTC'; SET LOCAL datestyle='ISO,YMD'; SET LOCAL extra_float_digits=3; SET LOCAL statement_timeout='2min'; SET LOCAL lock_timeout='10s'; SET LOCAL idle_in_transaction_session_timeout='3min'`)
	if err != nil {
		tx.Rollback()
		return nil, errors.New("migration transaction budgets unavailable")
	}
	return tx, nil
}

// Physical cluster + database OID survives DNS aliases, PgBouncer and role changes.
// Refuse when privileges cannot prove identity rather than trusting a caller ID.
func migrationIdentity(ctx context.Context, tx *sql.Tx) (string, error) {
	var cluster, database string
	err := tx.QueryRowContext(ctx, `SELECT system_identifier::text, (SELECT oid::text FROM pg_database WHERE datname=current_database()) FROM pg_control_system()`).Scan(&cluster, &database)
	if err != nil || cluster == "" || database == "" {
		return "", errors.New("physical database identity unavailable")
	}
	return migrationDigest([]string{cluster, database}), nil
}

// Bind actual defaults, constraints, indexes, policies, triggers and function
// bodies as well as migration history. A checked-in SQL checksum is insufficient.
func migrationSchema(ctx context.Context, tx *sql.Tx, order []string) (string, error) {
	var pgVersion, count, version int
	var dirty bool
	if tx.QueryRowContext(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&pgVersion) != nil || pgVersion < 170000 || pgVersion >= 180000 {
		return "", errors.New("migration API requires PostgreSQL17")
	}
	if tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(min(version),0),COALESCE(bool_or(dirty),true) FROM public.schema_migrations`).Scan(&count, &version, &dirty) != nil || count != 1 || dirty {
		return "", errors.New("migration API requires a clean reviewed schema checkpoint")
	}
	if err := verifyCatalogCheckpoint(ctx, tx, uint(version)); err != nil {
		return "", err
	}
	facts := map[string]any{"tool_contract": migrationDigest(fullBusinessContractJSON), "scope": MigrationAPIScope}
	for _, name := range order {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+name).Scan(&exists); err != nil {
			return "", errors.New("schema unavailable")
		}
		if !exists {
			facts[name] = nil
			continue
		}
		cols, err := catalogColumns(ctx, tx, name)
		if err != nil {
			return "", err
		}
		facts[name] = cols
	}
	queries := []string{
		`SELECT jsonb_build_array(c.relname,c.relkind,c.relrowsecurity,c.relforcerowsecurity)::text FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p','v','m','f') ORDER BY c.relname`,
		`SELECT jsonb_build_array(c.conrelid::regclass::text,c.conname,pg_get_constraintdef(c.oid),c.convalidated)::text FROM pg_constraint c JOIN pg_namespace n ON n.oid=c.connamespace WHERE n.nspname='public' ORDER BY c.conrelid::regclass::text,c.conname`,
		`SELECT jsonb_build_array(tablename,indexname,indexdef)::text FROM pg_indexes WHERE schemaname='public' ORDER BY tablename,indexname`,
		`SELECT jsonb_build_array(t.tgrelid::regclass::text,t.tgname,t.tgenabled,pg_get_triggerdef(t.oid))::text FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND NOT t.tgisinternal ORDER BY t.tgrelid::regclass::text,t.tgname`,
		// A superset of reachable functions covers PL/pgSQL/dynamic dependencies
		// across schemas, including helpers referenced by triggers/defaults/RLS.
		`SELECT jsonb_build_array(n.nspname,p.proname,pg_get_function_identity_arguments(p.oid),pg_get_functiondef(p.oid))::text FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE p.prokind IN ('f','p') ORDER BY n.nspname,p.proname,pg_get_function_identity_arguments(p.oid)`,
		`SELECT to_jsonb(p)::text FROM pg_policies p WHERE schemaname='public' ORDER BY tablename,policyname`,
		`SELECT to_jsonb(m)::text FROM public.schema_migrations m ORDER BY version`,
	}
	for i, query := range queries {
		rows, err := tx.QueryContext(ctx, query)
		if err != nil {
			return "", errors.New("schema fingerprint unavailable")
		}
		var values []string
		for rows.Next() {
			var value string
			if err = rows.Scan(&value); err != nil {
				break
			}
			values = append(values, value)
		}
		endErr := rows.Err()
		rows.Close()
		if err != nil || endErr != nil {
			return "", errors.New("schema fingerprint incomplete")
		}
		facts[fmt.Sprint(i)] = values
	}
	for _, table := range []string{controlledHistoryTable, "system_release_checkpoints"} {
		var exists bool
		if tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+table).Scan(&exists) != nil {
			return "", errors.New("migration history unavailable")
		}
		if exists {
			var history string
			if tx.QueryRowContext(ctx, `SELECT COALESCE(jsonb_agg(v ORDER BY v::text),'[]')::text FROM (SELECT to_jsonb(t) v FROM public.`+quoteBusiness(table)+` t) s`).Scan(&history) != nil {
				return "", errors.New("migration history unreadable")
			}
			facts[table] = history
		}
	}
	return migrationDigest(facts), nil
}

func migrationRows(ctx context.Context, tx *sql.Tx, source bool) (map[string][]rawRow, map[string]BusinessEquality, error) {
	return migrationReadPages(ctx, tx, source, nil)
}

func migrationReadPages(ctx context.Context, tx *sql.Tx, source bool, sink func(string, []rawRow) error) (map[string][]rawRow, map[string]BusinessEquality, error) {
	tables, order, err := migrationCatalog()
	if err != nil {
		return nil, nil, err
	}
	data := map[string][]rawRow{}
	digests := map[string]BusinessEquality{}
	users, err := businessUsers(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	mapping := map[string]string{}
	for _, user := range users {
		mapping[user.ID] = user.ID
	}
	size := 0
	for _, name := range order {
		table := tables[name]
		var exists bool
		if tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+name).Scan(&exists) != nil {
			return nil, nil, errors.New("table unavailable")
		}
		digest := businessDigest{}
		data[name] = []rawRow{}
		if !exists {
			if !source || !optionalBusinessTables[name] {
				return nil, nil, fmt.Errorf("required public.%s missing", name)
			}
		} else {
			cols, err := catalogColumns(ctx, tx, name)
			if err != nil {
				return nil, nil, err
			}
			if source {
				err = businessSourceVisibility(ctx, tx, name)
				if err == nil {
					err = validateBusinessSourceColumns(name, cols, table)
				}
			} else {
				err = validateBusinessTarget(ctx, tx, name, table)
			}
			if err != nil {
				return nil, nil, err
			}
			stream := streamBusiness
			if sink != nil {
				stream = streamMigrationPages
			}
			err = stream(ctx, tx, name, table.PrimaryKey, func(page []rawRow) error {
				for _, row := range page {
					if source {
						if err := projectBusinessRow(name, row, cols, table, mapping); err != nil {
							return err
						}
					}
					if err := digest.add(row, table); err != nil {
						return err
					}
					b, err := json.Marshal(row)
					if err != nil {
						return err
					}
					size += len(b)
					if sink == nil && size > MigrationAPIMaxBytes/2 {
						return errors.New("legacy in-memory snapshot budget exceeded; use chunk API")
					}
					if sink == nil {
						data[name] = append(data[name], row)
					}
				}
				if sink != nil {
					return sink(name, page)
				}
				return nil
			})
			if err != nil {
				return nil, nil, err
			}
		}
		digests[name], err = digest.finish()
		if err != nil {
			return nil, nil, err
		}
	}
	return data, digests, nil
}

func CaptureMigrationSnapshot(ctx context.Context, db *sql.DB, id string, lifetime time.Duration, authorization MigrationAuthorization) (MigrationSnapshot, error) {
	return captureMigrationPages(ctx, db, id, lifetime, authorization, nil)
}

func captureMigrationPages(ctx context.Context, db *sql.DB, id string, lifetime time.Duration, authorization MigrationAuthorization, sink func(string, []rawRow) error) (MigrationSnapshot, error) {
	var result MigrationSnapshot
	tx, err := migrationBegin(ctx, db, true)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL default_transaction_read_only=on`); err != nil {
		return result, errors.New("source readonly guard unavailable")
	}
	if err = businessSourceRole(ctx, tx); err != nil {
		return result, err
	}
	var role string
	if tx.QueryRowContext(ctx, `SELECT current_user`).Scan(&role) != nil || role != "readonly_release" {
		return result, errors.New("migration API source requires the dedicated readonly_release connection")
	}
	tables, order, err := migrationCatalog()
	if err != nil {
		return result, err
	}
	identity, err := migrationIdentity(ctx, tx)
	if err != nil {
		return result, err
	}
	fingerprint, err := migrationSchema(ctx, tx, order)
	if err != nil {
		return result, err
	}
	rows, digests, err := migrationReadPages(ctx, tx, true, sink)
	if err != nil {
		return result, err
	}
	now := time.Now().UTC()
	columns := map[string][]businessColumn{}
	for _, name := range order {
		columns[name] = tables[name].Columns
	}
	result = MigrationSnapshot{Manifest: MigrationManifest{Authorization: authorization, Format: 1, Scope: MigrationAPIScope, SnapshotID: id, SourceIdentitySHA256: identity, SourceSchemaSHA256: fingerprint, CreatedAt: now, ExpiresAt: now.Add(lifetime), Tables: digests, Columns: columns}, Rows: rows}
	result.Manifest.ToolSHA256 = migrationExecutableSHA256()
	result.Manifest.SnapshotSHA256 = migrationSnapshotHash(result.Manifest)
	if sink == nil {
		if err = validateMigrationSnapshot(result); err != nil {
			return MigrationSnapshot{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return MigrationSnapshot{}, errors.New("source snapshot failed")
	}
	return result, nil
}

func migrationSnapshotHash(manifest MigrationManifest) string {
	manifest.SnapshotSHA256 = ""
	return migrationDigest(manifest)
}
func migrationPlanHash(plan MigrationPlan) string { plan.PlanSHA256 = ""; return migrationDigest(plan) }

func migrationKey(row rawRow, columns []string) (string, error) {
	key := rawRow{}
	for _, col := range columns {
		value, ok := row[col]
		if !ok || string(value) == "null" {
			return "", errors.New("missing primary key")
		}
		key[col] = value
	}
	b, err := canonicalBusinessJSON(key)
	return string(b), err
}

func validateMigrationSnapshot(snapshot MigrationSnapshot) error {
	m := snapshot.Manifest
	if !migrationUUID(m.Authorization.JobID) || m.Authorization.SourceAudience == "" || m.Authorization.TargetAudience == "" || m.Authorization.SourceAudience == m.Authorization.TargetAudience || !migrationSHA.MatchString(m.Authorization.SubjectSHA256) {
		return errors.New("snapshot authorization binding missing")
	}
	tables, order, err := migrationCatalog()
	if err != nil {
		return err
	}
	if m.Format != 1 || m.Scope != MigrationAPIScope || !migrationUUID(m.SnapshotID) || !migrationSHA.MatchString(m.ToolSHA256) || m.SourceIdentitySHA256 == "" || m.SourceSchemaSHA256 == "" || m.SnapshotSHA256 != migrationSnapshotHash(m) || len(snapshot.Rows) != len(order) || len(m.Tables) != len(order) || len(m.Columns) != len(order) {
		return errors.New("snapshot integrity mismatch")
	}
	for _, name := range order {
		table := tables[name]
		if migrationDigest(m.Columns[name]) != migrationDigest(table.Columns) {
			return errors.New("snapshot field contract mismatch")
		}
		digest := businessDigest{}
		for _, row := range snapshot.Rows[name] {
			if len(row) != len(table.Columns) {
				return errors.New("snapshot column scope mismatch")
			}
			for _, col := range table.Columns {
				value, ok := row[col.Name]
				if !ok || (!col.Nullable && string(value) == "null") {
					return errors.New("snapshot field missing")
				}
			}
			if err := digest.add(row, table); err != nil {
				return err
			}
		}
		actual, err := digest.finish()
		if err != nil || actual != m.Tables[name] {
			return errors.New("snapshot content mismatch")
		}
	}
	// Every FK parent is inside this scope. Validate even before target planning.
	for _, name := range order {
		for _, fk := range tables[name].ForeignKeys {
			parents := map[string]bool{}
			for _, row := range snapshot.Rows[fk.Parent] {
				k, err := migrationKey(row, fk.ParentColumns)
				if err != nil {
					return err
				}
				parents[k] = true
			}
			for _, row := range snapshot.Rows[name] {
				ref := rawRow{}
				skip := false
				for i, col := range fk.Columns {
					if string(row[col]) == "null" {
						skip = true
					}
					ref[fk.ParentColumns[i]] = row[col]
				}
				if skip {
					continue
				}
				k, err := migrationKey(ref, fk.ParentColumns)
				if err != nil || !parents[k] {
					return errors.New("snapshot foreign-key dependency missing")
				}
			}
		}
	}
	return nil
}

// Only the optional target listener calls this, never normal Accounts startup.
func InitializeMigrationReceipts(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS accounts_migration_api;
 CREATE TABLE IF NOT EXISTS accounts_migration_api.receipts (
 plan_id uuid PRIMARY KEY, plan_sha256 text NOT NULL, subject text NOT NULL,
 receipt jsonb NOT NULL, applied_at timestamptz NOT NULL DEFAULT now());
 REVOKE ALL ON SCHEMA accounts_migration_api FROM PUBLIC;
 REVOKE ALL ON ALL TABLES IN SCHEMA accounts_migration_api FROM PUBLIC`)
	if err != nil {
		return errors.New("migration receipt initialization failed")
	}
	return nil
}

func migrationLock(ctx context.Context, tx *sql.Tx, order []string) error {
	// SHARE ROW EXCLUSIVE serializes planners/appliers and excludes writers. Locks
	// are taken before reading target baseline, in one stable reviewed order.
	names := make([]string, len(order))
	for i, name := range order {
		names[i] = "public." + quoteBusiness(name)
	}
	sort.Strings(names)
	if _, err := tx.ExecContext(ctx, `LOCK TABLE `+strings.Join(names, ",")+` IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return errors.New("target writers did not yield migration locks")
	}
	return nil
}

func migrationDiff(snapshot MigrationSnapshot, target map[string][]rawRow) (map[string][]rawRow, int, int, int, error) {
	tables, order, err := migrationCatalog()
	if err != nil {
		return nil, 0, 0, 0, err
	}
	missing := map[string][]rawRow{}
	inserts, matches, conflicts := 0, 0, 0
	for _, name := range order {
		index := map[string]string{}
		for _, row := range target[name] {
			key, e := migrationKey(row, tables[name].PrimaryKey)
			if e != nil {
				return nil, 0, 0, 0, e
			}
			b, e := canonicalBusinessJSON(row)
			if e != nil {
				return nil, 0, 0, 0, e
			}
			index[key] = string(b)
		}
		for _, row := range snapshot.Rows[name] {
			key, e := migrationKey(row, tables[name].PrimaryKey)
			if e != nil {
				return nil, 0, 0, 0, e
			}
			b, e := canonicalBusinessJSON(row)
			if e != nil {
				return nil, 0, 0, 0, e
			}
			if actual, ok := index[key]; ok {
				if actual == string(b) {
					matches++
				} else {
					conflicts++
				}
			} else {
				missing[name] = append(missing[name], row)
				inserts++
			}
		}
	}
	return missing, inserts, matches, conflicts, nil
}

// A private temporary clone checks all unique/expression/partial indexes and
// CHECK constraints in preview without business writes, defaults or triggers.
func migrationCheckUnique(ctx context.Context, tx *sql.Tx, order []string, tables map[string]businessTable, missing map[string][]rawRow) error {
	for i, name := range order {
		if len(missing[name]) == 0 {
			continue
		}
		temp := fmt.Sprintf("migration_preview_%d", i)
		if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE `+quoteBusiness(temp)+` (LIKE public.`+quoteBusiness(name)+` INCLUDING GENERATED INCLUDING CONSTRAINTS INCLUDING INDEXES) ON COMMIT DROP`); err != nil {
			return errors.New("preview constraint check unavailable")
		}
		var cols []string
		for _, c := range tables[name].Columns {
			if !c.Generated {
				cols = append(cols, c.Name)
			}
		}
		list := businessColumnList(cols)
		if _, err := tx.ExecContext(ctx, `INSERT INTO pg_temp.`+quoteBusiness(temp)+` (`+list+`) SELECT `+list+` FROM public.`+quoteBusiness(name)); err != nil {
			return errors.New("preview target constraints rejected")
		}
		for start := 0; start < len(missing[name]); start += fullBusinessBatch {
			end := start + fullBusinessBatch
			if end > len(missing[name]) {
				end = len(missing[name])
			}
			b, _ := json.Marshal(missing[name][start:end])
			if _, err := tx.ExecContext(ctx, `INSERT INTO pg_temp.`+quoteBusiness(temp)+` (`+list+`) SELECT `+list+` FROM jsonb_populate_recordset(NULL::public.`+quoteBusiness(name)+`,$1::jsonb)`, string(b)); err != nil {
				return errors.New("target unique or check constraint conflict")
			}
		}
	}
	return nil
}

func PreviewMigration(ctx context.Context, db *sql.DB, snapshot MigrationSnapshot, id string, lifetime time.Duration) (MigrationPlan, error) {
	var plan MigrationPlan
	if err := validateMigrationSnapshot(snapshot); err != nil {
		return plan, err
	}
	if !time.Now().Before(snapshot.Manifest.ExpiresAt) {
		return plan, errors.New("source snapshot expired")
	}
	tables, order, err := migrationCatalog()
	if err != nil {
		return plan, err
	}
	tx, err := migrationBegin(ctx, db, false)
	if err != nil {
		return plan, err
	}
	defer tx.Rollback()
	if err = migrationLock(ctx, tx, order); err != nil {
		return plan, err
	}
	identity, err := migrationIdentity(ctx, tx)
	if err != nil {
		return plan, err
	}
	if identity == snapshot.Manifest.SourceIdentitySHA256 {
		return plan, errors.New("source and target are the same physical database")
	}
	fingerprint, err := migrationSchema(ctx, tx, order)
	if err != nil {
		return plan, err
	}
	data, digests, err := migrationRows(ctx, tx, false)
	if err != nil {
		return plan, err
	}
	missing, inserts, matches, conflicts, err := migrationDiff(snapshot, data)
	if err != nil {
		return plan, err
	}
	if conflicts == 0 {
		if err = migrationCheckUnique(ctx, tx, order, tables, missing); err != nil {
			return plan, err
		}
	}
	now := time.Now().UTC()
	expiry := now.Add(lifetime)
	if snapshot.Manifest.ExpiresAt.Before(expiry) {
		expiry = snapshot.Manifest.ExpiresAt
	}
	plan = MigrationPlan{PlanID: id, State: "preview", Snapshot: snapshot.Manifest, TargetIdentitySHA256: identity, TargetSchemaSHA256: fingerprint, TargetBaselineSHA256: migrationDigest(digests), InsertRows: inserts, MatchingRows: matches, ConflictRows: conflicts, CreatedAt: now, ExpiresAt: expiry}
	plan.ToolSHA256 = migrationExecutableSHA256()
	if !migrationSHA.MatchString(plan.ToolSHA256) {
		return MigrationPlan{}, errors.New("migration executable identity unavailable")
	}
	if conflicts > 0 {
		plan.State = "conflict"
	}
	plan.PlanSHA256 = migrationPlanHash(plan)
	return plan, nil // rollback also drops all temporary validation objects
}

func ReadMigrationReceipt(ctx context.Context, db *sql.DB, id, hash, subject string) (MigrationPlan, bool, error) {
	var body []byte
	var storedHash, storedSubject string
	var plan MigrationPlan
	tx, err := migrationBegin(ctx, db, true)
	if err != nil {
		return plan, false, err
	}
	defer tx.Rollback()
	var exists bool
	if tx.QueryRowContext(ctx, `SELECT to_regclass('accounts_migration_api.receipts') IS NOT NULL`).Scan(&exists) != nil {
		return plan, false, errors.New("migration receipt state unavailable")
	}
	if !exists {
		return plan, false, nil
	}
	err = tx.QueryRowContext(ctx, `SELECT plan_sha256,subject,receipt FROM accounts_migration_api.receipts WHERE plan_id=$1`, id).Scan(&storedHash, &storedSubject, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return plan, false, nil
	}
	if err != nil {
		return plan, false, errors.New("migration receipt unavailable")
	}
	if storedHash != hash || storedSubject != subject {
		return plan, false, errors.New("migration receipt binding mismatch")
	}
	if json.Unmarshal(body, &plan) != nil {
		return plan, false, errors.New("migration receipt invalid")
	}
	identity, err := migrationIdentity(ctx, tx)
	if err != nil || identity != plan.TargetIdentitySHA256 {
		return MigrationPlan{}, false, errors.New("receipt belongs to a different physical target database")
	}
	preview := plan
	preview.State = "preview"
	if plan.State != "applied" || plan.PlanID != id || plan.PlanSHA256 != hash || migrationPlanHash(preview) != hash {
		return MigrationPlan{}, false, errors.New("receipt content binding mismatch")
	}
	return plan, true, nil
}

func ApplyMigration(ctx context.Context, db *sql.DB, snapshot MigrationSnapshot, plan MigrationPlan, hash, subject string, gates ...MigrationBackupGate) (MigrationPlan, error) {
	if hash == "" || hash != plan.PlanSHA256 || hash != migrationPlanHash(plan) {
		return MigrationPlan{}, errors.New("reviewed plan hash mismatch")
	}
	if done, ok, err := ReadMigrationReceipt(ctx, db, plan.PlanID, hash, subject); err != nil || ok {
		return done, err
	}
	if plan.State != "preview" || plan.ConflictRows != 0 || !time.Now().Before(plan.ExpiresAt) {
		return MigrationPlan{}, errors.New("plan conflicted or expired")
	}
	if err := validateMigrationSnapshot(snapshot); err != nil {
		return MigrationPlan{}, err
	}
	if migrationDigest(snapshot.Manifest) != migrationDigest(plan.Snapshot) {
		return MigrationPlan{}, errors.New("plan snapshot binding mismatch")
	}
	tables, order, err := migrationCatalog()
	if err != nil {
		return MigrationPlan{}, err
	}
	tx, err := migrationBegin(ctx, db, false)
	if err != nil {
		return MigrationPlan{}, err
	}
	defer tx.Rollback()
	// A transaction-scoped advisory lock orders concurrent retries. READ COMMITTED
	// refreshes visibility after the lock wait; business tables remain locked.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(674302318055)`); err != nil {
		return MigrationPlan{}, errors.New("migration retry lock unavailable")
	}
	// LOCK itself does not establish an MVCC snapshot; the receipt read follows.
	if err = migrationLock(ctx, tx, order); err != nil {
		return MigrationPlan{}, err
	}
	var body []byte
	var storedHash, storedSubject string
	err = tx.QueryRowContext(ctx, `SELECT plan_sha256,subject,receipt FROM accounts_migration_api.receipts WHERE plan_id=$1`, plan.PlanID).Scan(&storedHash, &storedSubject, &body)
	if err == nil {
		var done MigrationPlan
		if storedHash != hash || storedSubject != subject || json.Unmarshal(body, &done) != nil {
			return MigrationPlan{}, errors.New("migration receipt binding mismatch")
		}
		identity, e := migrationIdentity(ctx, tx)
		if e != nil || identity != done.TargetIdentitySHA256 || identity != plan.TargetIdentitySHA256 {
			return MigrationPlan{}, errors.New("committed receipt target identity changed")
		}
		return done, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return MigrationPlan{}, errors.New("migration receipt unavailable")
	}
	if !time.Now().Before(plan.ExpiresAt) {
		return MigrationPlan{}, errors.New("plan expired while waiting for target locks")
	}
	if plan.ToolSHA256 != migrationExecutableSHA256() {
		return MigrationPlan{}, errors.New("migration executor changed since preview")
	}
	for _, gate := range gates {
		if gate == nil {
			return MigrationPlan{}, errors.New("verified backup gate missing")
		}
		if err = gate(ctx, plan); err != nil {
			return MigrationPlan{}, err
		}
	}
	identity, err := migrationIdentity(ctx, tx)
	if err != nil {
		return MigrationPlan{}, err
	}
	if identity != plan.TargetIdentitySHA256 || identity == snapshot.Manifest.SourceIdentitySHA256 {
		return MigrationPlan{}, errors.New("database identity changed")
	}
	fingerprint, err := migrationSchema(ctx, tx, order)
	if err != nil {
		return MigrationPlan{}, err
	}
	if fingerprint != plan.TargetSchemaSHA256 {
		return MigrationPlan{}, errors.New("target schema drift")
	}
	data, digests, err := migrationRows(ctx, tx, false)
	if err != nil {
		return MigrationPlan{}, err
	}
	if migrationDigest(digests) != plan.TargetBaselineSHA256 {
		return MigrationPlan{}, errors.New("target baseline drift")
	}
	missing, inserts, matches, conflicts, err := migrationDiff(snapshot, data)
	if err != nil {
		return MigrationPlan{}, err
	}
	if conflicts != 0 || inserts != plan.InsertRows || matches != plan.MatchingRows {
		return MigrationPlan{}, errors.New("target plan drift")
	}
	for _, name := range order {
		for start := 0; start < len(missing[name]); start += fullBusinessBatch {
			end := start + fullBusinessBatch
			if end > len(missing[name]) {
				end = len(missing[name])
			}
			if err = insertBusiness(ctx, tx, name, tables[name], missing[name][start:end]); err != nil {
				return MigrationPlan{}, err
			}
		}
		data[name] = append(data[name], missing[name]...)
	}
	_, after, err := migrationRows(ctx, tx, false)
	if err != nil {
		return MigrationPlan{}, err
	}
	expected := map[string]BusinessEquality{}
	for _, name := range order {
		d := businessDigest{}
		for _, row := range data[name] {
			if err = d.add(row, tables[name]); err != nil {
				return MigrationPlan{}, err
			}
		}
		expected[name], err = d.finish()
		if err != nil {
			return MigrationPlan{}, err
		}
	}
	if migrationDigest(expected) != migrationDigest(after) {
		return MigrationPlan{}, errors.New("post-insert facts changed; transaction rolled back")
	}
	// This scope has one identity sequence. ALTER SEQUENCE RESTART is
	// transactional (setval is not); explicit imported IDs cannot collide later.
	var sequence sql.NullString
	var next int64
	if tx.QueryRowContext(ctx, `SELECT pg_get_serial_sequence('public.finance_operation_events','id'),COALESCE(max(id),0)+1 FROM public.finance_operation_events`).Scan(&sequence, &next) != nil {
		return MigrationPlan{}, errors.New("finance identity sequence unavailable")
	}
	if sequence.Valid {
		var current int64
		if tx.QueryRowContext(ctx, `SELECT last_value FROM `+sequence.String).Scan(&current) != nil {
			return MigrationPlan{}, errors.New("finance identity sequence unreadable")
		}
		if next <= current {
			next = current + 1
		}
		if _, err = tx.ExecContext(ctx, `ALTER SEQUENCE `+sequence.String+` RESTART WITH `+fmt.Sprint(next)); err != nil {
			return MigrationPlan{}, errors.New("finance identity sequence restart failed")
		}
	}
	finalSchema, err := migrationSchema(ctx, tx, order)
	if err != nil || finalSchema != plan.TargetSchemaSHA256 {
		return MigrationPlan{}, errors.New("target schema changed during apply")
	}
	if !time.Now().Before(plan.ExpiresAt) {
		return MigrationPlan{}, errors.New("plan expired before commit")
	}
	for _, gate := range gates {
		if err = gate(ctx, plan); err != nil {
			return MigrationPlan{}, err
		}
	}
	plan.State = "applied"
	body, _ = json.Marshal(plan)
	if _, err = tx.ExecContext(ctx, `INSERT INTO accounts_migration_api.receipts(plan_id,plan_sha256,subject,receipt) VALUES($1,$2,$3,$4::jsonb)`, plan.PlanID, hash, subject, string(body)); err != nil {
		return MigrationPlan{}, errors.New("migration receipt commit failed")
	}
	if err = tx.Commit(); err != nil {
		return MigrationPlan{}, errors.New("migration commit uncertain; retry exact plan")
	}
	return plan, nil
}
