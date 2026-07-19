package summary

import (
	"testing"

	"latere.ai/x/topos/adversarial/internal/ledger"
)

// TestSortByContentionTieBreaks drives the secondary and tertiary sort
// keys. All three records score 3, so the Score comparison ties and the
// ordering falls through to RoundIntroduced ASC, then AttackID ASC.
func TestSortByContentionTieBreaks(t *testing.T) {
	in := []ledger.Record{
		{AttackID: "c1-b", RoundIntroduced: ptr(2), RoundLastTouched: 5}, // score 3, intro 2
		{AttackID: "c1-a", RoundIntroduced: ptr(2), RoundLastTouched: 5}, // score 3, intro 2
		{AttackID: "c1-z", RoundIntroduced: ptr(1), RoundLastTouched: 4}, // score 3, intro 1
	}
	got := SortByContention(in)
	want := []string{"c1-z", "c1-a", "c1-b"}
	for i, w := range want {
		if got[i].AttackID != w {
			t.Errorf("index %d: got %q, want %q (full: %v)", i,
				got[i].AttackID, w, ids(got))
		}
	}
	// Verify all scores really tied, so the tie-break paths were the
	// thing under test rather than the Score comparison.
	for _, r := range got {
		if s := Score(r); s != 3 {
			t.Fatalf("expected all scores == 3, %s scored %d", r.AttackID, s)
		}
	}
}

// TestSortByContentionNilRoundIntroduced covers the nil-guard branches
// on RoundIntroduced: a record without an introduced round is treated
// as round 0 for the tie-break.
func TestSortByContentionNilRoundIntroduced(t *testing.T) {
	in := []ledger.Record{
		{AttackID: "c1-x", RoundIntroduced: ptr(1), RoundsSurvived: 2}, // score 2, intro 1
		{AttackID: "c1-y", RoundsSurvived: 2},                          // score 2, intro 0 (nil)
	}
	got := SortByContention(in)
	if got[0].AttackID != "c1-y" {
		t.Errorf("nil RoundIntroduced (round 0) should sort first: got %v", ids(got))
	}
}

func ids(rs []ledger.Record) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.AttackID
	}
	return out
}
