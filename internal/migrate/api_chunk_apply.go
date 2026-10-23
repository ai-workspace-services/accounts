package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type migrationChunkReader func(MigrationChunk) (migrationChunkPage, error)
type migrationChunkWork struct {
	tables                      map[string]businessTable
	order                       []string
	source                      map[string]string
	union                       map[string]string
	baseline                    map[string]BusinessEquality
	expected                    map[string]BusinessEquality
	inserts, matches, conflicts int
}

func migrationTempName(prefix string, index int) string {
	return `pg_temp.` + quoteBusiness(fmt.Sprintf("migration_%s_%d", prefix, index))
}
func migrationPKJoin(table businessTable, left, right string) string {
	terms := make([]string, len(table.PrimaryKey))
	for i, col := range table.PrimaryKey {
		terms[i] = left + "." + quoteBusiness(col) + "=" + right + "." + quoteBusiness(col)
	}
	return strings.Join(terms, " AND ")
}
func migrationWritableColumns(table businessTable) []string {
	var result []string
	for _, col := range table.Columns {
		if !col.Generated {
			result = append(result, col.Name)
		}
	}
	return result
}

func migrationQueryDigest(ctx context.Context, tx *sql.Tx, query string, table businessTable) (BusinessEquality, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return BusinessEquality{}, errors.New("streaming fingerprint unavailable")
	}
	defer rows.Close()
	digest := businessDigest{}
	for rows.Next() {
		var body string
		if rows.Scan(&body) != nil || len(body) > 1<<20 {
			return BusinessEquality{}, errors.New("fingerprint row exceeds reviewed limit")
		}
		var row rawRow
		if json.Unmarshal([]byte(body), &row) != nil {
			return BusinessEquality{}, errors.New("fingerprint row invalid")
		}
		if err = digest.add(row, table); err != nil {
			return BusinessEquality{}, err
		}
	}
	if rows.Err() != nil {
		return BusinessEquality{}, errors.New("fingerprint stream incomplete")
	}
	return digest.finish()
}

