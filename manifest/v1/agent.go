// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Values of Approvals.Mode.
const (
	ModePlan        = "plan"
	ModeConfirm     = "confirm"
	ModeProgressive = "progressive"
)

// The reasoning levels of AgentModel.Reasoning, which a resolved spec
// holds as AgentModel.Effort.
const (
	EffortMinimal = "minimal"
	EffortLow     = "low"
	EffortMedium  = "medium"
	EffortHigh    = "high"
)

// Efforts are the reasoning levels, least reasoning first: the values of
// AgentModel.Reasoning and AgentModel.Effort. A session's change of its
// level takes the same values (spec 015).
var Efforts = []string{EffortMinimal, EffortLow, EffortMedium, EffortHigh}

// Values of Machine.Kind.
const (
	MachineHost  = "host"
	MachineCella = "cella"
)

// Values of Machine.EgressMode (spec 052), Cella's egress modes: open
// reaches every host but a denied one, allowlist the hosts of egress and
// the ones a session's network joins, and none nothing.
const (
	EgressOpen      = "open"
	EgressAllowlist = "allowlist"
	EgressNone      = "none"
)

// Values of MemoryStoreRef.Access and SessionResource.Access.
const (
	AccessReadWrite = "readWrite"
	AccessReadOnly  = "readOnly"
)

// Values of Hook.Event, the events of spec 012.
const (
	HookPreToolUse  = "pre_tool_use"
	HookPostToolUse = "post_tool_use"
	HookTurnStart   = "turn_start"
	HookTurnEnd     = "turn_end"
	HookPreCompact  = "pre_compact"
)

// AgentSpec is everything an agent is: its model, instructions, tools,
// permissions, subagents, attachments, machine defaults and limits.
type AgentSpec struct {
	Description string     `json:"description,omitempty"`
	Model       AgentModel `json:"model"`
	// Instructions are the agent's own instructions. A manifest may give
	// InstructionsFile instead, a path relative to the manifest that the
	// resolver reads and inlines here; a resolved spec has no
	// InstructionsFile.
	Instructions     string `json:"instructions"`
	InstructionsFile string `json:"instructionsFile,omitempty"`
	// Tools absent is every built-in; an empty list is none. The
	// resolved spec always lists them.
	Tools        []Tool           `json:"tools"`
	Permissions  []Permission     `json:"permissions,omitempty"`
	Approvals    Approvals        `json:"approvals"`
	Hooks        []Hook           `json:"hooks,omitempty"`
	Subagents    []Subagent       `json:"subagents,omitempty"`
	Threads      Threads          `json:"threads"`
	Advisor      *Advisor         `json:"advisor,omitempty"`
	Skills       []Skill          `json:"skills,omitempty"`
	MCPServers   []MCPServer      `json:"mcpServers,omitempty"`
	MemoryStores []MemoryStoreRef `json:"memoryStores,omitempty"`
	Connections  []string         `json:"connections,omitempty"`
	// Repositories are the repositories a session of the agent works in
	// when it names none of its own (spec 019).
	Repositories []Repository `json:"repositories,omitempty"`
	Machine      Machine      `json:"machine"`
	Budget       Budget       `json:"budget,omitzero"`
	Limits       Limits       `json:"limits"`
	Context      Context      `json:"context"`
}

// AgentModel is the model an agent runs and the figures it gives for
// it. A figure the manifest leaves out comes from the catalog when the
// session starts (spec 007), and is not written into the resolved spec,
// so a catalog update does not change an agent's digest.
type AgentModel struct {
	Name    string `json:"name"`
	Family  string `json:"family,omitempty"`
	Dialect string `json:"dialect,omitempty"`
	BaseURL string `json:"baseURL,omitempty"`
	// Credential names a Credential by name or cred_ id, never a value.
	// The resolved spec holds its id.
	Credential string `json:"credential,omitempty"`
	// Effort and Reasoning are how much the model reasons before it
	// answers, one of Efforts, empty for the model's own (spec 049). A
	// manifest names it reasoning, or effort, the name it had before. A
	// resolved spec holds it under effort alone, the spelling every
	// stored version's digest covers, and the API answers it under
	// reasoning alone: Stored and Answered move it between the two.
	Effort          string   `json:"effort,omitempty"`
	Reasoning       string   `json:"reasoning,omitempty"`
	InputWindow     int64    `json:"inputWindow,omitempty"`
	MaxOutputTokens int64    `json:"maxOutputTokens,omitempty"`
	Pricing         *Pricing `json:"pricing,omitempty"`
}

