// Package critic provides an [adversarial.Critic] backed by the topos runtime
// (latere.ai/x/topos).
//
// An embedder running inside the topos world (wallfacer, the agents platform)
// uses [NewCriticFactory] to run critic forks through the governed runtime:
// model routing via Lux or Direct, a topos sandbox (local or Cella), and a
// trace record, instead of shelling out to local CLIs. The proposer stays on
// the claude CLI; see the claude backend package.
//
// Each round runs one topos agent over the assembled critic prompt (which
// already contains the diff) and returns the agent's text verbatim as
// [adversarial.CriticResult.Markdown]; the engine parses it like any other
// backend. Within the adversarial capability only this package imports the
// topos runtime, so the engine core ([adversarial]) and every other adversarial
// package stay free of that dependency (enforced by boundary_test.go).
package critic

import (
	"context"
	"fmt"

	xtopos "latere.ai/x/topos"
	adversarial "latere.ai/x/topos/adversarial"
	"latere.ai/x/topos/sandbox"
)

// Config wires a topos-backed critic to a model and sandbox.
type Config struct {
	// Model selects the model connection (Lux, Direct, or Fake). Tests set its
	// Client to a scripted model; production sets Kind and leaves Client nil.
	Model xtopos.ModelOptions
	// Sandbox is the execution backend; nil uses topos's local sandbox.
	Sandbox sandbox.Provider
	// Tools is the agent's tool grant, recorded on the run's trace node as
	// Grants. nil (the default) records no grant. It is an audit record, not a
	// sandbox: the runtime currently offers every agent tools.Builtins() (bash,
	// the file tools, and the search tools) whatever the grant says, so a nil
	// Tools does not by itself keep the critic from executing or mutating the
	// tree. Confine the Sandbox provider when that matters. The critic reasons
	// over the diff embedded in the prompt and needs no tool to do its job.
	Tools []string
}

// NewCriticFactory returns an [adversarial.CriticFactory] whose critics run one
// topos agent per round. forkIdx is threaded into the topos SessionID and the
// AgentSpec name so each fork is a distinct trace node.
func NewCriticFactory(cfg Config) adversarial.CriticFactory {
	return func(forkIdx int) adversarial.Critic {
		return &critic{cfg: cfg, forkIdx: forkIdx}
	}
}

type critic struct {
	cfg     Config
	forkIdx int
}

// Round runs the assembled critic prompt through a single-agent Pinned region
// and returns the agent's final text as CriticResult.Markdown. Token usage is
// not reported: topos's public RunResult exposes none.
func (c *critic) Round(ctx context.Context, in adversarial.CriticInput) (*adversarial.CriticResult, error) {
	// Match the subprocess critics, which bound each round by in.Deadline
	// (internal/agent.CodexCritic / ClaudeCritic pass it to the subprocess).
	if in.Deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, in.Deadline)
		defer cancel()
	}
	runner, err := xtopos.NewRunner(xtopos.Options{
		SessionID: fmt.Sprintf("adversarial-critic-%d-r%d", c.forkIdx, in.Round),
		Model:     c.cfg.Model,
		Sandbox:   c.cfg.Sandbox,
	})
	if err != nil {
		return nil, fmt.Errorf("topos critic: new runner: %w", err)
	}
	region := xtopos.Region{
		Autonomy: xtopos.Pinned,
		Entry: xtopos.AgentSpec{
			Name:  fmt.Sprintf("critic-%d", c.forkIdx),
			Role:  "critic",
			Tools: c.cfg.Tools,
		},
	}
	res, err := runner.Run(ctx, region, adversarial.AssemblePrompt(in))
	if err != nil {
		return nil, fmt.Errorf("topos critic: run: %w", err)
	}
	return &adversarial.CriticResult{Markdown: res.Final}, nil
}
