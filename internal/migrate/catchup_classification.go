package migrate

import (
	"encoding/hex"
	"errors"
)

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
type catchupFingerprint struct {
	present bool
	digest  [32]byte
}

func classifyCatchupFingerprints(baseline, source, target catchupFingerprint, immutable bool) CatchupAction {
	if !source.present {
		if baseline.present {
			if target.present && target.digest != baseline.digest {
				return CatchupConflict
			}
			return CatchupSourceDeleted
		}
		if target.present {
			return CatchupTargetChange
		}
		return CatchupAbsent
	}
	if !target.present {
		if baseline.present {
			return CatchupConflict
		}
		return CatchupInsert
	}
	if source.digest == target.digest {
		return CatchupMatch
	}
	if !baseline.present {
		return CatchupConflict
	}
	sourceChanged := source.digest != baseline.digest
	targetChanged := target.digest != baseline.digest
	if sourceChanged && !targetChanged && !immutable {
		return CatchupUpdate
	}
	if !sourceChanged && targetChanged {
		return CatchupTargetChange
	}
	return CatchupConflict
}
func ClassifyCatchup(baseline, source, target CatchupRow, immutable bool) (CatchupAction, error) {
	var fingerprints [3]catchupFingerprint
	for i, row := range []CatchupRow{baseline, source, target} {
		if (row.Present && !migrationSHA.MatchString(row.SHA256)) || (!row.Present && row.SHA256 != "") {
			return "", errors.New("invalid canonical row fingerprint")
		}
		if row.Present {
			decoded, err := hex.DecodeString(row.SHA256)
			if err != nil {
				return "", err
			}
			fingerprints[i].present = true
			copy(fingerprints[i].digest[:], decoded)
		}
	}
	return classifyCatchupFingerprints(fingerprints[0], fingerprints[1], fingerprints[2], immutable), nil
}
