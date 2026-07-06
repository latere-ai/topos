package adversarial

import "context"

// ReviewOptions configures a single adversarial review. It mirrors the [Engine]
// fields for the common case: a proposer and a critic factory debating a diff
// over Forks independent forks.
type ReviewOptions struct {
	// StateDir is required; the caller-chosen root under which the engine writes
	// sessions/<id>/. The engine invents no default, so an empty StateDir is a
	// caller error.
	StateDir    string
	Cwd         string        // working directory for agent calls
	Forks       int           // number of independent critic forks to run
	Proposer    Proposer      // drives the implementation agent
	NewCritic   CriticFactory // creates a critic for each fork
	MaxRounds   int           // per-fork internal-round cap (1 turn = 2 rounds)
	CostCap     int           // soft token budget across all forks
	TaskContext string        // verbatim task description
	DiffPatch   string        // unified diff to review
}

// Review runs an adversarial debate over opts.DiffPatch and returns the Summary.
// It is a thin convenience over [Engine] for the common single-call case; callers
// needing per-fork control use [Engine] directly.
func Review(ctx context.Context, opts ReviewOptions) (*Summary, error) {
	eng := &Engine{
		StateDir:    opts.StateDir,
		Cwd:         opts.Cwd,
		ForkCount:   opts.Forks,
		Proposer:    opts.Proposer,
		NewCritic:   opts.NewCritic,
		MaxRounds:   opts.MaxRounds,
		CostCap:     opts.CostCap,
		TaskContext: opts.TaskContext,
		DiffPatch:   opts.DiffPatch,
	}
	return eng.Run(ctx)
}
