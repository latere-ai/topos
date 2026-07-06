package adversarial

import (
	"context"
	"time"

	"latere.ai/x/topos/adversarial/internal/agent"
	"latere.ai/x/topos/adversarial/internal/critic"
	"latere.ai/x/topos/adversarial/internal/ledger"
	"latere.ai/x/topos/adversarial/internal/round"
	"latere.ai/x/topos/adversarial/internal/state"
	"latere.ai/x/topos/adversarial/internal/summary"
)

// Engine orchestrates the multi-fork adversarial debate. The caller
// supplies Proposer and NewCritic; all orchestration, state persistence,
// and ledger bookkeeping are handled internally.
//
// StateDir is the directory where .agon/sessions/<id>/ will be created.
// Set it to the worktree root or any writable path; the directory is
// created if it does not exist.
type Engine struct {
	StateDir    string        // parent of sessions/<id>/
	Cwd         string        // working directory for agent subprocess calls
	ForkCount   int           // number of independent critic forks to run
	Proposer    Proposer      // drives the implementation agent
	NewCritic   CriticFactory // creates a critic for each fork
	MaxRounds   int           // per-fork internal-round cap (1 turn = 2 rounds)
	CostCap     int           // soft token budget across all forks
	TaskContext string        // verbatim task description
	DiffPatch   string        // unified diff to review
}

// Run executes all forks serially and returns a [Summary].
// The session directory is created under StateDir at the start of Run
// and its path is included in the returned Summary.
func (e *Engine) Run(ctx context.Context) (*Summary, error) {
	forkCount := e.ForkCount
	if forkCount < 1 {
		forkCount = 1
	}
	sess, err := state.NewSession(e.StateDir, forkCount, time.Now())
	if err != nil {
		return nil, err
	}

	eng := &round.Engine{
		Sess:      sess,
		Cwd:       e.Cwd,
		ForkCount: forkCount,
		Proposer:  &proposerBridge{p: e.Proposer},
		NewCritic: func(forkIdx int) agent.Critic {
			return &criticBridge{c: e.NewCritic(forkIdx)}
		},
		MaxRounds:   e.MaxRounds,
		CostCap:     e.CostCap,
		TaskContext: e.TaskContext,
		DiffPatch:   e.DiffPatch,
		// Progress and Styled left at zero values: no stderr progress
		// output from the library path. Callers that want progress
		// lines should wrap the Proposer/Critic implementations.
	}

	sumRes, err := eng.Run(ctx)
	if err != nil {
		return nil, err
	}

	// Compute headline from ledger aggregate.
	agg, _ := ledger.Aggregate(sess)
	records := make([]ledger.Record, 0, len(agg))
	for _, r := range agg {
		records = append(records, r)
	}
	headline := ""
	if h := summary.PickHeadline(records); h != nil {
		headline = h.Claim
	}

	// Persist the session's terminal artifacts (summary.md + end.json) so
	// embedders get a complete on-disk record: the token usage, termination,
	// and attack stats. Best-effort: a write failure must not discard a
	// completed debate. Without this the run leaves no end.json, so callers
	// could not tell a finished run from a running one and saw zero token
	// usage.
	_ = summary.Persist(sumRes, agg, 0)

	out := &Summary{
		Termination: string(sumRes.Termination),
		Unresolved:  sumRes.Unresolved,
		Headline:    headline,
		SessionDir:  sess.Root,
		USD:         sumRes.USD,
		WallSeconds: sumRes.WallSeconds,
	}
	for _, f := range sumRes.Forks {
		out.Forks = append(out.Forks, ForkOutcome{
			Index:  f.Index,
			Topic:  f.Topic,
			Rounds: f.Rounds,
		})
	}
	return out, nil
}

// proposerBridge adapts the public Proposer to internal/round.Proposer.
type proposerBridge struct{ p Proposer }

func (b *proposerBridge) FirstRound(ctx context.Context, pointer string) (*agent.ProposerResult, error) {
	res, err := b.p.FirstRound(ctx, pointer)
	if err != nil {
		return nil, err
	}
	return toInternalProposerResult(res), nil
}

func (b *proposerBridge) NextRound(ctx context.Context, forkID, pointer string) (*agent.ProposerResult, error) {
	res, err := b.p.NextRound(ctx, forkID, pointer)
	if err != nil {
		return nil, err
	}
	return toInternalProposerResult(res), nil
}

func toInternalProposerResult(r *ProposerResult) *agent.ProposerResult {
	return &agent.ProposerResult{
		ForkID:   r.ForkID,
		Response: r.Response,
		Tokens:   r.Usage.Total(),
		Usage:    toInternalTokenUsage(r.Usage),
		USD:      r.USD,
		Duration: r.Duration,
	}
}

// criticBridge adapts the public Critic to internal/agent.Critic.
type criticBridge struct{ c Critic }

func (b *criticBridge) Round(ctx context.Context, in agent.CriticInput) (*agent.CriticResult, error) {
	pubIn := CriticInput{
		AspectName:   in.Aspect.Name,
		SystemPrompt: in.SystemPrompt,
		CriticIndex:  in.CriticIndex,
		Round:        in.Round,
		TaskContext:  in.TaskContext,
		DiffPatch:    in.DiffPatch,
		Cwd:          in.Cwd,
		Deadline:     in.Deadline,
		Model:        in.Model,
	}
	for _, r := range in.PriorRoundFiles {
		pubIn.PriorRoundFiles = append(pubIn.PriorRoundFiles, RoundFileRef{
			Path: r.Path, Round: r.Round, Role: r.Role,
		})
	}
	res, err := b.c.Round(ctx, pubIn)
	if err != nil {
		return nil, err
	}
	return &agent.CriticResult{
		Markdown: res.Markdown,
		Tokens:   res.Tokens,
		Usage:    toInternalTokenUsage(res.Usage),
		USD:      res.USD,
		Duration: res.Duration,
	}, nil
}

func toInternalTokenUsage(u TokenUsage) agent.TokenUsage {
	return agent.TokenUsage{
		Input:       u.Input,
		Output:      u.Output,
		CacheCreate: u.CacheCreate,
		CacheRead:   u.CacheRead,
	}
}

// Aspect field access requires the internal critic package; the bridge
// compiles because adversarial is inside the latere.ai/x/topos module.
var _ = critic.Aspect{} // import guard — ensures critic is used
