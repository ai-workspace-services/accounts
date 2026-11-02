package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

//go:embed migration_sequence_advance.sql
var migrationSequenceInstallation string

// advanceMigrationSequences only touches the two reviewed native sequences.
// Locks are held until the surrounding business transaction commits or rolls
// back. A dedicated non-owner login uses the owner-installed narrow helper.
func advanceMigrationSequences(ctx context.Context, tx *sql.Tx, finance, referral bool) error {
	if !finance && !referral {
		return nil
	}
	var role string
	if err := tx.QueryRowContext(ctx, `SELECT current_user`).Scan(&role); err != nil {
		return errors.New("migration sequence identity unavailable")
	}
	if role == "accounts_migration" {
		_, rest, ok := strings.Cut(migrationSequenceInstallation, "AS $body$")
		body, _, ok2 := strings.Cut(rest, "$body$;")
		if !ok || !ok2 {
			return errors.New("migration sequence helper contract invalid")
		}
		sum := sha256.Sum256([]byte(body))
		var valid bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS (
 SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace JOIN pg_language l ON l.oid=p.prolang
 WHERE n.nspname='accounts_migration_api' AND p.proname='advance_sequences' AND p.pronargs=0
 AND p.prorettype='pg_catalog.void'::regtype AND p.prosecdef AND l.lanname='plpgsql'
 AND p.prokind='f' AND p.provolatile='v' AND p.proparallel='u' AND NOT p.proisstrict AND NOT p.proleakproof
 AND p.proconfig=ARRAY['search_path=pg_catalog'] AND p.proowner=n.nspowner AND p.proowner<>(SELECT oid FROM pg_roles WHERE rolname=current_user)
 AND encode(sha256(convert_to(p.prosrc,'UTF8')),'hex')=$1
 AND has_function_privilege(current_user,p.oid,'EXECUTE')
 AND NOT EXISTS (SELECT 1 FROM aclexplode(COALESCE(p.proacl,acldefault('f',p.proowner))) a WHERE a.privilege_type='EXECUTE' AND a.grantee NOT IN (p.proowner,(SELECT oid FROM pg_roles WHERE rolname=current_user)))
 AND NOT EXISTS (SELECT 1 FROM aclexplode(COALESCE(n.nspacl,acldefault('n',n.nspowner))) a WHERE a.privilege_type='CREATE' AND a.grantee<>n.nspowner)
 AND NOT EXISTS (SELECT 1 FROM (VALUES ('public.finance_operation_events_id_seq'),('public.referral_campaign_revisions_revision_seq')) v(seq) WHERE NOT EXISTS (SELECT 1 FROM pg_class c WHERE c.oid=to_regclass(v.seq) AND c.relkind='S' AND c.relowner=p.proowner))
)`, hex.EncodeToString(sum[:])).Scan(&valid)
		if err != nil || !valid {
			return errors.New("verified restricted migration sequence helper unavailable")
		}
		if _, err = tx.ExecContext(ctx, `SELECT accounts_migration_api.advance_sequences()`); err != nil {
			return errors.New("restricted migration sequence advancement failed")
		}
		return nil
	}
	// Existing owner-operated paths retain support, but lock and validate both
	// sequences before reading their current values; no privilege fallback occurs.
	for _, item := range []struct {
		enabled                 bool
		table, column, sequence string
	}{{finance, "finance_operation_events", "id", "finance_operation_events_id_seq"}, {referral, "referral_campaign_revisions", "revision", "referral_campaign_revisions_revision_seq"}} {
		if !item.enabled {
			continue
		}
		name := "public." + item.sequence
		var valid bool
		err := tx.QueryRowContext(ctx, `SELECT pg_get_serial_sequence($1,$2)=$3 AND EXISTS (SELECT 1 FROM pg_class c JOIN pg_sequence s ON s.seqrelid=c.oid WHERE c.oid=to_regclass($3) AND c.relkind='S' AND s.seqincrement=1 AND s.seqcache=1 AND NOT s.seqcycle)`, "public."+item.table, item.column, name).Scan(&valid)
		if err != nil || !valid {
			return errors.New("reviewed migration sequence differs")
		}
		if _, err = tx.ExecContext(ctx, `ALTER SEQUENCE `+name+` INCREMENT BY 1`); err != nil {
			return errors.New("migration sequence lock unavailable")
		}
		var next int64
		query := `SELECT GREATEST(COALESCE(max(` + quoteBusiness(item.column) + `),0)+1,(SELECT last_value+1 FROM ` + name + `)) FROM public.` + quoteBusiness(item.table)
		if err = tx.QueryRowContext(ctx, query).Scan(&next); err != nil {
			return errors.New("migration sequence bound unavailable")
		}
		if _, err = tx.ExecContext(ctx, `ALTER SEQUENCE `+name+` RESTART WITH `+fmt.Sprint(next)); err != nil {
			return errors.New("migration sequence advancement failed")
		}
	}
	return nil
}
