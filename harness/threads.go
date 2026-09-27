// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
)

// Subagent is an agent a thread may spawn (spec 013). A nil or zero
// field is the parent's.
type Subagent struct {
	Name         string
	Instructions string
	Model        models.Model
	Connection   *models.Connection
	Entry        *models.Entry
	// Tools are the subagent's declared tools; nil holds the parent's.
	Tools     []string
	Mode      Mode
	Subagents map[string]Subagent
}

// Limits of spec 013.
const (
	DefaultMaxDepth      = 2
	MaxDepthLimit        = 4
	DefaultMaxConcurrent = 8
)

// Names of the thread tools.
const (
	ToolSpawn   = "spawn"
	ToolMessage = "message"
)

// The error codes of spec 013, the text of a call's error result.
const (
	CodeUnknownSubagent      = "unknown_subagent"
	CodeDepthExceeded        = "depth_exceeded"
	CodeTooManyThreads       = "too_many_threads"
	CodeThreadNotFound       = "thread_not_found"
	CodeIsolationUnavailable = "isolation_unavailable"
)

// errPause is a thread's turn that stopped waiting for a person: the
// call that drove it keeps no result, and the whole session goes idle
// with the thread's stop reason.
type errPause struct{ reason session.StopReason }

func (e *errPause) Error() string { return "harness: a thread waits: " + string(e.reason) }

func (h *Harness) maxDepth() int {
	d := h.c.MaxDepth
	if d <= 0 {
		d = DefaultMaxDepth
	}
	return min(d, MaxDepthLimit)
}

func (h *Harness) maxConcurrent() int {
	if h.c.MaxConcurrent <= 0 {
		return DefaultMaxConcurrent
	}
	return h.c.MaxConcurrent
}

// modeRank orders modes strictest first.
func modeRank(m Mode) int {
	switch m {
	case ModePlan:
		return 0
	case ModeProgressive:
		return 2
	}
	return 1
}

// stricterMode is the stricter of two modes; an empty mode is confirm.
func stricterMode(a, b Mode) Mode {
	if a == "" {
		a = ModeConfirm
	}
	if b == "" {
		return a
	}
	if modeRank(b) < modeRank(a) {
		return b
	}
	return a
}

// registry builds a thread's registry: the root registry's tools that
// the thread holds, and spawn and message when the thread has
// subagents and is below the depth limit.
func (t *turn) registry(names []string) (*tools.Registry, error) {
	r := t.root.Subset(names)
	if len(t.h.c.Subagents) == 0 || t.depth >= t.h.maxDepth() {
		return r, nil
	}
	for _, tool := range []tools.Tool{spawnTool{t}, messageTool{t}} {
		if err := r.AddBuiltin(tool); err != nil {
			return nil, err
		}
	}
	return r, nil
}

type spawnTool struct{ t *turn }

func (s spawnTool) Definition() tools.Definition {
	names := make([]string, 0, len(s.t.h.c.Subagents))
	for n := range s.t.h.c.Subagents {
		names = append(names, n)
	}
	slices.Sort(names)
	enum, err := json.Marshal(names)
	if err != nil {
		enum = []byte("[]")
	}
	return tools.Definition{
		Name:        ToolSpawn,
		Description: "Start a thread that runs one of your subagents on a task, on this same machine, and return its final answer. The thread does not see this conversation: give it a complete task. Several spawn calls in one step run their threads at once. Send the thread more work later with message.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"agent":{"type":"string","enum":` + string(enum) + `},"task":{"type":"string","minLength":1},"isolation":{"type":"string","enum":["shared","worktree"]},"tools":{"type":"array","items":{"type":"string"}},"budget":{"type":"number","minimum":0}},"required":["agent","task"],"additionalProperties":false}`),
	}
}

func (s spawnTool) Properties() tools.Properties {
	return tools.Properties{Parallel: true, Effect: tools.EffectNone}
}

