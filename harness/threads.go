// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/prompts"
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
	// Effort is the reasoning effort of the subagent's requests; empty
	// holds the parent's.
	Effort string
	// Tools are the subagent's declared tools; nil holds the parent's.
	Tools     []string
	Mode      Mode
	Subagents map[string]Subagent
	// Ignored are the fields the agent declares that its thread does not
	// use, recorded on thread.started so the log shows the thread did not
	// act as that agent.
	Ignored []string
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
	ToolAdvisor = "advisor"
)

// advisorAgent is the agent name of an advisor thread.
const advisorAgent = "advisor"

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

// stricterCeiling is the stricter of two ceilings, where an empty one
// bounds nothing.
func stricterCeiling(a, b Mode) Mode {
	if a == "" {
		return b
	}
	return stricterMode(a, b)
}

// mode is the approval mode the turn decides its next calls under (spec
// 041): the new mode of the latest session.policy_changed the turn has
// read, held to the thread's ceiling, and the mode the turn started with
// when the log holds none. A host with no operating-system sandbox
// decides progressive as confirm (spec 012), whichever way the mode came.
func (t *turn) mode() Mode {
	mode := t.h.c.Policy.Mode
	if m, ok := session.Mode(t.s, t.events()); ok {
		mode = stricterCeiling(Mode(m), t.h.c.ceiling)
	}
	if mode == "" {
		mode = ModeConfirm
	}
	if mode == ModeProgressive && t.h.c.Machine != nil && t.h.c.Machine.Info().Sandbox == machine.SandboxNone {
		return ModeConfirm
	}
	return mode
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
// the thread holds, spawn and message when the thread has subagents and
// is below the depth limit, the advisor when the agent has one, and the
// question tool for the session's own thread when the agent names it.
// A thread a spawn started holds no question tool: it ends its turn
// saying what it needs decided, and its parent, which holds the
// conversation with the person, asks (spec 039).
func (t *turn) registry(names []string) (*tools.Registry, error) {
	r := t.root.Subset(names)
	var extra []tools.Tool
	if len(t.h.c.Subagents) > 0 && t.depth < t.h.maxDepth() {
		extra = append(extra, spawnTool{t}, messageTool{t})
	}
	if t.h.c.Advisor != nil {
		extra = append(extra, advisorTool{t})
	}
	if t.h.c.Question && t.thread == "" {
		extra = append(extra, questionTool{t})
	}
	for _, tool := range extra {
		if err := r.AddBuiltin(tool); err != nil {
			return nil, err
		}
	}
	return r, nil
}

type advisorTool struct{ t *turn }

func (a advisorTool) Definition() tools.Definition {
	return tools.Definition{
		Name:        ToolAdvisor,
		Description: prompts.Text(prompts.ToolAdvisor),
		InputSchema: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string"}},"additionalProperties":false}`),
	}
}

func (a advisorTool) Properties() tools.Properties {
	return tools.Properties{Effect: tools.EffectNone}
}

func (a advisorTool) Run(ctx context.Context, c tools.Call) (tools.Result, error) {
	var in struct {
		Question string `json:"question"`
	}
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return tools.Result{}, err
	}
	return a.t.advise(ctx, c.ID, in.Question)
}

// advise sends the caller's transcript and question to its advisor
// thread, starting the thread on the first call, and returns the answer.
func (t *turn) advise(ctx context.Context, callID, question string) (tools.Result, error) {
	tr, err := session.Fold(t.events(), t.thread)
	if err != nil {
		return tools.Result{}, err
	}
	body := prompts.Render(prompts.AdvisorRequest, prompts.Data{"Transcript": renderTranscript(tr), "Question": strings.TrimSpace(question)})
	sub := t.advisorConfig()
	id, found := t.advisorThread()
	var e session.Event
	if found {
		e, err = t.event(session.TypeThreadMessage, session.ThreadMessage{From: t.thread, To: id, FromName: t.h.c.Name, ToolUseID: callID, Content: []lux.Block{{Type: ir.BlockText, Text: body}}})
		if err != nil {
			return tools.Result{}, err
		}
		e.Thread = id
	} else {
		e, err = t.event(session.TypeThreadStarted, session.ThreadStarted{
			Agent: session.AgentRef{Name: advisorAgent}, Parent: t.thread, ToolUseID: callID, Task: body,
			Isolation: "shared", Depth: t.depth + 1, Model: sub.Connection.Model, Tools: []string{},
		})
		if err != nil {
			return tools.Result{}, err
		}
		e.Thread, id = e.ID, e.ID
	}
	if err := t.commit(ctx, e); err != nil {
		return tools.Result{}, err
	}
	child, err := t.child(id, t.depth+1, sub, nil)
	if err != nil {
		return tools.Result{}, err
	}
	return child.answer(ctx)
}

