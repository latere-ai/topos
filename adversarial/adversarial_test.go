package adversarial_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	adversarial "latere.ai/x/topos/adversarial"
)

// stubProposer satisfies adversarial.Proposer.
type stubProposer struct {
	forkID string
	reply  string
}

func (s *stubProposer) FirstRound(_ context.Context, _ string) (*adversarial.ProposerResult, error) {
	return &adversarial.ProposerResult{ForkID: s.forkID, Response: s.reply, Duration: time.Millisecond}, nil
}

func (s *stubProposer) NextRound(_ context.Context, forkID, _ string) (*adversarial.ProposerResult, error) {
	return &adversarial.ProposerResult{ForkID: forkID, Response: s.reply, Duration: time.Millisecond}, nil
}

// stubCritic satisfies adversarial.Critic.
type stubCritic struct {
	rounds []string
	idx    int
}

func (s *stubCritic) Round(_ context.Context, _ adversarial.CriticInput) (*adversarial.CriticResult, error) {
	if s.idx >= len(s.rounds) {
		return &adversarial.CriticResult{
			Markdown: "# Critic 1 - round 99 attacks\n\naspect: security\n",
			Duration: time.Millisecond,
		}, nil
	}
	md := s.rounds[s.idx]
	s.idx++
	return &adversarial.CriticResult{Markdown: md, Duration: time.Millisecond}, nil
}

// TestAssemblePrompt verifies the public helper produces a non-empty
// string containing the expected sections.
func TestAssemblePrompt(t *testing.T) {
	in := adversarial.CriticInput{
		AspectName:   "security",
		SystemPrompt: "you are a security critic",
		TaskContext:  "add auth",
		DiffPatch:    "+x := 1",
	}
	got := adversarial.AssemblePrompt(in)
	for _, want := range []string{"security critic", "# Task", "add auth", "# Diff", "+x := 1"} {
		if !strings.Contains(got, want) {
			t.Errorf("AssemblePrompt output missing %q", want)
		}
	}
}

// TestEngineSteadyState runs the public Engine with stub proposer and critic,
// verifying it terminates cleanly and returns a Summary with correct fields.
func TestEngineSteadyState(t *testing.T) {
	// R1 critic: introduces attack c1-1
	r1 := "# Critic 1 - round 1 attacks\n\naspect: security\n\n" +
		"## c1-1 [main.go:5]\n\n" +
		"claim: password logged in plaintext\n\n" +
		"expected violation: log line exposes secret\n\n" +
		"reproduction:\n```\ngo test ./...\n```\n"
	// R3 critic: steady-state (no new attacks)
	r3 := "# Critic 1 - round 3 attacks\n\naspect: security\n"

	critic := &stubCritic{rounds: []string{r1, r3}}
	proposer := &stubProposer{forkID: "fork-abc", reply: "concede c1-1 — fixed by hashing"}

	eng := &adversarial.Engine{
		StateDir:    t.TempDir(),
		Cwd:         t.TempDir(),
		ForkCount:   1,
		Proposer:    proposer,
		NewCritic:   func(_ int) adversarial.Critic { return critic },
		MaxRounds:   6,
		CostCap:     1_000_000,
		TaskContext: "add user login",
		DiffPatch:   "+password := req.Password",
	}

	ctx := context.Background()
	sum, err := eng.Run(ctx)
	if err != nil {
		t.Fatalf("Engine.Run: %v", err)
	}
	if sum == nil {
		t.Fatal("nil Summary")
	}
	if len(sum.Forks) != 1 {
		t.Errorf("Forks = %d, want 1", len(sum.Forks))
	}
	if sum.SessionDir == "" {
		t.Error("SessionDir is empty")
	}
	// Attack was conceded → unresolved should be 0
	if sum.Unresolved != 0 {
		t.Errorf("Unresolved = %d, want 0", sum.Unresolved)
	}
}

// TestVerifierInterface verifies that a zero-value struct can satisfy
// adversarial.Verifier — a compile-time check.
func TestVerifierInterface(t *testing.T) {
	var v adversarial.Verifier = noopVerifier{}
	ctx := context.Background()
	res, err := v.Verify(ctx, adversarial.VerifyInput{})
	if err != nil || res != nil {
		t.Errorf("noopVerifier returned unexpected (%v, %v)", res, err)
	}
}

