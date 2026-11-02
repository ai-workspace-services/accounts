package migrate

import (
	"strings"
	"testing"
)

func TestClassifyCatchup(t *testing.T) {
	row := func(s string) CatchupRow { return CatchupRow{Present: true, SHA256: strings.Repeat(s, 64)} }
	a, b, c := row("a"), row("b"), row("c")
	cases := []struct {
		name                     string
		baseline, source, target CatchupRow
		immutable                bool
		want                     CatchupAction
	}{
		{"new", CatchupRow{}, a, CatchupRow{}, false, CatchupInsert},
		{"replay", CatchupRow{}, a, a, false, CatchupMatch},
		{"target only", CatchupRow{}, CatchupRow{}, a, false, CatchupTargetChange},
		{"new collision", CatchupRow{}, a, b, false, CatchupConflict},
		{"source edit", a, b, a, false, CatchupUpdate},
		{"immutable edit", a, b, a, true, CatchupConflict},
		{"target edit", a, a, b, false, CatchupTargetChange},
		{"both edited differently", a, b, c, false, CatchupConflict},
		{"both converged", a, b, b, false, CatchupMatch},
		{"source deleted", a, CatchupRow{}, a, false, CatchupSourceDeleted},
		{"source deleted target edited", a, CatchupRow{}, b, false, CatchupConflict},
		{"both deleted", a, CatchupRow{}, CatchupRow{}, false, CatchupSourceDeleted},
		{"target deleted", a, a, CatchupRow{}, false, CatchupConflict},
		{"all absent", CatchupRow{}, CatchupRow{}, CatchupRow{}, false, CatchupAbsent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ClassifyCatchup(tc.baseline, tc.source, tc.target, tc.immutable)
			if err != nil || got != tc.want {
				t.Fatalf("got %q err %v want %q", got, err, tc.want)
			}
		})
	}
	if _, err := ClassifyCatchup(CatchupRow{Present: true, SHA256: "bad"}, a, a, false); err == nil {
		t.Fatal("invalid hash accepted")
	}
	if _, err := ClassifyCatchup(CatchupRow{SHA256: strings.Repeat("a", 64)}, a, a, false); err == nil {
		t.Fatal("absent row carried hash")
	}
}