// advisorConfig is the advisor thread's configuration: the advisor's
// model and instructions, no tools, no subagents, no advisor of its own.
func (t *turn) advisorConfig() Config {
	a := *t.h.c.Advisor
	a.Name = advisorAgent
	if strings.TrimSpace(a.Instructions) == "" {
		a.Instructions = prompts.Text(prompts.AdvisorInstructions)
	}
	a.Subagents = nil
	cfg := t.childConfig(a)
	cfg.Advisor = nil
	return cfg
}

// advisorThread finds this thread's advisor thread.
func (t *turn) advisorThread() (string, bool) {
	for _, e := range t.events() {
		if e.Type != session.TypeThreadStarted || e.Redacted() {
			continue
		}
		var p session.ThreadStarted
		if e.Decode(&p) == nil && p.Parent == t.thread && p.Agent.Name == advisorAgent {
			return e.ID, true
		}
	}
	return "", false
}

// answer runs an advisor's turn and returns its answer as the call's
// result.
func (t *turn) answer(ctx context.Context) (tools.Result, error) {
	out, err := t.run(ctx)
	if err != nil {
		return tools.Result{}, err
	}
	if out.StopReason != session.StopEndTurn {
		stopped := prompts.Render(prompts.AdvisorStopped, prompts.Data{"Reason": string(out.StopReason), "Detail": out.Detail})
		return tools.Text(tools.OutcomeError, stopped), nil
	}
	return tools.Text(tools.OutcomeOK, finalText(t.events(), t.thread)), nil
}