// Level is the model's reasoning level under either name: reasoning, and
// effort where reasoning is empty.
func (m AgentModel) Level() string {
	if m.Reasoning != "" {
		return m.Reasoning
	}
	return m.Effort
}

// Stored is m as a resolved spec holds it: the level under effort and
// none under reasoning, so a spec that names it either way renders the
// bytes a version stored before the rename did.
func (m AgentModel) Stored() AgentModel {
	m.Effort, m.Reasoning = m.Level(), ""
	return m
}

// Answered is m as the API answers it: the level under reasoning and none
// under effort.
func (m AgentModel) Answered() AgentModel {
	m.Reasoning, m.Effort = m.Level(), ""
	return m
}

// Answered is s as the API answers it: every model it names, its own, its
// advisor's and each inline subagent's, with the level under reasoning.
// s itself is not changed.
func (s AgentSpec) Answered() AgentSpec {
	s.Model = s.Model.Answered()
	if s.Advisor != nil {
		a := *s.Advisor
		a.Model = a.Model.Answered()
		s.Advisor = &a
	}
	if s.Subagents != nil {
		subs := make([]Subagent, len(s.Subagents))
		for i, sub := range s.Subagents {
			if sub.Spec != nil {
				spec := sub.Spec.Answered()
				sub.Spec = &spec
			}
			subs[i] = sub
		}
		s.Subagents = subs
	}
	return s
}

// Pricing is USD per million tokens, each a decimal string.
type Pricing struct {
	Input      string `json:"input,omitempty"`
	Output     string `json:"output,omitempty"`
	CacheRead  string `json:"cacheRead,omitempty"`
	CacheWrite string `json:"cacheWrite,omitempty"`
}