func migrationStreamingBaseline(ctx context.Context, tx *sql.Tx, tables map[string]businessTable, order []string) (map[string]BusinessEquality, error) {
	// Preserve the existing normalized email/proxy identity rules without an
	// in-memory user map. Complete visibility is checked for every table below.
	var invalid bool
	if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.users WHERE email IS NULL OR btrim(email)='' OR uuid='00000000-0000-0000-0000-000000000000' OR proxy_uuid='00000000-0000-0000-0000-000000000000') OR EXISTS(SELECT 1 FROM public.users GROUP BY lower(btrim(email)) HAVING count(*)>1) OR EXISTS(SELECT 1 FROM public.users GROUP BY proxy_uuid HAVING count(*)>1)`).Scan(&invalid) != nil || invalid {
		return nil, errors.New("target identity uniqueness invalid")
	}
	result := map[string]BusinessEquality{}
	for _, name := range order {
		if err := validateBusinessTarget(ctx, tx, name, tables[name]); err != nil {
			return nil, err
		}
		digest, err := migrationQueryDigest(ctx, tx, `SELECT to_jsonb(t)::text FROM public.`+quoteBusiness(name)+` t`, tables[name])
		if err != nil {
			return nil, err
		}
		result[name] = digest
	}
	return result, nil
}

// Only temporary tables receive preview writes. PostgreSQL owns their spill to
// disk, constraints and exact native types; no source/target row map is built.
func migrationPrepareChunkWork(ctx context.Context, tx *sql.Tx, manifest MigrationManifest, read migrationChunkReader) (migrationChunkWork, error) {
	work := migrationChunkWork{source: map[string]string{}, union: map[string]string{}, expected: map[string]BusinessEquality{}}
	if err := validateMigrationChunkManifest(manifest); err != nil {
		return work, err
	}
	var err error
	work.tables, work.order, err = migrationCatalog()
	if err != nil {
		return work, err
	}
	work.baseline, err = migrationStreamingBaseline(ctx, tx, work.tables, work.order)
	if err != nil {
		return work, err
	}
	for i, name := range work.order {
		work.source[name] = migrationTempName("source", i)
		work.union[name] = migrationTempName("union", i)
		if _, err = tx.ExecContext(ctx, `CREATE TEMP TABLE `+quoteBusiness(fmt.Sprintf("migration_source_%d", i))+` (LIKE public.`+quoteBusiness(name)+` INCLUDING GENERATED INCLUDING CONSTRAINTS INCLUDING INDEXES) ON COMMIT DROP`); err != nil {
			return work, errors.New("temporary source staging unavailable")
		}
	}
	for _, chunk := range manifest.Chunks {
		if err = ctx.Err(); err != nil {
			return work, err
		}
		page, e := read(chunk)
		if e != nil {
			return work, e
		}
		if e = validateMigrationChunk(manifest.SnapshotID, chunk, page); e != nil {
			return work, e
		}
		data, e := json.Marshal(page.Rows)
		if e != nil {
			return work, e
		}
		list := businessColumnList(migrationWritableColumns(work.tables[chunk.Table]))
		if _, err = tx.ExecContext(ctx, `INSERT INTO `+work.source[chunk.Table]+` (`+list+`) SELECT `+list+` FROM jsonb_populate_recordset(NULL::public.`+quoteBusiness(chunk.Table)+`,$1::jsonb)`, string(data)); err != nil {
			return work, errors.New("source chunk violates native constraints")
		}
	}
	// SQL readback proves generated values/type conversion and complete page
	// content, not merely received checksums. Every reviewed FK stays in scope.
	for _, name := range work.order {
		table := work.tables[name]
		digest, e := migrationQueryDigest(ctx, tx, `SELECT to_jsonb(t)::text FROM `+work.source[name]+` t`, table)
		if e != nil || digest != manifest.Tables[name] {
			return work, errors.New("staged source table fingerprint differs")
		}
		for _, fk := range table.ForeignKeys {
			var joins, nonnull []string
			for i, col := range fk.Columns {
				joins = append(joins, "s."+quoteBusiness(col)+"=p."+quoteBusiness(fk.ParentColumns[i]))
				nonnull = append(nonnull, "s."+quoteBusiness(col)+" IS NOT NULL")
			}
			var orphan bool
			if tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+work.source[name]+` s WHERE `+strings.Join(nonnull, " AND ")+` AND NOT EXISTS(SELECT 1 FROM `+work.source[fk.Parent]+` p WHERE `+strings.Join(joins, " AND ")+`))`).Scan(&orphan) != nil || orphan {
				return work, errors.New("staged source FK dependency missing")
			}
		}
	}
	for i, name := range work.order {
		table := work.tables[name]
		source := work.source[name]
		target := `public.` + quoteBusiness(name)
		join := migrationPKJoin(table, "s", "t")
		var inserts, matches, conflicts int
		if tx.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE t.`+quoteBusiness(table.PrimaryKey[0])+` IS NULL),count(*) FILTER(WHERE t.`+quoteBusiness(table.PrimaryKey[0])+` IS NOT NULL AND to_jsonb(s)=to_jsonb(t)),count(*) FILTER(WHERE t.`+quoteBusiness(table.PrimaryKey[0])+` IS NOT NULL AND to_jsonb(s) IS DISTINCT FROM to_jsonb(t)) FROM `+source+` s LEFT JOIN `+target+` t ON `+join).Scan(&inserts, &matches, &conflicts) != nil {
			return work, errors.New("streaming conflict comparison unavailable")
		}
		work.inserts += inserts
		work.matches += matches
		work.conflicts += conflicts
		if conflicts != 0 {
			continue
		}
		if _, err = tx.ExecContext(ctx, `CREATE TEMP TABLE `+quoteBusiness(fmt.Sprintf("migration_union_%d", i))+` (LIKE `+target+` INCLUDING GENERATED INCLUDING CONSTRAINTS INCLUDING INDEXES) ON COMMIT DROP`); err != nil {
			return work, errors.New("temporary union staging unavailable")
		}
		list := businessColumnList(migrationWritableColumns(table))
		if _, err = tx.ExecContext(ctx, `INSERT INTO `+work.union[name]+` (`+list+`) SELECT `+list+` FROM `+target); err != nil {
			return work, errors.New("target union constraint check failed")
		}
		// Same-key complete matches are skipped; secondary/expression/partial unique
		// collisions reject the union transaction, without touching business rows.
		if _, err = tx.ExecContext(ctx, `INSERT INTO `+work.union[name]+` (`+list+`) SELECT `+list+` FROM `+source+` s WHERE NOT EXISTS(SELECT 1 FROM `+target+` t WHERE `+join+`)`); err != nil {
			return work, errors.New("source and target unique facts conflict")
		}
		work.expected[name], err = migrationQueryDigest(ctx, tx, `SELECT to_jsonb(t)::text FROM `+work.union[name]+` t`, table)
		if err != nil {
			return work, err
		}
	}
	return work, nil
}