// noopVerifier is a compile-time proof that any struct with the right
// method set satisfies adversarial.Verifier.
type noopVerifier struct{}

func (noopVerifier) Verify(_ context.Context, _ adversarial.VerifyInput) (*adversarial.VerifyResult, error) {
	return nil, nil
}

// TestEngineRun_WritesEndJSON verifies Engine.Run persists the session's
// terminal end.json (summary.md too), so embedders can tell a finished run from
// a running one and read its token usage.
func TestEngineRun_WritesEndJSON(t *testing.T) {
	critic := &stubCritic{rounds: []string{"# Critic 1 - round 1 attacks\n\naspect: security\n"}}
	proposer := &stubProposer{forkID: "fork-xyz", reply: "looks fine"}
	eng := &adversarial.Engine{
		StateDir:    t.TempDir(),
		Cwd:         t.TempDir(),
		ForkCount:   1,
		Proposer:    proposer,
		NewCritic:   func(_ int) adversarial.Critic { return critic },
		MaxRounds:   2,
		CostCap:     1_000_000,
		TaskContext: "add login",
		DiffPatch:   "+x := 1",
	}
	sum, err := eng.Run(context.Background())
	if err != nil {
		t.Fatalf("Engine.Run: %v", err)
	}
	for _, name := range []string{"end.json", "summary.md"} {
		if _, err := os.Stat(filepath.Join(sum.SessionDir, name)); err != nil {
			t.Errorf("expected %s written by the library path: %v", name, err)
		}
	}
}

// TestEngineCostCapZeroMeansUnbounded asserts a zero CostCap is treated as no
// token budget rather than an instantly-exhausted one. Before the guard, the
// meter reported used(0) >= cap(0) and the loop terminated on cost-cap before
// running a single round.
func TestEngineCostCapZeroMeansUnbounded(t *testing.T) {
	critic := &stubCritic{rounds: []string{"# Critic 1 - round 1 attacks\n\naspect: security\n"}}
	eng := &adversarial.Engine{
		StateDir:    t.TempDir(),
		Cwd:         t.TempDir(),
		ForkCount:   1,
		Proposer:    &stubProposer{forkID: "fork-abc", reply: "ack"},
		NewCritic:   func(_ int) adversarial.Critic { return critic },
		MaxRounds:   6,
		CostCap:     0,
		TaskContext: "add user login",
		DiffPatch:   "+x := 1",
	}
	sum, err := eng.Run(context.Background())
	if err != nil {
		t.Fatalf("Engine.Run: %v", err)
	}
	if sum.Termination == "cost-cap" || len(sum.Forks) == 0 || sum.Forks[0].Rounds < 1 {
		rounds := -1
		if len(sum.Forks) > 0 {
			rounds = sum.Forks[0].Rounds
		}
		t.Fatalf("CostCap: 0 fired cost-cap before any work; termination=%s, fork rounds=%d",
			sum.Termination, rounds)
	}
}

// TestEngineMaxRoundsZeroDefaults asserts an unset MaxRounds runs the default
// round budget instead of zero rounds.
func TestEngineMaxRoundsZeroDefaults(t *testing.T) {
	critic := &stubCritic{rounds: []string{"# Critic 1 - round 1 attacks\n\naspect: security\n"}}
	eng := &adversarial.Engine{
		StateDir:    t.TempDir(),
		Cwd:         t.TempDir(),
		ForkCount:   1,
		Proposer:    &stubProposer{forkID: "fork-abc", reply: "ack"},
		NewCritic:   func(_ int) adversarial.Critic { return critic },
		CostCap:     1_000_000,
		TaskContext: "add user login",
		DiffPatch:   "+x := 1",
	}
	sum, err := eng.Run(context.Background())
	if err != nil {
		t.Fatalf("Engine.Run: %v", err)
	}
	if len(sum.Forks) == 0 || sum.Forks[0].Rounds < 1 {
		rounds := -1
		if len(sum.Forks) > 0 {
			rounds = sum.Forks[0].Rounds
		}
		t.Fatalf("MaxRounds: 0 ran no rounds; termination=%s, fork rounds=%d", sum.Termination, rounds)
	}
}
