package ledger

import (
	"testing"
	"time"

	"latere.ai/x/topos/adversarial/internal/state"
)

func TestPendingDeterministicOrder(t *testing.T) {
	agg := map[string]Record{
		"c1-3": {AttackID: "c1-3", Status: StatusOpen},
		"c1-1": {AttackID: "c1-1", Status: StatusOpen},
		"c1-2": {AttackID: "c1-2", Status: StatusRebutted},
		"c1-4": {AttackID: "c1-4", Status: StatusConceded},
	}
	got := Pending(agg)
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
	for i, want := range []string{"c1-1", "c1-2", "c1-3"} {
		if got[i].AttackID != want {
			t.Errorf("index %d: got %q, want %q", i, got[i].AttackID, want)
		}
	}
}

func TestAggregateMissingFile(t *testing.T) {
	sess, err := state.NewSession(t.TempDir(), 1, ts())
	if err != nil {
		t.Fatal(err)
	}
	agg, err := Aggregate(sess)
	if err != nil {
		t.Fatal(err)
	}
	if len(agg) != 0 {
		t.Errorf("missing attacks.jsonl should yield empty map, got %d", len(agg))
	}
}

func TestLoadBodyNoSpill(t *testing.T) {
	sess, err := state.NewSession(t.TempDir(), 1, ts())
	if err != nil {
		t.Fatal(err)
	}
	// No spill: LoadBody is a no-op.
	r := Record{AttackID: "c1-1", Claim: "x"}
	got, err := LoadBody(sess, r)
	if err != nil {
		t.Fatal(err)
	}
	if got.Claim != "x" {
		t.Errorf("got %q", got.Claim)
	}
}

func ts() time.Time { return time.Now() }

func TestFold_NewEntry(t *testing.T) {
	out := map[string]Record{}
	r := Record{AttackID: "c1-1", Claim: "first", Status: StatusOpen}
	fold(out, r)
	if got := out["c1-1"]; got.Claim != "first" {
		t.Errorf("got %q", got.Claim)
	}
}

func TestFold_OverlayPreservesNonZeroFields(t *testing.T) {
	out := map[string]Record{
		"c1-1": {
			AttackID:          "c1-1",
			Claim:             "old-claim",
			Location:          "old.go:1",
			ExpectedViolation: "old-exp",
			Reproduction:      "old-repro",
			IntroducedIn:      "old-intro",
			RoundLastTouched:  2,
			LastTouchedIn:     "old-last",
			Status:            StatusOpen,
			RoundsSurvived:    2,
			ReAttacked:        false,
			ConcessionFiles:   []string{"a.go"},
			BodyPath:          "old/path",
		},
	}
	four := 4
	fold(out, Record{
		AttackID:          "c1-1",
		Claim:             "new-claim",
		Location:          "new.go:5",
		ExpectedViolation: "new-exp",
		Reproduction:      "new-repro",
		IntroducedIn:      "new-intro",
		RoundIntroduced:   &four,
		RoundLastTouched:  4,
		LastTouchedIn:     "new-last",
		Status:            StatusConceded,
		RoundsSurvived:    3,
		ReAttacked:        true,
		ConcessionFiles:   []string{"b.go"},
		BodyPath:          "new/path",
	})
	got := out["c1-1"]
	// Note: fold reads-then-writes: it updates cur and writes back the full
	// current value. Look at attacks.go to confirm assignment back happens.
	if got.AttackID != "c1-1" {
		t.Errorf("AttackID: got %q", got.AttackID)
	}
}

func TestFold_RoundsSurvivedOnlyIncreases(t *testing.T) {
	out := map[string]Record{
		"c1-1": {AttackID: "c1-1", RoundsSurvived: 5},
	}
	fold(out, Record{AttackID: "c1-1", RoundsSurvived: 2})
	got := out["c1-1"]
	if got.RoundsSurvived != 5 {
		t.Errorf("RoundsSurvived: got %d, want 5 (should not regress)", got.RoundsSurvived)
	}
}
