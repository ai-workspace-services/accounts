package migrate

// The usage reconciliation is deliberately separate from initial recovery and
// schema migration. It never executes DDL, deletes rows, or updates ledger rows.
import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"account/internal/dbruntime"
	schema "account/sql"
)

var usageMergeTables = []string{"account_billing_profiles", "account_quota_states", "traffic_minute_buckets", "billing_ledger", "finance_invoices", "finance_payments", "finance_refunds"}

type UsageMergeOptions struct {
	SourceGeneration      string
	TargetGeneration      string
	SchemaSHA256          string
	BillingSHA256         string
	Apply                 bool
	WritersPaused         bool
	ExpectedPlanSHA256    string
	ExpectedTargetVersion uint
}

type UsageMergeTable struct {
	Source     BusinessEquality `json:"source"`
	Before     BusinessEquality `json:"before"`
	After      BusinessEquality `json:"after"`
	Append     int64            `json:"append"`
	Update     int64            `json:"update"`
	Equal      int64            `json:"equal"`
	Conflicts  int64            `json:"conflicts"`
	TargetOnly int64            `json:"target_only"`
}

// Receipts contain aggregate hashes and counts, never user keys or row values.
type UsageMergeReceipt struct {
	Format               int                        `json:"format"`
	Result               string                     `json:"result"`
	SourceGeneration     string                     `json:"source_generation"`
	TargetGeneration     string                     `json:"target_generation"`
	SourceIdentitySHA256 string                     `json:"source_identity_sha256"`
	TargetIdentitySHA256 string                     `json:"target_identity_sha256"`
	TargetVersion        uint                       `json:"target_version"`
	SnapshotStartedAt    time.Time                  `json:"snapshot_started_at"`
	CompletedAt          time.Time                  `json:"completed_at"`
	PlanSHA256           string                     `json:"plan_sha256"`
	SchemaBeforeSHA256   string                     `json:"schema_before_sha256"`
	SchemaAfterSHA256    string                     `json:"schema_after_sha256"`
	UsersBefore          BusinessEquality           `json:"users_before"`
	UsersAfter           BusinessEquality           `json:"users_after"`
	MappedUsers          int                        `json:"mapped_users"`
	MissingUsers         int                        `json:"missing_users"`
	Tables               map[string]UsageMergeTable `json:"tables"`
	Conflicts            int64                      `json:"conflicts"`
	SourceReadOnly       bool                       `json:"source_read_only"`
	TargetWrites         bool                       `json:"target_writes"`
}

type usageMergeIndex map[[32]byte][32]byte

