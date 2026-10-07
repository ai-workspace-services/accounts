package migrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"
	"time"

	schema "account/sql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const FullBusinessVersion = 2026100701
const FullBusinessBillingSHA256 = "a7133f3ef2ea9013a055cfd1442a7488d2b837f289e0f5d9b61624d4fde9bc53"
const fullBusinessBatch = 1000

// This reviewed catalog is the native Accounts artifact plus Billing #44.
// Never infer the transport scope from the live source's table enumeration.
//
//go:embed full_business_contract.json
var fullBusinessContractJSON []byte

type businessColumn struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Nullable  bool   `json:"nullable"`
	Generated bool   `json:"generated"`
}
type businessFK struct {
	Columns       []string `json:"columns"`
	Parent        string   `json:"parent"`
	ParentColumns []string `json:"parent_columns"`
}
type businessTable struct {
	Columns     []businessColumn `json:"columns"`
	PrimaryKey  []string         `json:"primary_key"`
	ForeignKeys []businessFK     `json:"foreign_keys"`
}
type FullBusinessOptions struct {
	Environment   string
	SchemaSHA256  string
	BillingSHA256 string
	WritersPaused bool
	DryRun        bool
	CompareOnly   bool
}
type BusinessEquality struct {
	Rows   int64  `json:"rows"`
	SHA256 string `json:"sha256"`
}
type FullBusinessReceipt struct {
	SourceIdentitySHA256    string                      `json:"source_identity_sha256"`
	SourceSnapshotSHA256    string                      `json:"source_snapshot_sha256"`
	SourceCatalogSHA256     string                      `json:"source_catalog_sha256"`
	Format                  int                         `json:"format"`
	Result                  string                      `json:"result"`
	Environment             string                      `json:"environment"`
	SchemaSHA256            string                      `json:"schema_sha256"`
	BillingSHA256           string                      `json:"billing_schema_sha256"`
	MigrationVersion        uint                        `json:"migration_version"`
	SnapshotStartedAt       time.Time                   `json:"snapshot_started_at"`
	CompletedAt             time.Time                   `json:"completed_at"`
	Tables                  map[string]BusinessEquality `json:"tables"`
	SourceTables            int                         `json:"source_table_count"`
	UserCount               int                         `json:"user_count"`
	BatchSize               int                         `json:"batch_size"`
	SourceReadOnly          bool                        `json:"source_read_only"`
	FullBusinessEqual       bool                        `json:"full_business_equal"`
	TargetWrites            bool                        `json:"target_writes"`
	SequencePolicy          string                      `json:"sequence_policy"`
	DatabaseCutoverApproved bool                        `json:"database_cutover_approved"`
	CoreUsers               CoreUsersEvidence           `json:"core_users"`
}

// CoreUsersEquality is the deliberately small identity contract consumed by
// the edge cutover gate.  It contains only non-reversible digests: email,
// password hash and authoritative Proxy UUID remain aligned without placing
// user rows or password material in a workflow artifact.
type CoreUsersEquality struct {
	Count              int    `json:"count"`
	EmailSHA256        string `json:"email_sha256"`
	PasswordHashSHA256 string `json:"password_hash_sha256"`
	EmailProxySHA256   string `json:"email_proxy_sha256"`
}

type CoreUsersEvidence struct {
	Source CoreUsersEquality `json:"source"`
	Target CoreUsersEquality `json:"target"`
}

func fullBusinessContract() (map[string]businessTable, []string, error) {
	var tables map[string]businessTable
	if json.Unmarshal(fullBusinessContractJSON, &tables) != nil || len(tables) != 53 {
		return nil, nil, errors.New("invalid compiled full-business catalog")
	}
	_, manifest, err := schema.NativeArtifact()
	if err != nil {
		return nil, nil, err
	}
	for _, name := range manifest.BusinessTables {
		if _, ok := tables[name]; !ok {
			return nil, nil, errors.New("full-business catalog differs from native Accounts manifest")
		}
	}
	order, err := businessOrder(tables)
	return tables, order, err
}

func businessOrder(tables map[string]businessTable) ([]string, error) {
	remaining := map[string]bool{}
	for name := range tables {
		remaining[name] = true
	}
	var order []string
	for len(remaining) > 0 {
		var ready []string
		for name := range remaining {
			eligible := true
			for _, fk := range tables[name].ForeignKeys {
				if _, ok := tables[fk.Parent]; !ok {
					return nil, errors.New("unreviewed foreign-key parent")
				}
				if remaining[fk.Parent] {
					eligible = false
				}
			}
			if eligible {
				ready = append(ready, name)
			}
		}
		if len(ready) == 0 {
			return nil, errors.New("full-business foreign-key cycle requires review")
		}
		sort.Strings(ready)
		for _, name := range ready {
			order = append(order, name)
			delete(remaining, name)
		}
	}
	return order, nil
}

func quoteBusiness(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
func businessColumnList(names []string) string {
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = quoteBusiness(name)
	}
	return strings.Join(parts, ",")
}

