package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ClaudeProposer drives the proposer-clone via claude --resume.
//
// Verbose toggles --output-format stream-json --verbose so each
// tool-use / thinking / text event the agent emits can be surfaced
// to EventOut as it arrives. The final result event still drives
// the returned ProposerResult; callers do not need to know which
// output format was used.
type ClaudeProposer struct {
	Bin      string
	Cwd      string
	RootID   string
	Model    string
	Deadline time.Duration
	Verbose  bool
	EventOut io.Writer

	// DisallowedTools, when non-empty, is passed to claude as
	// --disallowedTools so the proposer cannot use those tools. Callers
	// embedding adversarial as a verifier set this (read-only) to guarantee the
	// proposer argues and concedes but never edits the working tree it runs
	// in. Empty (the default) applies no tool restriction.
	DisallowedTools []string
}

// TokenUsage captures the per-call token breakdown reported by claude's
// `--output-format json`. Tokens reports a single billed-input total
// (Input + CacheCreate + CacheRead) plus Output. The fields stay
// separate so callers can show prompt-cache hit rate or estimate cost.
type TokenUsage struct {
	Input       int `json:"input_tokens"`
	Output      int `json:"output_tokens"`
	CacheCreate int `json:"cache_creation_input_tokens"`
	CacheRead   int `json:"cache_read_input_tokens"`
}

// Total returns the sum of every token bucket. Useful as a single
// cost-cap dial; for billing accuracy use the individual fields and the
// model's per-bucket price.
func (u TokenUsage) Total() int {
	return u.Input + u.Output + u.CacheCreate + u.CacheRead
}

// Add accumulates other into u in place.
func (u *TokenUsage) Add(other TokenUsage) {
	u.Input += other.Input
	u.Output += other.Output
	u.CacheCreate += other.CacheCreate
	u.CacheRead += other.CacheRead
}

// ProposerResult is one round's outcome.
type ProposerResult struct {
	ForkID       string
	Response     string
	Tokens       int
	Usage        TokenUsage
	USD          float64
	Stdout       []byte
	ChangedFiles []string
	Duration     time.Duration
}

// Typed errors per spec 17.
var (
	ErrCwdMismatch    = errors.New("claude --resume cwd mismatch")
	ErrAuth           = errors.New("claude auth failure")
	ErrTimeout        = errors.New("claude call timed out")
	ErrJSON           = errors.New("claude JSON parse failed")
	ErrEmptyResult    = errors.New("claude returned empty result")
	ErrUnexpectedFork = errors.New("claude session_id mismatch")
	ErrAgentError     = errors.New("claude reported is_error")
)

type claudeJSON struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	SessionID    string  `json:"session_id"`
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// decodeClaudeStreamResult scans stream-json stdout (one JSON object
// per line) for the final {"type":"result",...} event and decodes it
// into a claudeJSON. Returns the same shape as a single-shot
// --output-format json reply, so the rest of the parser does not
// need to know which mode was used.
func decodeClaudeStreamResult(stdout []byte) (claudeJSON, error) {
	var last claudeJSON
	var found bool
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := DecodeJSONLine(line, &probe); err != nil {
			continue
		}
		if probe.Type != "result" {
			continue
		}
		var parsed claudeJSON
		if err := DecodeJSONLine(line, &parsed); err != nil {
			continue
		}
		last = parsed
		found = true
	}
	if !found {
		return claudeJSON{}, fmt.Errorf("no result event in stream-json stdout (%d bytes)", len(stdout))
	}
	return last, nil
}

// usage extracts the typed TokenUsage from a parsed claude JSON
// document. Helper to keep callsites readable.
func (j *claudeJSON) usage() TokenUsage {
	return TokenUsage{
		Input:       j.Usage.InputTokens,
		Output:      j.Usage.OutputTokens,
		CacheCreate: j.Usage.CacheCreationInputTokens,
		CacheRead:   j.Usage.CacheReadInputTokens,
	}
}

