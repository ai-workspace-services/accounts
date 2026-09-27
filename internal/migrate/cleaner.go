package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Cleaner removes invalid indexes while preserving tables and triggers.
type Cleaner struct{}

func NewCleaner() *Cleaner {
	return &Cleaner{}
}

func (c *Cleaner) Clean(ctx context.Context, dsn string, force bool) error {
	if !force {
		return errors.New("clean requires --force confirmation")
	}

	db, err := openDB(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	tx, err := db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := dropInvalidIndexes(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	fmt.Println("✅ Invalid-index cleanup completed; tables and triggers were preserved")
	return nil
}

func dropInvalidIndexes(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname) FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname NOT IN ('pglogical', 'pg_catalog', 'information_schema') AND (NOT i.indisvalid OR NOT i.indisready)")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var identifier string
		if err := rows.Scan(&identifier); err != nil {
			return err
		}
		fmt.Printf("→ Dropping invalid index %s\n", identifier)
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("DROP INDEX IF EXISTS %s", identifier)); err != nil {
			return err
		}
		fmt.Printf("✅ Dropped index %s\n", identifier)
	}

	return rows.Err()
}
