package adversarial

import (
	"context"
	"time"
)

// Proposer drives the implementation agent across debate forks.
// FirstRound creates the fork; NextRound continues an existing one.
// pointer is the mediator's message directing the agent to review
// critic comments (the engine computes it from the session state).
type Proposer interface {
	FirstRound(ctx context.Context, pointer string) (*ProposerResult, error)
	NextRound(ctx context.Context, forkID, pointer string) (*ProposerResult, error)
}

// Critic drives one critic agent for one fork. The engine calls Round
// once per critic turn; the Critic is stateless across calls.
type Critic interface {
	Round(ctx context.Context, in CriticInput) (*CriticResult, error)
}

// CriticFactory creates a Critic for the given fork index (1-based).
type CriticFactory func(forkIdx int) Critic

// ProposerResult is one proposer round's outcome.
type ProposerResult struct {
	ForkID   string // fork session ID (set by FirstRound, reused by NextRound)
	Response string // agent's full markdown response
	Usage    TokenUsage
	USD      float64
	Duration time.Duration
}

// CriticInput is the per-round input the engine passes to a Critic.
// SystemPrompt already contains the fully assembled critic prompt; call
// [AssemblePrompt] to combine SystemPrompt, TaskContext, DiffPatch, and
// PriorRoundFiles into a single string for the underlying agent.
type CriticInput struct {
	AspectName      string         // critic's declared topic (e.g. "security")
	SystemPrompt    string         // aspect + round-contract system prompt
	CriticIndex     int            // 1-based fork index
	Round           int            // 1-based internal round number
	TaskContext     string         // verbatim task description
	DiffPatch       string         // unified diff of the artifact
	PriorRoundFiles []RoundFileRef // non-empty from round 3 onward
	Cwd             string         // working directory
	Deadline        time.Duration
	Model           string // override model; empty = default
}

// RoundFileRef points at a prior round's output file the critic should read.
type RoundFileRef struct {
	Path  string
	Round int
	Role  string // "critic" or "proposer"
}

// CriticResult is what a Critic produced for one round.
type CriticResult struct {
	Markdown string // raw markdown attack document
	Tokens   int    // total tokens (input + output)
	Usage    TokenUsage
	USD      float64
	Duration time.Duration
}

// TokenUsage is a per-call token breakdown.
type TokenUsage struct {
	Input       int
	Output      int
	CacheCreate int
	CacheRead   int
}

// Total returns the sum of all token buckets.
func (u TokenUsage) Total() int {
	return u.Input + u.Output + u.CacheCreate + u.CacheRead
}

// Summary is what [Engine.Run] returns on success.
type Summary struct {
	// Termination reason: "steady-state", "cost-cap", "max-turn",
	// "interrupted", or "malformed-output".
	Termination string
	Forks       []ForkOutcome
	Unresolved  int    // attacks not conceded or rebutted at run end
	Headline    string // claim text of highest-contention unresolved attack
	SessionDir  string // absolute path to the sessions/<id>/ folder
	USD         float64
	WallSeconds int
}

// ForkOutcome describes one fork's result.
type ForkOutcome struct {
	Index  int    // 1-based
	Topic  string // aspect the critic declared in R1
	Rounds int    // internal rounds executed
}

// Verifier is the top-level interface for adversarial post-run verification.
// It is the integration seam for tools that want to embed adversarial
// verification as a plugin step without assembling an Engine directly.
// Implementations return (nil, nil) to signal a skip: verification disabled,
// or the diff too trivial to debate.
type Verifier interface {
	Verify(ctx context.Context, in VerifyInput) (*VerifyResult, error)
}

// VerifyInput parameterizes one [Verifier.Verify] call.
type VerifyInput struct {
	TaskPrompt    string // task description or intent
	Criteria      string // acceptance criteria; empty means no bar
	SessionID     string // implementation-agent session ID (proposer path)
	DiffPatch     string // pre-computed git diff
	Cwd           string // working directory
	StateDir      string // where sessions/<id>/ is written
	ForkCount     int    // number of critic forks
	MaxRounds     int    // debate rounds per fork
	CostCapTokens int    // soft token budget
}

// VerifyResult is returned by a successful [Verifier.Verify] call.
// nil means the verifier was skipped (disabled or diff too small).
type VerifyResult struct {
	Unresolved int    // open attacks at run end
	Headline   string // markdown claim of highest-contention unresolved attack
	SessionDir string // absolute path to the session folder
	USD        float64
}

// AssemblePrompt builds the full prompt string to feed to an LLM for
// one critic round. Critic implementations call this in their Round
// method to convert CriticInput into a single string.
func AssemblePrompt(in CriticInput) string {
	return assemblePrompt(in)
}
