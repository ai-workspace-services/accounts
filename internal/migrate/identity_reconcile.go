package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	schema "account/sql"
)

// IdentityReconcileOptions applies only an exact, reviewed identity plan.
type IdentityReconcileOptions struct {
	Environment        string
	SchemaSHA256       string
	Apply              bool
	ExpectedPlanSHA256 string
}
type IdentityReceipt struct {
	Scope         string         `json:"scope"`
	PlanSHA256    string         `json:"plan_sha256"`
	Users         int            `json:"users"`
	ChangedUsers  int            `json:"changed_users"`
	ChangedFields map[string]int `json:"changed_fields"`
	Applied       bool           `json:"applied"`
}

// Runtime counters and timestamps are excluded: repairing identity must not
// rewind independent audit history or continuously rewrite otherwise equal users.
var identityColumns = []string{"uuid", "username", "email", "password", "role", "level", "groups", "permissions", "created_at", "mfa_totp_secret", "mfa_enabled", "mfa_secret_issued_at", "mfa_confirmed_at", "email_verified_at", "email_verified", "active", "proxy_uuid", "proxy_uuid_expires_at", "subscription_valid_from", "subscription_valid_until", "archived_at", "account_lifecycle_state", "account_lifecycle_changed_at", "account_lifecycle_actor_type", "account_lifecycle_actor_ref", "account_lifecycle_reason", "account_lifecycle_transition_id"}

type identityRow map[string]json.RawMessage