func ReconcileUsage(ctx context.Context, sourceDSN, targetDSN string, opts UsageMergeOptions) (UsageMergeReceipt, error) {
	r := UsageMergeReceipt{Format: 1, Result: "preview", SourceGeneration: opts.SourceGeneration, TargetGeneration: opts.TargetGeneration, Tables: map[string]UsageMergeTable{}}
	_, manifest, err := schema.NativeArtifact()
	if err != nil {
		return r, err
	}
	if err := validateGenerationPair(opts.SourceGeneration, opts.TargetGeneration); err != nil {
		return r, err
	}
	if !strings.HasPrefix(opts.SourceGeneration, "prod-v") || opts.ExpectedTargetVersion == 0 || opts.SchemaSHA256 != manifest.SchemaSHA256 || opts.BillingSHA256 != FullBusinessBillingSHA256 {
		return r, errors.New("usage merge requires consecutive PROD generations, target version and compiled schema hashes")
	}
	if opts.Apply && (!opts.WritersPaused || !validUsageHash(opts.ExpectedPlanSHA256)) {
		return r, errors.New("apply requires paused target writers and the reviewed preview SHA-256")
	}
	if err = validateBusinessConnections(sourceDSN, targetDSN); err != nil {
		return r, err
	}
	r.SourceIdentitySHA256, err = dbruntime.ConnectionIdentity(sourceDSN)
	if err != nil {
		return r, errors.New("invalid source identity")
	}
	r.TargetIdentitySHA256, err = dbruntime.ConnectionIdentity(targetDSN)
	if err != nil || r.SourceIdentitySHA256 == r.TargetIdentitySHA256 {
		return r, errors.New("source and target identity must differ")
	}
	source, err := openDB(ctx, sourceDSN)
	if err != nil {
		return r, errors.New("source connection failed")
	}
	defer source.Close()
	target, err := openDB(ctx, targetDSN)
	if err != nil {
		return r, errors.New("target connection failed")
	}
	defer target.Close()
	src, err := source.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return r, errors.New("cannot begin source snapshot")
	}
	defer src.Rollback()
	if _, err = src.ExecContext(ctx, `SET LOCAL default_transaction_read_only=on; SET LOCAL timezone='UTC'; SET LOCAL datestyle='ISO,YMD'; SET LOCAL statement_timeout='5min'; SET LOCAL lock_timeout='15s'; SET LOCAL idle_in_transaction_session_timeout='5min'`); err != nil {
		return r, errors.New("cannot enforce source readonly budgets")
	}
	if err = businessSourceRole(ctx, src); err != nil {
		return r, err
	}
	r.SourceReadOnly = true
	dst, err := target.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable, ReadOnly: !opts.Apply})
	if err != nil {
		return r, errors.New("cannot begin target transaction")
	}
	defer dst.Rollback()
	if _, err = dst.ExecContext(ctx, `SET LOCAL timezone='UTC'; SET LOCAL datestyle='ISO,YMD'; SET LOCAL statement_timeout='5min'; SET LOCAL lock_timeout='15s'; SET LOCAL idle_in_transaction_session_timeout='5min'`); err != nil {
		return r, errors.New("cannot enforce target budgets")
	}
	tables, order, err := fullBusinessContract()
	if err != nil {
		return r, err
	}
	if opts.Apply {
		// Lock before the first target data snapshot. Keep triggers and all FK,
		// CHECK and UNIQUE guards active throughout the transaction.
		locks := []string{"public.schema_migrations", "public.accounts_controlled_migration_history"}
		for _, name := range order {
			locks = append(locks, "public."+quoteBusiness(name))
		}
		if _, err = dst.ExecContext(ctx, "LOCK TABLE "+strings.Join(locks, ",")+" IN SHARE ROW EXCLUSIVE MODE"); err != nil {
			return r, errors.New("cannot lock target business tables")
		}
		if _, err = dst.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1::bigint)`, migrationAdvisoryLockKey); err != nil {
			return r, errors.New("cannot lock reconciliation")
		}
	}
	var dbName string
	var pgVersion, count int
	var version uint
	var dirty bool
	if dst.QueryRowContext(ctx, `SELECT current_database(),current_setting('server_version_num')::int`).Scan(&dbName, &pgVersion) != nil || dbName != "account" || pgVersion < 170000 || pgVersion >= 180000 {
		return r, errors.New("target requires PostgreSQL17 account database")
	}
	if dst.QueryRowContext(ctx, `SELECT count(*),COALESCE(min(version),0),COALESCE(bool_or(dirty),true) FROM public.schema_migrations`).Scan(&count, &version, &dirty) != nil || count != 1 || dirty || version != opts.ExpectedTargetVersion {
		return r, errors.New("target migration version or clean state differs")
	}
	if err = verifyCatalogCheckpoint(ctx, dst, version); err != nil {
		return r, err
	}
	r.TargetVersion = version
	if _, err = businessScope(ctx, dst, tables, false); err != nil {
		return r, err
	}
	for _, name := range order {
		if err = validateBusinessTarget(ctx, dst, name, tables[name]); err != nil {
			return r, err
		}
	}
	r.SnapshotStartedAt = time.Now().UTC()
	r.SchemaBeforeSHA256, err = usageSchemaDigest(ctx, dst)
	if err != nil {
		return r, err
	}
	if err = businessSourceVisibility(ctx, src, "users"); err != nil {
		return r, err
	}
	sourceUsers, err := businessUsers(ctx, src)
	if err != nil || len(sourceUsers) == 0 {
		return r, errors.New("source user matching population unavailable")
	}
	targetUsers, err := businessUsers(ctx, dst)
	if err != nil {
		return r, err
	}
	mapping := map[string]string{}
	for key, user := range sourceUsers {
		matched, ok := targetUsers[key]
		if !ok || user.Proxy != matched.Proxy {
			r.MissingUsers++
			continue
		}
		mapping[user.ID] = matched.ID
		r.MappedUsers++
	}
	if r.MissingUsers != 0 {
		return r, errors.New("missing or ambiguous user identity mapping; reconcile identities first")
	}
	// Snapshot every target business table to detect trigger side effects on
	// tables outside the narrow data scope as well as the protected catalog.
	before := map[string]BusinessEquality{}
	for _, name := range order {
		before[name], _, err = usageReadIndex(ctx, dst, name, tables[name], nil)
		if err != nil {
			return r, err
		}
	}
	r.UsersBefore = before["users"]
	expected := map[string]usageMergeIndex{}
	sourceCols := map[string][]ColumnDefinition{}
	for _, name := range usageMergeTables {
		cols, e := catalogColumns(ctx, src, name)
		if e != nil || validateBusinessSourceColumns(name, cols, tables[name]) != nil {
			return r, fmt.Errorf("source public.%s schema is not reviewed", name)
		}
		if e = businessSourceVisibility(ctx, src, name); e != nil {
			return r, e
		}
		sourceCols[name] = cols
		targetDigest, targetIndex, e := usageReadIndex(ctx, dst, name, tables[name], nil)
		if e != nil {
			return r, e
		}
		sourceDigest, sourceIndex, e := usageReadIndex(ctx, src, name, tables[name], func(row rawRow) error { return projectBusinessRow(name, row, cols, tables[name], mapping) })
		if e != nil {
			return r, e
		}
		stats, wanted := usageMergePlan(name, sourceIndex, targetIndex)
		stats.Source = sourceDigest
		stats.Before = targetDigest
		r.Tables[name] = stats
		expected[name] = wanted
		r.Conflicts += stats.Conflicts
	}
	// A snapshot quota/balance cannot erase independent V3 usage or money.
	// Conservatively refuse any quota replacement while target-only accounting
	// facts exist. Append-only transfers without quota replacement remain valid.
	if r.Tables["account_quota_states"].Update+r.Tables["account_quota_states"].Append > 0 {
		for _, name := range usageMergeTables[2:] {
			if r.Tables[name].TargetOnly > 0 {
				r.Conflicts++
				break
			}
		}
	}
	// A missing V3 profile cannot safely be initialized from old V2 pricing.
	// Profiles are comparison-only and all differences are conflicts.
	plan, err := json.Marshal(struct {
		Source, Target, Schema string
		Version                uint
		Users                  BusinessEquality
		Before                 map[string]BusinessEquality
		Tables                 map[string]UsageMergeTable
		Conflicts              int64
	}{r.SourceIdentitySHA256, r.TargetIdentitySHA256, r.SchemaBeforeSHA256, version, r.UsersBefore, before, r.Tables, r.Conflicts})
	if err != nil {
		return r, errors.New("cannot bind reconciliation plan")
	}
	sum := sha256.Sum256(plan)
	r.PlanSHA256 = hex.EncodeToString(sum[:])
	if r.Conflicts > 0 {
		r.Result = "conflict"
		return r, errors.New("accounting or quota conflicts detected; no target writes committed")
	}
	if opts.Apply && opts.ExpectedPlanSHA256 != r.PlanSHA256 {
		return r, errors.New("source snapshot or target baseline changed; repeat preview")
	}
	if opts.Apply {
		for _, name := range usageMergeTables {
			if name == "account_billing_profiles" {
				continue
			}
			err = streamBusiness(ctx, src, name, tables[name].PrimaryKey, func(page []rawRow) error {
				for _, row := range page {
					if err := projectBusinessRow(name, row, sourceCols[name], tables[name], mapping); err != nil {
						return err
					}
				}
				return usageApplyBatch(ctx, dst, name, tables[name], page)
			})
			if err != nil {
				return r, err
			}
		}
	}
	for _, name := range order {
		after, index, e := usageReadIndex(ctx, dst, name, tables[name], nil)
		if e != nil {
			return r, e
		}
		if want, ok := expected[name]; ok {
			stats := r.Tables[name]
			stats.After = after
			r.Tables[name] = stats
			if opts.Apply && !usageIndexesEqual(want, index) {
				return r, fmt.Errorf("post-merge public.%s fingerprint differs; transaction rolled back", name)
			}
		} else if before[name] != after {
			return r, fmt.Errorf("protected public.%s changed; transaction rolled back", name)
		}
		if name == "users" {
			r.UsersAfter = after
		}
	}
	r.SchemaAfterSHA256, err = usageSchemaDigest(ctx, dst)
	if err != nil || r.SchemaAfterSHA256 != r.SchemaBeforeSHA256 {
		return r, errors.New("schema changed; transaction rolled back")
	}
	if err = src.Commit(); err != nil {
		return r, errors.New("source snapshot completion failed")
	}
	if err = dst.Commit(); err != nil {
		return r, errors.New("target transaction failed; merge not accepted")
	}
	r.CompletedAt = time.Now().UTC()
	r.TargetWrites = opts.Apply
	if opts.Apply {
		r.Result = "merged"
	}
	return r, nil
}

func validUsageHash(value string) bool {
	b, e := hex.DecodeString(value)
	return e == nil && len(b) == sha256.Size && strings.ToLower(value) == value
}

func usageReadIndex(ctx context.Context, tx *sql.Tx, name string, table businessTable, project func(rawRow) error) (BusinessEquality, usageMergeIndex, error) {
	digest := businessDigest{}
	index := usageMergeIndex{}
	err := streamBusiness(ctx, tx, name, table.PrimaryKey, func(page []rawRow) error {
		for _, row := range page {
			if project != nil {
				if e := project(row); e != nil {
					return e
				}
			}
			if e := digest.add(row, table); e != nil {
				return e
			}
			leaf := digest.Leaves[len(digest.Leaves)-1]
			if _, ok := index[leaf.Key]; ok {
				return errors.New("duplicate mapped accounting key")
			}
			index[leaf.Key] = leaf.Row
		}
		return nil
	})
	if err != nil {
		return BusinessEquality{}, nil, err
	}
	result, err := digest.finish()
	return result, index, err
}

func usageMergePlan(name string, source, target usageMergeIndex) (UsageMergeTable, usageMergeIndex) {
	stats := UsageMergeTable{}
	expected := usageMergeIndex{}
	for key, value := range target {
		expected[key] = value
	}
	for key, value := range source {
		prior, ok := target[key]
		switch {
		case ok && prior == value:
			stats.Equal++
		case name == "account_billing_profiles":
			stats.Conflicts++
		case !ok:
			stats.Append++
			expected[key] = value
		case name == "account_quota_states":
			stats.Update++
			expected[key] = value
		default:
			stats.Conflicts++
		}
	}
	for key := range target {
		if _, ok := source[key]; !ok {
			stats.TargetOnly++
		}
	}
	return stats, expected
}

func usageIndexesEqual(a, b usageMergeIndex) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if got, ok := b[key]; !ok || got != value {
			return false
		}
	}
	return true
}

func usageApplyBatch(ctx context.Context, tx *sql.Tx, name string, table businessTable, page []rawRow) error {
	cols := []string{}
	updates := []string{}
	for _, c := range table.Columns {
		if !c.Generated {
			cols = append(cols, c.Name)
			if c.Name != "account_uuid" {
				updates = append(updates, quoteBusiness(c.Name)+"=EXCLUDED."+quoteBusiness(c.Name))
			}
		}
	}
	data, err := json.Marshal(page)
	if err != nil {
		return errors.New("cannot encode accounting batch")
	}
	list := businessColumnList(cols)
	suffix := " ON CONFLICT (" + businessColumnList(table.PrimaryKey) + ") DO NOTHING"
	if name == "account_quota_states" {
		suffix = " ON CONFLICT (account_uuid) DO UPDATE SET " + strings.Join(updates, ",") + " WHERE target IS DISTINCT FROM EXCLUDED"
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO public."+quoteBusiness(name)+" AS target ("+list+") SELECT "+list+" FROM jsonb_populate_recordset(NULL::public."+quoteBusiness(name)+",$1::jsonb)"+suffix, string(data))
	if err != nil {
		return fmt.Errorf("public.%s merge rejected by database constraints; transaction rolled back", name)
	}
	return nil
}

// Catalog definition fingerprint includes columns, defaults, constraints,
// triggers, indexes, policies, privileges, sequences and public functions.
func usageSchemaDigest(ctx context.Context, tx *sql.Tx) (string, error) {
	queries := []string{
		`SELECT to_jsonb(c)::text FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' ORDER BY c.oid`,
		`SELECT to_jsonb(a)::text FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' ORDER BY a.attrelid,a.attnum`,
		`SELECT to_jsonb(d)::text FROM pg_attrdef d JOIN pg_class c ON c.oid=d.adrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' ORDER BY d.oid`,
		`SELECT to_jsonb(c)::text FROM pg_constraint c WHERE c.connamespace='public'::regnamespace ORDER BY c.oid`,
		`SELECT to_jsonb(t)::text FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid WHERE c.relnamespace='public'::regnamespace ORDER BY t.oid`,
		`SELECT to_jsonb(i)::text FROM pg_index i JOIN pg_class c ON c.oid=i.indrelid WHERE c.relnamespace='public'::regnamespace ORDER BY i.indexrelid`,
		`SELECT to_jsonb(p)::text FROM pg_policy p JOIN pg_class c ON c.oid=p.polrelid WHERE c.relnamespace='public'::regnamespace ORDER BY p.oid`,
		`SELECT to_jsonb(p)::text FROM pg_proc p WHERE p.pronamespace='public'::regnamespace ORDER BY p.oid`,
		`SELECT to_jsonb(s)::text FROM pg_sequence s JOIN pg_class c ON c.oid=s.seqrelid WHERE c.relnamespace='public'::regnamespace ORDER BY s.seqrelid`,
		`SELECT to_jsonb(s)::text FROM public.schema_migrations s ORDER BY version`,
		`SELECT to_jsonb(s)::text FROM public.accounts_controlled_migration_history s ORDER BY to_version`,
	}
	h := sha256.New()
	for _, query := range queries {
		rows, e := tx.QueryContext(ctx, query)
		if e != nil {
			return "", errors.New("cannot fingerprint complete target schema")
		}
		for rows.Next() {
			var value string
			if rows.Scan(&value) != nil {
				rows.Close()
				return "", errors.New("invalid schema fingerprint row")
			}
			fmt.Fprintf(h, "%d:%s", len(value), value)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return "", errors.New("incomplete schema fingerprint")
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
