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
	if got.RoundIntroduced == nil || *got.RoundIntroduced != 4 {
		t.Errorf("RoundIntroduced: got %v, want 4", got.RoundIntroduced)
	}
	for _, f := range []struct{ name, got, want string }{
		{"Location", got.Location, "new.go:5"},
		{"Claim", got.Claim, "new-claim"},
		{"ExpectedViolation", got.ExpectedViolation, "new-exp"},
		{"Reproduction", got.Reproduction, "new-repro"},
		{"IntroducedIn", got.IntroducedIn, "new-intro"},
		{"LastTouchedIn", got.LastTouchedIn, "new-last"},
		{"BodyPath", got.BodyPath, "new/path"},
		{"Status", string(got.Status), string(StatusConceded)},
	} {
		if f.got != f.want {
			t.Errorf("%s: got %q, want %q", f.name, f.got, f.want)
		}
	}
	if got.RoundLastTouched != 4 {
		t.Errorf("RoundLastTouched: got %d, want 4", got.RoundLastTouched)
	}
	if got.RoundsSurvived != 3 {
		t.Errorf("RoundsSurvived: got %d, want 3", got.RoundsSurvived)
	}
	if !got.ReAttacked {
		t.Error("ReAttacked: got false, want true")
	}
	if len(got.ConcessionFiles) != 1 || got.ConcessionFiles[0] != "b.go" {
		t.Errorf("ConcessionFiles: got %v, want [b.go]", got.ConcessionFiles)
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