func PreviewMigrationChunks(ctx context.Context, db *sql.DB, manifest MigrationManifest, id string, lifetime time.Duration, read migrationChunkReader) (MigrationPlan, error) {
	var plan MigrationPlan
	tables, order, err := migrationCatalog()
	if err != nil {
		return plan, err
	}
	_ = tables
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
	if identity == manifest.SourceIdentitySHA256 {
		return plan, errors.New("source and target are the same physical database")
	}
	schema, err := migrationSchema(ctx, tx, order)
	if err != nil {
		return plan, err
	}
	work, err := migrationPrepareChunkWork(ctx, tx, manifest, read)
	if err != nil {
		return plan, err
	}
	now := time.Now().UTC()
	expiry := now.Add(lifetime)
	if manifest.ExpiresAt.Before(expiry) {
		expiry = manifest.ExpiresAt
	}
	plan = MigrationPlan{ToolSHA256: migrationExecutableSHA256(), PlanID: id, State: "preview", Snapshot: manifest, TargetIdentitySHA256: identity, TargetSchemaSHA256: schema, TargetBaselineSHA256: migrationDigest(work.baseline), InsertRows: work.inserts, MatchingRows: work.matches, ConflictRows: work.conflicts, CreatedAt: now, ExpiresAt: expiry}
	if work.conflicts > 0 {
		plan.State = "conflict"
	}
	plan.PlanSHA256 = migrationPlanHash(plan)
	return plan, nil
}

