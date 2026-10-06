package migrate

import (
	"context"
	"database/sql"
	"errors"
)

// VerifyNativeRuntime checks the same compiled 53-table contract used by the
// transfer tool, with catalog reads only. Application startup never repairs or
// upgrades the schema and never scans the copied business rows here.
func VerifyNativeRuntime(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("native runtime database unavailable")
	}
	tables, order, err := fullBusinessContract()
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return errors.New("cannot verify native runtime schema")
	}
	defer tx.Rollback()
	var database string
	var pgVersion, count, version int
	var dirty bool
	if tx.QueryRowContext(ctx, `SELECT current_database(),current_setting('server_version_num')::int`).Scan(&database, &pgVersion) != nil ||
		database != "account" || pgVersion < 170000 || pgVersion >= 180000 {
		return errors.New("native runtime requires PostgreSQL17 account database")
	}
	if tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(min(version),0),COALESCE(bool_or(dirty),true) FROM public.schema_migrations`).Scan(&count, &version, &dirty) != nil ||
		count != 1 || version != FullBusinessVersion || dirty {
		return errors.New("native runtime requires the exact clean Billing checkpoint")
	}
	if _, err = businessScope(ctx, tx, tables, false); err != nil {
		return err
	}
	for _, name := range order {
		if err = validateBusinessTarget(ctx, tx, name, tables[name]); err != nil {
			return err
		}
	}
	return tx.Commit()
}
