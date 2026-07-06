package adversarial_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adversarial "latere.ai/x/topos/adversarial"
)

// TestReview_EndToEnd runs the thin Review surface with a stub proposer and
// critic and asserts it drives the same debate as the Engine path: it returns a
// Summary and writes sessions/<id>/ under the caller-provided StateDir.
func TestReview_EndToEnd(t *testing.T) {
	r1 := "# Critic 1 - round 1 attacks\n\naspect: security\n\n" +
		"## c1-1 [main.go:5]\n\n" +
		"claim: password logged in plaintext\n\n" +
		"expected violation: log line exposes secret\n\n" +
		"reproduction:\n```\ngo test ./...\n```\n"
	r3 := "# Critic 1 - round 3 attacks\n\naspect: security\n"

	critic := &stubCritic{rounds: []string{r1, r3}}
	proposer := &stubProposer{forkID: "fork-abc", reply: "concede c1-1 — fixed by hashing"}

	stateDir := t.TempDir()
	sum, err := adversarial.Review(context.Background(), adversarial.ReviewOptions{
		StateDir:    stateDir,
		Cwd:         t.TempDir(),
		Forks:       1,
		Proposer:    proposer,
		NewCritic:   func(_ int) adversarial.Critic { return critic },
		MaxRounds:   6,
		CostCap:     1_000_000,
		TaskContext: "add user login",
		DiffPatch:   "+password := req.Password",
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if sum == nil {
		t.Fatal("nil Summary")
	}
	if len(sum.Forks) != 1 {
		t.Errorf("Forks = %d, want 1", len(sum.Forks))
	}
	if sum.Unresolved != 0 {
		t.Errorf("Unresolved = %d, want 0", sum.Unresolved)
	}

	// The engine writes sessions/<id>/ under the caller's StateDir and invents
	// no path of its own.
	wantPrefix := filepath.Join(stateDir, "sessions") + string(os.PathSeparator)
	if !strings.HasPrefix(sum.SessionDir, wantPrefix) {
		t.Errorf("SessionDir = %q, want it under %q", sum.SessionDir, wantPrefix)
	}
	if _, err := os.Stat(filepath.Join(sum.SessionDir, "end.json")); err != nil {
		t.Errorf("expected end.json under the session dir: %v", err)
	}
}

// TestReview_EmptyStateDirErrors pins the brand-neutral contract: StateDir is
// required, and an empty one is a caller error rather than a guess at a location
// (no ~/.latere, no .topos/, no cwd-relative sessions/).
func TestReview_EmptyStateDirErrors(t *testing.T) {
	_, err := adversarial.Review(context.Background(), adversarial.ReviewOptions{
		StateDir:  "",
		Forks:     1,
		Proposer:  &stubProposer{forkID: "f", reply: "ok"},
		NewCritic: func(_ int) adversarial.Critic { return &stubCritic{} },
	})
	if err == nil {
		t.Fatal("expected an error for empty StateDir, got nil")
	}
	if !strings.Contains(err.Error(), "StateDir") {
		t.Errorf("error should name StateDir, got %v", err)
	}
}