// Tool is one entry of spec.tools: a built-in by name, or a
// client-executed tool with its description and input schema. Its JSON
// form is the bare name when only Name is set, an object otherwise.
type Tool struct {
	Name        string          `json:"name"`
	OutputLimit int             `json:"outputLimit,omitempty"`
	Client      bool            `json:"client,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// toolObject is Tool without its methods, for the object form.
type toolObject Tool

// MarshalJSON writes the bare name when only Name is set.
func (t Tool) MarshalJSON() ([]byte, error) {
	if t.OutputLimit == 0 && !t.Client && t.Description == "" && len(t.InputSchema) == 0 {
		return json.Marshal(t.Name)
	}
	return json.Marshal(toolObject(t))
}

// errToolForm is a tool entry that is neither a name nor an object.
var errToolForm = errors.New("v1: a tool is a name or an object")

// UnmarshalJSON reads a name or an object.
func (t *Tool) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case len(b) > 0 && b[0] == '"':
		*t = Tool{}
		return json.Unmarshal(b, &t.Name)
	case len(b) > 0 && b[0] == '{':
		var o toolObject
		if err := json.Unmarshal(b, &o); err != nil {
			return err
		}
		*t = Tool(o)
		return nil
	}
	return errToolForm
}

// Approvals are how the agent's sessions decide calls when no
// authorizer supplies the lists (spec 012).
type Approvals struct {
	Mode          string     `json:"mode"`
	AlwaysAllow   []string   `json:"alwaysAllow,omitempty"`
	AlwaysConfirm []string   `json:"alwaysConfirm,omitempty"`
	Thresholds    Thresholds `json:"thresholds"`
}

// Thresholds are the progressive mode's score cut-offs, each in [0, 1]
// and increasing.
type Thresholds struct {
	FlagAt  *float64 `json:"flagAt"`
	AskAt   *float64 `json:"askAt"`
	BlockAt *float64 `json:"blockAt"`
}

// Hook is a command hook (spec 012). Timeout is a Go duration.
type Hook struct {
	Event   string `json:"event"`
	Matcher string `json:"matcher,omitempty"`
	Command string `json:"command"`
	Timeout string `json:"timeout,omitempty"`
}

// Subagent is an agent this agent's threads may spawn under Name:
// either Agent, a reference to another agent, or Spec, an inline one.
// The resolved spec holds the reference as agent_<ulid>@<n>.
type Subagent struct {
	Name  string     `json:"name"`
	Agent string     `json:"agent,omitempty"`
	Spec  *AgentSpec `json:"spec,omitempty"`
}

// Threads bound the session's graph of threads (spec 013).
type Threads struct {
	MaxDepth      *int `json:"maxDepth"`
	MaxConcurrent *int `json:"maxConcurrent"`
}

// Advisor is the model the agent consults with its transcript (spec
// 013), given in the form of spec.model.
type Advisor struct {
	Model        AgentModel `json:"model"`
	Instructions string     `json:"instructions,omitempty"`
}

// Skill is an Agent Skills folder: a path on the machine, or a git
// repository at a ref with an optional path inside it (spec 011).
type Skill struct {
	Path string `json:"path,omitempty"`
	Git  string `json:"git,omitempty"`
	Ref  string `json:"ref,omitempty"`
}

// MCPServer is an MCP server the agent's tools include (spec 021):
// Command for stdio or URL for streamable HTTP, exactly one. Connection
// names the Connection whose credential an HTTP server's requests
// carry.
type MCPServer struct {
	Name       string            `json:"name"`
	Command    string            `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	URL        string            `json:"url,omitempty"`
	Connection string            `json:"connection,omitempty"`
}

// MemoryStoreRef attaches a memory store by name or mem_ id; the
// resolved spec holds its id.
type MemoryStoreRef struct {
	Name   string `json:"name"`
	Access string `json:"access"`
}

// Repository is a git repository an agent's sessions work in: an https
// URL and a ref, a branch, a tag or a commit, absent for the
// repository's default branch (spec 019).
type Repository struct {
	URL string `json:"url"`
	Ref string `json:"ref,omitempty"`
}

// Machine is the machine an agent's sessions default to (spec 009).
// Image, Environment and Resources apply to a Cella machine; Roots and
// ReadPaths to the host. EgressMode is the mode of the machine's egress
// beside Egress, its hosts (spec 052): absent is allowlist and stays
// absent in a resolved spec, so an agent that names none keeps its digest;
// Egress is set only with allowlist.
type Machine struct {
	Kind        string    `json:"kind"`
	Image       string    `json:"image,omitempty"`
	Environment string    `json:"environment,omitempty"`
	Resources   Resources `json:"resources,omitzero"`
	EgressMode  string    `json:"egressMode,omitempty"`
	Egress      []string  `json:"egress,omitempty"`
	Roots       []string  `json:"roots,omitempty"`
	ReadPaths   []string  `json:"readPaths,omitempty"`
}

// Mode is the machine's egress mode, EgressAllowlist when it names none.
func (m Machine) Mode() string {
	if m.EgressMode == "" {
		return EgressAllowlist
	}
	return m.EgressMode
}

// Resources are Kubernetes quantities for a Cella machine.
type Resources struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	Disk   string `json:"disk,omitempty"`
}

// Budget bounds a session's spend. MaxCost is USD as a decimal string.
type Budget struct {
	MaxCost string `json:"maxCost,omitempty"`
}

// Limits are a session's wall-clock bounds as Go durations.
type Limits struct {
	TurnTimeout string `json:"turnTimeout"`
	MaxAge      string `json:"maxAge"`
}

// Context is how the harness manages the context window (spec 010).
type Context struct {
	CompactAt *float64 `json:"compactAt"`
}