func (s spawnTool) Run(ctx context.Context, c tools.Call) (tools.Result, error) {
	var in struct {
		Agent     string   `json:"agent"`
		Task      string   `json:"task"`
		Isolation string   `json:"isolation"`
		Tools     []string `json:"tools"`
	}
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return tools.Result{}, err
	}
	return s.t.spawn(ctx, c.ID, in.Agent, in.Task, in.Isolation, in.Tools)
}

type messageTool struct{ t *turn }

func (m messageTool) Definition() tools.Definition {
	return tools.Definition{
		Name:        ToolMessage,
		Description: "Send a message to a thread you spawned and return its answer. Set end to true to end the thread after this turn.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"thread":{"type":"string","minLength":1},"content":{"type":"string","minLength":1},"end":{"type":"boolean"}},"required":["thread","content"],"additionalProperties":false}`),
	}
}

func (m messageTool) Properties() tools.Properties {
	return tools.Properties{Parallel: true, Effect: tools.EffectNone}
}

func (m messageTool) Run(ctx context.Context, c tools.Call) (tools.Result, error) {
	var in struct {
		Thread  string `json:"thread"`
		Content string `json:"content"`
		End     bool   `json:"end"`
	}
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return tools.Result{}, err
	}
	return m.t.message(ctx, c.ID, in.Thread, in.Content, in.End)
}

func errorResult(code, text string) tools.Result {
	return tools.Text(tools.OutcomeError, code+": "+text)
}

// spawn starts a thread and runs its first turn.
func (t *turn) spawn(ctx context.Context, callID, agent, task, isolation string, narrow []string) (tools.Result, error) {
	sub, ok := t.h.c.Subagents[agent]
	if !ok {
		return errorResult(CodeUnknownSubagent, fmt.Sprintf("no subagent named %q", agent)), nil
	}
	if t.depth+1 > t.h.maxDepth() {
		return errorResult(CodeDepthExceeded, fmt.Sprintf("a thread at depth %d may not spawn", t.depth)), nil
	}
	if isolation == "worktree" {
		return errorResult(CodeIsolationUnavailable, "worktree isolation is not available on this machine; spawn with isolation shared"), nil
	}
	held := t.reg.Names()
	var names []string
	for _, n := range held {
		if n == ToolSpawn || n == ToolMessage {
			continue
		}
		if sub.Tools != nil && !slices.Contains(sub.Tools, n) {
			continue
		}
		if narrow != nil && !slices.Contains(narrow, n) {
			continue
		}
		names = append(names, n)
	}
	cfg := t.childConfig(sub)
	started := session.ThreadStarted{
		Agent: session.AgentRef{Name: agent}, Parent: t.thread, ToolUseID: callID, Task: task,
		Isolation: "shared", Depth: t.depth + 1, Model: cfg.Connection.Model, Tools: names,
	}
	e, err := t.event(session.TypeThreadStarted, started)
	if err != nil {
		return tools.Result{}, err
	}
	e.Thread = e.ID
	if !t.sh.claim(t.h.maxConcurrent()) {
		return errorResult(CodeTooManyThreads, fmt.Sprintf("the session already runs %d threads", t.h.maxConcurrent())), nil
	}
	defer t.sh.release()
	if err := t.commit(ctx, e); err != nil {
		return tools.Result{}, err
	}
	child, err := t.child(e.ID, t.depth+1, cfg, names)
	if err != nil {
		return tools.Result{}, err
	}
	return child.drive(ctx)
}

