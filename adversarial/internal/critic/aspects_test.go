package critic

import (
	"strings"
	"testing"
)

func TestBuiltinAll(t *testing.T) {
	a := Builtin()
	for _, name := range []string{"functional-logic", "security", "code-quality", "performance"} {
		if _, ok := a[name]; !ok {
			t.Errorf("missing builtin aspect: %q", name)
		}
	}
}

func TestLookupGeneric(t *testing.T) {
	a := Lookup("concurrency-safety")
	if a.Name != "concurrency-safety" {
		t.Errorf("name not propagated: %q", a.Name)
	}
	if len(a.ForbiddenKeywords) != 0 {
		t.Errorf("generic aspect should have no forbidden keywords, got %v", a.ForbiddenKeywords)
	}
	if !strings.Contains(a.SystemPrompt, "concurrency-safety") {
		t.Error("generic prompt should embed the aspect name")
	}
}

func TestAssemble(t *testing.T) {
	a := Lookup("security")
	got := Assemble(a, 1, 3, "Prior rounds: r1, r2")
	if !strings.Contains(got, "Round: 3 (critic-1)") {
		t.Errorf("missing round marker: %q", got[:200])
	}
	if !strings.Contains(got, "Prior rounds: r1, r2") {
		t.Error("missing prior rounds note")
	}
}

// TestContradictionReproductionRule checks the per-attack hard rules
// in both the auto and aspect-locked skeletons forbid ellipsis-only
// reproductions and require both contradicting passages to be quoted.
// Reproduces a real session in agents-byzantine-tolerance where the
// agent claimed two passages contradict, then "proved" it with a
// reproduction that quoted only one of them and trailed off into
// "...The first non-smoke run should be:" - which is not evidence at
// all.
func TestContradictionReproductionRule(t *testing.T) {
	prompts := map[string]string{
		"auto":           Auto(1, 1, nil).SystemPrompt,
		"aspect-locked":  Lookup("security").SystemPrompt,
		"generic-aspect": Lookup("instruction-completeness").SystemPrompt,
	}
	for label, p := range prompts {
		for _, want := range []string{
			"contradiction or ambiguity claims",
			"BOTH passages in full",
			"file:line",
			`"..."`,
			"forbidden",
		} {
			if !strings.Contains(p, want) {
				t.Errorf("%s prompt missing %q", label, want)
			}
		}
	}
}

// TestAssembleR3DispositionContract checks the R3+ contract is in the
// system prompt: the agent must be told to (a) read the prior round
// files, (b) react to each prior attack via re-attack/withdraw/drop,
// (c) allocate a fresh id for new attacks. Without this contract the
// critic just runs another fresh attack round and the orchestrator's
// "# Prior rounds" pointer is wasted.
func TestAssembleR3DispositionContract(t *testing.T) {
	a := Lookup("security")
	r3 := Assemble(a, 1, 3, "")
	for _, want := range []string{
		"Round 3+ responsibilities",
		"# Prior rounds",
		"re-attack",
		"withdraw",
		"concede c<i>-<seq>",
		"rebut c<i>-<seq>",
		"push-back c<i>-<seq>",
		"NEW attacks",
		"c<i>-<next>",
	} {
		if !strings.Contains(r3, want) {
			t.Errorf("R3 prompt missing %q", want)
		}
	}

	// R1 and R2 must NOT carry the contract - they are fresh-attack
	// rounds, and dragging the contract in early would confuse the
	// agent into hunting for prior rounds that don't exist.
	for _, round := range []int{1, 2} {
		got := Assemble(a, 1, round, "")
		if strings.Contains(got, "Round 3+ responsibilities") {
			t.Errorf("R%d prompt should not include R3+ contract", round)
		}
	}
}

func TestAuto_WithPriorTopics(t *testing.T) {
	a := Auto(2, 4, []string{"security", "performance", "  ", ""})
	if !strings.Contains(a.SystemPrompt, "security") {
		t.Errorf("Auto should include prior topic 'security' in avoid list")
	}
	if !strings.Contains(a.SystemPrompt, "performance") {
		t.Errorf("Auto should include prior topic 'performance' in avoid list")
	}
}

func TestAuto_FirstCritic(t *testing.T) {
	a := Auto(1, 4, nil)
	if !strings.Contains(a.SystemPrompt, "critic 1") {
		t.Errorf("Auto for first critic should mention 'critic 1' fallback line: %q", a.SystemPrompt)
	}
}

func TestLocked(t *testing.T) {
	a := Locked(2, 4, "security")
	if a.Name != "security" {
		t.Errorf("name: got %q, want security", a.Name)
	}
	if a.SystemPrompt == "" {
		t.Error("SystemPrompt empty")
	}
}
