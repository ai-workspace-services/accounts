package migrate

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"os"
	"sort"
)

// catchupBaselinePage stores only canonical primary-key and row digests. The
// associated committed receipt must be verified before using any page as a base.
type catchupBaselineLeaf struct {
	KeySHA256 string `json:"key_sha256"`
	RowSHA256 string `json:"row_sha256"`
}
type catchupBaselinePage struct {
	PlanID  string                `json:"plan_id"`
	Table   string                `json:"table"`
	Ordinal int                   `json:"ordinal"`
	Leaves  []catchupBaselineLeaf `json:"leaves"`
}

type catchupBaselineRef struct {
	ID      string `json:"id"`
	PlanID  string `json:"plan_id"`
	Table   string `json:"table"`
	Ordinal int    `json:"ordinal"`
	SHA256  string `json:"sha256"`
}

func catchupBaselineRefFor(page catchupBaselinePage) (catchupBaselineRef, error) {
	if !migrationUUID(page.PlanID) || page.Ordinal < 0 || len(page.Leaves) == 0 || len(page.Leaves) > 1000 {
		return catchupBaselineRef{}, errors.New("invalid catchup baseline page")
	}
	tables, _, err := migrationCatalog()
	if err != nil {
		return catchupBaselineRef{}, err
	}
	if _, ok := tables[page.Table]; !ok {
		return catchupBaselineRef{}, errors.New("unknown catchup baseline table")
	}
	previous := ""
	for _, leaf := range page.Leaves {
		if !migrationSHA.MatchString(leaf.KeySHA256) || !migrationSHA.MatchString(leaf.RowSHA256) || leaf.KeySHA256 <= previous {
			return catchupBaselineRef{}, errors.New("invalid or unordered catchup baseline leaf")
		}
		previous = leaf.KeySHA256
	}
	hash := migrationDigest(page)
	ns, err := uuid.Parse(page.PlanID)
	if err != nil {
		return catchupBaselineRef{}, err
	}
	id := uuid.NewSHA1(ns, []byte(fmt.Sprintf("catchup/%s/%d/%s", page.Table, page.Ordinal, hash))).String()
	return catchupBaselineRef{ID: id, PlanID: page.PlanID, Table: page.Table, Ordinal: page.Ordinal, SHA256: hash}, nil
}

func (s *migrationStore) writeCatchupBaseline(subject string, page catchupBaselinePage) (catchupBaselineRef, error) {
	ref, err := catchupBaselineRefFor(page)
	if err != nil {
		return catchupBaselineRef{}, err
	}
	var existing catchupBaselinePage
	err = s.read("catchup-baseline", ref.ID, subject, &existing)
	if err == nil {
		if actual, e := catchupBaselineRefFor(existing); e != nil || actual != ref {
			return catchupBaselineRef{}, errors.New("baseline content mismatch")
		}
		return ref, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return catchupBaselineRef{}, err
	}
	if err = s.write("catchup-baseline", ref.ID, subject, page); err != nil {
		return catchupBaselineRef{}, err
	}
	return ref, nil
}

func (s *migrationStore) readCatchupBaseline(subject string, ref catchupBaselineRef) (catchupBaselinePage, error) {
	var page catchupBaselinePage
	if err := s.read("catchup-baseline", ref.ID, subject, &page); err != nil {
		return page, err
	}
	actual, err := catchupBaselineRefFor(page)
	if err != nil || actual != ref {
		return catchupBaselinePage{}, errors.New("baseline reference mismatch")
	}
	return page, nil
}

// catchupBaselineIndex becomes visible only after every encrypted page is
// validated. The caller must first verify the database's transactional receipt.
type catchupBaselineIndex struct {
	PlanID               string               `json:"plan_id"`
	PlanSHA256           string               `json:"plan_sha256"`
	TargetIdentitySHA256 string               `json:"target_identity_sha256"`
	SnapshotSHA256       string               `json:"snapshot_sha256"`
	Pages                []catchupBaselineRef `json:"pages"`
}

