package ledger

import (
	"strings"
	"testing"
)

// markdownSidecar is the legacy "## "-delimited body shape older
// side-cars used before the JSON spillDoc format. parseSpill must
// still round-trip it for sessions written by prior versions.
const markdownSidecar = "## Claim\nthe claim body\nsecond line\n\n" +
	"## Expected violation\nthe expected violation\n\n" +
	"## Reproduction\nrun the repro steps\n"

// TestParseSpillMarkdownBackCompat pins that parseSpill decodes the
// legacy markdown side-car when the body is not JSON, exercising the
// splitOnHeader/splitLines/startsWith/trimSection helpers that only
// run on the backward-compat path.
func TestParseSpillMarkdownBackCompat(t *testing.T) {
	claim, exp, repro := parseSpill(markdownSidecar)
	if claim != "the claim body\nsecond line" {
		t.Errorf("claim: got %q", claim)
	}
	if exp != "the expected violation" {
		t.Errorf("expected_violation: got %q", exp)
	}
	if repro != "run the repro steps" {
		t.Errorf("reproduction: got %q", repro)
	}
}

// TestLoadBodyMarkdownSidecar drives the backward-compat path through
// the public LoadBody entry point: a side-car written in the legacy
// markdown shape resolves into the inline body fields.
func TestLoadBodyMarkdownSidecar(t *testing.T) {
	s := freshSession(t)
	rel := "forks/critic-1/attacks/c1-1.md"
	if err := s.AtomicWrite(rel, []byte(markdownSidecar)); err != nil {
		t.Fatal(err)
	}
	got, err := LoadBody(s, Record{AttackID: "c1-1", BodyPath: rel})
	if err != nil {
		t.Fatal(err)
	}
	if got.Claim != "the claim body\nsecond line" {
		t.Errorf("claim: got %q", got.Claim)
	}
	if got.ExpectedViolation != "the expected violation" {
		t.Errorf("expected_violation: got %q", got.ExpectedViolation)
	}
	if got.Reproduction != "run the repro steps" {
		t.Errorf("reproduction: got %q", got.Reproduction)
	}
}

// TestLoadBodyMissingSidecar covers the error path when body_path
// points at a file that does not exist: LoadBody returns the original
// record and the read error.
func TestLoadBodyMissingSidecar(t *testing.T) {
	s := freshSession(t)
	in := Record{AttackID: "c1-1", BodyPath: "forks/critic-1/attacks/gone.md"}
	got, err := LoadBody(s, in)
	if err == nil {
		t.Fatal("expected error for missing side-car")
	}
	if got.AttackID != "c1-1" {
		t.Errorf("original record should be returned on error: got %q", got.AttackID)
	}
}

// TestSplitOnHeaderIgnoresPreamble asserts splitOnHeader drops any
// text preceding the first header and starts a new section on each
// header line.
func TestSplitOnHeaderIgnoresPreamble(t *testing.T) {
	in := "preamble line dropped\n## First\nbody one\n## Second\nbody two\n"
	got := splitOnHeader(in, "## ")
	if len(got) != 2 {
		t.Fatalf("section count: got %d, want 2 (%q)", len(got), got)
	}
	if !strings.HasPrefix(got[0], "First") || !strings.Contains(got[0], "body one") {
		t.Errorf("section 0: got %q", got[0])
	}
	if !strings.HasPrefix(got[1], "Second") || !strings.Contains(got[1], "body two") {
		t.Errorf("section 1: got %q", got[1])
	}
}

func TestSplitLines(t *testing.T) {
	// Trailing newline does not yield a trailing empty element; a body
	// without a trailing newline still contributes its final line.
	if got := splitLines("a\nb\nc\n"); len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Errorf("with trailing newline: got %q", got)
	}
	if got := splitLines("x\ny"); len(got) != 2 || got[1] != "y" {
		t.Errorf("without trailing newline: got %q", got)
	}
	if got := splitLines(""); len(got) != 0 {
		t.Errorf("empty input should yield no lines: got %q", got)
	}
}

func TestStartsWith(t *testing.T) {
	if !startsWith("## Claim", "## ") {
		t.Error("prefix present")
	}
	if startsWith("no", "longer-prefix") {
		t.Error("prefix longer than string must be false")
	}
	if startsWith("abc", "xy") {
		t.Error("non-matching prefix must be false")
	}
}

// TestTrimSectionNoNewline covers trimSection's fallthrough: a section
// consisting only of the header line (no newline after it) trims to
// the empty string.
func TestTrimSectionNoNewline(t *testing.T) {
	if got := trimSection("Claim", "Claim"); got != "" {
		t.Errorf("header-only section should trim to empty: got %q", got)
	}
	if got := trimSection("Claim\n\nvalue\n\n", "Claim"); got != "value" {
		t.Errorf("leading/trailing blanks should be trimmed: got %q", got)
	}
}