var optionalBusinessTables = map[string]bool{
	"account_lifecycle_events": true, "mfa_recovery_codes": true, "password_recovery_challenges": true,
	"finance_invoices": true, "finance_payments": true, "finance_refunds": true, "finance_operations": true,
	"finance_operation_events": true, "cloud_vendor_costs": true,
}

// CopyFullBusiness streams one read-only source snapshot into one empty target
// transaction. CompareOnly permits a populated target and maps UUIDs by email.
// The host owner must prove paused writers and the exact image/schema first.
// A Supabase session-pooler postgres login is accepted only for a read-only
// repeatable-read transaction; no source write is part of this operation.
func CopyFullBusiness(ctx context.Context, sourceDSN, targetDSN string, options FullBusinessOptions) (FullBusinessReceipt, error) {
	receipt := FullBusinessReceipt{Format: 1, Environment: options.Environment, MigrationVersion: FullBusinessVersion,
		Tables: map[string]BusinessEquality{}, BatchSize: fullBusinessBatch, SequencePolicy: "next value above copied maximum; never read nextval on source"}
	_, manifest, err := schema.NativeArtifact()
	if err != nil {
		return receipt, err
	}
	if (options.Environment != "prod" && options.Environment != "uat") || !options.WritersPaused ||
		options.SchemaSHA256 != manifest.SchemaSHA256 || options.BillingSHA256 != FullBusinessBillingSHA256 {
		return receipt, errors.New("full-business transfer requires explicit environment, paused writers and both reviewed schema hashes")
	}
	receipt.SchemaSHA256 = manifest.SchemaSHA256
	receipt.BillingSHA256 = FullBusinessBillingSHA256
	tables, order, err := fullBusinessContract()
	if err != nil {
		return receipt, err
	}
	if err = validateBusinessConnections(sourceDSN, targetDSN); err != nil {
		return receipt, err
	}
	source, err := openDB(ctx, sourceDSN)
	if err != nil {
		return receipt, errors.New("full-business source connection failed")
	}
	defer source.Close()
	target, err := openDB(ctx, targetDSN)
	if err != nil {
		return receipt, errors.New("full-business target connection failed")
	}
	defer target.Close()
	src, err := source.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return receipt, errors.New("cannot start read-only business snapshot")
	}
	defer src.Rollback()
	// Session poolers can ignore startup GUCs. Enforce the connection's readonly
	// default inside this already-readonly snapshot, then verify both settings.
	// LOCAL automatically restores the prior state on commit/rollback and cannot
	// leave a shared pooler's backend readonly for the live Serverless service.
	if _, err = src.ExecContext(ctx, `SET LOCAL default_transaction_read_only=on`); err != nil {
		return receipt, errors.New("cannot enforce readonly source connection within snapshot")
	}
	dst, err := target.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable, ReadOnly: options.CompareOnly || options.DryRun})
	if err != nil {
		return receipt, errors.New("cannot start business target transaction")
	}
	defer dst.Rollback()
	for _, tx := range []*sql.Tx{src, dst} {
		if _, err = tx.ExecContext(ctx, `SET LOCAL timezone='UTC'; SET LOCAL datestyle='ISO,YMD'; SET LOCAL extra_float_digits=3; SET LOCAL statement_timeout='5min'; SET LOCAL lock_timeout='15s'; SET LOCAL idle_in_transaction_session_timeout='5min'`); err != nil {
			return receipt, errors.New("cannot enforce business transaction budgets")
		}
	}
	receipt.SnapshotStartedAt = time.Now().UTC()
	srcCfg, _ := pgx.ParseConfig(sourceDSN)
	identity, _ := json.Marshal(struct {
		Host     string
		Port     uint16
		Database string
		Role     string
	}{srcCfg.Host, srcCfg.Port, srcCfg.Database, srcCfg.User})
	identitySum := sha256.Sum256(identity)
	receipt.SourceIdentitySHA256 = hex.EncodeToString(identitySum[:])
	var snapshot string
	if err = src.QueryRowContext(ctx, `SELECT pg_current_snapshot()::text`).Scan(&snapshot); err != nil {
		return receipt, errors.New("cannot identify readonly source snapshot")
	}
	snapshotSum := sha256.Sum256([]byte(snapshot))
	receipt.SourceSnapshotSHA256 = hex.EncodeToString(snapshotSum[:])
	if err = businessSourceRole(ctx, src); err != nil {
		return receipt, err
	}
	sourceTables, err := businessScope(ctx, src, tables, true)
	if err != nil {
		return receipt, err
	}
	if _, err = businessScope(ctx, dst, tables, false); err != nil {
		return receipt, err
	}
	for name := range sourceTables {
		if err = businessSourceVisibility(ctx, src, name); err != nil {
			return receipt, err
		}
	}
	receipt.SourceTables = len(sourceTables)
	receipt.SourceReadOnly = true
	var dbName string
	var pgVersion, version, count int
	var dirty bool
	if err = dst.QueryRowContext(ctx, `SELECT current_database(),current_setting('server_version_num')::int`).Scan(&dbName, &pgVersion); err != nil || dbName != "account" || pgVersion < 170000 || pgVersion >= 180000 {
		return receipt, errors.New("target must be the native PostgreSQL17 account database")
	}
	if err = dst.QueryRowContext(ctx, `SELECT count(*),COALESCE(min(version),0),COALESCE(bool_or(dirty),true) FROM public.schema_migrations`).Scan(&count, &version, &dirty); err != nil || count != 1 || version != FullBusinessVersion || dirty {
		return receipt, errors.New("target requires the exact clean native Billing checkpoint")
	}
	if !options.CompareOnly && !options.DryRun {
		if _, err = dst.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1::bigint)`, migrationAdvisoryLockKey); err != nil {
			return receipt, errors.New("cannot lock full-business initialization")
		}
		names := []string{"public.schema_migrations"}
		for _, name := range order {
			names = append(names, "public."+quoteBusiness(name))
		}
		if _, err = dst.ExecContext(ctx, "LOCK TABLE "+strings.Join(names, ",")+" IN ACCESS EXCLUSIVE MODE"); err != nil {
			return receipt, errors.New("cannot exclusively lock target business tables")
		}
	}
	for _, name := range order {
		if err = validateBusinessTarget(ctx, dst, name, tables[name]); err != nil {
			return receipt, err
		}
		if !options.CompareOnly {
			var nonempty bool
			if err = dst.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM public."+quoteBusiness(name)+" LIMIT 1)").Scan(&nonempty); err != nil || nonempty {
				return receipt, fmt.Errorf("public.%s must be empty; existing business data is never reset", name)
			}
		}
	}
	sourceCatalog := map[string][]ColumnDefinition{}
	for name := range sourceTables {
		cols, err := catalogColumns(ctx, src, name)
		if err != nil {
			return receipt, fmt.Errorf("cannot inspect source public.%s", name)
		}
		if err = validateBusinessSourceColumns(name, cols, tables[name]); err != nil {
			return receipt, err
		}
		if name == "email_blacklist" && legacyBlacklistSource(cols) {
			var emailKey bool
			err = src.QueryRowContext(ctx, `SELECT count(*)=1 FROM pg_constraint c
 WHERE c.conrelid='public.email_blacklist'::regclass AND c.contype='p'
 AND c.conkey=ARRAY[(SELECT attnum FROM pg_attribute
 WHERE attrelid=c.conrelid AND attname='email' AND NOT attisdropped)]`).Scan(&emailKey)
			if err != nil || !emailKey {
				return receipt, errors.New("legacy email blacklist requires the exact email primary key")
			}
		}
		sourceCatalog[name] = cols
	}
	catalogData, _ := json.Marshal(sourceCatalog)
	catalogSum := sha256.Sum256(catalogData)
	receipt.SourceCatalogSHA256 = hex.EncodeToString(catalogSum[:])
	sourceUsers, err := businessUsers(ctx, src)
	if err != nil {
		return receipt, err
	}
	if len(sourceUsers) == 0 {
		return receipt, errors.New("empty source user population requires an explicit scope review")
	}
	targetUsers := sourceUsers
	if options.CompareOnly {
		targetUsers, err = businessUsers(ctx, dst)
		if err != nil {
			return receipt, err
		}
	}
	uuidMap, err := businessUserMap(sourceUsers, targetUsers)
	if err != nil {
		return receipt, err
	}
	receipt.UserCount = len(sourceUsers)
	sourceCore, err := coreUsersDigest(sourceUsers)
	if err != nil {
		return receipt, err
	}
	receipt.CoreUsers.Source = sourceCore
	if options.CompareOnly {
		targetCore, err := coreUsersDigest(targetUsers)
		if err != nil {
			return receipt, err
		}
		if !equalCoreUsers(sourceUsers, targetUsers) {
			return receipt, errors.New("core user email, password hash or Proxy UUID differs")
		}
		receipt.CoreUsers.Target = targetCore
	}
	for _, name := range order {
		sourceColumns := sourceCatalog[name]
		// A preview inspects catalogs/privileges and user matching; it never reads
		// the large ledger or transfers a row. Counts in preview are not equality.
		if options.DryRun {
			continue
		}
		sourceDigest := businessDigest{}
		if sourceTables[name] {
			sourceKey := tables[name].PrimaryKey
			if name == "email_blacklist" && legacyBlacklistSource(sourceColumns) {
				sourceKey = []string{"email"}
			}
			err = streamBusiness(ctx, src, name, sourceKey, func(page []rawRow) error {
				for _, row := range page {
					if err := projectBusinessRow(name, row, sourceColumns, tables[name], uuidMap); err != nil {
						return err
					}
					if err := sourceDigest.add(row, tables[name]); err != nil {
						return err
					}
				}
				if options.CompareOnly {
					return nil
				}
				return insertBusiness(ctx, dst, name, tables[name], page)
			})
			if err != nil {
				return receipt, err
			}
		}
		expected, err := sourceDigest.finish()
		if err != nil {
			return receipt, err
		}
		receipt.Tables[name] = expected
	}
	// Verify after ALL inserts: an insert trigger on a later table must not
	// silently mutate facts copied earlier in the same target transaction.
	if !options.DryRun {
		for _, name := range order {
			targetDigest := businessDigest{}
			if err = streamBusiness(ctx, dst, name, tables[name].PrimaryKey, func(page []rawRow) error {
				for _, row := range page {
					if err := targetDigest.add(row, tables[name]); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return receipt, err
			}
			actual, err := targetDigest.finish()
			if err != nil {
				return receipt, err
			}
			if receipt.Tables[name] != actual {
				return receipt, fmt.Errorf("full-field equality failed for public.%s", name)
			}
		}
		targetAfter, err := businessUsers(ctx, dst)
		if err != nil {
			return receipt, err
		}
		targetCore, err := coreUsersDigest(targetAfter)
		if err != nil || !equalCoreUsers(sourceUsers, targetAfter) {
			return receipt, errors.New("core user email, password hash or Proxy UUID differs after target copy")
		}
		receipt.CoreUsers.Target = targetCore
	}
	if options.DryRun {
		receipt.Result = "eligible"
	} else {
		receipt.FullBusinessEqual = true
		receipt.Result = "equal"
		if options.CompareOnly {
			if err = businessSequences(ctx, dst, false); err != nil {
				return receipt, err
			}
		}
		if !options.CompareOnly {
			if err = businessSequences(ctx, dst, true); err != nil {
				return receipt, err
			}
			receipt.TargetWrites = true
			receipt.Result = "copied"
		}
	}
	// Read-only source ends first: a failed snapshot cannot leave target rows.
	if err = src.Commit(); err != nil {
		return receipt, errors.New("source snapshot completion failed")
	}
	if err = dst.Commit(); err != nil {
		return receipt, errors.New("target business transaction did not commit")
	}
	receipt.CompletedAt = time.Now().UTC()
	return receipt, nil
}

func validateBusinessConnections(source, target string) error {
	src, e1 := pgx.ParseConfig(source)
	dst, e2 := pgx.ParseConfig(target)
	sourceRole := ""
	if e1 == nil {
		sourceRole = strings.Split(src.User, ".")[0]
	}
	if e1 != nil || e2 != nil || src.Database == "" || dst.Database != "account" ||
		(sourceRole != "readonly_release" && sourceRole != "postgres") || dst.User == "readonly_release" {
		return errors.New("invalid readonly source or native target connection contract")
	}
	local := src.Host == "localhost" || src.Host == "127.0.0.1" || src.Host == "::1"
	if !local {
		for _, fallback := range src.Fallbacks {
			if fallback.TLSConfig == nil {
				return errors.New("remote source forbids plaintext TLS fallback")
			}
		}
	}
	if !local && src.TLSConfig == nil {
		return errors.New("remote source requires TLS")
	}
	if src.Host == dst.Host && src.Port == dst.Port && src.Database == dst.Database {
		return errors.New("source and target must be different databases")
	}
	return nil
}

func businessScope(ctx context.Context, tx *sql.Tx, tables map[string]businessTable, source bool) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.relname,c.relkind::text FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p','v','m','f') ORDER BY c.relname`)
	if err != nil {
		return nil, errors.New("cannot read complete business table scope")
	}
	defer rows.Close()
	actual := map[string]bool{}
	for rows.Next() {
		var name, kind string
		if rows.Scan(&name, &kind) != nil {
			return nil, errors.New("cannot decode business scope")
		}
		if name == "schema_migrations" || name == "system_release_checkpoints" {
			continue
		}
		if _, ok := tables[name]; !ok || kind != "r" {
			return nil, errors.New("unreviewed public business relation requires scope update")
		}
		actual[name] = true
	}
	if rows.Err() != nil {
		return nil, errors.New("incomplete business scope")
	}
	for name := range tables {
		if !actual[name] && (!source || !optionalBusinessTables[name]) {
			return nil, fmt.Errorf("required public.%s missing", name)
		}
	}
	return actual, nil
}

