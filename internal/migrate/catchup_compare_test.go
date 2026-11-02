package migrate

import (
	"encoding/binary"
	"testing"
)

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

func BenchmarkCatchupLedger92143(b *testing.B) {
	const n = 92143
	leaves := make([]businessLeaf, n)
	for i := range leaves {
		binary.BigEndian.PutUint64(leaves[i].Key[:8], uint64(i))
		binary.BigEndian.PutUint64(leaves[i].Row[:8], uint64(i+1))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := compareCatchupTable("billing_ledger", leaves, leaves, leaves)
		if err != nil || out.Matches != n {
			b.Fatalf("comparison failed: %v %#v", err, out)
		}
	}
}

func TestCatchupActionSelectionNeverOverwritesConflict(t *testing.T) {
	leaf := func(key, value byte) businessLeaf { var l businessLeaf; l.Key[0] = key; l.Row[0] = value; return l }
	base := []businessLeaf{leaf(1, 1), leaf(2, 1), leaf(3, 1)}
	src := []businessLeaf{leaf(1, 2), leaf(2, 1), leaf(4, 1)}
	dst := []businessLeaf{leaf(1, 1), leaf(2, 2), leaf(3, 1), leaf(5, 1)}
	summary, actions, err := classifyCatchupTable("users", base, src, dst, true)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Inserts != 1 || summary.SourceUpdates != 1 || summary.TargetChanges != 2 || summary.SourceDeletions != 1 || len(actions) != 2 {
		t.Fatalf("selection: %+v %d", summary, len(actions))
	}
	if actions[leaf(1, 1).Key] != CatchupUpdate || actions[leaf(4, 1).Key] != CatchupInsert {
		t.Fatal("missing allowed actions")
	}
	dst[0] = leaf(1, 3)
	summary, actions, err = classifyCatchupTable("users", base, src, dst, true)
	if err != nil || summary.Conflicts != 1 || actions != nil {
		t.Fatal("conflicting selection must be withheld")
	}
	summary, actions, err = classifyCatchupTable("billing_ledger", base, src, []businessLeaf{leaf(1, 1)}, true)
	if err != nil || summary.Conflicts == 0 || actions != nil {
		t.Fatal("immutable update must be withheld")
	}
}
