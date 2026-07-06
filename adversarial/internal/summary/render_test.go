package summary

import (
	"strings"
	"testing"
	"unicode/utf8"

	"latere.ai/x/topos/adversarial/internal/ledger"
	"latere.ai/x/topos/adversarial/internal/round"
)

// TestOneLineRuneBoundary pins that oneLine truncates on a rune
// boundary: byte-slicing multibyte UTF-8 (the repo ships zh content)
// would split a rune and emit invalid UTF-8 into summary.md.
func TestOneLineRuneBoundary(t *testing.T) {
	got := oneLine(strings.Repeat("世", 300))
	if !utf8.ValidString(got) {
		t.Errorf("oneLine produced invalid UTF-8: %q", got)
	}
	if want := strings.Repeat("世", 200) + "..."; got != want {
		t.Errorf("oneLine truncation: got %d runes, want 200 runes + ellipsis", utf8.RuneCountInString(got))
	}
}

func TestDecideClean(t *testing.T) {
	d := Decide(&round.Summary{Termination: round.TermSteadyState, Unresolved: 0})
	if d.Surface || d.ExitCode != 0 {
		t.Errorf("clean: %+v", d)
	}
}

func TestDecideUnresolved(t *testing.T) {
	d := Decide(&round.Summary{Termination: round.TermSteadyState, Unresolved: 2})
	if !d.Surface || d.ExitCode != 1 {
		t.Errorf("unresolved: %+v", d)
	}
}

func TestDecideInterrupted(t *testing.T) {
	d := Decide(&round.Summary{Termination: round.TermInterrupted})
	if !d.Surface || d.ExitCode != 130 {
		t.Errorf("interrupted: %+v", d)
	}
}

func TestRenderHasHeadlineAndStats(t *testing.T) {
	r := &Render{Format: "markdown"}
	agg := map[string]ledger.Record{
		"c1-1": {
			AttackID: "c1-1", Aspect: "security", Location: "x.go:1",
			Claim: "leak", ExpectedViolation: "panic", Reproduction: "go run", Status: ledger.StatusUnresolved,
			RoundIntroduced: ptr(1), RoundLastTouched: 3, ReAttacked: true,
		},
		"c1-2": {
			AttackID: "c1-2", Aspect: "security", Status: ledger.StatusConceded,
			Claim: "off by one", ConcessionFiles: []string{"x.go"},
		},
	}
	b, _ := r.Bytes(&round.Summary{Termination: round.TermSteadyState, Unresolved: 1}, agg)
	s := string(b)
	if !strings.Contains(s, "Headline") {
		t.Error("missing Headline section")
	}
	if !strings.Contains(s, "## Stats") {
		t.Error("missing Stats section")
	}
	if !strings.Contains(s, "critic-found-bug rate: 1/2") {
		t.Errorf("stat line incorrect; body:\n%s", s)
	}
}

func TestWriteResolved_AllStatuses(t *testing.T) {
	r := &Render{Format: "markdown"}
	agg := map[string]ledger.Record{
		"c1-1": {
			AttackID: "c1-1", Aspect: "security",
			Claim: "conceded one", ConcessionFiles: []string{"a.go", "b.go"},
			Status: ledger.StatusConceded,
		},
		"c1-2": {
			AttackID: "c1-2", Aspect: "security",
			Claim:  "rebutted one",
			Status: ledger.StatusRebutted,
		},
		"c1-3": {
			AttackID: "c1-3", Aspect: "security",
			Claim:  "withdrawn one",
			Status: ledger.StatusWithdrawn,
		},
	}
	b, _ := r.Bytes(&round.Summary{Termination: round.TermSteadyState, Unresolved: 0}, agg)
	s := string(b)
	for _, want := range []string{
		"[conceded] conceded one",
		"a.go, b.go",
		"[rebutted] rebutted one",
		"[withdrawn] withdrawn one",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}

func TestOneLine(t *testing.T) {
	if got := oneLine("a\nb\nc"); got != "a b c" {
		t.Errorf("multiline collapse: got %q", got)
	}
	if got := oneLine("  spaces  "); got != "spaces" {
		t.Errorf("trim spaces: got %q", got)
	}
	long := strings.Repeat("x", 250)
	got := oneLine(long)
	if len(got) > 203 {
		t.Errorf("long string not truncated: len=%d", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("long string should end with ellipsis: %q", got[len(got)-10:])
	}
}

func TestSortByContention_Determinism(t *testing.T) {
	mk := func(id string, lastTouched int, reAttacked bool) ledger.Record {
		return ledger.Record{
			AttackID: id, Status: ledger.StatusUnresolved,
			RoundIntroduced: ptr(1), RoundLastTouched: lastTouched, ReAttacked: reAttacked,
		}
	}
	in := []ledger.Record{
		mk("c1-1", 3, false),
		mk("c1-2", 4, true),
		mk("c1-3", 2, false),
		mk("c1-4", 4, false),
	}
	out := SortByContention(in)
	// Highest contention first; equal scores break by AttackID.
	if out[0].AttackID != "c1-2" { // 4 + 1 = 5
		t.Errorf("first: got %q, want c1-2", out[0].AttackID)
	}
	if out[1].AttackID != "c1-4" { // 4 + 0 = 4
		t.Errorf("second: got %q, want c1-4", out[1].AttackID)
	}
}