func businessSourceRole(ctx context.Context, tx *sql.Tx) error {
	var valid bool
	err := tx.QueryRowContext(ctx, `SELECT current_setting('transaction_read_only')='on'
 AND current_setting('default_transaction_read_only')='on' AND (
 (current_user='postgres' AND EXISTS (SELECT 1 FROM pg_roles WHERE rolname=current_user AND (rolsuper OR rolbypassrls))) OR
 (current_user='readonly_release' AND EXISTS (
 SELECT 1 FROM pg_roles WHERE rolname=current_user AND current_user='readonly_release' AND rolcanlogin
 AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls AND NOT rolinherit)
 AND NOT EXISTS (SELECT 1 FROM pg_auth_members WHERE member=(SELECT oid FROM pg_roles WHERE rolname=current_user))
 AND NOT EXISTS (SELECT 1 FROM pg_class c WHERE c.relnamespace='public'::regnamespace AND c.relkind IN ('r','p')
 AND (has_table_privilege(current_user,c.oid,'INSERT') OR has_table_privilege(current_user,c.oid,'UPDATE') OR has_table_privilege(current_user,c.oid,'DELETE') OR has_table_privilege(current_user,c.oid,'TRUNCATE')))
 AND NOT EXISTS (SELECT 1 FROM pg_class c WHERE c.relnamespace='public'::regnamespace AND c.relkind='S' AND has_sequence_privilege(current_user,c.oid,'USAGE,UPDATE'))))`).Scan(&valid)
	if err != nil || !valid {
		return errors.New("source requires an enforced read-only transaction and a readonly_release or Supabase postgres session-pooler role")
	}
	return nil
}

