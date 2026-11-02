package migrate

import "testing"

func TestCatchupSummaryAndImmutableFacts(t *testing.T) {
	leaf := func(key, value byte) businessLeaf { var x businessLeaf; x.Key[0] = key; x.Row[0] = value; return x }
	base := []businessLeaf{leaf(1, 1), leaf(2, 1), leaf(3, 1), leaf(4, 1)}
	source := []businessLeaf{leaf(1, 2), leaf(2, 1), leaf(3, 2), leaf(5, 1)}
	target := []businessLeaf{leaf(1, 1), leaf(2, 2), leaf(3, 3), leaf(4, 1), leaf(6, 1)}
	got, err := compareCatchupTable("users", base, source, target)
	if err != nil {
		t.Fatal(err)
	}
	if got != (CatchupTableSummary{Inserts: 1, SourceUpdates: 1, TargetChanges: 2, SourceDeletions: 1, Conflicts: 1}) {
		t.Fatalf("unexpected %#v", got)
	}
	immutable, err := compareCatchupTable("billing_ledger", base, source, target)
	if err != nil {
		t.Fatal(err)
	}
	if immutable.SourceUpdates != 0 || immutable.Conflicts != 2 {
		t.Fatalf("immutable %#v", immutable)
	}
	if _, err = compareCatchupTable("users", []businessLeaf{leaf(1, 1), leaf(1, 2)}, nil, nil); err == nil {
		t.Fatal("duplicate base accepted")
	}
	if _, err = compareCatchupTable("not_a_table", nil, nil, nil); err == nil {
		t.Fatal("unknown table accepted")
	}
}
