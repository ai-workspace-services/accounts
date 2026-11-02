package migrate

import (
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
	type states struct{ base, src, dst catchupFingerprint }
	all := make(map[[32]byte]states)
	for i, leaves := range [][]businessLeaf{baseline, source, target} {
		for _, leaf := range leaves {
			state := all[leaf.Key]
			row := catchupFingerprint{present: true, digest: leaf.Row}
			switch i {
			case 0:
				if state.base.present {
					return CatchupTableSummary{}, errors.New("duplicate catchup primary-key fingerprint")
				}
				state.base = row
			case 1:
				if state.src.present {
					return CatchupTableSummary{}, errors.New("duplicate catchup primary-key fingerprint")
				}
				state.src = row
			case 2:
				if state.dst.present {
					return CatchupTableSummary{}, errors.New("duplicate catchup primary-key fingerprint")
				}
				state.dst = row
			}
			all[leaf.Key] = state
		}
	}
	for _, state := range all {
		action := classifyCatchupFingerprints(state.base, state.src, state.dst, catchupImmutableFacts[table])
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