func businessSourceVisibility(ctx context.Context, tx *sql.Tx, name string) error {
	var valid bool
	err := tx.QueryRowContext(ctx, `SELECT has_table_privilege(current_user,c.oid,'SELECT') AND (
 (EXISTS (SELECT 1 FROM pg_roles WHERE rolname=current_user AND (rolsuper OR rolbypassrls))) OR NOT c.relrowsecurity OR (
 EXISTS(SELECT 1 FROM pg_policy p WHERE p.polrelid=c.oid AND p.polname='release_initialization_readonly' AND p.polcmd='r' AND p.polpermissive
 AND p.polroles=ARRAY[(SELECT oid FROM pg_roles WHERE rolname=current_user)] AND pg_get_expr(p.polqual,p.polrelid)='true' AND p.polwithcheck IS NULL)
 AND NOT EXISTS(SELECT 1 FROM pg_policy p WHERE p.polrelid=c.oid AND NOT p.polpermissive AND p.polcmd IN ('r','*') AND (0::oid=ANY(p.polroles) OR (SELECT oid FROM pg_roles WHERE rolname=current_user)=ANY(p.polroles)))))
 FROM pg_class c WHERE c.relnamespace='public'::regnamespace AND c.relname=$1 AND c.relkind='r'`, name).Scan(&valid)
	if err != nil || !valid {
		return fmt.Errorf("complete readonly visibility not proven for public.%s", name)
	}
	// row_security remains ON: OFF rejects SELECT for an ordinary readonly role.
	return nil
}

