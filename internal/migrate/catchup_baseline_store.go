package migrate

import (
	"errors"
	"fmt"
	"github.com/google/uuid"
	"os"
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
