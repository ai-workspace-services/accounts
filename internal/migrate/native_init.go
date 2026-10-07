package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"

	schema "account/sql"
)

type NativeInitOptions struct {
	Environment      string
	SchemaSHA256     string
	WritersPaused    bool
	DryRun           bool
	LockTimeout      time.Duration
	StatementTimeout time.Duration
}

type NativeInitReceipt struct {
	Result                  string   `json:"result"`
	Environment             string   `json:"environment"`
	Database                string   `json:"database"`
	SchemaSHA256            string   `json:"schema_sha256"`
	MigrationVersion        uint     `json:"migration_version"`
	BusinessTables          []string `json:"business_tables"`
	BusinessRows            int      `json:"business_rows"`
	DatabaseCutoverApproved bool     `json:"database_cutover_approved"`
}

// InitializeNative installs the reviewed final schema once, without replaying
// historical upgrades or seeding application rows. Playbooks must independently
// verify the canonical CMDB host, persistent disk and stopped writer containers.
func InitializeNative(ctx context.Context, dsn string, options NativeInitOptions) (NativeInitReceipt, error) {
	var receipt NativeInitReceipt
	body, manifest, err := schema.NativeArtifact()
	if err != nil {
		return receipt, err
	}
	if options.Environment != "uat" && options.Environment != "prod" {
		return receipt, errors.New("native initialization requires explicit uat or prod environment")
	}
	if !options.WritersPaused || options.SchemaSHA256 != manifest.SchemaSHA256 {
		return receipt, errors.New("native initialization requires paused writers and the exact reviewed schema hash")
	}
	if options.LockTimeout <= 0 || options.LockTimeout > time.Minute || options.StatementTimeout <= 0 || options.StatementTimeout > 5*time.Minute {
		return receipt, errors.New("native initialization timeout budgets are invalid")
	}
	db, err := openDB(ctx, dsn)
	if err != nil {
		return receipt, errors.New("native initialization database connection failed")
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return receipt, errors.New("cannot start native initialization transaction")
	}
	defer tx.Rollback()
	for key, value := range map[string]time.Duration{"lock_timeout": options.LockTimeout, "statement_timeout": options.StatementTimeout} {
		if _, err = tx.ExecContext(ctx, "SELECT set_config($1,$2,true)", key, fmt.Sprintf("%dms", value.Milliseconds())); err != nil {
			return receipt, errors.New("cannot set native initialization timeout")
		}
	}
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1::bigint)", migrationAdvisoryLockKey); err != nil {
		return receipt, errors.New("cannot acquire native initialization lock")
	}
	var database string
	var objectCount int
	if err = tx.QueryRowContext(ctx, "SELECT current_database()").Scan(&database); err != nil || database != "account" {
		return receipt, errors.New("native initialization target must be the account database")
	}
	if err = tx.QueryRowContext(ctx, nativeObjectCountSQL).Scan(&objectCount); err != nil || objectCount != 0 {
		return receipt, errors.New("database contains application objects or its state cannot be verified; initialization refused")
	}
	receipt = NativeInitReceipt{Result: "eligible", Environment: options.Environment, Database: database,
		SchemaSHA256: manifest.SchemaSHA256, MigrationVersion: manifest.MigrationVersion, BusinessTables: manifest.BusinessTables}
	if options.DryRun {
		return receipt, nil
	}
	if _, err = tx.ExecContext(ctx, string(body)); err != nil {
		return NativeInitReceipt{}, errors.New("native schema SQL failed; transaction rolled back")
	}
	rows, err := tx.QueryContext(ctx, "SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p') ORDER BY c.relname")
	if err != nil {
		return NativeInitReceipt{}, errors.New("cannot verify initialized business tables")
	}
	actual := make([]string, 0, len(manifest.BusinessTables))
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return NativeInitReceipt{}, errors.New("cannot inspect initialized business tables")
		}
		actual = append(actual, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(actual) != len(manifest.BusinessTables) {
		return NativeInitReceipt{}, errors.New("initialized business scope differs; transaction rolled back")
	}
	for i, name := range actual {
		if name != manifest.BusinessTables[i] {
			return NativeInitReceipt{}, errors.New("initialized business scope differs; transaction rolled back")
		}
	}
	for _, name := range actual {
		if !regexp.MustCompile(`^[a-z][a-z0-9_]*$`).MatchString(name) {
			return NativeInitReceipt{}, errors.New("invalid business table identifier")
		}
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM public."`+name+`"`).Scan(&count); err != nil || count != 0 {
			return NativeInitReceipt{}, errors.New("native initialization must not create business rows; transaction rolled back")
		}
	}
	if _, err = tx.ExecContext(ctx, "CREATE TABLE public.schema_migrations (version bigint PRIMARY KEY, dirty boolean NOT NULL)"); err != nil {
		return NativeInitReceipt{}, errors.New("cannot establish native migration version; transaction rolled back")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO public.schema_migrations(version,dirty) VALUES ($1,false)", manifest.MigrationVersion); err != nil {
		return NativeInitReceipt{}, errors.New("cannot establish native migration version; transaction rolled back")
	}
	if err = tx.Commit(); err != nil {
		return NativeInitReceipt{}, errors.New("native initialization commit failed")
	}
	receipt.Result = "initialized"
	return receipt, nil
}

// Extension-owned objects do not establish an application baseline. Inspect all
// application schemas, not just public.users, before any native DDL is executed.
const nativeObjectCountSQL = `SELECT
 (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname<>'information_schema' AND c.relkind IN ('r','p','v','m','S','f')
 AND NOT EXISTS(SELECT 1 FROM pg_depend d WHERE d.classid='pg_class'::regclass AND d.objid=c.oid AND d.deptype='e'))
 + (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
 WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname<>'information_schema'
 AND NOT EXISTS(SELECT 1 FROM pg_depend d WHERE d.classid='pg_proc'::regclass AND d.objid=p.oid AND d.deptype='e'))
 + (SELECT count(*) FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace
 WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname<>'information_schema' AND t.typtype IN ('e','d','r','m')
 AND NOT EXISTS(SELECT 1 FROM pg_depend d WHERE d.classid='pg_type'::regclass AND d.objid=t.oid AND d.deptype='e'))`
