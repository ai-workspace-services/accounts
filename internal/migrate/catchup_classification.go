package migrate

import "errors"

// CatchupAction classifies one stable business key using the previous source
// snapshot as baseline. It does not execute writes or resolve conflicts.
type CatchupAction string

const (
	CatchupInsert        CatchupAction = "insert"
	CatchupUpdate        CatchupAction = "source_update"
	CatchupMatch         CatchupAction = "match"
	CatchupTargetChange  CatchupAction = "target_change"
	CatchupSourceDeleted CatchupAction = "source_deleted"
	CatchupConflict      CatchupAction = "conflict"
	CatchupAbsent        CatchupAction = "absent"
)

// CatchupRow is an optional SHA-256 of a canonical row. An absent value is
// distinct from a present row with a hash, including when a source was deleted.
type CatchupRow struct {
	Present bool
	SHA256  string
}

// ClassifyCatchup compares snapshots without losing target-only data. A source
// change to an immutable fact is always a conflict unless both sides already
// have identical content. Source deletions are reported but never propagated.
func ClassifyCatchup(baseline, source, target CatchupRow, immutable bool) (CatchupAction, error) {
	for _, row := range []CatchupRow{baseline, source, target} {
		if (row.Present && !migrationSHA.MatchString(row.SHA256)) || (!row.Present && row.SHA256 != "") {
			return "", errors.New("invalid canonical row fingerprint")
		}
	}
	if !source.Present {
		if baseline.Present {
			if target.Present && target.SHA256 != baseline.SHA256 {
				return CatchupConflict, nil
			}
			return CatchupSourceDeleted, nil
		}
		if target.Present {
			return CatchupTargetChange, nil
		}
		return CatchupAbsent, nil
	}
	if !target.Present {
		if baseline.Present {
			return CatchupConflict, nil
		}
		return CatchupInsert, nil
	}
	if source.SHA256 == target.SHA256 {
		return CatchupMatch, nil
	}
	if !baseline.Present {
		return CatchupConflict, nil
	}
	sourceChanged := source.SHA256 != baseline.SHA256
	targetChanged := target.SHA256 != baseline.SHA256
	if sourceChanged && !targetChanged && !immutable {
		return CatchupUpdate, nil
	}
	if !sourceChanged && targetChanged {
		return CatchupTargetChange, nil
	}
	return CatchupConflict, nil
}
