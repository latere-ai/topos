// Package critic provides an [adversarial.Critic] backed by the topos runtime
// (latere.ai/x/topos).
//
// An embedder running inside the topos world (wallfacer, the agents platform)
// uses [NewCriticFactory] to run critic forks through the governed runtime:
// model routing via Lux or Direct, a topos sandbox (local or Cella), and a
// lineage record, instead of shelling out to local CLIs. The proposer stays on
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
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/sandbox"
)

// Config wires a topos-backed critic to a model and sandbox.
type Config struct {
	// Model selects the brain connection (Lux, Direct, or Fake). Ignored when
	// Brain is set.
	Model xtopos.ModelOptions
	// Sandbox is the execution backend; nil uses topos's local sandbox.
	Sandbox sandbox.Provider
	// Brain, when non-nil, overrides Model with a caller-supplied model. Tests
	// inject a scripted model here; production leaves it nil and sets Model.
	Brain models.Model
	// Tools is the agent's tool grant. nil (the default) grants no tools, which
	// is the read-only posture: topos's only builtin is bash, so withholding it
	// leaves the agent no way to execute or mutate the tree. The critic reasons
	// over the diff embedded in the prompt.
	Tools []string
}

// NewCriticFactory returns an [adversarial.CriticFactory] whose critics run one
// topos agent per round. forkIdx is threaded into the topos SessionID and the
// AgentSpec name so each fork is a distinct lineage node.
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
// not reported: topos's public RunResult exposes none (see spec 39 OQ-3).
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
		Brain:     c.cfg.Brain,
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
