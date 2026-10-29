package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// VerifyNativeRuntime checks the same compiled 55-table contract used by the
// transfer tool, with catalog reads only. Application startup never repairs or
// upgrades the schema and never scans the copied business rows here.
func VerifyNativeRuntime(ctx context.Context, db *sql.DB) error {
	_, err := VerifiedNativeRuntimeVersion(ctx, db)
	return err
}

// VerifiedNativeRuntimeVersion accepts only reviewed, data-only catalog
// checkpoints over the unchanged native schema and verifies their SQL receipts.
func VerifiedNativeRuntimeVersion(ctx context.Context, db *sql.DB) (uint, error) {
	if db == nil {
		return 0, errors.New("native runtime database unavailable")
	}
	tables, order, err := fullBusinessContract()
	if err != nil {
		return 0, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return 0, errors.New("cannot verify native runtime schema")
	}
	defer tx.Rollback()
	var database string
	var pgVersion, count, version int
	var dirty bool
	if tx.QueryRowContext(ctx, `SELECT current_database(),current_setting('server_version_num')::int`).Scan(&database, &pgVersion) != nil ||
		database != "account" || pgVersion < 170000 || pgVersion >= 180000 {
		return 0, errors.New("native runtime requires PostgreSQL17 account database")
	}
	if tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(min(version),0),COALESCE(bool_or(dirty),true) FROM public.schema_migrations`).Scan(&count, &version, &dirty) != nil ||
		count != 1 || dirty {
		return 0, errors.New("native runtime requires the exact clean Billing checkpoint")
	}
	if err := verifyCatalogCheckpoint(ctx, tx, uint(version)); err != nil {
		return 0, err
	}
	if _, err = businessScope(ctx, tx, tables, false); err != nil {
		return 0, err
	}
	for _, name := range order {
		if err = validateBusinessTargetCatalog(ctx, tx, name, tables[name], true); err != nil {
			return 0, err
		}
		var readable bool
		if tx.QueryRowContext(ctx, `SELECT has_table_privilege(current_user,to_regclass($1),'SELECT')`, "public."+name).Scan(&readable) != nil || !readable {
			return 0, errors.New("native runtime requires SELECT on every business table")
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return uint(version), nil
}

func verifyCatalogCheckpoint(ctx context.Context, tx *sql.Tx, version uint) error {
	if version == FullBusinessVersion {
		return nil
	}
	if version != 2026101901 && version != 2026101902 {
		return errors.New("native runtime requires a reviewed clean catalog checkpoint")
	}
	for _, step := range []struct {
		from, to uint
		hash     string
	}{
		{2026101703, 2026101901, "b9fee298512e7d888fe619917c8238120f0e227f4ea348e59397d074c41f878d"},
		{2026101901, 2026101902, "53f9aaf7c56a14fd7c132c6649b79582cd2063f8903b94efb4e23cbf4895437c"},
	} {
		if step.to > version {
			continue
		}
		var from uint
		var hash string
		if err := tx.QueryRowContext(ctx, `SELECT from_version,sql_sha256 FROM public.accounts_controlled_migration_history WHERE to_version=$1`, step.to).Scan(&from, &hash); err != nil || from != step.from || hash != step.hash {
			return errors.New("native runtime catalog migration receipt differs")
		}
	}
	return nil
}

// runtimeBusinessVisibility accepts only a reviewed role-specific, permissive
// ALL policy that exposes the complete table and permits writes. It does not
// create policies or change privileges. The strict transfer verifier never uses
// this runtime exception.
func runtimeBusinessVisibility(ctx context.Context, tx *sql.Tx, name string) error {
	var valid bool
	err := tx.QueryRowContext(ctx, `SELECT has_table_privilege(current_user,c.oid,'SELECT') AND (
 EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid=c.oid AND p.polname='accounts_runtime_access'
 AND p.polcmd='*' AND p.polpermissive AND p.polroles=ARRAY[(SELECT oid FROM pg_roles WHERE rolname=current_user)]
 AND pg_get_expr(p.polqual,p.polrelid)='true' AND pg_get_expr(p.polwithcheck,p.polrelid)='true')
 AND NOT EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.member=(SELECT oid FROM pg_roles WHERE rolname=current_user))
 AND NOT EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid=c.oid AND NOT p.polpermissive
 AND (0::oid=ANY(p.polroles) OR (SELECT oid FROM pg_roles WHERE rolname=current_user)=ANY(p.polroles))))
 FROM pg_class c WHERE c.relnamespace='public'::regnamespace AND c.relname=$1 AND c.relkind='r'`, name).Scan(&valid)
	if err != nil || !valid {
		return fmt.Errorf("reviewed runtime RLS access not proven for public.%s", name)
	}
	return nil
}
