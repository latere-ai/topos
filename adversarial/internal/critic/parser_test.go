package critic

import (
	"strings"
	"testing"
)

const sampleR1 = "# Critic 2 - round 1 attacks\n\n" +
	"aspect: security\n\n" +
	"## c2-1 [src/api.py:88]\n\n" +
	"claim: The search handler concatenates user-supplied input directly into a SQL `LIKE` pattern without escaping.\n\n" +
	"expected violation: An attacker can probe the table by submitting `q=%' OR 1=1--`, which terminates the LIKE pattern and injects boolean logic.\n\n" +
	"reproduction:\n```\ncurl 'http://localhost:8000/search?q=%25%27%20OR%201%3D1--'\n```\n\n" +
	"---\n\n" +
	"## c2-2 [src/auth.py:42]\n\n" +
	"claim: The login endpoint logs the full Authorization header on auth failure, leaking bearer tokens to the application log.\n\n" +
	"expected violation: A failed login with a valid-shaped bearer token writes that token to stdout and any structured-log sink the app forwards to.\n\n" +
	"reproduction:\n```\ncurl -i -H 'Authorization: Bearer test-token-9f1' http://localhost:8000/auth/wrong\n```\n"

func TestParseHappy(t *testing.T) {
	out, stats, err := Parse(sampleR1, "security", 2, 1, nil, ParseOption{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Errorf("attacks: got %d, want 2", len(out))
	}
	if stats.KeptIntroduce != 2 {
		t.Errorf("kept introduce: got %d, want 2", stats.KeptIntroduce)
	}
	if out[0].AttackID != "c2-1" {
		t.Errorf("first id: got %q", out[0].AttackID)
	}
}

// TestParseFenceWithHeaderLine pins the fence-aware tokenizer: a "## "
// line inside a reproduction fence (critics routinely quote markdown
// counterexamples) must not be mistaken for a new section header. Before
// the fix the body split there, extractFenced found no closing fence,
// and the otherwise-valid attack was dropped as DroppedNoReproduce.
func TestParseFenceWithHeaderLine(t *testing.T) {
	doc := "# Critic 1 - round 1 attacks\n\naspect: security\n\n" +
		"## c1-1 [src/api.py:88]\n\n" +
		"claim: the search handler concatenates user input into a SQL LIKE pattern without escaping.\n\n" +
		"expected violation: an attacker can inject boolean logic via q=%' OR 1=1--.\n\n" +
		"reproduction:\n```\ncurl 'http://localhost/search?q=1'\n## Section Two\nHTTP/1.1 200 OK\n```\n"
	out, stats, err := Parse(doc, "security", 1, 1, nil, ParseOption{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("attacks: got %d, want 1", len(out))
	}
	if stats.KeptIntroduce != 1 {
		t.Errorf("kept introduce: got %d, want 1", stats.KeptIntroduce)
	}
	if stats.DroppedNoReproduce != 0 {
		t.Errorf("dropped no-reproduce: got %d, want 0", stats.DroppedNoReproduce)
	}
	if !strings.Contains(out[0].Reproduction, "## Section Two") {
		t.Errorf("reproduction lost the fenced header line: %q", out[0].Reproduction)
	}
}

// TestParseMalformedHeaderCounted pins that a section whose "## " header
// fails the expected shape is counted in DroppedMalformedHeader, so the
// buckets reconcile to Total instead of an attack vanishing silently.
func TestParseMalformedHeaderCounted(t *testing.T) {
	doc := "# Critic 1 - round 1 attacks\n\naspect: security\n\n" +
		"## c1-1 [x.py:1]\n\nclaim: leaks token\n\nexpected violation: panic\n\nreproduction:\n```\nrun\n```\n\n" +
		"---\n\n" +
		"## c1-2 no brackets here\n\nclaim: whatever\n"
	out, stats, err := Parse(doc, "security", 1, 1, nil, ParseOption{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("attacks: got %d, want 1", len(out))
	}
	if stats.DroppedMalformedHeader != 1 {
		t.Errorf("DroppedMalformedHeader: got %d, want 1", stats.DroppedMalformedHeader)
	}
	sum := stats.KeptIntroduce + stats.KeptReAttack + stats.KeptWithdraw +
		stats.DroppedNoReproduce + stats.DroppedStyle + stats.DroppedCrossAspect +
		stats.DroppedMalformedHeader
	if sum != stats.Total {
		t.Errorf("buckets %d do not reconcile to Total %d", sum, stats.Total)
	}
}

// TestParseRenamedCountedOncePerSection pins that a section is counted
// in Renamed at most once. A cross-critic id (c2-1 written by critic 1)
// is normalized to c1-1: exactly one rename, where the old code counted
// the normalization steps and reported two.
func TestParseRenamedCountedOncePerSection(t *testing.T) {
	doc := "# Critic 1 - round 1 attacks\n\naspect: security\n\n" +
		"## c2-1 [x.py:1]\n\nclaim: leaks token\n\nexpected violation: panic\n\nreproduction:\n```\nrun\n```\n"
	out, stats, err := Parse(doc, "security", 1, 1, nil, ParseOption{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].AttackID != "c1-1" {
		t.Fatalf("attacks: got %+v, want one c1-1", out)
	}
	if stats.Renamed != 1 {
		t.Errorf("Renamed: got %d, want 1", stats.Renamed)
	}
}

func TestDropNoReproduction(t *testing.T) {
	doc := "# Critic 1 - round 1 attacks\n\naspect: security\n\n## c1-1 [x.py:1]\n\nclaim: x\n\nexpected violation: panic in y\n"
	_, stats, err := Parse(doc, "security", 1, 1, nil, ParseOption{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.DroppedNoReproduce != 1 {
		t.Errorf("dropped: got %d, want 1", stats.DroppedNoReproduce)
	}
}

func TestDropStyle(t *testing.T) {
	doc := "# Critic 1 - round 1 attacks\n\naspect: code-quality\n\n## c1-1 [x.py:1]\n\nclaim: This function should be named more idiomatic.\n\nexpected violation: it bothers me\n\nreproduction:\n```\nrun it\n```\n"
	_, stats, err := Parse(doc, "code-quality", 1, 1, nil, ParseOption{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.DroppedStyle != 1 {
		t.Errorf("dropped style: got %d", stats.DroppedStyle)
	}
}

func TestAllowStyleAttacksKeepsStyle(t *testing.T) {
	doc := "# Critic 1 - round 1 attacks\n\naspect: code-quality\n\n## c1-1 [x.py:1]\n\nclaim: This function should be named more idiomatic.\n\nexpected violation: it bothers me\n\nreproduction:\n```\nrun it\n```\n"
	out, stats, err := Parse(doc, "code-quality", 1, 1, nil, ParseOption{AllowStyleAttacks: true})
	if err != nil {
		t.Fatal(err)
	}
	if stats.DroppedStyle != 0 {
		t.Errorf("DroppedStyle: got %d, want 0 with AllowStyleAttacks", stats.DroppedStyle)
	}
	if len(out) != 1 {
		t.Errorf("attacks: got %d, want 1 kept", len(out))
	}
}

func TestDropCrossAspect(t *testing.T) {
	doc := "# Critic 1 - round 1 attacks\n\naspect: performance\n\n## c1-1 [x.py:1]\n\nclaim: SQL injection in the search handler.\n\nexpected violation: panic\n\nreproduction:\n```\ngo\n```\n"
	_, stats, err := Parse(doc, "performance", 1, 1, nil, ParseOption{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.DroppedCrossAspect != 1 {
		t.Errorf("dropped cross-aspect: got %d", stats.DroppedCrossAspect)
	}
}

func TestRoundTripRender(t *testing.T) {
	out, _, err := Parse(sampleR1, "security", 2, 1, nil, ParseOption{})
	if err != nil {
		t.Fatal(err)
	}
	rendered := Render(2, 1, "security", out)
	out2, _, err := Parse(string(rendered), "security", 2, 1, nil, ParseOption{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(out2) {
		t.Errorf("round trip lost attacks: %d -> %d", len(out), len(out2))
	}
	for i := range out {
		if out[i].AttackID != out2[i].AttackID {
			t.Errorf("id mismatch [%d]: %q vs %q", i, out[i].AttackID, out2[i].AttackID)
		}
	}
}

func TestNormalizerCollision(t *testing.T) {
	doc := "# Critic 1 - round 1 attacks\n\naspect: security\n\n" +
		"## c1-1 [x:1]\n\nclaim: a\n\nexpected violation: panic\n\nreproduction:\n```\na\n```\n\n" +
		"## c1-1 [y:1]\n\nclaim: b\n\nexpected violation: panic\n\nreproduction:\n```\nb\n```\n"
	out, stats, err := Parse(doc, "security", 1, 1, nil, ParseOption{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("attacks: got %d, want 2", len(out))
	}
	if out[0].AttackID == out[1].AttackID {
		t.Errorf("collision not resolved: both are %q", out[0].AttackID)
	}
	if stats.Renamed == 0 {
		t.Error("expected renamed counter to fire")
	}
}

func TestBadHeader(t *testing.T) {
	_, _, err := Parse("# wrong header\n", "security", 1, 1, nil, ParseOption{})
	if err == nil {
		t.Fatal("expected error for malformed top header")
	}
	if !strings.Contains(err.Error(), "header") {
		t.Errorf("error should mention header: %v", err)
	}
}
