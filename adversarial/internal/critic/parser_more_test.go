package critic

import (
	"strings"
	"testing"
)

func TestWithdrawValidAndInvalid(t *testing.T) {
	docInvalid := "# Critic 1 - round 3 attacks\n\naspect: security\n\n## c1-9 [x:1] (withdraw)\n\nreason: not real\n"
	out, stats, err := Parse(docInvalid, "security", 1, 3, nil, ParseOption{})
	if err != nil {
		t.Fatal(err)
	}
	// Unknown id under (withdraw) is treated as introduce; will be
	// dropped because there's no reproduction.
	if stats.DroppedNoReproduce == 0 {
		t.Errorf("withdraw of unknown id should fall back to introduce and drop without repro; got %+v", stats)
	}
	_ = out

	docValid := "# Critic 1 - round 3 attacks\n\naspect: security\n\n## c1-2 [x:1] (withdraw)\n\nreason: false positive\n"
	out, stats, err = Parse(docValid, "security", 1, 3, []string{"c1-2"}, ParseOption{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.KeptWithdraw != 1 || len(out) != 1 {
		t.Fatalf("expected 1 kept withdraw, got %+v out=%d", stats, len(out))
	}
	if out[0].Disposition != DispWithdraw || out[0].WithdrawReason != "false positive" {
		t.Errorf("withdraw not preserved: %+v", out[0])
	}
}

func TestReAttackUnknownIdFallsBack(t *testing.T) {
	doc := "# Critic 1 - round 3 attacks\n\naspect: security\n\n" +
		"## c1-99 [x:1] (re-attack)\n\nclaim: tighter\n\nexpected violation: panic\n\nreproduction:\n```\ngo\n```\n"
	out, stats, err := Parse(doc, "security", 1, 3, []string{"c1-1"}, ParseOption{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Disposition != DispIntroduce {
		t.Errorf("re-attack of unknown id should fall back to introduce: %+v", out)
	}
	if stats.Renamed == 0 {
		t.Error("expected renamed counter to fire")
	}
}

func TestRenderRoundTripWithWithdraw(t *testing.T) {
	atks := []Attack{
		{
			AttackID: "c1-1", Aspect: "security", Round: 1, Disposition: DispIntroduce,
			Location: "x:1", Claim: "leak", ExpectedViolation: "panic", Reproduction: "go test",
		},
		{
			AttackID: "c1-2", Aspect: "security", Round: 3, Disposition: DispWithdraw,
			Location: "y:1", WithdrawReason: "false positive",
		},
	}
	r := Render(1, 3, "security", atks)
	got := string(r)
	if !strings.Contains(got, "(withdraw)") {
		t.Errorf("missing withdraw tag: %s", got)
	}
	if !strings.Contains(got, "reason: false positive") {
		t.Error("missing reason line")
	}
}

func TestStyleAttackKeptWhenAllowed(t *testing.T) {
	doc := "# Critic 1 - round 1 attacks\n\naspect: code-quality\n\n## c1-1 [x:1]\n\nclaim: This function should be named more idiomatic.\n\nexpected violation: it bothers me\n\nreproduction:\n```\nrun\n```\n"
	out, stats, err := Parse(doc, "code-quality", 1, 1, nil, ParseOption{AllowStyleAttacks: true})
	if err != nil {
		t.Fatal(err)
	}
	if stats.DroppedStyle != 0 || len(out) != 1 {
		t.Errorf("style should be kept under AllowStyleAttacks: %+v", stats)
	}
}

func TestEmptyHeaderError(t *testing.T) {
	_, _, err := Parse("", "security", 1, 1, nil, ParseOption{})
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

// TestParseRenamesReusedPriorIDOnIntroduce reproduces the bug a adversarial
// session in agents-byzantine-tolerance hit: the R3 critic emitted a
// completely new claim under "## c1-1 [...]" (the R1 id), no
// "(re-attack)" marker, no acknowledgement of R2's defense. The parser
// used to silently accept the id, collapsing two unrelated attacks
// onto a single ledger entry. With the fix, the new claim is kept but
// renamed to a fresh id and stats.Renamed is bumped so the drift is
// auditable.
func TestParseRenamesReusedPriorIDOnIntroduce(t *testing.T) {
	doc := "# Critic 1 - round 3 attacks\n\naspect: instruction-completeness\n\n" +
		"## c1-1 [results/README.md:54]\n\n" +
		"claim: brand-new claim about a different flaw entirely.\n\n" +
		"expected violation: panic at runtime\n\n" +
		"reproduction:\n```\nuv run python\n```\n"
	out, stats, err := Parse(doc, "instruction-completeness", 1, 3, []string{"c1-1", "c1-2"}, ParseOption{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("attacks: got %d, want 1", len(out))
	}
	if out[0].AttackID == "c1-1" {
		t.Errorf("reused prior id was not renamed: AttackID=%q", out[0].AttackID)
	}
	if out[0].AttackID != "c1-3" {
		t.Errorf("expected fresh id c1-3 (next after maxSeq=2), got %q", out[0].AttackID)
	}
	if out[0].Disposition != DispIntroduce {
		t.Errorf("disposition: got %v, want DispIntroduce", out[0].Disposition)
	}
	if stats.Renamed == 0 {
		t.Error("stats.Renamed should fire when a prior id is reused without (re-attack)")
	}
	if stats.KeptIntroduce != 1 {
		t.Errorf("KeptIntroduce: got %d, want 1", stats.KeptIntroduce)
	}
}

// TestParseReAttackPreservesPriorID is the partner assertion: when the
// critic correctly tags "(re-attack)" with a prior id, the id is
// preserved (no rename) and disposition is DispReAttack. Guards
// against an over-eager fix that would also rename re-attacks.
func TestParseReAttackPreservesPriorID(t *testing.T) {
	doc := "# Critic 1 - round 3 attacks\n\naspect: security\n\n" +
		"## c1-1 [x.go:1] (re-attack)\n\n" +
		"claim: refined: framework escape is incomplete because the encoder runs after the unsafe call.\n\n" +
		"expected violation: panic at runtime\n\n" +
		"reproduction:\n```\ngo test\n```\n"
	out, stats, err := Parse(doc, "security", 1, 3, []string{"c1-1"}, ParseOption{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].AttackID != "c1-1" {
		t.Fatalf("re-attack id should be preserved; got %+v", out)
	}
	if out[0].Disposition != DispReAttack {
		t.Errorf("disposition: got %v, want DispReAttack", out[0].Disposition)
	}
	if stats.KeptReAttack != 1 {
		t.Errorf("KeptReAttack: got %d, want 1", stats.KeptReAttack)
	}
	if stats.Renamed != 0 {
		t.Errorf("stats.Renamed should NOT fire for a valid re-attack; got %d", stats.Renamed)
	}
}

func TestExtractDeclaredAspect(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"missing", "no aspect line", ""},
		{"basic", "aspect: security\n", "security"},
		{"with-extra-text", "# Header\n\naspect: performance\n\nbody", "performance"},
		{"trims-spaces", "aspect:    code-quality   \n", "code-quality"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExtractDeclaredAspect(c.in); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestIsStyleShaped(t *testing.T) {
	cases := []struct {
		name, claim, exp string
		want             bool
	}{
		{"non-style claim", "memory leak", "panic", false},
		{"style + concrete", "should be named foo for clarity", "panic at line 5", false},
		{"style + fenced", "should be named foo for clarity", "```\nfoo()\n```", false},
		{"pure style", "should be named foo for clarity", "this is preference", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isStyleShaped(c.claim, c.exp); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
