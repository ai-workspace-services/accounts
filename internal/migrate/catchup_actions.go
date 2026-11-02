package migrate

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// selectReviewedCatchupActions reads target state only after verifying that the
// caller holds every writer-excluding lock. No business data is modified here.
func selectReviewedCatchupActions(ctx context.Context, tx *sql.Tx, prepared preparedCatchup, manifest MigrationManifest, reviewed CatchupPreview) (map[string]map[[32]byte]CatchupAction, error) {
	for _, name := range prepared.order {
		var locked bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid=pg_backend_pid() AND relation=to_regclass($1) AND mode='ShareRowExclusiveLock' AND granted)`, "public."+name).Scan(&locked)
		if err != nil || !locked {
			return nil, errors.New("catchup writer lock missing")
		}
	}
	current, err := compareCatchupTargetTx(ctx, tx, prepared.old, manifest, prepared.base, prepared.source, prepared.tables, prepared.order, reviewed.BaselinePlanID, reviewed.BaselinePlanSHA256)
	if err != nil {
		return nil, err
	}
	if err = verifyCatchupRevalidation(reviewed, current, time.Now()); err != nil {
		return nil, err
	}
	result := make(map[string]map[[32]byte]CatchupAction, len(prepared.order))
	for _, name := range prepared.order {
		target, err := catchupTargetLeaves(ctx, tx, name, prepared.tables[name])
		if err != nil {
			return nil, err
		}
		summary, actions, err := classifyCatchupTable(name, prepared.base[name], prepared.source[name].Leaves, target, true)
		if err != nil {
			return nil, err
		}
		if summary != reviewed.Tables[name] || summary.Conflicts != 0 {
			return nil, errors.New("catchup action selection differs")
		}
		result[name] = actions
	}
	return result, nil
}