// message sends a thread this thread spawned more work and runs its turn.
func (t *turn) message(ctx context.Context, callID, thread, content string, end bool) (tools.Result, error) {
	started, ok := t.spawned(thread)
	if !ok {
		return errorResult(CodeThreadNotFound, fmt.Sprintf("no thread %s that this thread spawned", thread)), nil
	}
	if t.ended(thread) {
		return errorResult(CodeThreadNotFound, fmt.Sprintf("thread %s has ended", thread)), nil
	}
	sub, ok := t.h.c.Subagents[started.Agent.Name]
	if !ok {
		return errorResult(CodeUnknownSubagent, fmt.Sprintf("thread %s runs %q, which is no longer a subagent", thread, started.Agent.Name)), nil
	}
	e, err := t.event(session.TypeThreadMessage, session.ThreadMessage{
		From: t.thread, To: thread, FromName: t.h.c.Name, ToolUseID: callID,
		Content: []lux.Block{{Type: ir.BlockText, Text: content}},
	})
	if err != nil {
		return tools.Result{}, err
	}
	e.Thread = thread
	if !t.sh.claim(t.h.maxConcurrent()) {
		return errorResult(CodeTooManyThreads, fmt.Sprintf("the session already runs %d threads", t.h.maxConcurrent())), nil
	}
	defer t.sh.release()
	if err := t.commit(ctx, e); err != nil {
		return tools.Result{}, err
	}
	child, err := t.child(thread, started.Depth, t.childConfig(sub), started.Tools)
	if err != nil {
		return tools.Result{}, err
	}
	res, err := child.drive(ctx)
	if err != nil || !end {
		return res, err
	}
	ended, err := t.event(session.TypeThreadEnded, session.ThreadEnded{Reason: "completed", FinalText: finalText(t.events(), thread)})
	if err != nil {
		return tools.Result{}, err
	}
	ended.Thread = thread
	return res, t.commit(ctx, ended)
}

// childConfig is a subagent's configuration under this thread: its own
// model and instructions, the stricter mode, and its own subagents.
func (t *turn) childConfig(sub Subagent) Config {
	cfg := t.h.c
	cfg.Name = sub.Name
	cfg.Instructions = sub.Instructions
	if sub.Model != nil {
		cfg.Model = sub.Model
	}
	if sub.Connection != nil {
		cfg.Connection = *sub.Connection
	}
	if sub.Entry != nil {
		cfg.Entry = *sub.Entry
	}
	cfg.Policy.Mode = stricterMode(t.h.c.Policy.Mode, sub.Mode)
	cfg.Subagents = sub.Subagents
	cfg.Checkpoint = nil
	cfg.Prompt.Threads = len(sub.Subagents) > 0
	return cfg
}

// child builds the turn of a thread over the same log, machine and
// deadline.
func (t *turn) child(id string, depth int, cfg Config, names []string) (*turn, error) {
	h, err := New(cfg)
	if err != nil {
		return nil, err
	}
	var last uint64
	if evs := t.events(); len(evs) > 0 {
		last = evs[len(evs)-1].Seq
	}
	ct := &turn{h: h, s: t.s, sh: t.sh, l: t.l, root: t.root, num: t.num, start: t.start, deadline: t.deadline, startSeq: last, seen: last, thread: id, depth: depth}
	if ct.reg, err = ct.registry(names); err != nil {
		return nil, err
	}
	return ct, nil
}

// drive runs a thread's turn and returns the result of the call that
// drove it: the thread's final text when it went idle at end of turn, a
// pause when it waits for a person, an error naming any other stop.
func (t *turn) drive(ctx context.Context) (tools.Result, error) {
	out, err := t.run(ctx)
	if err != nil {
		return tools.Result{}, err
	}
	switch out.StopReason {
	case session.StopEndTurn:
		text := finalText(t.events(), t.thread)
		return tools.Text(tools.OutcomeOK, strings.TrimSpace(text+"\n\nThread "+t.thread+" is idle; send it more work with message.")), nil
	case session.StopToolConfirmation, session.StopToolResult:
		return tools.Result{}, &errPause{reason: out.StopReason}
	}
	return tools.Text(tools.OutcomeError, fmt.Sprintf("Thread %s stopped: %s %s", t.thread, out.StopReason, out.Detail)), nil
}