func (s *migrationServer) captureCommittedBaseline(ctx context.Context, plan MigrationPlan, subject string) error {
	preview := plan
	preview.State = "preview"
	if plan.State != "applied" || !migrationUUID(plan.PlanID) || migrationPlanHash(preview) != plan.PlanSHA256 || migrationSnapshotHash(plan.Snapshot) != plan.Snapshot.SnapshotSHA256 || !migrationSHA.MatchString(plan.TargetIdentitySHA256) {
		return errors.New("committed baseline plan binding invalid")
	}
	tables, order, err := migrationCatalog()
	if err != nil {
		return err
	}
	if len(plan.Snapshot.Tables) != len(order) {
		return errors.New("baseline table coverage differs")
	}
	return s.store.locked(ctx, func() error {
		index := catchupBaselineIndex{PlanID: plan.PlanID, PlanSHA256: plan.PlanSHA256, TargetIdentitySHA256: plan.TargetIdentitySHA256, SnapshotSHA256: plan.Snapshot.SnapshotSHA256}
		digests := map[string]*businessDigest{}
		ordinals := map[string]int{}
		for _, name := range order {
			digests[name] = &businessDigest{}
		}
		for _, chunk := range plan.Snapshot.Chunks {
			if err := ctx.Err(); err != nil {
				return err
			}
			table, ok := tables[chunk.Table]
			if !ok || chunk.Ordinal != ordinals[chunk.Table] {
				return errors.New("baseline chunk order invalid")
			}
			ordinals[chunk.Table]++
			page, err := s.chunkReader(subject)(chunk)
			if err != nil {
				return err
			}
			if err = validateMigrationChunk(plan.Snapshot.SnapshotID, chunk, page); err != nil {
				return err
			}
			pageDigest := businessDigest{}
			for _, row := range page.Rows {
				if err = pageDigest.add(row, table); err != nil {
					return err
				}
				if err = digests[chunk.Table].add(row, table); err != nil {
					return err
				}
			}
			leaves := make([]catchupBaselineLeaf, 0, len(pageDigest.Leaves))
			for _, leaf := range pageDigest.Leaves {
				leaves = append(leaves, catchupBaselineLeaf{KeySHA256: hex.EncodeToString(leaf.Key[:]), RowSHA256: hex.EncodeToString(leaf.Row[:])})
			}
			sort.Slice(leaves, func(i, j int) bool { return leaves[i].KeySHA256 < leaves[j].KeySHA256 })
			ref, err := s.store.writeCatchupBaseline(subject, catchupBaselinePage{PlanID: plan.PlanID, Table: chunk.Table, Ordinal: chunk.Ordinal, Leaves: leaves})
			if err != nil {
				return err
			}
			index.Pages = append(index.Pages, ref)
		}
		for _, name := range order {
			got, err := digests[name].finish()
			if err != nil || got != plan.Snapshot.Tables[name] {
				return errors.New("baseline source fingerprint mismatch")
			}
		}
		var existing catchupBaselineIndex
		err := s.store.read("catchup-index", plan.PlanID, subject, &existing)
		if err == nil {
			if migrationDigest(existing) != migrationDigest(index) {
				return errors.New("baseline index differs")
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return s.store.write("catchup-index", plan.PlanID, subject, index)
	})
}

// loadCommittedBaseline accepts only a database-verified receipt and checks the
// complete source fingerprint again before returning hashes for comparison.
func (s *migrationServer) loadCommittedBaseline(ctx context.Context, planID, planHash, subject string) (map[string][]businessLeaf, error) {
	plan, ok, err := ReadMigrationReceipt(ctx, s.DB, planID, planHash, subject)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("committed baseline receipt required")
	}
	var index catchupBaselineIndex
	if err = s.store.read("catchup-index", planID, subject, &index); err != nil {
		return nil, err
	}
	if index.PlanID != plan.PlanID || index.PlanSHA256 != plan.PlanSHA256 || index.TargetIdentitySHA256 != plan.TargetIdentitySHA256 || index.SnapshotSHA256 != plan.Snapshot.SnapshotSHA256 {
		return nil, errors.New("baseline receipt binding mismatch")
	}
	_, order, err := migrationCatalog()
	if err != nil {
		return nil, err
	}
	out := make(map[string][]businessLeaf, len(order))
	ordinal := map[string]int{}
	for _, name := range order {
		out[name] = nil
	}
	for _, ref := range index.Pages {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if ref.PlanID != planID || ref.Ordinal != ordinal[ref.Table] {
			return nil, errors.New("baseline page order mismatch")
		}
		ordinal[ref.Table]++
		page, e := s.store.readCatchupBaseline(subject, ref)
		if e != nil {
			return nil, e
		}
		if _, ok := out[page.Table]; !ok {
			return nil, errors.New("baseline table unknown")
		}
		for _, item := range page.Leaves {
			key, e := hex.DecodeString(item.KeySHA256)
			if e != nil {
				return nil, e
			}
			value, e := hex.DecodeString(item.RowSHA256)
			if e != nil {
				return nil, e
			}
			var leaf businessLeaf
			copy(leaf.Key[:], key)
			copy(leaf.Row[:], value)
			out[page.Table] = append(out[page.Table], leaf)
		}
	}
	for _, name := range order {
		digest := businessDigest{Leaves: append([]businessLeaf(nil), out[name]...)}
		got, e := digest.finish()
		if e != nil || got != plan.Snapshot.Tables[name] {
			return nil, errors.New("baseline table fingerprint mismatch")
		}
	}
	return out, nil
}