func validateBusinessTarget(ctx context.Context, tx *sql.Tx, name string, table businessTable) error {
	cols, err := catalogColumns(ctx, tx, name)
	if err != nil || len(cols) != len(table.Columns) {
		return fmt.Errorf("native target public.%s column scope differs", name)
	}
	for i, c := range cols {
		expected := table.Columns[i]
		if c.Name != expected.Name || c.Type != expected.Type || c.Nullable != expected.Nullable || c.Generated != expected.Generated {
			return fmt.Errorf("native target public.%s.%s column definition differs", name, expected.Name)
		}
	}
	var visible bool
	if err = tx.QueryRowContext(ctx, `SELECT NOT relrowsecurity OR (SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user) OR (relowner=(SELECT oid FROM pg_roles WHERE rolname=current_user) AND NOT relforcerowsecurity) FROM pg_class WHERE oid=to_regclass($1)`, "public."+name).Scan(&visible); err != nil || !visible {
		return errors.New("complete target visibility is not proven")
	}
	var actualPK string
	if err = tx.QueryRowContext(ctx, `SELECT string_agg(a.attname,',' ORDER BY k.ord) FROM pg_constraint c CROSS JOIN LATERAL unnest(c.conkey) WITH ORDINALITY k(num,ord) JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=k.num WHERE c.conrelid=to_regclass($1) AND c.contype='p'`, "public."+name).Scan(&actualPK); err != nil || actualPK != strings.Join(table.PrimaryKey, ",") {
		return fmt.Errorf("native public.%s primary key differs", name)
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.confrelid::regclass::text, string_agg(a.attname,',' ORDER BY k.ord),string_agg(b.attname,',' ORDER BY k.ord),c.convalidated,c.condeferrable FROM pg_constraint c CROSS JOIN LATERAL unnest(c.conkey,c.confkey) WITH ORDINALITY k(localnum,parentnum,ord) JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=k.localnum JOIN pg_attribute b ON b.attrelid=c.confrelid AND b.attnum=k.parentnum WHERE c.conrelid=to_regclass($1) AND c.contype='f' GROUP BY c.oid`, "public."+name)
	if err != nil {
		return errors.New("cannot verify target foreign keys")
	}
	defer rows.Close()
	expected := map[string]bool{}
	for _, fk := range table.ForeignKeys {
		expected[fk.Parent+":"+strings.Join(fk.Columns, ",")+":"+strings.Join(fk.ParentColumns, ",")] = true
	}
	for rows.Next() {
		var parent, cols, pcols string
		var validated, deferred bool
		if rows.Scan(&parent, &cols, &pcols, &validated, &deferred) != nil {
			return errors.New("cannot decode target foreign keys")
		}
		key := strings.TrimPrefix(parent, "public.") + ":" + cols + ":" + pcols
		if !expected[key] || !validated || deferred {
			return fmt.Errorf("native public.%s foreign key differs", name)
		}
		delete(expected, key)
	}
	if rows.Err() != nil || len(expected) != 0 {
		return fmt.Errorf("native public.%s foreign key coverage differs", name)
	}
	return nil
}

// These nullable fields were introduced by 2026091301. An older source that
// never had the columns has no values to preserve; use that migration's NULL
// defaults without changing subscriptions, quota, ledger or existing fields.
func absentNativeUserMetadata(c businessColumn) bool {
	switch c.Name {
	case "subscription_valid_from", "subscription_valid_until", "last_active_at", "archived_at":
		return c.Nullable && c.Type == "timestamp with time zone"
	}
	return false
}

func legacyBlacklistSource(columns []ColumnDefinition) bool {
	for _, c := range columns {
		if c.Name == "uuid" {
			return false
		}
	}
	return true
}

func absentBlacklistUUID(name string, c businessColumn) bool {
	return name == "email_blacklist" && c.Name == "uuid" && c.Type == "uuid" && !c.Nullable
}

func validateBusinessSourceColumns(name string, source []ColumnDefinition, target businessTable) error {
	expected := map[string]businessColumn{}
	for _, c := range target.Columns {
		expected[c.Name] = c
	}
	for _, c := range source {
		want, ok := expected[c.Name]
		if !ok || c.Type != want.Type {
			return fmt.Errorf("unmapped source column/type public.%s.%s", name, c.Name)
		}
		delete(expected, c.Name)
	}
	for key, column := range expected {
		if absentBlacklistUUID(name, column) {
			continue
		}
		if name != "users" || (!strings.HasPrefix(key, "account_lifecycle_") && !absentNativeUserMetadata(column)) {
			return fmt.Errorf("missing source column public.%s.%s requires explicit projection review", name, key)
		}
	}
	return nil
}

type businessUser struct{ ID, Email, PasswordHash, Proxy string }

func businessUsers(ctx context.Context, tx *sql.Tx) (map[string]businessUser, error) {
	rows, err := tx.QueryContext(ctx, `SELECT uuid::text,email,password,proxy_uuid::text FROM public.users ORDER BY uuid`)
	if err != nil {
		return nil, errors.New("cannot read user matching keys")
	}
	defer rows.Close()
	users := map[string]businessUser{}
	proxies := map[string]bool{}
	for rows.Next() {
		var u businessUser
		if rows.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Proxy) != nil {
			return nil, errors.New("user email/Proxy key is missing")
		}
		key := strings.ToLower(strings.TrimSpace(u.Email))
		id, e1 := uuid.Parse(u.ID)
		proxy, e2 := uuid.Parse(u.Proxy)
		if key == "" || users[key].ID != "" || proxies[u.Proxy] || e1 != nil || e2 != nil || id.String() != u.ID || proxy.String() != u.Proxy || id == uuid.Nil || proxy == uuid.Nil {
			return nil, errors.New("user email or Proxy UUID uniqueness is invalid")
		}
		users[key] = u
		proxies[u.Proxy] = true
	}
	if rows.Err() != nil {
		return nil, errors.New("incomplete user matching keys")
	}
	return users, nil
}

func coreUsersDigest(users map[string]businessUser) (CoreUsersEquality, error) {
	if len(users) == 0 {
		return CoreUsersEquality{}, errors.New("core user contract requires a non-empty source population")
	}
	keys := make([]string, 0, len(users))
	for key := range users {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	digest := func(value func(businessUser) string, label string) string {
		h := sha256.New()
		fmt.Fprintf(h, "core-users-v1:%s:%d:", label, len(keys))
		for _, key := range keys {
			user := users[key]
			fmt.Fprintf(h, "%d:%s", len(key), key)
			valueBytes := []byte(value(user))
			fmt.Fprintf(h, "%d:", len(valueBytes))
			h.Write(valueBytes)
		}
		return hex.EncodeToString(h.Sum(nil))
	}
	return CoreUsersEquality{Count: len(keys),
		EmailSHA256:        digest(func(u businessUser) string { return u.Email }, "email"),
		PasswordHashSHA256: digest(func(u businessUser) string { return u.PasswordHash }, "password-hash"),
		EmailProxySHA256:   digest(func(u businessUser) string { return u.Proxy }, "email-proxy"),
	}, nil
}

func equalCoreUsers(source, target map[string]businessUser) bool {
	if len(source) != len(target) {
		return false
	}
	for key, user := range source {
		other, ok := target[key]
		if !ok || user.Email != other.Email || user.PasswordHash != other.PasswordHash || user.Proxy != other.Proxy {
			return false
		}
	}
	return true
}
func businessUserMap(source, target map[string]businessUser) (map[string]string, error) {
	if len(source) != len(target) {
		return nil, errors.New("source and target user populations differ")
	}
	mapping := map[string]string{}
	for key, u := range source {
		other, ok := target[key]
		if !ok || other.Email != u.Email || other.Proxy != u.Proxy {
			return nil, errors.New("source email or authoritative Proxy UUID differs")
		}
		mapping[u.ID] = other.ID
	}
	return mapping, nil
}

// UUID-like references without a database FK are still business references.
// Historical audit actors and non-UUID service actors are preserved verbatim.
var businessUserReferences = map[string][]string{
	"users": {"uuid", "account_lifecycle_actor_ref"}, "audit_logs": {"actor_uuid"}, "finance_payments": {"account_uuid"}, "finance_operations": {"actor_ref"},
	"finance_operation_events": {"actor_ref"}, "overlay_devices": {"user_id"}, "overlay_networks": {"owner_user_id"},
	"overlay_registrations": {"owner_user_id"}, "tenant_memberships": {"user_id"}, "xworkmate_profiles": {"user_id"},
	"homepage_video_settings": {"updated_by"}, "account_lifecycle_events": {"actor_ref"}, "task_session_events": {"actor_id"},
}

func projectBusinessRow(name string, row rawRow, source []ColumnDefinition, table businessTable, mapping map[string]string) error {
	if len(row) != len(source) {
		return fmt.Errorf("source public.%s row coverage differs", name)
	}
	for _, c := range table.Columns {
		if _, ok := row[c.Name]; !ok {
			if absentBlacklistUUID(name, c) {
				var email string
				if json.Unmarshal(row["email"], &email) != nil || bytes.Equal(row["email"], []byte("null")) {
					return errors.New("legacy email blacklist key is invalid")
				}
				// UUIDv5 is stable across copy/compare snapshots. Use the exact
				// original email bytes, preserving case and historical key identity.
				row[c.Name], _ = json.Marshal(uuid.NewSHA1(uuid.NameSpaceOID,
					[]byte("accounts:email_blacklist:"+email)).String())
				continue
			}
			if name != "users" || (!strings.HasPrefix(c.Name, "account_lifecycle_") && !absentNativeUserMetadata(c)) {
				return errors.New("unreviewed source field omission")
			}
			row[c.Name] = json.RawMessage("null")
			if c.Name == "account_lifecycle_state" {
				row[c.Name] = json.RawMessage(`"active"`)
			}
		}
	}
	// Preserve the generated verification fact; never invent a verification time.
	if name == "users" {
		var verified bool
		if json.Unmarshal(row["email_verified"], &verified) != nil || verified != (string(row["email_verified_at"]) != "null") {
			return errors.New("source email verification timestamp/flag requires explicit correction")
		}
	}
	refs := append([]string{}, businessUserReferences[name]...)
	for _, c := range table.Columns {
		if c.Name == "account_uuid" || c.Name == "user_uuid" {
			refs = append(refs, c.Name)
		}
	}
	for _, fk := range table.ForeignKeys {
		if fk.Parent == "users" {
			for i, col := range fk.Columns {
				if fk.ParentColumns[i] == "uuid" {
					refs = append(refs, col)
				}
			}
		}
	}
	done := map[string]bool{}
	for _, col := range refs {
		if done[col] {
			continue
		}
		done[col] = true
		value := rowString(row, col)
		if id, ok := mapping[value]; ok {
			row[col], _ = json.Marshal(id)
		} else if (col == "account_uuid" || col == "user_uuid") && value != "" {
			return fmt.Errorf("orphan source user reference in public.%s", name)
		}
	}
	return nil
}

// A NO SCROLL server cursor keeps the snapshot stable and bounds client memory.
// FETCH pages avoid OFFSET scans and per-row queries, including composite PKs.
func streamBusiness(ctx context.Context, tx *sql.Tx, name string, pk []string, consume func([]rawRow) error) error {
	if _, err := tx.ExecContext(ctx, "DECLARE full_business_page NO SCROLL CURSOR FOR SELECT to_jsonb(t)::text FROM public."+quoteBusiness(name)+" t ORDER BY "+businessColumnList(pk)); err != nil {
		return fmt.Errorf("cannot open public.%s business cursor", name)
	}
	defer tx.ExecContext(ctx, `CLOSE full_business_page`)
	for {
		rows, err := tx.QueryContext(ctx, "FETCH FORWARD 1000 FROM full_business_page")
		if err != nil {
			return fmt.Errorf("cannot stream public.%s", name)
		}
		page := make([]rawRow, 0, fullBusinessBatch)
		size := 0
		for rows.Next() {
			var text string
			if rows.Scan(&text) != nil || len(text) > 1024*1024 {
				rows.Close()
				return errors.New("business row exceeds reviewed size or cannot decode")
			}
			size += len(text)
			if size > 8*1024*1024 {
				rows.Close()
				return errors.New("business page exceeds reviewed memory budget")
			}
			var row rawRow
			if json.Unmarshal([]byte(text), &row) != nil {
				rows.Close()
				return errors.New("invalid business row JSON")
			}
			page = append(page, row)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("incomplete public.%s stream", name)
		}
		if len(page) == 0 {
			break
		}
		if err = consume(page); err != nil {
			return err
		}
	}
	return nil
}
func insertBusiness(ctx context.Context, tx *sql.Tx, name string, table businessTable, page []rawRow) error {
	var cols []string
	for _, c := range table.Columns {
		if !c.Generated {
			cols = append(cols, c.Name)
		}
	}
	data, err := json.Marshal(page)
	if err != nil {
		return errors.New("cannot encode bounded business batch")
	}
	list := businessColumnList(cols)
	// OVERRIDING handles the native finance event identity, with FK/check/unique
	// constraints and insert guards left enabled. Never disable ALL triggers.
	result, err := tx.ExecContext(ctx, "INSERT INTO public."+quoteBusiness(name)+" ("+list+") OVERRIDING SYSTEM VALUE SELECT "+list+" FROM jsonb_populate_recordset(NULL::public."+quoteBusiness(name)+",$1::jsonb)", string(data))
	if err != nil {
		return fmt.Errorf("business batch insert rejected for public.%s; target transaction will roll back", name)
	}
	n, err := result.RowsAffected()
	if err != nil || n != int64(len(page)) {
		return errors.New("business batch row count differs")
	}
	return nil
}

type businessLeaf struct{ Key, Row [32]byte }
type businessDigest struct{ Leaves []businessLeaf }

func (d *businessDigest) add(row rawRow, table businessTable) error {
	if len(row) != len(table.Columns) {
		return errors.New("business equality column coverage differs")
	}
	normalized := rawRow{}
	for _, c := range table.Columns {
		v, ok := row[c.Name]
		if !ok {
			return errors.New("business equality field missing")
		}
		if strings.HasPrefix(c.Type, "timestamp") && string(v) != "null" {
			var value string
			if json.Unmarshal(v, &value) != nil {
				return errors.New("invalid business timestamp")
			}
			at, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return errors.New("business timestamp is outside canonical transport range")
			}
			v, _ = json.Marshal(at.UTC().Format(time.RFC3339Nano))
		}
		normalized[c.Name] = v
	}
	key := rawRow{}
	for _, col := range table.PrimaryKey {
		key[col] = normalized[col]
	}
	keyData, err := canonicalBusinessJSON(key)
	if err != nil {
		return err
	}
	rowData, err := canonicalBusinessJSON(normalized)
	if err != nil {
		return err
	}
	d.Leaves = append(d.Leaves, businessLeaf{sha256.Sum256(keyData), sha256.Sum256(rowData)})
	return nil
}
func (d *businessDigest) finish() (BusinessEquality, error) {
	sort.Slice(d.Leaves, func(i, j int) bool { return bytes.Compare(d.Leaves[i].Key[:], d.Leaves[j].Key[:]) < 0 })
	hash := sha256.New()
	fmt.Fprintf(hash, "full-business-v1:%d:", len(d.Leaves))
	for i, leaf := range d.Leaves {
		if i > 0 && leaf.Key == d.Leaves[i-1].Key {
			return BusinessEquality{}, errors.New("duplicate canonical business primary key")
		}
		hash.Write(leaf.Key[:])
		hash.Write(leaf.Row[:])
	}
	result := BusinessEquality{Rows: int64(len(d.Leaves)), SHA256: hex.EncodeToString(hash.Sum(nil))}
	d.Leaves = nil
	return result, nil
}
func canonicalBusinessJSON(row rawRow) ([]byte, error) {
	body, err := json.Marshal(row)
	if err != nil {
		return nil, errors.New("cannot canonicalize business record")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil, errors.New("invalid business canonical JSON")
	}
	var out bytes.Buffer
	if err = writeBusinessCanonical(&out, value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
func writeBusinessCanonical(out io.Writer, value any) error {
	switch v := value.(type) {
	case nil:
		fmt.Fprint(out, "z;")
	case bool:
		fmt.Fprintf(out, "b%t;", v)
	case string:
		fmt.Fprintf(out, "s%d:%s", len(v), v)
	case json.Number:
		n, ok := new(big.Rat).SetString(v.String())
		if !ok {
			return errors.New("invalid exact business number")
		}
		s := n.RatString()
		fmt.Fprintf(out, "n%d:%s", len(s), s)
	case []any:
		fmt.Fprintf(out, "a%d:", len(v))
		for _, item := range v {
			if err := writeBusinessCanonical(out, item); err != nil {
				return err
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		fmt.Fprintf(out, "o%d:", len(keys))
		for _, key := range keys {
			writeBusinessCanonical(out, key)
			if err := writeBusinessCanonical(out, v[key]); err != nil {
				return err
			}
		}
	default:
		return errors.New("unsupported business JSON type")
	}
	return nil
}
func businessSequences(ctx context.Context, tx *sql.Tx, advance bool) error {
	for _, item := range [][2]string{{"sandbox_bindings", "id"}, {"finance_operation_events", "id"}} {
		var sequence string
		var maximum int64
		if err := tx.QueryRowContext(ctx, `SELECT pg_get_serial_sequence($1,$2)`, "public."+item[0], item[1]).Scan(&sequence); err != nil || sequence != "public."+item[0]+"_id_seq" {
			return errors.New("unreviewed native business sequence")
		}
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(max("+quoteBusiness(item[1])+"),0) FROM public."+quoteBusiness(item[0])).Scan(&maximum); err != nil {
			return errors.New("cannot check copied business sequence maximum")
		}
		var current int64
		var called bool
		if err := tx.QueryRowContext(ctx, "SELECT last_value,is_called FROM public."+quoteBusiness(item[0]+"_id_seq")).Scan(&current, &called); err != nil {
			return errors.New("cannot read target sequence checkpoint")
		}
		if maximum > 0 && (maximum > current || (maximum == current && !called)) {
			if !advance {
				return errors.New("target business sequence is below copied maximum")
			}
			if _, err := tx.ExecContext(ctx, `SELECT setval($1::regclass,$2,true)`, sequence, maximum); err != nil {
				return errors.New("cannot advance target business sequence")
			}
		}
	}
	return nil
}