func ApplyMigrationChunks(ctx context.Context, db *sql.DB, plan MigrationPlan, hash, subject string, read migrationChunkReader, gate MigrationBackupGate) (MigrationPlan, error) {
	if hash == "" || hash != plan.PlanSHA256 || hash != migrationPlanHash(plan) {
		return MigrationPlan{}, errors.New("reviewed chunk plan hash mismatch")
	}
	if done, ok, err := ReadMigrationReceipt(ctx, db, plan.PlanID, hash, subject); err != nil || ok {
		return done, err
	}
	if plan.State != "preview" || plan.ConflictRows != 0 {
		return MigrationPlan{}, errors.New("chunk plan not applicable")
	}
	_, order, err := migrationCatalog()
	if err != nil {
		return MigrationPlan{}, err
	}
	tx, err := migrationBegin(ctx, db, false)
	if err != nil {
		return MigrationPlan{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(674302318055)`); err != nil {
		return MigrationPlan{}, errors.New("chunk apply lock unavailable")
	}
	if err = migrationLock(ctx, tx, order); err != nil {
		return MigrationPlan{}, err
	}
	var storedHash, storedSubject string
	var body []byte
	err = tx.QueryRowContext(ctx, `SELECT plan_sha256,subject,receipt FROM accounts_migration_api.receipts WHERE plan_id=$1`, plan.PlanID).Scan(&storedHash, &storedSubject, &body)
	if err == nil {
		var done MigrationPlan
		identity, e := migrationIdentity(ctx, tx)
		if e != nil || storedHash != hash || storedSubject != subject || json.Unmarshal(body, &done) != nil || done.TargetIdentitySHA256 != identity || identity != plan.TargetIdentitySHA256 {
			return MigrationPlan{}, errors.New("committed chunk receipt binding mismatch")
		}
		preview := done
		preview.State = "preview"
		if done.State != "applied" || done.PlanID != plan.PlanID || done.PlanSHA256 != hash || migrationPlanHash(preview) != hash {
			return MigrationPlan{}, errors.New("committed chunk receipt content mismatch")
		}
		return done, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return MigrationPlan{}, errors.New("chunk receipt unavailable")
	}
	check := func() error {
		if !time.Now().Before(plan.ExpiresAt) {
			return errors.New("chunk plan expired")
		}
		if gate == nil {
			return errors.New("verified backup gate required")
		}
		return gate(ctx, plan)
	}
	if err = check(); err != nil {
		return MigrationPlan{}, err
	}
	if plan.ToolSHA256 != migrationExecutableSHA256() {
		return MigrationPlan{}, errors.New("chunk executor changed")
	}
	identity, err := migrationIdentity(ctx, tx)
	if err != nil || identity != plan.TargetIdentitySHA256 || identity == plan.Snapshot.SourceIdentitySHA256 {
		return MigrationPlan{}, errors.New("chunk target identity changed")
	}
	schema, err := migrationSchema(ctx, tx, order)
	if err != nil || schema != plan.TargetSchemaSHA256 {
		return MigrationPlan{}, errors.New("chunk target schema changed")
	}
	work, err := migrationPrepareChunkWork(ctx, tx, plan.Snapshot, read)
	if err != nil {
		return MigrationPlan{}, err
	}
	if migrationDigest(work.baseline) != plan.TargetBaselineSHA256 || work.inserts != plan.InsertRows || work.matches != plan.MatchingRows || work.conflicts != 0 {
		return MigrationPlan{}, errors.New("chunk target baseline or plan changed")
	}
	for _, name := range order {
		table := work.tables[name]
		list := businessColumnList(migrationWritableColumns(table))
		target := `public.` + quoteBusiness(name)
		join := migrationPKJoin(table, "s", "t")
		if _, err = tx.ExecContext(ctx, `INSERT INTO `+target+` (`+list+`) OVERRIDING SYSTEM VALUE SELECT `+list+` FROM `+work.source[name]+` s WHERE NOT EXISTS(SELECT 1 FROM `+target+` t WHERE `+join+`)`); err != nil {
			return MigrationPlan{}, errors.New("chunk business insert rejected; transaction rolled back")
		}
	}
	actual, err := migrationStreamingBaseline(ctx, tx, work.tables, order)
	if err != nil || migrationDigest(actual) != migrationDigest(work.expected) {
		return MigrationPlan{}, errors.New("chunk post-apply union fingerprint differs")
	}
	// Native finance event IDs are the sole selected identity sequence. Its
	// transactional RESTART preserves rollback safety and avoids copied-ID reuse.
	var sequence sql.NullString
	var next, current int64
	if tx.QueryRowContext(ctx, `SELECT pg_get_serial_sequence('public.finance_operation_events','id'),COALESCE(max(id),0)+1 FROM public.finance_operation_events`).Scan(&sequence, &next) != nil {
		return MigrationPlan{}, errors.New("chunk finance sequence unavailable")
	}
	if sequence.Valid && plan.InsertRows > 0 {
		if tx.QueryRowContext(ctx, `SELECT last_value FROM `+sequence.String).Scan(&current) != nil {
			return MigrationPlan{}, errors.New("chunk finance sequence unreadable")
		}
		if next <= current {
			next = current + 1
		}
		if _, err = tx.ExecContext(ctx, `ALTER SEQUENCE `+sequence.String+` RESTART WITH `+fmt.Sprint(next)); err != nil {
			return MigrationPlan{}, errors.New("chunk finance sequence update failed")
		}
	}
	finalSchema, err := migrationSchema(ctx, tx, order)
	if err != nil || finalSchema != schema {
		return MigrationPlan{}, errors.New("chunk schema changed during apply")
	}
	if err = check(); err != nil {
		return MigrationPlan{}, err
	}
	plan.State = "applied"
	body, _ = json.Marshal(plan)
	if _, err = tx.ExecContext(ctx, `INSERT INTO accounts_migration_api.receipts(plan_id,plan_sha256,subject,receipt) VALUES($1,$2,$3,$4::jsonb)`, plan.PlanID, hash, subject, string(body)); err != nil {
		return MigrationPlan{}, errors.New("chunk receipt insert failed")
	}
	if err = tx.Commit(); err != nil {
		return MigrationPlan{}, errors.New("chunk commit uncertain; retry exact plan")
	}
	return plan, nil
}

func (s *migrationServer) chunkReader(subject string) migrationChunkReader {
	return func(chunk MigrationChunk) (migrationChunkPage, error) {
		var page migrationChunkPage
		err := s.store.read("target-chunk", chunk.ID, subject, &page)
		return page, err
	}
}
