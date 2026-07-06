package ledger

import (
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/adversarial/internal/state"
)

func freshSession(t *testing.T) *state.Session {
	t.Helper()
	s, err := state.NewSession(t.TempDir(), 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAppendAndAggregate(t *testing.T) {
	s := freshSession(t)
	r1Round := 1
	rec := Record{
		AttackID: "c1-1", CriticIndex: 1, Aspect: "security",
		RoundIntroduced: &r1Round, Location: "x:1", Claim: "c", ExpectedViolation: "ev", Reproduction: "rp",
		Status: StatusOpen, RoundLastTouched: 1,
	}
	if err := Append(s, rec); err != nil {
		t.Fatal(err)
	}
	if err := Append(s, Record{AttackID: "c1-1", CriticIndex: 1, Aspect: "security", Status: StatusRebutted, RoundLastTouched: 2}); err != nil {
		t.Fatal(err)
	}
	if err := Append(s, Record{AttackID: "c1-1", CriticIndex: 1, Aspect: "security", Status: StatusConceded, RoundLastTouched: 4, ConcessionFiles: []string{"x.go"}}); err != nil {
		t.Fatal(err)
	}
	agg, err := Aggregate(s)
	if err != nil {
		t.Fatal(err)
	}
	final := agg["c1-1"]
	if final.Status != StatusConceded {
		t.Errorf("final status: got %s", final.Status)
	}
	if final.Claim != "c" {
		t.Errorf("claim should fold from R1: got %q", final.Claim)
	}
	if final.RoundLastTouched != 4 {
		t.Errorf("round_last_touched: got %d", final.RoundLastTouched)
	}
}

func TestPending(t *testing.T) {
	s := freshSession(t)
	round1 := 1
	for _, st := range []Status{StatusOpen, StatusRebutted, StatusConceded, StatusWithdrawn} {
		_ = Append(s, Record{AttackID: "c1-" + string(st)[:1], CriticIndex: 1, Aspect: "a", Status: st, RoundIntroduced: &round1, Location: "x", Claim: "x", ExpectedViolation: "x", Reproduction: "x", RoundLastTouched: 1})
	}
	agg, _ := Aggregate(s)
	pending := Pending(agg)
	if len(pending) != 2 {
		t.Errorf("pending count: got %d, want 2", len(pending))
	}
}

func TestBodySpill(t *testing.T) {
	s := freshSession(t)
	big := strings.Repeat("x", SpillThreshold+1)
	if err := Append(s, Record{AttackID: "c1-1", CriticIndex: 1, Aspect: "a", Claim: big, ExpectedViolation: "y", Reproduction: "z", Status: StatusOpen, RoundLastTouched: 1}); err != nil {
		t.Fatal(err)
	}
	agg, _ := Aggregate(s)
	r := agg["c1-1"]
	if r.BodyPath == "" {
		t.Error("expected body_path set after spill")
	}
	if r.Claim != "" {
		t.Error("inline claim should be blanked when spilled")
	}
	loaded, err := LoadBody(s, r)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Claim != big {
		t.Errorf("claim round-trip: got len=%d", len(loaded.Claim))
	}
}

// TestBodySpillWithEmbeddedHeaders pins that a spilled body whose Claim
// contains a "## " line round-trips intact. The old markdown side-car
// split on every "## " line, so an embedded header truncated the Claim
// and mis-routed the trailing content.
func TestBodySpillWithEmbeddedHeaders(t *testing.T) {
	s := freshSession(t)
	claim := "## Reproduction\nthis text is really part of the claim\n" + strings.Repeat("x", SpillThreshold)
	if err := Append(s, Record{AttackID: "c1-1", CriticIndex: 1, Aspect: "a", Claim: claim, ExpectedViolation: "y", Reproduction: "z", Status: StatusOpen, RoundLastTouched: 1}); err != nil {
		t.Fatal(err)
	}
	agg, _ := Aggregate(s)
	r := agg["c1-1"]
	if r.BodyPath == "" {
		t.Fatal("expected body_path set after spill")
	}
	loaded, err := LoadBody(s, r)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Claim != claim {
		t.Errorf("claim round-trip corrupted: got len=%d, want len=%d", len(loaded.Claim), len(claim))
	}
	if loaded.ExpectedViolation != "y" || loaded.Reproduction != "z" {
		t.Errorf("fields corrupted: exp=%q repro=%q", loaded.ExpectedViolation, loaded.Reproduction)
	}
}

func TestTruncatedTrailingLineTolerated(t *testing.T) {
	s := freshSession(t)
	round1 := 1
	if err := Append(s, Record{AttackID: "c1-1", CriticIndex: 1, RoundIntroduced: &round1, Status: StatusOpen, RoundLastTouched: 1}); err != nil {
		t.Fatal(err)
	}
	// Append a bare partial line (no newline-terminated JSON object).
	if err := s.AppendLine("attacks.jsonl", []byte(`{"truncated": true`)); err != nil {
		t.Fatal(err)
	}
	agg, err := Aggregate(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := agg["c1-1"]; !ok {
		t.Error("first record should still be present")
	}
}