func identityRows(ctx context.Context, tx *sql.Tx) (map[string]identityRow, error) {
	cols := make([]string, len(identityColumns))
	for i, c := range identityColumns {
		cols[i] = quoteBusiness(c)
	}
	rows, err := tx.QueryContext(ctx, "SELECT row_to_json(s) FROM (SELECT "+strings.Join(cols, ",")+" FROM public.users ORDER BY uuid) s")
	if err != nil {
		return nil, errors.New("cannot read reviewed identity columns")
	}
	defer rows.Close()
	out := map[string]identityRow{}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, errors.New("cannot scan identity")
		}
		var row identityRow
		if json.Unmarshal(raw, &row) != nil {
			return nil, errors.New("cannot decode identity")
		}
		var id string
		if json.Unmarshal(row["uuid"], &id) != nil {
			return nil, errors.New("invalid identity key")
		}
		out[id] = row
	}
	return out, rows.Err()
}
func identityPlan(source, target map[string]identityRow, binding string) (IdentityReceipt, error) {
	r := IdentityReceipt{Scope: "identity_profile", Users: len(source), ChangedFields: map[string]int{}}
	if len(source) == 0 || len(source) != len(target) {
		return r, errors.New("user sets differ; identity reconciliation never adds or removes users")
	}
	ids := make([]string, 0, len(source))
	for id := range source {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		dst, ok := target[id]
		if !ok {
			return r, errors.New("stable user UUID sets differ")
		}
		changed := false
		for _, c := range identityColumns {
			if string(source[id][c]) != string(dst[c]) {
				r.ChangedFields[c]++
				changed = true
			}
		}
		if changed {
			r.ChangedUsers++
		}
	}
	// Hash complete canonical input without exposing per-user values or digests.
	raw, _ := json.Marshal([]any{binding, source, target})
	digest := sha256.Sum256(raw)
	r.PlanSHA256 = hex.EncodeToString(digest[:])
	return r, nil
}
func ReconcileIdentities(ctx context.Context, sourceDSN, targetDSN string, opt IdentityReconcileOptions) (IdentityReceipt, error) {
	var receipt IdentityReceipt
	_, manifest, err := schema.NativeArtifact()
	if err != nil {
		return receipt, err
	}
	if (opt.Environment != "prod" && opt.Environment != "uat") || opt.SchemaSHA256 != manifest.SchemaSHA256 {
		return receipt, errors.New("explicit environment and exact native schema checksum required")
	}
	if opt.Apply {
		b, e := hex.DecodeString(opt.ExpectedPlanSHA256)
		if e != nil || len(b) != 32 {
			return receipt, errors.New("apply requires exact preview plan SHA256")
		}
	}
	if err = validateBusinessConnections(sourceDSN, targetDSN); err != nil {
		return receipt, err
	}
	srcDB, err := openDB(ctx, sourceDSN)
	if err != nil {
		return receipt, errors.New("source connection failed")
	}
	defer srcDB.Close()
	dstDB, err := openDB(ctx, targetDSN)
	if err != nil {
		return receipt, errors.New("target connection failed")
	}
	defer dstDB.Close()
	src, err := srcDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return receipt, errors.New("source readonly transaction failed")
	}
	defer src.Rollback()
	if _, err = src.ExecContext(ctx, `SET LOCAL default_transaction_read_only=on; SET LOCAL statement_timeout='5min'; SET LOCAL timezone='UTC'; SET LOCAL datestyle='ISO,YMD'`); err != nil {
		return receipt, errors.New("cannot enforce source readonly")
	}
	if err = businessSourceRole(ctx, src); err != nil {
		return receipt, err
	}
	dst, err := dstDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted, ReadOnly: !opt.Apply})
	if err != nil {
		return receipt, errors.New("target transaction failed")
	}
	defer dst.Rollback()
	if _, err = dst.ExecContext(ctx, `SET LOCAL lock_timeout='15s'; SET LOCAL statement_timeout='5min'; SET LOCAL timezone='UTC'; SET LOCAL datestyle='ISO,YMD'`); err != nil {
		return receipt, errors.New("target budgets failed")
	}
	if opt.Apply {
		if _, err = dst.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1::bigint)`, migrationAdvisoryLockKey); err != nil {
			return receipt, errors.New("identity lock failed")
		}
		if _, err = dst.ExecContext(ctx, `LOCK TABLE public.schema_migrations, public.users IN ACCESS EXCLUSIVE MODE`); err != nil {
			return receipt, errors.New("target identity table lock failed")
		}
	}
	var database string
	var pgversion, version int
	var dirty bool
	if err = dst.QueryRowContext(ctx, `SELECT current_database(),current_setting('server_version_num')::int`).Scan(&database, &pgversion); err != nil || database != "account" || pgversion < 170000 || pgversion >= 180000 {
		return receipt, errors.New("target must be PostgreSQL17 account database")
	}
	if err = dst.QueryRowContext(ctx, `SELECT version,dirty FROM public.schema_migrations`).Scan(&version, &dirty); err != nil || dirty {
		return receipt, errors.New("clean target checkpoint required")
	}
	if err = verifyCatalogCheckpoint(ctx, dst, uint(version)); err != nil {
		return receipt, err
	}
	a, err := identityRows(ctx, src)
	if err != nil {
		return receipt, err
	}
	b, err := identityRows(ctx, dst)
	if err != nil {
		return receipt, err
	}
	// Bind both database identities without storing connection credentials.
	var srcIdentity, dstIdentity string
	if err = src.QueryRowContext(ctx, `SELECT current_database()||'@'||inet_server_addr()::text||':'||inet_server_port()::text`).Scan(&srcIdentity); err != nil {
		return receipt, errors.New("cannot bind source identity")
	}
	if err = dst.QueryRowContext(ctx, `SELECT current_database()||'@'||inet_server_addr()::text||':'||inet_server_port()::text`).Scan(&dstIdentity); err != nil {
		return receipt, errors.New("cannot bind target identity")
	}
	receipt, err = identityPlan(a, b, fmt.Sprintf("%s:%s:%s:%s:%d", opt.Environment, srcIdentity, dstIdentity, opt.SchemaSHA256, version))
	if err != nil {
		return receipt, err
	}
	if !opt.Apply {
		return receipt, nil
	}
	if receipt.PlanSHA256 != opt.ExpectedPlanSHA256 {
		return receipt, errors.New("identity plan changed; repeat preview")
	}
	for id, row := range a {
		fields := []string{}
		for _, c := range identityColumns {
			if c != "uuid" && string(row[c]) != string(b[id][c]) {
				fields = append(fields, c)
			}
		}
		if len(fields) == 0 {
			continue
		}
		// Expand only changed reviewed columns; leave all other target facts intact.
		sets := make([]string, len(fields))
		for i, c := range fields {
			sets[i] = quoteBusiness(c) + "=s." + quoteBusiness(c)
		}
		payload, _ := json.Marshal(row)
		q := `UPDATE public.users u SET ` + strings.Join(sets, ",") + ` FROM jsonb_populate_record(NULL::public.users,$1::jsonb) s WHERE u.uuid=$2::uuid`
		if _, err = dst.ExecContext(ctx, q, string(payload), id); err != nil {
			return receipt, errors.New("identity update failed; transaction rolled back")
		}
	}
	after, err := identityRows(ctx, dst)
	if err != nil {
		return receipt, err
	}
	check, err := identityPlan(a, after, "")
	if err != nil || check.ChangedUsers != 0 {
		return receipt, errors.New("post-apply identity verification failed")
	}
	if err = src.Commit(); err != nil {
		return receipt, errors.New("source snapshot close failed")
	}
	if err = dst.Commit(); err != nil {
		return receipt, errors.New("identity commit failed")
	}
	receipt.Applied = true
	return receipt, nil
}
