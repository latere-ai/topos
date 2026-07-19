package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"latere.ai/x/topos/adversarial/internal/critic"
)

// Critic is the interface every critic driver satisfies.
type Critic interface {
	Round(ctx context.Context, in CriticInput) (*CriticResult, error)
}

// CriticInput parameterizes one critic round.
type CriticInput struct {
	Aspect          critic.Aspect
	CriticIndex     int
	Round           int
	SystemPrompt    string
	TaskContext     string
	DiffPatch       string
	PriorRoundFiles []RoundFileRef
	Cwd             string
	Deadline        time.Duration
	Model           string
}

// RoundFileRef points at a prior round file for the critic to read.
type RoundFileRef struct {
	Path  string
	Round int
	Role  string
}

// CriticResult is one round's outcome.
type CriticResult struct {
	Markdown string
	ThreadID string
	Tokens   int
	Usage    TokenUsage
	USD      float64
	Stdout   []byte
	Duration time.Duration
}

// Typed errors per spec 18.
var (
	ErrRateLimit    = errors.New("agent reported rate limit")
	ErrTTYRequired  = errors.New("agent requires a TTY")
	ErrEmptyContent = errors.New("agent returned empty content")
)

// NewCritic returns a Critic for the named family. Panics on unknown.
func NewCritic(family string) Critic {
	switch family {
	case "codex":
		return &CodexCritic{}
	case "claude":
		return &ClaudeCritic{}
	default:
		panic("unknown critic family: " + family)
	}
}

// AssemblePrompt is the deterministic prompt a critic driver feeds to
// the agent: aspect system prompt + task + diff + pointers to prior
// rounds.
func AssemblePrompt(in CriticInput) string {
	var b strings.Builder
	b.WriteString(in.SystemPrompt)
	b.WriteString("\n\n")
	b.WriteString(directives)
	b.WriteString("\n\n# Task\n\n")
	b.WriteString(in.TaskContext)
	b.WriteString("\n\n# Diff\n\n```diff\n")
	b.WriteString(in.DiffPatch)
	b.WriteString("\n```\n")
	if len(in.PriorRoundFiles) > 0 {
		b.WriteString("\n# Prior rounds\n\n")
		for _, r := range in.PriorRoundFiles {
			fmt.Fprintf(&b, "- @%s - round %d %s\n", r.Path, r.Round, r.Role)
		}
	}
	return b.String()
}

// directives is appended to every critic system prompt to keep the
// agent on-task: emit ONLY the markdown document, no preamble, no tool
// calls, no thinking aloud.
const directives = `Critical output rules:
1. Your entire reply MUST be the markdown attack document and nothing
   else. No preamble like "I'll review this" or "Let me start by". No
   trailing summary. Just the document.
2. Do NOT run shell commands, search the file tree, or otherwise
   investigate beyond the diff, task context, and prior round files
   provided above. Reading the files listed under "# Prior rounds" is
   expected (round 3+ requires it); reading anything else is not.
   The reproduction in each attack is what proves the bug; you do not
   need to verify it with a tool.
3. If you decide there is nothing to attack, emit an empty document
   that still has the top header and "aspect:" line, then stop.
4. The very first non-blank line of your reply MUST be the top header
   "# Critic <i> - round <n> attacks".`

// CodexCritic invokes `codex exec --sandbox read-only --json`.
//
// Verbose + EventOut surface intermediate item.completed events
// (tool_call / command_execution) to the writer as they arrive.
// Codex already streams its events, so unlike ClaudeCritic this
// does not need to switch CLI flags - just opt into the per-line
// streaming path of agent.Exec and tee the events.
type CodexCritic struct {
	Bin      string
	Verbose  bool
	EventOut io.Writer
}