// renderTranscript is a transcript as text for an advisor: who said
// what, the calls made and what they returned, each result cut to a
// few kilobytes.
func renderTranscript(tr session.Transcript) string {
	const maxResult = 4 << 10
	var entries []prompts.Data
	for _, m := range tr.Messages {
		for _, blk := range m.Blocks {
			switch blk.Type {
			case ir.BlockText:
				entries = append(entries, prompts.Data{"Kind": "text", "Agent": m.Role == ir.RoleAssistant, "Text": blk.Text})
			case ir.BlockToolUse:
				if blk.ToolUse != nil {
					entries = append(entries, prompts.Data{"Kind": "call", "Name": blk.ToolUse.Name, "Args": string(blk.ToolUse.Args)})
				}
			case ir.BlockToolResult:
				if blk.ToolResult == nil {
					continue
				}
				var out []string
				for _, in := range blk.ToolResult.Blocks {
					out = append(out, in.Text)
				}
				text := strings.Join(out, "\n")
				cut := len(text) > maxResult
				if cut {
					text = text[:maxResult]
				}
				entries = append(entries, prompts.Data{"Kind": "result", "Text": text, "Cut": cut})
			}
		}
	}
	return prompts.Render(prompts.AdvisorTranscript, prompts.Data{"Entries": entries})
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
		Description: prompts.Text(prompts.ToolSpawn),
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
		Description: prompts.Text(prompts.ToolMessage),
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
		return errorResult(CodeUnknownSubagent, prompts.Render(prompts.ThreadUnknownSubagent, prompts.Data{"Agent": agent})), nil
	}
	if t.depth+1 > t.h.maxDepth() {
		return errorResult(CodeDepthExceeded, prompts.Render(prompts.ThreadDepthExceeded, prompts.Data{"Depth": t.depth})), nil
	}
	var wt machine.Worktrees
	if isolation == "worktree" {
		var ok bool
		if wt, ok = t.h.c.Machine.(machine.Worktrees); !ok {
			return errorResult(CodeIsolationUnavailable, prompts.Text(prompts.ThreadNoWorktrees)), nil
		}
	}
	held := t.reg.Names()
	var names []string
	for _, n := range held {
		// The question and publish tools are the session's own thread's:
		// the thread that holds the conversation with the person asks,
		// and a session has one app (specs 039 and 043).
		if n == ToolSpawn || n == ToolMessage || n == ToolQuestion || n == session.ToolPublish {
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
	id := session.NewID(session.PrefixEvent)
	started := session.ThreadStarted{
		Agent: session.AgentRef{Name: agent}, Parent: t.thread, ToolUseID: callID, Task: task,
		Isolation: "shared", Depth: t.depth + 1, Model: cfg.Connection.Model, Tools: names,
		Ignored: sub.Ignored,
	}
	var note string
	if wt != nil {
		branch := fmt.Sprintf("agents/%s/%s.%s", agent, t.s.ID, id)
		m, err := wt.Worktree(ctx, id, branch)
		if err != nil {
			return errorResult(CodeIsolationUnavailable, err.Error()), nil
		}
		cfg.Machine = m
		started.Isolation, started.Branch, started.Workdir = "worktree", branch, m.Info().Workdir
		if dirty(ctx, t.h.c.Machine) {
			note = prompts.Text(prompts.ThreadUncommitted)
		}
	}
	e, err := t.event(session.TypeThreadStarted, started)
	if err != nil {
		return tools.Result{}, err
	}
	e.ID, e.Thread = id, id
	if !t.sh.claim(t.h.maxConcurrent()) {
		return errorResult(CodeTooManyThreads, prompts.Render(prompts.ThreadTooMany, prompts.Data{"Max": t.h.maxConcurrent()})), nil
	}
	defer t.sh.release()
	if err := t.commit(ctx, e); err != nil {
		return tools.Result{}, err
	}
	child, err := t.child(e.ID, t.depth+1, cfg, names)
	if err != nil {
		return tools.Result{}, err
	}
	res, err := child.drive(ctx)
	if err != nil || started.Isolation != "worktree" {
		return res, err
	}
	return child.commitWorktree(ctx, started, res, note)
}

// threadMachine is the machine a thread runs on: its worktree's for an
// isolated thread, reopened by name, otherwise the session's.
func (t *turn) threadMachine(ctx context.Context, started session.ThreadStarted, id string) (machine.Machine, error) {
	if started.Isolation != "worktree" {
		return t.h.c.Machine, nil
	}
	wt, ok := t.h.c.Machine.(machine.Worktrees)
	if !ok {
		return nil, fmt.Errorf("%s: this machine keeps no worktrees", CodeIsolationUnavailable)
	}
	return wt.Worktree(ctx, id, started.Branch)
}

// dirty reports uncommitted changes in a machine's working directory.
func dirty(ctx context.Context, m machine.Machine) bool {
	res, err := m.Exec(ctx, machine.ExecRequest{Command: "git status --porcelain", Timeout: time.Minute})
	return err == nil && res.ExitCode == 0 && len(strings.TrimSpace(string(res.Output))) > 0
}

// commitWorktree commits an isolated thread's changes to its branch at
// the end of its turn and names the branch and commit in the result.
func (t *turn) commitWorktree(ctx context.Context, started session.ThreadStarted, res tools.Result, note string) (tools.Result, error) {
	m := t.h.c.Machine
	env := map[string]string{
		"GIT_AUTHOR_NAME": started.Agent.Name, "GIT_AUTHOR_EMAIL": started.Agent.Name + "@agents.topos.invalid",
		"GIT_COMMITTER_NAME": started.Agent.Name, "GIT_COMMITTER_EMAIL": started.Agent.Name + "@agents.topos.invalid",
	}
	msg := fmt.Sprintf("topos thread %s turn %d\n\nTopos-Session: %s\nTopos-Agent: %s", t.thread, t.num, t.s.ID, started.Agent.Name)
	script := "git add -A && (git diff --cached --quiet || git commit -q -m " + shellQuote(msg) + ") && git rev-parse --short HEAD"
	out, err := m.Exec(ctx, machine.ExecRequest{Command: script, Env: env, Timeout: 2 * time.Minute})
	if err != nil {
		return tools.Result{}, err
	}
	if out.ExitCode != 0 {
		failed := prompts.Render(prompts.ThreadCommitFailed, prompts.Data{"Branch": started.Branch, "Output": strings.TrimSpace(string(out.Output))})
		return tools.Text(tools.OutcomeError, failed), nil
	}
	lines := []string{prompts.Render(prompts.ThreadBranch, prompts.Data{"Branch": started.Branch, "Commit": strings.TrimSpace(string(out.Output))})}
	if note != "" {
		lines = append(lines, note)
	}
	for _, l := range lines {
		res.Content = append(res.Content, lux.Block{Type: ir.BlockText, Text: l})
	}
	return res, nil
}

// shellQuote renders an argument for the machine's shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// message sends a thread this thread spawned more work and runs its turn.
func (t *turn) message(ctx context.Context, callID, thread, content string, end bool) (tools.Result, error) {
	started, ok := t.spawned(thread)
	if !ok {
		return errorResult(CodeThreadNotFound, prompts.Render(prompts.ThreadNotSpawned, prompts.Data{"Thread": thread})), nil
	}
	if t.ended(thread) {
		return errorResult(CodeThreadNotFound, prompts.Render(prompts.ThreadEnded, prompts.Data{"Thread": thread})), nil
	}
	sub, ok := t.h.c.Subagents[started.Agent.Name]
	if !ok {
		return errorResult(CodeUnknownSubagent, prompts.Render(prompts.ThreadSubagentGone, prompts.Data{"Thread": thread, "Agent": started.Agent.Name})), nil
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
		return errorResult(CodeTooManyThreads, prompts.Render(prompts.ThreadTooMany, prompts.Data{"Max": t.h.maxConcurrent()})), nil
	}
	defer t.sh.release()
	if err := t.commit(ctx, e); err != nil {
		return tools.Result{}, err
	}
	cfg := t.childConfig(sub)
	if cfg.Machine, err = t.threadMachine(ctx, started, thread); err != nil {
		return errorResult(CodeIsolationUnavailable, err.Error()), nil
	}
	child, err := t.child(thread, started.Depth, cfg, started.Tools)
	if err != nil {
		return tools.Result{}, err
	}
	res, err := child.drive(ctx)
	if err == nil && started.Isolation == "worktree" {
		res, err = child.commitWorktree(ctx, started, res, "")
	}
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
	if sub.Effort != "" {
		cfg.Effort = sub.Effort
	}
	cfg.Policy.Mode = stricterMode(t.mode(), sub.Mode)
	cfg.ceiling = stricterCeiling(t.h.c.ceiling, sub.Mode)
	cfg.Subagents = sub.Subagents
	// The advisor belongs to the agent whose configuration names it, and
	// only the session's own thread asks a person.
	cfg.Advisor = nil
	cfg.Question = false
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
		return tools.Text(tools.OutcomeOK, idle(text, t.thread)), nil
	case session.StopToolConfirmation, session.StopToolResult:
		return tools.Result{}, &errPause{reason: out.StopReason}
	}
	stopped := prompts.Render(prompts.ThreadStopped, prompts.Data{"Thread": t.thread, "Reason": string(out.StopReason), "Detail": out.Detail})
	return tools.Text(tools.OutcomeError, stopped), nil
}

