package round

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/topos/adversarial/internal/agent"
	"latere.ai/x/topos/adversarial/internal/ansi"
	"latere.ai/x/topos/adversarial/internal/ledger"
	"latere.ai/x/topos/adversarial/internal/state"
)

type stubProposer struct {
	first, next func(string) (*agent.ProposerResult, error)
}

func (s *stubProposer) FirstRound(_ context.Context, pointer string) (*agent.ProposerResult, error) {
	return s.first(pointer)
}

func (s *stubProposer) NextRound(_ context.Context, _ string, pointer string) (*agent.ProposerResult, error) {
	return s.next(pointer)
}

type stubCritic struct {
	rounds []string
	idx    int
	inputs []agent.CriticInput
}

func (s *stubCritic) Round(_ context.Context, in agent.CriticInput) (*agent.CriticResult, error) {
	s.inputs = append(s.inputs, in)
	if s.idx >= len(s.rounds) {
		return &agent.CriticResult{Markdown: "# Critic 1 - round 99 attacks\n\naspect: security\n", Duration: time.Millisecond}, nil
	}
	out := &agent.CriticResult{Markdown: s.rounds[s.idx], Duration: time.Millisecond}
	s.idx++
	return out, nil
}

func TestEngineSingleForkSteadyState(t *testing.T) {
	sess, err := state.NewSession(t.TempDir(), 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r1 := "# Critic 1 - round 1 attacks\n\naspect: security\n\n## c1-1 [x.go:1]\n\nclaim: leaks token\n\nexpected violation: panic at runtime\n\nreproduction:\n```\ngo test\n```\n"
	r3 := "# Critic 1 - round 3 attacks\n\naspect: security\n" // empty: no new
	r5 := "# Critic 1 - round 5 attacks\n\naspect: security\n" // empty: steady state at R5

	e := &Engine{
		Sess: sess, Cwd: t.TempDir(),
		ForkCount: 1,
		Proposer: &stubProposer{
			first: func(_ string) (*agent.ProposerResult, error) {
				return &agent.ProposerResult{ForkID: "fork-1", Response: "rebut c1-1: framework escapes", Tokens: 10}, nil
			},
			next: func(_ string) (*agent.ProposerResult, error) {
				return &agent.ProposerResult{ForkID: "fork-1", Response: "no further action", Tokens: 10}, nil
			},
		},
		NewCritic: func(_ int) agent.Critic { return &stubCritic{rounds: []string{r1, r3, r5}} },
		MaxRounds: 6, CostCap: 100000, TaskContext: "task", DiffPatch: "diff",
	}
	sum, err := e.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Termination != TermSteadyState {
		t.Errorf("termination: got %s, want steady-state", sum.Termination)
	}
	agg, _ := ledger.Aggregate(sess)
	if r := agg["c1-1"]; r.Status != ledger.StatusRebutted && r.Status != ledger.StatusUnresolved {
		t.Errorf("attack status after rebut: got %s", r.Status)
	}
}

func TestEngineCostCap(t *testing.T) {
	sess, err := state.NewSession(t.TempDir(), 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r1 := "# Critic 1 - round 1 attacks\n\naspect: security\n\n## c1-1 [x:1]\n\nclaim: leaks\n\nexpected violation: panic\n\nreproduction:\n```\nx\n```\n"
	e := &Engine{
		Sess: sess, Cwd: t.TempDir(),
		ForkCount: 1,
		Proposer: &stubProposer{
			first: func(string) (*agent.ProposerResult, error) {
				return &agent.ProposerResult{ForkID: "f", Response: "rebut c1-1: ok", Tokens: 50000}, nil
			},
			next: func(string) (*agent.ProposerResult, error) {
				return &agent.ProposerResult{ForkID: "f", Response: "rebut c1-1: ok", Tokens: 50000}, nil
			},
		},
		NewCritic: func(_ int) agent.Critic {
			return &stubCritic{rounds: []string{r1, r1, r1, r1, r1}}
		},
		MaxRounds: 6, CostCap: 10000, TaskContext: "t", DiffPatch: "d",
	}
	sum, err := e.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Termination != TermCostCap {
		t.Errorf("termination: got %s, want cost-cap", sum.Termination)
	}
}

// TestCriticRoundPriorFiles ensures R3 onward receives pointers to the
// previous critic and proposer round files. Without these the critic
// agent has no way to react to the proposer's defense and tends to emit
// an empty document (the spec-mandated "nothing to attack" shape).
func TestCriticRoundPriorFiles(t *testing.T) {
	sess, err := state.NewSession(t.TempDir(), 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r1 := "# Critic 1 - round 1 attacks\n\naspect: security\n\n## c1-1 [x.go:1]\n\nclaim: leaks token\n\nexpected violation: panic at runtime\n\nreproduction:\n```\ngo test\n```\n"
	r3 := "# Critic 1 - round 3 attacks\n\naspect: security\n\n## c1-1 [x.go:1] (re-attack)\n\nclaim: still leaks\n\nexpected violation: panic\n\nreproduction:\n```\ngo test\n```\n"
	r5 := "# Critic 1 - round 5 attacks\n\naspect: security\n"

	cri := &stubCritic{rounds: []string{r1, r3, r5}}
	e := &Engine{
		Sess: sess, Cwd: t.TempDir(),
		ForkCount: 1,
		Proposer: &stubProposer{
			first: func(string) (*agent.ProposerResult, error) {
				return &agent.ProposerResult{ForkID: "fork-1", Response: "rebut c1-1: ok", Tokens: 10}, nil
			},
			next: func(string) (*agent.ProposerResult, error) {
				return &agent.ProposerResult{ForkID: "fork-1", Response: "rebut c1-1: ok", Tokens: 10}, nil
			},
		},
		NewCritic: func(_ int) agent.Critic { return cri },
		MaxRounds: 6, CostCap: 100000, TaskContext: "task", DiffPatch: "diff",
	}
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(cri.inputs) < 2 {
		t.Fatalf("got %d critic inputs, want >= 2", len(cri.inputs))
	}
	r1in := cri.inputs[0]
	if len(r1in.PriorRoundFiles) != 0 {
		t.Errorf("R1 PriorRoundFiles: got %d, want 0", len(r1in.PriorRoundFiles))
	}
	r3in := cri.inputs[1]
	if len(r3in.PriorRoundFiles) != 2 {
		t.Fatalf("R3 PriorRoundFiles: got %d, want 2", len(r3in.PriorRoundFiles))
	}
	wantCritic := sess.Path(filepath.Join("forks", "critic-1", "rounds", "r1-critic.md"))
	wantProposer := sess.Path(filepath.Join("forks", "critic-1", "rounds", "r2-proposer.md"))
	if r3in.PriorRoundFiles[0].Path != wantCritic || r3in.PriorRoundFiles[0].Role != "critic" || r3in.PriorRoundFiles[0].Round != 1 {
		t.Errorf("R3 PriorRoundFiles[0]: got %+v, want path=%s round=1 role=critic", r3in.PriorRoundFiles[0], wantCritic)
	}
	if r3in.PriorRoundFiles[1].Path != wantProposer || r3in.PriorRoundFiles[1].Role != "proposer" || r3in.PriorRoundFiles[1].Round != 2 {
		t.Errorf("R3 PriorRoundFiles[1]: got %+v, want path=%s round=2 role=proposer", r3in.PriorRoundFiles[1], wantProposer)
	}
}

func TestEnginePerForkUsageStats(t *testing.T) {
	sess, err := state.NewSession(t.TempDir(), 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r1 := "# Critic 1 - round 1 attacks\n\naspect: security\n\n## c1-1 [x.go:1]\n\nclaim: leaks token\n\nexpected violation: panic at runtime\n\nreproduction:\n```\ngo test\n```\n"
	r3 := "# Critic 1 - round 3 attacks\n\naspect: security\n"
	r5 := "# Critic 1 - round 5 attacks\n\naspect: security\n"
	cu := agent.TokenUsage{Input: 1000, Output: 200, CacheCreate: 800, CacheRead: 4000}
	pu := agent.TokenUsage{Input: 500, Output: 150, CacheCreate: 0, CacheRead: 3000}
	const criticUSD = 0.0125
	const proposerUSD = 0.0080
	sc := &usageCritic{
		rounds: []string{r1, r3, r5},
		usage:  cu,
		usd:    criticUSD,
	}
	e := &Engine{
		Sess: sess, Cwd: t.TempDir(),
		ForkCount: 1,
		Proposer: &stubProposer{
			first: func(string) (*agent.ProposerResult, error) {
				return &agent.ProposerResult{ForkID: "fork-1", Response: "rebut c1-1: ok", Tokens: 10, Usage: pu, USD: proposerUSD}, nil
			},
			next: func(string) (*agent.ProposerResult, error) {
				return &agent.ProposerResult{ForkID: "fork-1", Response: "no further action", Tokens: 10, Usage: pu, USD: proposerUSD}, nil
			},
		},
		NewCritic: func(_ int) agent.Critic { return sc },
		MaxRounds: 6, CostCap: 1_000_000, TaskContext: "task", DiffPatch: "diff",
	}
	sum, err := e.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Forks) != 1 {
		t.Fatalf("want 1 fork, got %d", len(sum.Forks))
	}
	got := sum.Forks[0].Usage
	criticRounds := sc.idx
	wantCritic := agent.TokenUsage{
		Input:       cu.Input * criticRounds,
		Output:      cu.Output * criticRounds,
		CacheCreate: cu.CacheCreate * criticRounds,
		CacheRead:   cu.CacheRead * criticRounds,
	}
	if got.Critic != wantCritic {
		t.Errorf("critic usage: got %+v, want %+v", got.Critic, wantCritic)
	}
	if got.Total.Total() != got.Critic.Total()+got.Proposer.Total() {
		t.Errorf("total mismatch: got %+v", got.Total)
	}
	if sum.Usage != got.Total {
		t.Errorf("run-level usage: got %+v, want %+v", sum.Usage, got.Total)
	}
	proposerRounds := sum.Forks[0].Rounds - criticRounds
	wantCriticUSD := criticUSD * float64(criticRounds)
	wantProposerUSD := proposerUSD * float64(proposerRounds)
	wantTotalUSD := wantCriticUSD + wantProposerUSD
	if !floatEq(got.CriticUSD, wantCriticUSD) {
		t.Errorf("critic USD: got %v, want %v", got.CriticUSD, wantCriticUSD)
	}
	if !floatEq(got.ProposerUSD, wantProposerUSD) {
		t.Errorf("proposer USD: got %v, want %v", got.ProposerUSD, wantProposerUSD)
	}
	if !floatEq(got.TotalUSD, wantTotalUSD) {
		t.Errorf("total USD: got %v, want %v", got.TotalUSD, wantTotalUSD)
	}
	if !floatEq(sum.USD, wantTotalUSD) {
		t.Errorf("run-level USD: got %v, want %v", sum.USD, wantTotalUSD)
	}
	for _, r := range got.Rounds {
		want := criticUSD
		if r.Role == "proposer" {
			want = proposerUSD
		}
		if !floatEq(r.USD, want) {
			t.Errorf("round %d (%s) USD: got %v, want %v", r.Round, r.Role, r.USD, want)
		}
	}
	statsBytes, err := os.ReadFile(filepath.Join(sess.Root, "forks", "critic-1", "stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	var on map[string]any
	if err := json.Unmarshal(statsBytes, &on); err != nil {
		t.Fatalf("stats.json invalid JSON: %v", err)
	}
	if on["schema"] != "agon.fork-stats.v0" {
		t.Errorf("stats.json schema: %v", on["schema"])
	}
	if on["topic"] != "security" {
		t.Errorf("stats.json topic: %v", on["topic"])
	}
	usageJSON, ok := on["usage"].(map[string]any)
	if !ok {
		t.Fatalf("stats.json usage block missing or wrong shape: %v", on["usage"])
	}
	for _, k := range []string{"critic_usd", "proposer_usd", "total_usd"} {
		if _, ok := usageJSON[k]; !ok {
			t.Errorf("stats.json usage missing %q", k)
		}
	}
	if v, _ := usageJSON["total_usd"].(float64); !floatEq(v, wantTotalUSD) {
		t.Errorf("stats.json total_usd: got %v, want %v", v, wantTotalUSD)
	}
}

func floatEq(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

type usageCritic struct {
	rounds []string
	idx    int
	usage  agent.TokenUsage
	usd    float64
}

func (s *usageCritic) Round(_ context.Context, _ agent.CriticInput) (*agent.CriticResult, error) {
	if s.idx >= len(s.rounds) {
		return &agent.CriticResult{
			Markdown: "# Critic 1 - round 99 attacks\n\naspect: security\n",
			Duration: time.Millisecond, Usage: s.usage, USD: s.usd, Tokens: s.usage.Input + s.usage.Output,
		}, nil
	}
	out := &agent.CriticResult{
		Markdown: s.rounds[s.idx], Duration: time.Millisecond,
		Usage: s.usage, USD: s.usd, Tokens: s.usage.Input + s.usage.Output,
	}
	s.idx++
	return out, nil
}

// TestEngineStyledProgressEmitsANSI asserts that with Styled=true,
// progress lines carry ANSI escapes around the [agon] prefix and
// role words, while plain mode (Styled=false) leaves them alone.
// The test pins this at the engine layer because callers gate Styled
// on stderr-TTY: a regression that ships ANSI to a piped log would
// corrupt downstream tooling.
func TestEngineStyledProgressEmitsANSI(t *testing.T) {
	for _, tc := range []struct {
		name        string
		styled      bool
		wantEscapes bool
	}{
		{"styled", true, true},
		{"plain", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess, err := state.NewSession(t.TempDir(), 1, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			r1 := "# Critic 1 - round 1 attacks\n\naspect: security\n\n## c1-1 [x.go:1]\n\nclaim: leaks token\n\nexpected violation: panic\n\nreproduction:\n```\ngo test\n```\n"
			var buf strings.Builder
			e := &Engine{
				Sess: sess, Cwd: t.TempDir(),
				ForkCount: 1,
				Proposer: &stubProposer{
					first: func(string) (*agent.ProposerResult, error) {
						return &agent.ProposerResult{ForkID: "f", Response: "rebut c1-1: ok", Tokens: 10}, nil
					},
					next: func(string) (*agent.ProposerResult, error) { return nil, nil },
				},
				NewCritic: func(_ int) agent.Critic { return &stubCritic{rounds: []string{r1}} },
				MaxRounds: 2, CostCap: 1_000_000, TaskContext: "task", DiffPatch: "diff",
				Progress:          &buf,
				HeartbeatInterval: -1,
				Styled:            tc.styled,
			}
			if _, err := e.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			has := strings.Contains(out, "\x1b[")
			if has != tc.wantEscapes {
				t.Errorf("Styled=%v: ANSI present=%v, want %v. output:\n%q", tc.styled, has, tc.wantEscapes, out)
			}
			if tc.styled {
				// Specific decorations we want to see.
				for _, want := range []string{
					ansi.Bold + ansi.Cyan + "[agon]" + ansi.Reset,
					roleCriticColor + "critic" + ansi.Reset,
					roleProposerCol + "proposer" + ansi.Reset,
				} {
					if !strings.Contains(out, want) {
						t.Errorf("styled output missing %q", want)
					}
				}
			}
		})
	}
}

// TestEngineHeartbeatDuringSlowAgent asserts that while an agent
// call is in flight, the engine emits "still running, Ns elapsed"
// progress lines on the configured interval. The bug this rules out
// is the silence the user complained about: a 30s critic call with
// nothing on stderr until it finished, which reads as "stuck".
func TestEngineHeartbeatDuringSlowAgent(t *testing.T) {
	sess, err := state.NewSession(t.TempDir(), 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r1 := "# Critic 1 - round 1 attacks\n\naspect: security\n\n## c1-1 [x.go:1]\n\nclaim: leaks token\n\nexpected violation: panic at runtime\n\nreproduction:\n```\ngo test\n```\n"

	var (
		mu  sync.Mutex
		buf strings.Builder
	)
	w := &lockedWriter{w: &buf, mu: &mu}

	slow := &slowCritic{
		rounds: []string{r1},
		delay:  300 * time.Millisecond,
	}
	e := &Engine{
		Sess: sess, Cwd: t.TempDir(),
		ForkCount: 1,
		Proposer: &stubProposer{
			first: func(string) (*agent.ProposerResult, error) {
				return &agent.ProposerResult{ForkID: "f", Response: "rebut c1-1: ok", Tokens: 10}, nil
			},
			next: func(string) (*agent.ProposerResult, error) {
				return &agent.ProposerResult{ForkID: "f", Response: "ok", Tokens: 10}, nil
			},
		},
		NewCritic: func(_ int) agent.Critic { return slow },
		// MaxRounds=2 = one critic + one proposer = enough to exercise
		// the slow-call heartbeat once and then exit cleanly.
		MaxRounds: 2, CostCap: 1_000_000, TaskContext: "task", DiffPatch: "diff",
		Progress:          w,
		HeartbeatInterval: 80 * time.Millisecond,
	}
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	out := buf.String()
	mu.Unlock()

	// At 80ms ticks for a 300ms call we should see at least 2 ticks.
	n := strings.Count(out, "still running")
	if n < 2 {
		t.Errorf("expected ≥2 'still running' lines from a 300ms critic at 80ms tick, got %d. output:\n%s", n, out)
	}
	// The heartbeat should be cancelled the moment the call returns:
	// no heartbeat line may appear AFTER the "done in" line for the
	// same role/turn (otherwise we have a goroutine leak).
	doneIdx := strings.Index(out, " critic done in ")
	if doneIdx < 0 {
		t.Fatalf("no 'critic done in' marker in output:\n%s", out)
	}
	tail := out[doneIdx:]
	if strings.Contains(tail, "T1 critic: still running") {
		t.Errorf("heartbeat fired AFTER 'critic done' - goroutine not cancelled. tail:\n%s", tail)
	}
}

// TestEngineInterruptDuringAgentCall asserts that cancelling the parent
// context while an agent call is in flight ends the run as TermInterrupted
// (not ErrAgentFatal) so the finalize/summary path still runs and the
// already-completed work is persisted rather than discarded. Without the
// fix, runFork wraps the cancellation error as ErrAgentFatal and Run
// returns (nil, err), skipping finalize entirely.
func TestEngineInterruptDuringAgentCall(t *testing.T) {
	sess, err := state.NewSession(t.TempDir(), 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r1 := "# Critic 1 - round 1 attacks\n\naspect: security\n\n## c1-1 [x.go:1]\n\nclaim: leaks token\n\nexpected violation: panic at runtime\n\nreproduction:\n```\ngo test\n```\n"

	ctx, cancel := context.WithCancel(context.Background())
	// A slow critic that blocks long enough for us to cancel mid-call;
	// slowCritic returns ctx.Err() when the context is cancelled.
	slow := &slowCritic{rounds: []string{r1}, delay: 5 * time.Second}
	e := &Engine{
		Sess: sess, Cwd: t.TempDir(),
		ForkCount: 1,
		Proposer: &stubProposer{
			first: func(string) (*agent.ProposerResult, error) {
				return &agent.ProposerResult{ForkID: "f", Response: "rebut c1-1: ok", Tokens: 10}, nil
			},
			next: func(string) (*agent.ProposerResult, error) { return nil, nil },
		},
		NewCritic: func(_ int) agent.Critic { return slow },
		MaxRounds: 6, CostCap: 1_000_000, TaskContext: "task", DiffPatch: "diff",
		HeartbeatInterval: -1,
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	sum, err := e.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned error on interrupt, want nil (finalize must run): %v", err)
	}
	if sum == nil {
		t.Fatal("Run returned nil summary on interrupt; finalize was skipped")
	}
	if sum.Termination != TermInterrupted {
		t.Errorf("termination: got %s, want interrupted", sum.Termination)
	}
}

// TestEngineHeartbeatDisabledWhenNegativeInterval pins the escape
// hatch: callers that want to silence the heartbeat (without nilling
// Progress) can pass a negative interval.
func TestEngineHeartbeatDisabledWhenNegativeInterval(t *testing.T) {
	sess, err := state.NewSession(t.TempDir(), 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r1 := "# Critic 1 - round 1 attacks\n\naspect: security\n\n## c1-1 [x.go:1]\n\nclaim: leaks token\n\nexpected violation: panic\n\nreproduction:\n```\ngo test\n```\n"

	var buf strings.Builder
	slow := &slowCritic{rounds: []string{r1}, delay: 200 * time.Millisecond}
	e := &Engine{
		Sess: sess, Cwd: t.TempDir(),
		ForkCount: 1,
		Proposer: &stubProposer{
			first: func(string) (*agent.ProposerResult, error) {
				return &agent.ProposerResult{ForkID: "f", Response: "rebut c1-1: ok", Tokens: 10}, nil
			},
			next: func(string) (*agent.ProposerResult, error) { return nil, nil },
		},
		NewCritic: func(_ int) agent.Critic { return slow },
		MaxRounds: 2, CostCap: 1_000_000, TaskContext: "task", DiffPatch: "diff",
		Progress:          &buf,
		HeartbeatInterval: -1,
	}
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "still running") {
		t.Errorf("negative interval should disable heartbeat, but found 'still running' in:\n%s", buf.String())
	}
}

// overlapWriter flags whether two Write calls are ever in flight at the
// same time. Each Write holds the "active" count high for a fixed window
// so a concurrent write reliably overlaps it.
type overlapWriter struct {
	active int32
	raced  int32
}

func (w *overlapWriter) Write(p []byte) (int, error) {
	if atomic.AddInt32(&w.active, 1) > 1 {
		atomic.StoreInt32(&w.raced, 1)
	}
	time.Sleep(50 * time.Millisecond)
	atomic.AddInt32(&w.active, -1)
	return len(p), nil
}

// TestHeartbeatStopWaitsForGoroutine asserts that the heartbeat stop()
// does not return until the heartbeat goroutine has stopped writing, so
// the main loop's next write to e.Progress never overlaps an in-flight
// heartbeat write. Before the fix stop() only closed the done channel
// and returned immediately, racing the goroutine on a bare os.Stderr.
func TestHeartbeatStopWaitsForGoroutine(t *testing.T) {
	w := &overlapWriter{}
	e := &Engine{Progress: w, HeartbeatInterval: 10 * time.Millisecond}
	stop := e.startHeartbeat(time.Now(), "[agon] test")
	// Let the heartbeat fire and enter an in-flight Write (started at the
	// 10ms tick, held active until 60ms).
	time.Sleep(40 * time.Millisecond)
	stop()
	// stop() must have waited for the in-flight write to finish; writing
	// now must not overlap the heartbeat goroutine.
	e.progf("[agon] test: done")
	if atomic.LoadInt32(&w.raced) != 0 {
		t.Error("main-loop write overlapped an in-flight heartbeat write; stop() did not wait")
	}
}

// slowCritic blocks for delay before returning, so tests can probe
// the heartbeat path without hitting a real subprocess.
type slowCritic struct {
	rounds []string
	idx    int
	delay  time.Duration
}

func (s *slowCritic) Round(ctx context.Context, _ agent.CriticInput) (*agent.CriticResult, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	md := "# Critic 1 - round 99 attacks\n\naspect: security\n"
	if s.idx < len(s.rounds) {
		md = s.rounds[s.idx]
	}
	s.idx++
	return &agent.CriticResult{Markdown: md, Duration: s.delay}, nil
}

// lockedWriter serializes writes from the heartbeat goroutine with
// the main loop's writes; without it the test's Read of buf races
// the heartbeat's writes.
type lockedWriter struct {
	w  *strings.Builder
	mu *sync.Mutex
}

func (lw *lockedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.w.Write(p)
}

// TestTurnOf locks in the round-to-turn mapping. Because a user-facing
// turn re-interprets to pairs, R1+R2 must collapse to T1, R3+R4 to T2,
// etc. A regression here would silently double or halve what users think
// they're paying for.
func TestTurnOf(t *testing.T) {
	cases := []struct{ round, turn int }{
		{1, 1},
		{2, 1},
		{3, 2},
		{4, 2},
		{5, 3},
		{6, 3},
		{7, 4},
		{100, 50},
	}
	for _, c := range cases {
		if got := turnOf(c.round); got != c.turn {
			t.Errorf("turnOf(R%d): got T%d, want T%d", c.round, got, c.turn)
		}
	}
}

// TestEngineProgressUsesTurnLabel asserts the user-facing label is
// T<turn>, not R<round>. Was a UX complaint: with R1=critic and
// R2=proposer, a turn count of 3 reads to a user as "three messages"
// when it actually meant three rounds = 1.5 exchanges.
func TestEngineProgressUsesTurnLabel(t *testing.T) {
	sess, err := state.NewSession(t.TempDir(), 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r1 := "# Critic 1 - round 1 attacks\n\naspect: security\n\n## c1-1 [x.go:1]\n\nclaim: leaks token\n\nexpected violation: panic at runtime\n\nreproduction:\n```\ngo test\n```\n"
	r3 := "# Critic 1 - round 3 attacks\n\naspect: security\n\n## c1-1 [x.go:1] (re-attack)\n\nclaim: still leaks\n\nexpected violation: panic\n\nreproduction:\n```\ngo test\n```\n"

	var buf strings.Builder
	e := &Engine{
		Sess: sess, Cwd: t.TempDir(),
		ForkCount: 1,
		Proposer: &stubProposer{
			first: func(string) (*agent.ProposerResult, error) {
				return &agent.ProposerResult{ForkID: "f", Response: "rebut c1-1: ok", Tokens: 10}, nil
			},
			next: func(string) (*agent.ProposerResult, error) {
				return &agent.ProposerResult{ForkID: "f", Response: "rebut c1-1: ok", Tokens: 10}, nil
			},
		},
		NewCritic: func(_ int) agent.Critic {
			return &stubCritic{rounds: []string{r1, r3}}
		},
		// MaxRounds=4 is exactly 2 pairs (2 turns in user terms).
		MaxRounds: 4, CostCap: 100000, TaskContext: "task", DiffPatch: "diff",
		Progress: &buf,
	}
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"T1 critic running",
		"T1 critic done",
		"T1 proposer running",
		"T1 proposer done",
		"T2 critic running",
		"T2 proposer running",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("progress missing %q. full output:\n%s", want, out)
		}
	}
	// And: no stray R<n> labels in done/running lines (they were the
	// old shape).
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, " running...") && !strings.Contains(line, " done in ") {
			continue
		}
		if strings.Contains(line, " R1 ") || strings.Contains(line, " R2 ") ||
			strings.Contains(line, " R3 ") || strings.Contains(line, " R4 ") {
			t.Errorf("progress line still uses R<n> label: %s", line)
		}
	}
}

// TestEngineProgressIncludesPerRoundTokens captures one of the UX
// asks: every "R<n> {role} done" progress line must show the per-call
// in/out/cache_create/cache_read so an operator can see the cache
// ramp-up live, not only on the terminated summary line. The test
// pipes the engine's Progress writer to a buffer and greps for the
// expected suffixes round by round.
func TestEngineProgressIncludesPerRoundTokens(t *testing.T) {
	sess, err := state.NewSession(t.TempDir(), 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r1 := "# Critic 1 - round 1 attacks\n\naspect: security\n\n## c1-1 [x.go:1]\n\nclaim: leaks token\n\nexpected violation: panic at runtime\n\nreproduction:\n```\ngo test\n```\n"
	r3 := "# Critic 1 - round 3 attacks\n\naspect: security\n"
	r5 := "# Critic 1 - round 5 attacks\n\naspect: security\n"
	cu := agent.TokenUsage{Input: 111, Output: 22, CacheCreate: 333, CacheRead: 4444}
	pu := agent.TokenUsage{Input: 55, Output: 66, CacheCreate: 0, CacheRead: 7777}

	var buf strings.Builder
	e := &Engine{
		Sess: sess, Cwd: t.TempDir(),
		ForkCount: 1,
		Proposer: &stubProposer{
			first: func(string) (*agent.ProposerResult, error) {
				return &agent.ProposerResult{ForkID: "fork-1", Response: "rebut c1-1: ok", Tokens: pu.Input + pu.Output, Usage: pu, USD: 0.0080}, nil
			},
			next: func(string) (*agent.ProposerResult, error) {
				return &agent.ProposerResult{ForkID: "fork-1", Response: "no further action", Tokens: pu.Input + pu.Output, Usage: pu, USD: 0.0080}, nil
			},
		},
		NewCritic: func(_ int) agent.Critic {
			return &usageCritic{rounds: []string{r1, r3, r5}, usage: cu, usd: 0.0125}
		},
		MaxRounds: 6, CostCap: 1_000_000, TaskContext: "task", DiffPatch: "diff",
		Progress: &buf,
	}
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// Every "done" line must carry token counts.
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, " done in ") {
			continue
		}
		for _, want := range []string{"in=", "out=", "cache_create=", "cache_read="} {
			if !strings.Contains(line, want) {
				t.Errorf("done line missing %q: %s", want, line)
			}
		}
	}

	// Critic-side and proposer-side numbers are distinct, so each
	// must appear at least once with its own values.
	wantCritic := fmt.Sprintf("in=%d out=%d cache_create=%d cache_read=%d cost=$0.0125",
		cu.Input, cu.Output, cu.CacheCreate, cu.CacheRead)
	if !strings.Contains(out, wantCritic) {
		t.Errorf("critic progress missing %q. full output:\n%s", wantCritic, out)
	}
	wantProposer := fmt.Sprintf("in=%d out=%d cache_create=%d cache_read=%d cost=$0.0080",
		pu.Input, pu.Output, pu.CacheCreate, pu.CacheRead)
	if !strings.Contains(out, wantProposer) {
		t.Errorf("proposer progress missing %q. full output:\n%s", wantProposer, out)
	}
}

// TestFmtUsageOmitsCostWhenZero pins down the formatting branch a
// codex critic or the e2e mock takes (no total_cost_usd reported).
// Without this, progress lines show cost=$0.0000 which misreads as
// "this run was free" rather than "the agent did not surface it".
func TestFmtUsageOmitsCostWhenZero(t *testing.T) {
	u := agent.TokenUsage{Input: 1, Output: 2, CacheCreate: 3, CacheRead: 4}
	if got := fmtUsage(u, 0); strings.Contains(got, "cost=") {
		t.Errorf("zero usd should not surface cost field: %s", got)
	}
	if got := fmtUsage(u, 0.05); !strings.Contains(got, "cost=$0.0500") {
		t.Errorf("nonzero usd should surface cost field: %s", got)
	}
}

// TestEnginePromptCachingPerFork verifies that each fork's recorded
// usage actually shows prompt caching at work: R1 of every fork creates
// cache (CacheCreate>0, CacheRead=0) and later rounds read from cache
// (CacheRead>0). The orchestrator does not control the cache (the agent
// CLI does), so the assertion here is that the fork-level accounting
// preserves the per-call cache_creation_input_tokens /
// cache_read_input_tokens that the agents reported - and does so
// independently for every fork.
func TestEnginePromptCachingPerFork(t *testing.T) {
	sess, err := state.NewSession(t.TempDir(), 2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r1 := "# Critic 1 - round 1 attacks\n\naspect: security\n\n## c1-1 [x.go:1]\n\nclaim: leaks token\n\nexpected violation: panic at runtime\n\nreproduction:\n```\ngo test\n```\n"
	r3 := "# Critic 1 - round 3 attacks\n\naspect: security\n"
	r5 := "# Critic 1 - round 5 attacks\n\naspect: security\n"

	// Per-fork sequence the stubs replay. R1 critic primes the cache;
	// R3/R5 critic and R4 proposer hit it. R2 proposer (FirstRound)
	// primes the proposer-side cache.
	criticUsages := []agent.TokenUsage{
		{Input: 200, Output: 100, CacheCreate: 5000, CacheRead: 0}, // R1
		{Input: 50, Output: 80, CacheCreate: 100, CacheRead: 5000}, // R3
		{Input: 30, Output: 50, CacheCreate: 0, CacheRead: 5100},   // R5
	}
	proposerUsages := []agent.TokenUsage{
		{Input: 100, Output: 200, CacheCreate: 4000, CacheRead: 0},  // R2
		{Input: 50, Output: 100, CacheCreate: 200, CacheRead: 4000}, // R4
	}

	prop := &cachingProposer{usages: proposerUsages}
	e := &Engine{
		Sess: sess, Cwd: t.TempDir(),
		ForkCount: 2,
		Proposer:  prop,
		NewCritic: func(_ int) agent.Critic {
			return &cachingCritic{rounds: []string{r1, r3, r5}, usages: criticUsages}
		},
		MaxRounds: 6, CostCap: 1_000_000, TaskContext: "task", DiffPatch: "diff",
	}

	sum, err := e.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Forks) != 2 {
		t.Fatalf("forks: got %d, want 2", len(sum.Forks))
	}

	for _, f := range sum.Forks {
		fu := f.Usage
		// Critic side: at least one round read from cache.
		if fu.Critic.CacheRead == 0 {
			t.Errorf("fork %d: critic CacheRead = 0; prompt caching not observed on critic side", f.Index)
		}
		if fu.Critic.CacheCreate == 0 {
			t.Errorf("fork %d: critic CacheCreate = 0; cache never primed", f.Index)
		}
		// Proposer side: --resume reuses the conversation, so R4 must
		// see cache reads even though R2 only created cache.
		if fu.Proposer.CacheRead == 0 {
			t.Errorf("fork %d: proposer CacheRead = 0; --resume cache not seen", f.Index)
		}
		if fu.Proposer.CacheCreate == 0 {
			t.Errorf("fork %d: proposer CacheCreate = 0; --resume never primed cache", f.Index)
		}
		// A working cache should be paying for itself: cache reads
		// should outweigh fresh inputs across the fork.
		if fu.Total.CacheRead <= fu.Total.Input {
			t.Errorf("fork %d: cache_read=%d <= input=%d; cache not amortising input cost",
				f.Index, fu.Total.CacheRead, fu.Total.Input)
		}
		// Per-round assertions: R1 must NOT yet show cache reads (the
		// cache it creates is what later rounds consume). At least one
		// critic and one proposer round after R1 must show CacheRead>0.
		var sawR1 bool
		var sawCriticHit, sawProposerHit bool
		for _, r := range fu.Rounds {
			if r.Round == 1 {
				sawR1 = true
				if r.Usage.CacheRead != 0 {
					t.Errorf("fork %d R1: CacheRead=%d, want 0 (R1 should only create cache)",
						f.Index, r.Usage.CacheRead)
				}
				if r.Usage.CacheCreate == 0 {
					t.Errorf("fork %d R1: CacheCreate=0, want >0 (R1 should prime cache)", f.Index)
				}
				continue
			}
			if r.Usage.CacheRead == 0 {
				continue
			}
			switch r.Role {
			case "critic":
				sawCriticHit = true
			case "proposer":
				sawProposerHit = true
			}
		}
		if !sawR1 {
			t.Errorf("fork %d: missing R1 in per-round breakdown", f.Index)
		}
		if !sawCriticHit {
			t.Errorf("fork %d: no critic round after R1 shows CacheRead>0", f.Index)
		}
		if !sawProposerHit {
			t.Errorf("fork %d: no proposer round after R1 shows CacheRead>0", f.Index)
		}
	}

	// Cross-fork independence: forks must not share state, so each fork
	// reports its own cache primer in R1 (one CacheCreate event per
	// fork on the critic side, not one for the whole run).
	if sum.Forks[0].Usage.Critic.CacheCreate != sum.Forks[1].Usage.Critic.CacheCreate {
		t.Errorf("per-fork critic CacheCreate diverged: f1=%d f2=%d (forks should be independent)",
			sum.Forks[0].Usage.Critic.CacheCreate, sum.Forks[1].Usage.Critic.CacheCreate)
	}
}

// cachingCritic replays a per-round sequence of TokenUsage values so a
// test can simulate the cache-create / cache-read transition that a
// real claude critic produces across rounds.
type cachingCritic struct {
	rounds []string
	usages []agent.TokenUsage
	idx    int
}

func (s *cachingCritic) Round(_ context.Context, _ agent.CriticInput) (*agent.CriticResult, error) {
	var u agent.TokenUsage
	if s.idx < len(s.usages) {
		u = s.usages[s.idx]
	}
	md := "# Critic 1 - round 99 attacks\n\naspect: security\n"
	if s.idx < len(s.rounds) {
		md = s.rounds[s.idx]
	}
	s.idx++
	return &agent.CriticResult{
		Markdown: md, Duration: time.Millisecond,
		Usage: u, Tokens: u.Input + u.Output,
	}, nil
}

// cachingProposer replays per-fork proposer usage. FirstRound resets
// the per-fork counter so each fork sees the same R2/R4 sequence.
type cachingProposer struct {
	usages []agent.TokenUsage
	idx    int
}

func (s *cachingProposer) FirstRound(_ context.Context, _ string) (*agent.ProposerResult, error) {
	s.idx = 0
	return s.next()
}

func (s *cachingProposer) NextRound(_ context.Context, _ string, _ string) (*agent.ProposerResult, error) {
	return s.next()
}

func (s *cachingProposer) next() (*agent.ProposerResult, error) {
	var u agent.TokenUsage
	if s.idx < len(s.usages) {
		u = s.usages[s.idx]
	}
	s.idx++
	return &agent.ProposerResult{
		ForkID: "fork", Response: "rebut c1-1: ok",
		Tokens: u.Input + u.Output, Usage: u,
	}, nil
}

// TestEngineProposerChangedFilesPopulated proves the engine attributes
// the files a proposer round edits in cwd to the conceded attack's
// ConcessionFiles. Before the fix the proposer never reported changed
// files, so ConcessionFiles was always empty.
func TestEngineProposerChangedFilesPopulated(t *testing.T) {
	cwd := t.TempDir()
	gitInit(t, cwd)

	sess, err := state.NewSession(t.TempDir(), 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r1 := "# Critic 1 - round 1 attacks\n\naspect: security\n\n## c1-1 [x.go:1]\n\nclaim: leaks token\n\nexpected violation: panic at runtime\n\nreproduction:\n```\ngo test\n```\n"
	r3 := "# Critic 1 - round 3 attacks\n\naspect: security\n"
	r5 := "# Critic 1 - round 5 attacks\n\naspect: security\n"

	e := &Engine{
		Sess: sess, Cwd: cwd,
		ForkCount: 1,
		Proposer: &stubProposer{
			first: func(_ string) (*agent.ProposerResult, error) {
				// Simulate the proposer fixing the attack by editing a file
				// in cwd, then conceding.
				if werr := os.WriteFile(filepath.Join(cwd, "fix.go"), []byte("package x\n"), 0o644); werr != nil {
					return nil, werr
				}
				return &agent.ProposerResult{ForkID: "fork-1", Response: "concede c1-1: fixed the leak", Tokens: 10}, nil
			},
			next: func(_ string) (*agent.ProposerResult, error) {
				return &agent.ProposerResult{ForkID: "fork-1", Response: "no further action", Tokens: 10}, nil
			},
		},
		NewCritic: func(_ int) agent.Critic { return &stubCritic{rounds: []string{r1, r3, r5}} },
		MaxRounds: 6, CostCap: 100000, TaskContext: "task", DiffPatch: "diff",
	}
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	agg, err := ledger.Aggregate(sess)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := agg["c1-1"]
	if !ok {
		t.Fatalf("attack c1-1 not in ledger: %v", agg)
	}
	if rec.Status != ledger.StatusConceded {
		t.Fatalf("status: got %s, want conceded", rec.Status)
	}
	found := false
	for _, f := range rec.ConcessionFiles {
		if strings.Contains(f, "fix.go") {
			found = true
		}
	}
	if !found {
		t.Errorf("ConcessionFiles missing fix.go: %v", rec.ConcessionFiles)
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

func TestDefenseLineParsing(t *testing.T) {
	out := defenseLineRE.FindAllStringSubmatch("here is text\nconcede c1-1\nrebut c1-2\n", -1)
	if len(out) != 2 {
		t.Errorf("got %d matches, want 2", len(out))
	}
	if out[0][1] != "concede" || out[0][2] != "c1-1" {
		t.Errorf("first: %v", out[0])
	}
	if !strings.HasPrefix(out[1][2], "c1-") {
		t.Errorf("second: %v", out[1])
	}
}

func TestFmtDur(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{500 * time.Millisecond, "500ms"},
		{999 * time.Millisecond, "999ms"},
		{1 * time.Second, "1.0s"},
		{1500 * time.Millisecond, "1.5s"},
		{15 * time.Second, "15.0s"},
	}
	for _, c := range cases {
		if got := fmtDur(c.in); got != c.want {
			t.Errorf("fmtDur(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