// Round runs one codex critic round.
func (c *CodexCritic) Round(ctx context.Context, in CriticInput) (*CriticResult, error) {
	bin := c.Bin
	if bin == "" {
		bin = "codex"
	}
	prompt := AssemblePrompt(in)
	args := []string{"exec", "--skip-git-repo-check", "--sandbox", "read-only", "--json", prompt}
	if in.Model != "" {
		args = append(args, "--model", in.Model)
	}
	run := Run{
		Bin: bin, Args: args, Cwd: in.Cwd, Env: CleanEnv(), Deadline: in.Deadline,
	}
	if c.Verbose && c.EventOut != nil {
		run.OnStdoutLine = func(line []byte) {
			if msg := FormatCodexStreamEvent(line); msg != "" {
				_, _ = fmt.Fprintln(c.EventOut, msg)
			}
		}
	}
	res, err := Exec(ctx, run)
	if err != nil {
		stderr := string(res.Stderr)
		switch {
		case res.Killed:
			return nil, fmt.Errorf("%w: %w", ErrTimeout, err)
		case strings.Contains(stderr, "rate limit"):
			return nil, fmt.Errorf("%w: %s", ErrRateLimit, stderr)
		case strings.Contains(stderr, "stdin is not a terminal"):
			return nil, fmt.Errorf("%w: %s", ErrTTYRequired, stderr)
		}
		return nil, fmt.Errorf("codex exec: %w (stderr=%q)", err, stderr)
	}

	var (
		out      strings.Builder
		threadID string
		usage    TokenUsage
		eventCt  int
	)
	visit := func(raw json.RawMessage) error {
		eventCt++
		// Codex's --json event shapes have evolved across releases. Accept:
		//   {"type":"thread.started","thread_id":"..."}
		//   {"type":"item.completed","item":{"type":"agent_message","content":"..."}}
		//   {"type":"agent_message","content":"..."}
		//   {"type":"agent_text","text":"..."}
		//   {"type":"task_complete","result":"..."}
		//   {"type":"message","message":{"content":[{"type":"text","text":"..."}]}}
		//   {"type":"turn.completed","usage":{"input_tokens":N,"cached_input_tokens":N,"output_tokens":N,"reasoning_output_tokens":N}}
		var ev struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Content  string `json:"content"`
			Text     string `json:"text"`
			Result   string `json:"result"`
			Item     struct {
				Type    string `json:"type"`
				Content string `json:"content"`
				Text    string `json:"text"`
			} `json:"item"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			Usage struct {
				InputTokens           int `json:"input_tokens"`
				CachedInputTokens     int `json:"cached_input_tokens"`
				OutputTokens          int `json:"output_tokens"`
				ReasoningOutputTokens int `json:"reasoning_output_tokens"`
				CacheReadInputTokens  int `json:"cache_read_input_tokens"`
				CacheCreationTokens   int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		// A line that is not a JSON event (evolving codex output shapes,
		// non-JSON noise) is skipped rather than aborting the whole stream.
		// Matches the tolerant json.Unmarshal checks below.
		if json.Unmarshal(raw, &ev) == nil {
			appendIfNonEmpty := func(s string) {
				if s == "" {
					return
				}
				if out.Len() > 0 {
					out.WriteString("\n")
				}
				out.WriteString(s)
			}
			switch ev.Type {
			case "thread.started", "task_started":
				if ev.ThreadID != "" {
					threadID = ev.ThreadID
				}
			case "turn.completed", "token_count":
				// Codex billing: input_tokens is the FULL prompt (including
				// the cached portion), so the fresh-input bucket equals
				// input_tokens - cached_input_tokens. reasoning_output is
				// model-generated tokens, fold into Output. CacheCreate is
				// not a concept openai surfaces here; cache_read maps to
				// our CacheRead. Some future codex revisions emit anthropic-
				// shaped fields - tolerate both.
				fresh := ev.Usage.InputTokens - ev.Usage.CachedInputTokens
				if fresh < 0 {
					fresh = ev.Usage.InputTokens
				}
				usage.Input += fresh
				usage.Output += ev.Usage.OutputTokens + ev.Usage.ReasoningOutputTokens
				usage.CacheRead += ev.Usage.CachedInputTokens + ev.Usage.CacheReadInputTokens
				usage.CacheCreate += ev.Usage.CacheCreationTokens
			case "item.completed":
				switch ev.Item.Type {
				case "agent_message", "agent_text":
					// Different codex releases use either content or text;
					// prefer the populated one.
					if ev.Item.Content != "" {
						appendIfNonEmpty(ev.Item.Content)
					} else {
						appendIfNonEmpty(ev.Item.Text)
					}
				}
			case "agent_message":
				appendIfNonEmpty(ev.Content)
			case "agent_text":
				appendIfNonEmpty(ev.Text)
			case "task_complete":
				appendIfNonEmpty(ev.Result)
			case "message":
				// {"message":{"content":[{"type":"text","text":"..."}, ...]}}
				var parts []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if json.Unmarshal(ev.Message.Content, &parts) == nil {
					for _, p := range parts {
						if p.Type == "text" {
							appendIfNonEmpty(p.Text)
						}
					}
				} else {
					// Plain string content shape.
					var s string
					if json.Unmarshal(ev.Message.Content, &s) == nil {
						appendIfNonEmpty(s)
					}
				}
			}
		}
		return nil
	}
	if err := StreamJSON(strings.NewReader(string(res.Stdout)), visit); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrJSON, err)
	}
	// Last-resort fallback: if no event-shape produced content but stdout
	// is non-empty and looks like markdown (no `{` at the start), treat
	// raw stdout as the critic's output. Lets us survive a codex CLI that
	// silently drops --json or emits an undocumented shape.
	if out.Len() == 0 && len(res.Stdout) > 0 {
		trimmed := strings.TrimSpace(string(res.Stdout))
		if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
			out.WriteString(trimmed)
		}
	}
	if out.Len() == 0 {
		preview := string(res.Stdout)
		if len(preview) > 500 {
			preview = preview[:500] + "...(truncated)"
		}
		return nil, fmt.Errorf("%w: %d events parsed, no content. stdout preview: %q. stderr: %q",
			ErrEmptyContent, eventCt, preview, string(res.Stderr))
	}
	return &CriticResult{
		Markdown: out.String(),
		ThreadID: threadID,
		Tokens:   usage.Input + usage.Output,
		Usage:    usage,
		Stdout:   res.Stdout,
		Duration: res.Duration,
	}, nil
}

// ClaudeCritic invokes a fresh `claude -p` per round (no --resume,
// no --fork-session - see spec 18).
//
// Verbose toggles --output-format stream-json --verbose. When set,
// each tool-use / thinking / text event is fed to EventOut as it
// arrives so an operator who enabled verbose streaming can see what
// the agent is doing without waiting for the full call to finish.
type ClaudeCritic struct {
	Bin      string
	Verbose  bool
	EventOut io.Writer
}

// Round runs one claude critic round.
func (c *ClaudeCritic) Round(ctx context.Context, in CriticInput) (*CriticResult, error) {
	bin := c.Bin
	if bin == "" {
		bin = "claude"
	}
	prompt := AssemblePrompt(in)
	var args []string
	if c.Verbose {
		args = []string{"--output-format", "stream-json", "--verbose", "--print", prompt}
	} else {
		args = []string{"--output-format", "json", "--print", prompt}
	}
	if in.Model != "" {
		args = append(args, "--model", in.Model)
	}
	run := Run{
		Bin: bin, Args: args, Cwd: in.Cwd, Env: CleanEnv(), Deadline: in.Deadline,
	}
	if c.Verbose && c.EventOut != nil {
		run.OnStdoutLine = func(line []byte) {
			if msg := FormatClaudeStreamEvent(line); msg != "" {
				_, _ = fmt.Fprintln(c.EventOut, msg)
			}
		}
	}
	res, err := Exec(ctx, run)
	if err != nil {
		if res.Killed {
			return nil, fmt.Errorf("%w: %w", ErrTimeout, err)
		}
		return nil, fmt.Errorf("claude exec: %w (stderr=%q)", err, string(res.Stderr))
	}
	var parsed claudeJSON
	if c.Verbose {
		parsed, err = decodeClaudeStreamResult(res.Stdout)
	} else {
		err = DecodeJSONLine(res.Stdout, &parsed)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrJSON, err)
	}
	if parsed.IsError {
		return nil, fmt.Errorf("%w: subtype=%q", ErrAgentError, parsed.Subtype)
	}
	if parsed.Result == "" {
		return nil, ErrEmptyContent
	}
	use := parsed.usage()
	return &CriticResult{
		Markdown: parsed.Result,
		ThreadID: parsed.SessionID,
		Tokens:   use.Input + use.Output,
		Usage:    use,
		USD:      parsed.TotalCostUSD,
		Stdout:   res.Stdout,
		Duration: res.Duration,
	}, nil
}
