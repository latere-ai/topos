package state

import (
	"encoding/json"
	"time"

	"latere.ai/x/topos/adversarial/internal/agent"
)

// StartFile is the schema written to <session>/start.json.
type StartFile struct {
	Schema      string      `json:"schema"`
	SessionID   string      `json:"session_id"`
	StartedAt   time.Time   `json:"started_at"`
	Proposer    AgentRef    `json:"proposer"`
	Critic      AgentRef    `json:"critic"`
	TaskContext string      `json:"task_context"`
	TaskSource  string      `json:"task_source"`
	Diff        DiffSnap    `json:"diff"`
	Config      ConfigSnap  `json:"config"`
	RootSession RootSession `json:"root_session"`

	AgonVersion string `json:"agon_version"`
	GoVersion   string `json:"go_version"`
}

// AgentRef is the embedded agent identity.
type AgentRef struct {
	Agent string `json:"agent"`
	Model string `json:"model"`
}

// DiffSnap is the run-level diff snapshot summary.
type DiffSnap struct {
	From         string   `json:"from"`
	To           string   `json:"to"`
	ChangedLines int      `json:"changed_lines"`
	Files        []string `json:"files"`
	PatchPath    string   `json:"patch_path"`
}

// ConfigSnap mirrors a subset of cli.Flags onto disk for audit. Topics
// are declared by each critic at runtime (see ForkOutcome.Topic in the
// round package); they're not in the run-level config snapshot.
type ConfigSnap struct {
	MaxTurn         int    `json:"max_turn"`
	SideCount       int    `json:"side_count"`
	CostCap         int    `json:"cost_cap"`
	ChangedLinesMin int    `json:"changed_lines_min"`
	Format          string `json:"format"`
	MainModel       string `json:"main_model"`
	SideModel       string `json:"side_model"`
}

// RootSession captures the (optional) root claude pointer.
type RootSession struct {
	ID             string `json:"id,omitempty"`
	TranscriptPath string `json:"transcript_path,omitempty"`
	Cwd            string `json:"cwd"`
}

// EndFile is the schema written to <session>/end.json.
type EndFile struct {
	Schema      string         `json:"schema"`
	SessionID   string         `json:"session_id"`
	EndedAt     time.Time      `json:"ended_at"`
	Termination Termination    `json:"termination"`
	Stats       Stats          `json:"stats"`
	Headline    *HeadlineRef   `json:"headline"`
	ExitCode    int            `json:"exit_code"`
	SummaryPath string         `json:"summary_path"`
	Extras      map[string]any `json:"extras,omitempty"`
}

// Termination is the why-it-ended block on EndFile.
type Termination struct {
	Reason    string `json:"reason"`
	ForkIndex *int   `json:"fork_index"`
	Round     *int   `json:"round"`
}

// Stats summarizes counts by status, per-fork rounds, tokens, wall time.
type Stats struct {
	TotalAttacks          int               `json:"total_attacks"`
	ByStatus              map[string]int    `json:"by_status"`
	RoundsExecutedPerFork []int             `json:"rounds_executed_per_fork"`
	TokensUsed            int               `json:"tokens_used"`
	TokenUsage            *agent.TokenUsage `json:"token_usage,omitempty"`
	CostCap               int               `json:"cost_cap"`
	WallSeconds           int               `json:"wall_seconds"`
}

// HeadlineRef points at the headline attack id and its score.
type HeadlineRef struct {
	AttackID   string `json:"attack_id"`
	Contention int    `json:"contention"`
}

// TranscriptRecord is one line in <session>/transcript.jsonl.
type TranscriptRecord struct {
	TS    time.Time `json:"ts"`
	Fork  int       `json:"fork"`
	Round int       `json:"round"`
	Role  string    `json:"role"`
	Path  string    `json:"path"`
	MS    int       `json:"ms"`
}

// LogRecord is one line of the cross-session <state-dir>/log.jsonl.
// "kind" discriminates "run" vs "skipped".
type LogRecord struct {
	TS           time.Time `json:"ts"`
	Kind         string    `json:"kind"`
	Session      string    `json:"session,omitempty"`
	Termination  string    `json:"termination,omitempty"`
	Unresolved   int       `json:"unresolved,omitempty"`
	Tokens       int       `json:"tokens,omitempty"`
	WallSeconds  int       `json:"wall_s,omitempty"`
	Summary      string    `json:"summary,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	ChangedLines int       `json:"changed_lines,omitempty"`
	Threshold    int       `json:"threshold,omitempty"`
}

// On-disk schema versions.
const (
	// SchemaStart identifies the start.json schema.
	SchemaStart = "agon.start.v0"
	// SchemaEnd identifies the end.json schema.
	SchemaEnd = "agon.end.v0"
)

// WriteStart writes <session>/start.json. Idempotent within a fresh
// session because AtomicWrite renames in.
func WriteStart(s *Session, x *StartFile) error {
	if x.Schema == "" {
		x.Schema = SchemaStart
	}
	b, err := json.MarshalIndent(x, "", "  ")
	if err != nil {
		return err
	}
	return s.AtomicWrite("start.json", append(b, '\n'))
}

// WriteEnd writes <session>/end.json.
func WriteEnd(s *Session, x *EndFile) error {
	if x.Schema == "" {
		x.Schema = SchemaEnd
	}
	b, err := json.MarshalIndent(x, "", "  ")
	if err != nil {
		return err
	}
	return s.AtomicWrite("end.json", append(b, '\n'))
}

// AppendTranscript appends one record to <session>/transcript.jsonl.
func AppendTranscript(s *Session, r *TranscriptRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.AppendLine("transcript.jsonl", b)
}

// AppendLog appends one record to <state-dir>/log.jsonl.
func AppendLog(stateDirAbs string, r *LogRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return AppendCrossSessionLog(stateDirAbs, b)
}
