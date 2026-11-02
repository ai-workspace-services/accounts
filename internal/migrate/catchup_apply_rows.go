package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type catchupBackupGate func(context.Context, CatchupPreview) error

// executeCatchupRows is deliberately internal: its caller must hold the
// transaction open, persist a transactional receipt and commit or roll back.
// Network callers reach it only through the authenticated, receipt-bound apply
// path; a target backup gate and revalidation are mandatory for new writes.
func executeCatchupRows(ctx context.Context, tx *sql.Tx, prepared preparedCatchup, manifest MigrationManifest, reviewed CatchupPreview, read migrationChunkReader, gate catchupBackupGate) error {
	if gate == nil {
		return errors.New("catchup backup proof required")
	}
	actions, err := selectReviewedCatchupActions(ctx, tx, prepared, manifest, reviewed)
	if err != nil {
		return err
	}
	if err = gate(ctx, reviewed); err != nil {
		return err
	}
	expected := make(map[string]BusinessEquality, len(prepared.order))
	for _, name := range prepared.order {
		leaves, e := catchupTargetLeaves(ctx, tx, name, prepared.tables[name])
		if e != nil {
			return e
		}
		union := make(map[[32]byte][32]byte, len(leaves))
		for _, leaf := range leaves {
			if _, ok := union[leaf.Key]; ok {
				return errors.New("duplicate target key")
			}
			union[leaf.Key] = leaf.Row
		}
		for _, leaf := range prepared.source[name].Leaves {
			if action, ok := actions[name][leaf.Key]; ok {
				_, exists := union[leaf.Key]
				if (action == CatchupInsert && exists) || (action == CatchupUpdate && !exists) {
					return errors.New("catchup key state differs")
				}
				union[leaf.Key] = leaf.Row
			}
		}
		digest := businessDigest{Leaves: make([]businessLeaf, 0, len(union))}
		for key, row := range union {
			digest.Leaves = append(digest.Leaves, businessLeaf{Key: key, Row: row})
		}
		expected[name], e = digest.finish()
		if e != nil {
			return e
		}
	}
	for _, name := range prepared.order {
		table := prepared.tables[name]
		target := `public.` + quoteBusiness(name)
		cols := migrationWritableColumns(table)
		list := businessColumnList(cols)
		pk := map[string]bool{}
		for _, c := range table.PrimaryKey {
			pk[c] = true
		}
		var assignments []string
		for _, c := range cols {
			if !pk[c] {
				assignments = append(assignments, quoteBusiness(c)+`=s.`+quoteBusiness(c))
			}
		}
		for _, chunk := range manifest.Chunks {
			if chunk.Table != name {
				continue
			}
			if err = ctx.Err(); err != nil {
				return err
			}
			page, e := read(chunk)
			if e != nil {
				return e
			}
			if e = validateMigrationChunk(manifest.SnapshotID, chunk, page); e != nil {
				return e
			}
			var inserts, updates []rawRow
			for _, row := range page.Rows {
				d := businessDigest{}
				if e = d.add(row, table); e != nil {
					return e
				}
				key := d.Leaves[0].Key
				action, ok := actions[name][key]
				if !ok {
					continue
				}
				delete(actions[name], key)
				switch action {
				case CatchupInsert:
					inserts = append(inserts, row)
				case CatchupUpdate:
					if catchupImmutableFacts[name] || len(assignments) == 0 {
						return errors.New("catchup update forbidden")
					}
					updates = append(updates, row)
				default:
					return errors.New("unsupported catchup action")
				}
			}
			for _, batch := range []struct {
				rows   []rawRow
				update bool
			}{{inserts, false}, {updates, true}} {
				if len(batch.rows) == 0 {
					continue
				}
				data, e := json.Marshal(batch.rows)
				if e != nil {
					return e
				}
				var query string
				if batch.update {
					query = `UPDATE ` + target + ` AS t SET ` + strings.Join(assignments, ",") + ` FROM jsonb_populate_recordset(NULL::` + target + `,$1::jsonb) AS s WHERE ` + migrationPKJoin(table, "s", "t")
				} else {
					query = `INSERT INTO ` + target + ` (` + list + `) OVERRIDING SYSTEM VALUE SELECT ` + list + ` FROM jsonb_populate_recordset(NULL::` + target + `,$1::jsonb) AS s`
					if name == "referral_reward_campaign_snapshots" {
						// The reward insert trigger can already create this row. Never
						// overwrite it; below we require it to match the source exactly.
						query += ` ON CONFLICT (reward_id) DO NOTHING`
					}
				}
				result, e := tx.ExecContext(ctx, query, string(data))
				if e != nil {
					return errors.New("catchup business mutation rejected")
				}
				n, e := result.RowsAffected()
				if e == nil && !batch.update && name == "referral_reward_campaign_snapshots" {
					e = tx.QueryRowContext(ctx, `SELECT count(*) FROM jsonb_populate_recordset(NULL::public.referral_reward_campaign_snapshots,$1::jsonb) AS s JOIN public.referral_reward_campaign_snapshots AS t ON t.reward_id=s.reward_id AND to_jsonb(t)=to_jsonb(s)`, string(data)).Scan(&n)
				}
				if e != nil || n != int64(len(batch.rows)) {
					return errors.New("catchup mutation count differs")
				}
			}
		}
		if len(actions[name]) != 0 {
			return errors.New("catchup source action missing")
		}
	}
	// Explicitly inserted identity/serial keys must not be reused by normal
	// application writes. ALTER SEQUENCE RESTART is transactional in PostgreSQL.
	for _, name := range prepared.order {
		if reviewed.Tables[name].Inserts == 0 {
			continue
		}
		for _, pk := range prepared.tables[name].PrimaryKey {
			var sequence sql.NullString
			if err = tx.QueryRowContext(ctx, `SELECT pg_get_serial_sequence($1,$2)`, "public."+quoteBusiness(name), pk).Scan(&sequence); err != nil {
				return errors.New("catchup sequence discovery failed")
			}
			if !sequence.Valid {
				continue
			}
			var quoted string
			if err = tx.QueryRowContext(ctx, `SELECT format('%I.%I', n.nspname,c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.oid=$1::regclass AND c.relkind='S'`, sequence.String).Scan(&quoted); err != nil {
				return errors.New("catchup sequence unavailable")
			}
			var next, current int64
			if err = tx.QueryRowContext(ctx, `SELECT COALESCE(max(`+quoteBusiness(pk)+`),0)+1 FROM public.`+quoteBusiness(name)).Scan(&next); err != nil {
				return errors.New("catchup sequence bound unavailable")
			}
			if err = tx.QueryRowContext(ctx, `SELECT last_value FROM `+quoted).Scan(&current); err != nil {
				return errors.New("catchup sequence unreadable")
			}
			if next <= current {
				next = current + 1
			}
			if _, err = tx.ExecContext(ctx, `ALTER SEQUENCE `+quoted+` RESTART WITH `+fmt.Sprint(next)); err != nil {
				return errors.New("catchup sequence update failed")
			}
		}
	}
	for _, name := range prepared.order {
		got, e := migrationQueryDigest(ctx, tx, `SELECT to_jsonb(t)::text FROM public.`+quoteBusiness(name)+` t`, prepared.tables[name])
		if e != nil || got != expected[name] {
			return errors.New("catchup post-write fingerprint differs")
		}
	}
	return nil
}