// FirstRound creates a fork and processes R1 in one shot.
func (p *ClaudeProposer) FirstRound(ctx context.Context, pointer string) (*ProposerResult, error) {
	args := append([]string{"--resume", p.RootID, "--fork-session"}, p.outputArgs()...)
	args = append(args, p.toolArgs()...)
	args = append(args, "--print", pointer)
	if p.Model != "" {
		args = append(args, "--model", p.Model)
	}
	return p.run(ctx, args, "" /* expected fork id */)
}

// NextRound continues an existing fork.
func (p *ClaudeProposer) NextRound(ctx context.Context, forkID, pointer string) (*ProposerResult, error) {
	args := append([]string{"--resume", forkID}, p.outputArgs()...)
	args = append(args, p.toolArgs()...)
	args = append(args, "--print", pointer)
	if p.Model != "" {
		args = append(args, "--model", p.Model)
	}
	return p.run(ctx, args, forkID)
}

// outputArgs returns the --output-format args for the run, picking
// stream-json when Verbose so we can tap intermediate events.
func (p *ClaudeProposer) outputArgs() []string {
	if p.Verbose {
		return []string{"--output-format", "stream-json", "--verbose"}
	}
	return []string{"--output-format", "json"}
}

// toolArgs returns the --disallowedTools args when DisallowedTools is set, so
// the proposer cannot invoke those tools. Empty yields no args.
func (p *ClaudeProposer) toolArgs() []string {
	if len(p.DisallowedTools) == 0 {
		return nil
	}
	return []string{"--disallowedTools", strings.Join(p.DisallowedTools, ",")}
}

func (p *ClaudeProposer) run(ctx context.Context, args []string, expectFork string) (*ProposerResult, error) {
	bin := p.Bin
	if bin == "" {
		bin = "claude"
	}
	run := Run{
		Bin: bin, Args: args, Cwd: p.Cwd, Env: CleanEnv(), Deadline: p.Deadline,
	}
	if p.Verbose && p.EventOut != nil {
		run.OnStdoutLine = func(line []byte) {
			if msg := FormatClaudeStreamEvent(line); msg != "" {
				_, _ = fmt.Fprintln(p.EventOut, msg)
			}
		}
	}
	res, err := Exec(ctx, run)
	if err != nil {
		if res.Killed {
			return nil, fmt.Errorf("%w: %w", ErrTimeout, err)
		}
		stderr := string(res.Stderr)
		if strings.Contains(stderr, "No conversation found with session ID") {
			return nil, fmt.Errorf("%w: %s", ErrCwdMismatch, stderr)
		}
		if strings.Contains(stderr, "Authentication error") || strings.Contains(stderr, "401") {
			return nil, fmt.Errorf("%w: %s", ErrAuth, stderr)
		}
		return nil, fmt.Errorf("claude exec: %w (stderr=%q)", err, stderr)
	}

	var parsed claudeJSON
	if p.Verbose {
		parsed, err = decodeClaudeStreamResult(res.Stdout)
	} else {
		err = DecodeJSONLine(res.Stdout, &parsed)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrJSON, err)
	}
	if parsed.IsError {
		return nil, fmt.Errorf("%w: subtype=%q result=%q", ErrAgentError, parsed.Subtype, parsed.Result)
	}
	if parsed.Result == "" {
		return nil, ErrEmptyResult
	}
	if expectFork != "" && parsed.SessionID != expectFork {
		return nil, fmt.Errorf("%w: got %q, want %q", ErrUnexpectedFork, parsed.SessionID, expectFork)
	}
	use := parsed.usage()
	return &ProposerResult{
		ForkID:   parsed.SessionID,
		Response: parsed.Result,
		Tokens:   use.Input + use.Output,
		Usage:    use,
		USD:      parsed.TotalCostUSD,
		Stdout:   res.Stdout,
		Duration: res.Duration,
	}, nil
}
