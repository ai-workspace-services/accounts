package migrate

import (
	"encoding/hex"
	"errors"
)

// CatchupTableSummary contains only counts. No primary keys or business values
// are exposed in a migration report.
type CatchupTableSummary struct {
	Inserts         int `json:"inserts"`
	SourceUpdates   int `json:"source_updates"`
	Matches         int `json:"matches"`
	TargetChanges   int `json:"target_changes"`
	SourceDeletions int `json:"source_deletions"`
	Conflicts       int `json:"conflicts"`
}

// Immutable facts never receive automatic updates. This is intentionally a
// conservative subset; review other tables before any apply path is enabled.
var catchupImmutableFacts = map[string]bool{
	"billing_ledger": true, "finance_invoices": true, "finance_payments": true, "finance_refunds": true, "finance_operation_events": true, "audit_logs": true,
	"referral_reward_events": true, "account_lifecycle_events": true,
}

// compareCatchupTable consumes already-verified canonical fingerprints. It
// detects duplicate keys independently in each stream and never writes rows.
func compareCatchupTable(table string, baseline, source, target []businessLeaf) (CatchupTableSummary, error) {
	var out CatchupTableSummary
	tables, _, err := migrationCatalog()
	if err != nil {
		return out, err
	}
	if _, ok := tables[table]; !ok {
		return out, errors.New("unknown catchup table")
	}
	type states struct{ base, src, dst CatchupRow }
	all := make(map[[32]byte]*states)
	for i, leaves := range [][]businessLeaf{baseline, source, target} {
		seen := make(map[[32]byte]struct{}, len(leaves))
		for _, leaf := range leaves {
			if _, ok := seen[leaf.Key]; ok {
				return CatchupTableSummary{}, errors.New("duplicate catchup primary-key fingerprint")
			}
			seen[leaf.Key] = struct{}{}
			state := all[leaf.Key]
			if state == nil {
				state = &states{}
				all[leaf.Key] = state
			}
			row := CatchupRow{Present: true, SHA256: hex.EncodeToString(leaf.Row[:])}
			switch i {
			case 0:
				state.base = row
			case 1:
				state.src = row
			case 2:
				state.dst = row
			}
		}
	}
	for _, state := range all {
		action, e := ClassifyCatchup(state.base, state.src, state.dst, catchupImmutableFacts[table])
		if e != nil {
			return CatchupTableSummary{}, e
		}
		switch action {
		case CatchupInsert:
			out.Inserts++
		case CatchupUpdate:
			out.SourceUpdates++
		case CatchupMatch:
			out.Matches++
		case CatchupTargetChange:
			out.TargetChanges++
		case CatchupSourceDeleted:
			out.SourceDeletions++
		case CatchupConflict:
			out.Conflicts++
		case CatchupAbsent:
			return CatchupTableSummary{}, errors.New("unexpected absent key")
		}
	}
	return out, nil
}