// spawned finds a thread this thread spawned.
func (t *turn) spawned(id string) (session.ThreadStarted, bool) {
	for _, e := range t.events() {
		if e.Type != session.TypeThreadStarted || e.ID != id || e.Redacted() {
			continue
		}
		var p session.ThreadStarted
		if e.Decode(&p) == nil && p.Parent == t.thread {
			return p, true
		}
	}
	return session.ThreadStarted{}, false
}

func (t *turn) ended(id string) bool {
	return slices.ContainsFunc(t.events(), func(e session.Event) bool {
		return e.Type == session.TypeThreadEnded && e.Thread == id
	})
}

// finalText is the text of a thread's latest agent.message.
func finalText(evs []session.Event, thread string) string {
	for _, e := range slices.Backward(evs) {
		if e.Type != session.TypeAgentMessage || e.Thread != thread || e.Redacted() {
			continue
		}
		var p session.AgentMessage
		if e.Decode(&p) != nil {
			return ""
		}
		var parts []string
		for _, b := range p.Message.Blocks {
			if b.Type == ir.BlockText && strings.TrimSpace(b.Text) != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// resumeThread settles an open spawn or message call on resume: the
// thread it drove continues its turn, or, when that turn had already
// ended, its final text becomes the call's result without running it
// again. A call that started no thread is closed as unknown_effect.
func (t *turn) resumeThread(ctx context.Context, c pendingCall) (tools.Result, error) {
	var thread string
	var driven uint64
	for _, e := range t.events() {
		switch {
		case e.Type == session.TypeThreadStarted && !e.Redacted():
			var p session.ThreadStarted
			if e.Decode(&p) == nil && p.ToolUseID == c.use.ToolUseID {
				thread, driven = e.ID, e.Seq
			}
		case e.Type == session.TypeThreadMessage && !e.Redacted():
			var p session.ThreadMessage
			if e.Decode(&p) == nil && p.ToolUseID == c.use.ToolUseID {
				thread, driven = e.Thread, e.Seq
			}
		}
	}
	if thread == "" {
		return unknownEffect(), nil
	}
	if done, text := settled(t.events(), thread, driven); done {
		return tools.Text(tools.OutcomeOK, strings.TrimSpace(text+"\n\nThread "+thread+" is idle; send it more work with message.")), nil
	}
	started, ok := t.spawned(thread)
	if !ok {
		return unknownEffect(), nil
	}
	sub, ok := t.h.c.Subagents[started.Agent.Name]
	if !ok {
		return errorResult(CodeUnknownSubagent, fmt.Sprintf("thread %s runs %q, which is no longer a subagent", thread, started.Agent.Name)), nil
	}
	if !t.sh.claim(t.h.maxConcurrent()) {
		return errorResult(CodeTooManyThreads, fmt.Sprintf("the session already runs %d threads", t.h.maxConcurrent())), nil
	}
	defer t.sh.release()
	child, err := t.child(thread, started.Depth, t.childConfig(sub), started.Tools)
	if err != nil {
		return tools.Result{}, err
	}
	return child.drive(ctx)
}

// settled reports whether a thread's turn after the event that drove it
// already ended with an answer and nothing open.
func settled(evs []session.Event, thread string, driven uint64) (bool, string) {
	if len(openCalls(evs, thread)) > 0 {
		return false, ""
	}
	for _, e := range slices.Backward(evs) {
		if e.Seq <= driven {
			return false, ""
		}
		if e.Type != session.TypeAgentMessage || e.Thread != thread {
			continue
		}
		var p session.AgentMessage
		if e.Decode(&p) != nil || p.Truncated {
			return false, ""
		}
		for _, b := range p.Message.Blocks {
			if b.Type == ir.BlockToolUse {
				return false, ""
			}
		}
		return true, finalText(evs, thread)
	}
	return false, ""
}

// claim takes a place among the session's running threads.
func (s *shared) claim(limit int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running >= limit {
		return false
	}
	s.running++
	return true
}

func (s *shared) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running--
}
