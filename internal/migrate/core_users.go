package migrate

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"account/internal/dbruntime"
	schema "account/sql"
	"github.com/google/uuid"
)

// CoreUsersOptions narrows the transfer to the users identity surface. The
// target schema is still required to be the reviewed native checkpoint, but
// no dynamic business table is read or written.
type CoreUsersOptions struct {
	Environment   string
	SchemaSHA256  string
	BillingSHA256 string
	WritersPaused bool
	CompareOnly   bool
}

// CopyCoreUsers reconciles users by normalized email. Existing target UUIDs are
// retained while only email, password hash and authoritative Proxy UUID are
// updated; missing source users receive new target UUIDs. Target-only emails
// are refused so the operation never deletes users or unrelated references.
func CopyCoreUsers(ctx context.Context, sourceDSN, targetDSN string, options CoreUsersOptions) (FullBusinessReceipt, error) {
	receipt := FullBusinessReceipt{Format: 1, Scope: "core_users", Environment: options.Environment,
		MigrationVersion: FullBusinessVersion, Tables: map[string]BusinessEquality{},
		BatchSize: fullBusinessBatch, SequencePolicy: "core-users-only; target user UUIDs may differ"}
	_, manifest, err := schema.NativeArtifact()
	if err != nil {
		return receipt, err
	}
	if (options.Environment != "prod" && options.Environment != "uat") || !options.WritersPaused ||
		options.SchemaSHA256 != manifest.SchemaSHA256 || options.BillingSHA256 != FullBusinessBillingSHA256 {
		return receipt, errors.New("core-user transfer requires explicit environment, paused writers and both reviewed schema hashes")
	}
	receipt.SchemaSHA256 = manifest.SchemaSHA256
	receipt.BillingSHA256 = FullBusinessBillingSHA256
	tables, _, err := fullBusinessContract()
	if err != nil {
		return receipt, err
	}
	usersTable, ok := tables["users"]
	if !ok {
		return receipt, errors.New("reviewed users table is missing")
	}
	if err = validateBusinessConnections(sourceDSN, targetDSN); err != nil {
		return receipt, err
	}
	receipt.SourceIdentitySHA256, err = dbruntime.ConnectionIdentity(sourceDSN)
	if err != nil {
		return receipt, errors.New("core-user source connection identity is invalid")
	}
	source, err := openDB(ctx, sourceDSN)
	if err != nil {
		return receipt, errors.New("core-user source connection failed")
	}
	defer source.Close()
	target, err := openDB(ctx, targetDSN)
	if err != nil {
		return receipt, errors.New("core-user target connection failed")
	}
	defer target.Close()
	src, err := source.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return receipt, errors.New("cannot start read-only core-user snapshot")
	}
	defer src.Rollback()
	if _, err = src.ExecContext(ctx, `SET LOCAL default_transaction_read_only=on; SET LOCAL timezone='UTC'; SET LOCAL datestyle='ISO,YMD'; SET LOCAL statement_timeout='5min'; SET LOCAL lock_timeout='15s'; SET LOCAL idle_in_transaction_session_timeout='5min'`); err != nil {
		return receipt, errors.New("cannot enforce core-user readonly transaction")
	}
	dst, err := target.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable, ReadOnly: options.CompareOnly})
	if err != nil {
		return receipt, errors.New("cannot start core-user target transaction")
	}
	defer dst.Rollback()
	if _, err = dst.ExecContext(ctx, `SET LOCAL timezone='UTC'; SET LOCAL datestyle='ISO,YMD'; SET LOCAL statement_timeout='5min'; SET LOCAL lock_timeout='15s'; SET LOCAL idle_in_transaction_session_timeout='5min'`); err != nil {
		return receipt, errors.New("cannot enforce core-user transaction budgets")
	}
	receipt.SnapshotStartedAt = time.Now().UTC()
	if err = businessSourceRole(ctx, src); err != nil {
		return receipt, err
	}
	columns, err := catalogColumns(ctx, src, "users")
	if err != nil || validateBusinessSourceColumns("users", columns, usersTable) != nil {
		return receipt, errors.New("source users schema differs from reviewed native contract")
	}
	if err = businessSourceVisibility(ctx, src, "users"); err != nil {
		return receipt, err
	}
	receipt.SourceTables = 1
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
	if err = validateBusinessTarget(ctx, dst, "users", usersTable); err != nil {
		return receipt, err
	}
	sourceUsers, err := businessUsers(ctx, src)
	if err != nil || len(sourceUsers) == 0 {
		return receipt, errors.New("core-user source population is empty or unreadable")
	}
	receipt.UserCount = len(sourceUsers)
	receipt.CoreUsers.Source, err = coreUsersDigest(sourceUsers)
	if err != nil {
		return receipt, err
	}
	targetUsers, err := businessUsers(ctx, dst)
	if err != nil {
		return receipt, err
	}
	missing, err := coreUserMissing(sourceUsers, targetUsers)
	if err != nil {
		return receipt, err
	}
	if options.CompareOnly {
		if !equalCoreUsers(sourceUsers, targetUsers) {
			return receipt, errors.New("core user email, password hash or Proxy UUID differs")
		}
	} else {
		if _, err = dst.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1::bigint)`, migrationAdvisoryLockKey); err != nil {
			return receipt, errors.New("cannot lock core-user transfer")
		}
		if _, err = dst.ExecContext(ctx, `LOCK TABLE public.schema_migrations, public.users IN ACCESS EXCLUSIVE MODE`); err != nil {
			return receipt, errors.New("cannot exclusively lock target users table")
		}
		// Refresh after the lock so the email set and retained UUIDs are based on
		// the same target state that receives the reconciliation.
		targetUsers, err = businessUsers(ctx, dst)
		if err != nil {
			return receipt, err
		}
		missing, err = coreUserMissing(sourceUsers, targetUsers)
		if err != nil {
			return receipt, err
		}
		for key, targetUser := range targetUsers {
			sourceUser := sourceUsers[key]
			if targetUser.Email == sourceUser.Email && targetUser.PasswordHash == sourceUser.PasswordHash && targetUser.Proxy == sourceUser.Proxy {
				// Replays must not fire metadata triggers for already-aligned users.
				continue
			}
			if _, err = dst.ExecContext(ctx, `UPDATE public.users SET email=$1,password=$2,proxy_uuid=$3 WHERE uuid=$4::uuid`,
				sourceUser.Email, sourceUser.PasswordHash, sourceUser.Proxy, targetUser.ID); err != nil {
				return receipt, errors.New("cannot reconcile existing core user")
			}
		}
		missingSet := map[string]bool{}
		for _, key := range missing {
			missingSet[key] = true
		}
		if err = streamBusiness(ctx, src, "users", usersTable.PrimaryKey, func(page []rawRow) error {
			filtered := make([]rawRow, 0, len(page))
			for _, row := range page {
				key := strings.ToLower(strings.TrimSpace(rowString(row, "email")))
				if !missingSet[key] {
					continue
				}
				mapping := map[string]string{rowString(row, "uuid"): uuid.NewString()}
				if err := projectBusinessRow("users", row, columns, usersTable, mapping); err != nil {
					return err
				}
				filtered = append(filtered, row)
			}
			if len(filtered) == 0 {
				return nil
			}
			return insertBusiness(ctx, dst, "users", usersTable, filtered)
		}); err != nil {
			return receipt, err
		}
		targetUsers, err = businessUsers(ctx, dst)
		if err != nil || !equalCoreUsers(sourceUsers, targetUsers) {
			return receipt, errors.New("core user email, password hash or Proxy UUID differs after target reconciliation")
		}
	}
	receipt.CoreUsers.Target, err = coreUsersDigest(targetUsers)
	if err != nil {
		return receipt, err
	}
	receipt.FullBusinessEqual = true
	receipt.TargetWrites = !options.CompareOnly
	if options.CompareOnly {
		receipt.Result = "equal"
	} else {
		receipt.Result = "copied"
	}
	if err = src.Commit(); err != nil {
		return receipt, errors.New("source core-user snapshot completion failed")
	}
	if err = dst.Commit(); err != nil {
		return receipt, errors.New("target core-user transaction did not commit")
	}
	receipt.CompletedAt = time.Now().UTC()
	return receipt, nil
}