// idle is the result of a call whose thread went idle: the thread's final
// text, then the line that says it takes more work.
func idle(text, thread string) string {
	return strings.TrimSpace(text + "\n\n" + prompts.Render(prompts.ThreadIdle, prompts.Data{"Thread": thread}))
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
		return tools.Text(tools.OutcomeOK, idle(text, thread)), nil
	}
	started, ok := t.spawned(thread)
	if !ok {
		return unknownEffect(), nil
	}
	if started.Agent.Name == advisorAgent && t.h.c.Advisor != nil {
		child, err := t.child(thread, started.Depth, t.advisorConfig(), nil)
		if err != nil {
			return tools.Result{}, err
		}
		return child.answer(ctx)
	}
	sub, ok := t.h.c.Subagents[started.Agent.Name]
	if !ok {
		return errorResult(CodeUnknownSubagent, prompts.Render(prompts.ThreadSubagentGone, prompts.Data{"Thread": thread, "Agent": started.Agent.Name})), nil
	}
	if !t.sh.claim(t.h.maxConcurrent()) {
		return errorResult(CodeTooManyThreads, prompts.Render(prompts.ThreadTooMany, prompts.Data{"Max": t.h.maxConcurrent()})), nil
	}
	defer t.sh.release()
	cfg := t.childConfig(sub)
	var err error
	if cfg.Machine, err = t.threadMachine(ctx, started, thread); err != nil {
		return errorResult(CodeIsolationUnavailable, err.Error()), nil
	}
	child, err := t.child(thread, started.Depth, cfg, started.Tools)
	if err != nil {
		return tools.Result{}, err
	}
	res, err := child.drive(ctx)
	if err == nil && started.Isolation == "worktree" {
		res, err = child.commitWorktree(ctx, started, res, "")
	}
	return res, err
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
