package migrate

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestCatchupBaselineEncryptedAndRestartable(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{7}, 32)
	s, err := newMigrationStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	page := catchupBaselinePage{PlanID: "11111111-1111-4111-8111-111111111111", Table: "users", Leaves: []catchupBaselineLeaf{{KeySHA256: strings.Repeat("a", 64), RowSHA256: strings.Repeat("b", 64)}}}
	ref, err := s.writeCatchupBaseline("subject", page)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.writeCatchupBaseline("subject", page)
	if err != nil || again != ref {
		t.Fatalf("retry failed %v", err)
	}
	encrypted, err := os.ReadFile(s.path("catchup-baseline", ref.ID))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte(page.Leaves[0].RowSHA256)) {
		t.Fatal("plaintext digest persisted")
	}
	restarted, err := newMigrationStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := restarted.readCatchupBaseline("subject", ref)
	if err != nil || len(got.Leaves) != 1 || got.Leaves[0] != page.Leaves[0] {
		t.Fatalf("restart read failed: %v", err)
	}
	if _, err = restarted.readCatchupBaseline("another-subject", ref); err == nil {
		t.Fatal("wrong subject accepted")
	}
	altered := ref
	altered.Ordinal++
	if _, err = restarted.readCatchupBaseline("subject", altered); err == nil {
		t.Fatal("altered reference accepted")
	}
	encrypted[len(encrypted)-1] ^= 1
	if err = os.WriteFile(s.path("catchup-baseline", ref.ID), encrypted, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.readCatchupBaseline("subject", ref); err == nil {
		t.Fatal("tampered page accepted")
	}
}
func TestCatchupBaselineRejectsUnorderedLeaves(t *testing.T) {
	p := catchupBaselinePage{PlanID: "11111111-1111-4111-8111-111111111111", Table: "users", Leaves: []catchupBaselineLeaf{{KeySHA256: strings.Repeat("b", 64), RowSHA256: strings.Repeat("c", 64)}, {KeySHA256: strings.Repeat("a", 64), RowSHA256: strings.Repeat("d", 64)}}}
	if _, err := catchupBaselineRefFor(p); err == nil {
		t.Fatal("unsorted page accepted")
	}
}
