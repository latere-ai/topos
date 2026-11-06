// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"path"
	"slices"
	"strings"

	"latere.ai/x/pkg/hostmatch"
	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// The holds a call's machine enforces whatever the call does (spec 101):
// no host outside the session's network is reached without its own
// approval; the git host refuses a push to a protected branch; the
// working directory is kept in checkpoints it can be restored from.
const (
	HoldEgress        = "egress"
	HoldProtectedRefs = "protected_refs"
	HoldCheckpoints   = "checkpoints"
)

// The visibility of a session's repositories at their git host (spec
// 101): every one private, every one public, or some of each.
const (
	RepositoriesPrivate = "private"
	RepositoriesPublic  = "public"
	RepositoriesMixed   = "mixed"
)

// ReasonDenials is the reason a call asks for while the session is in
// the fallback of repeated refusals (spec 101).
const ReasonDenials = "after repeated refusals, every change waits for the person until they write"

// Handoff is what an agent or send_message call hands a subagent (spec
// 101): whether the subagent holds a tool whose effect is write, one
// whose effect is external, and, for agent, its isolation, shared or
// worktree. The task text is never part of it.
type Handoff struct {
	Writes    bool
	External  bool
	Isolation string
}

// Answer is a person's confirmation as a Learner is told it (spec 101):
// the approval and who gave it, how the person answered as the client
// measured it, how long the call waited from the event that asked to the
// confirmation's own, in the log's time, and whether the confirmation
// added a pattern. The pattern and the note are never part of it.
type Answer struct {
	Approve         bool
	By              string
	AnswerMS        *int64
	WaitedMS        int64
	DetailsOpened   *bool
	SuggestionShown *bool
	Surface         string
	Remembered      bool
}

// answerOf is the Answer of a confirmation that waited waitedMS.
func answerOf(c session.UserToolConfirmation, waitedMS int64) Answer {
	return Answer{
		Approve: c.Decision == session.DecisionAllow, By: c.Sender.Subject, AnswerMS: c.AnswerMS, WaitedMS: max(waitedMS, 0),
		DetailsOpened: c.DetailsOpened, SuggestionShown: c.SuggestionShown, Surface: c.Surface, Remembered: c.Remember != "",
	}
}

// ClaimedScope is the scope Topos's own claims about a call give, as its
// agent.tool_use records it without a decision service's reading (spec
// 101): source topos, and no member a claim leaves unknown.
func ClaimedScope(r tools.Reach) session.CallScope {
	s := session.CallScope{Undo: r.Undo, Contact: r.Contact, Source: session.ScopeFromTopos}
	if !r.Crosses.Empty() {
		c := r.Crosses
		s.Crosses = &c
	}
	if r.Target != nil {
		s.TargetClass, s.TargetKind = r.Target.Class, r.Target.Kind
	}
	return s
}

// Changes reports whether a call of a tool with props changes something:
// its effect is neither none nor read.
func Changes(props tools.Properties) bool {
	return props.Effect != tools.EffectNone && props.Effect != tools.EffectRead
}

// InFallback is d for a call the session decides while it asks for every
// call that changes something (spec 101): an allow or a flag of such a
// call asks the person, with decided_by denials and review probability 1,
// whatever always_allow, a remembered pattern, the sandbox rule, the risk
// score or a decision service decided. An ask and a block stay as they
// are; a call that changes nothing, one that reaches memory folders alone,
// and every call of plan mode are decided as before.
func InFallback(c Call, d Decision) Decision {
	if !c.Fallback || c.Memory || !Changes(c.Props) || ModeOn(c.Policy.Mode, c.MachineKind) == ModePlan {
		return d
	}
	if d.Verdict != VerdictAllow && d.Verdict != VerdictFlag {
		return d
	}
	return Decision{Verdict: VerdictAsk, Reason: ReasonDenials, ReviewProbability: 1, Suggestion: d.Suggestion, DecidedBy: session.DecidedDenials, Scope: d.Scope}
}

// noReach are the tools that read or change nothing, whose calls claim
// undo none (spec 101).
var noReach = []string{
	tools.NameRead, tools.NameGlob, tools.NameGrep, tools.NameHistory, tools.NameTodo,
	ToolQuestion, ToolAgent, ToolSendMessage, ToolAdvisor,
}

// Reaching is what a step knows to claim each call's reach by (spec
// 101): the machine's kind and working directory; whether it keeps each
// turn's working directory in a checkpoint; whether the session's
// repositories are reached through a git host whose ref rules refuse a
// push to a protected branch; the session's network, nil on a machine no
// session network governs; the organization's boundary, nil when no
// allow named one; and the hosts an earlier web_fetch of the session
// reached.
type Reaching struct {
	Kind, Workdir string
	Checkpoints   bool
	ProtectedRefs bool
	Network       *Network
	Boundary      *session.Boundary
	Fetched       map[string]bool
}

// reaching is the step's Reaching.
func (t *turn) reaching(network *Network) Reaching {
	info := t.h.c.Machine.Info()
	return Reaching{
		Kind: info.Kind, Workdir: info.Workdir, Checkpoints: t.h.c.Checkpoint != nil && info.Kind != machine.KindDevice,
		ProtectedRefs: t.h.c.ProtectedRefs && len(session.Repositories(t.s)) > 0,
		Network:       network, Boundary: t.s.Boundary, Fetched: FetchedHosts(t.events()),
	}
}

// Holds are the limits the machine enforces on every call whatever it
// does (spec 101): egress on a Cella machine whose session network is an
// allowlist or none, protected_refs, and checkpoints.
func (r Reaching) Holds() []string {
	var out []string
	if r.Kind == machine.KindCella && r.Network != nil && (r.Network.Mode == session.NetworkAllowlist || r.Network.Mode == session.NetworkNone) {
		out = append(out, HoldEgress)
	}
	if r.ProtectedRefs {
		out = append(out, HoldProtectedRefs)
	}
	if r.Checkpoints {
		out = append(out, HoldCheckpoints)
	}
	return out
}

// Of is Topos's own claims about a call of tool by name (spec 101),
// filled only from what the harness knows without reading the call's
// input as text: a tool that reads or changes nothing, a file tool's path
// against the working directory, a fetch's host against the session's
// network, the organization's boundary and the session's earlier
// fetches, a tool that says its own reach, and nothing for bash, an MCP
// tool, a client's tool or a call that reaches memory folders alone.
func (r Reaching) Of(tool tools.Tool, name string, c tools.Call, memory bool) tools.Reach {
	props := tool.Properties()
	if props.Client || memory {
		return tools.Reach{}
	}
	switch {
	case slices.Contains(noReach, name) && !Changes(props):
		return tools.Reach{Undo: session.UndoNone}
	case name == tools.NameWrite || name == tools.NameEdit:
		return r.file(tool, c.Input)
	case name == tools.NameWebFetch:
		return r.fetch(c.Input)
	}
	if rc, ok := tool.(tools.Reacher); ok {
		return rc.Reach(c)
	}
	return tools.Reach{}
}

// file is the reach of a write or an edit: whether its path leaves the
// working directory, and its target a path the session owns on a Cella
// machine and one others use on a host or a person's own computer.
func (r Reaching) file(tool tools.Tool, input jsontext.Value) tools.Reach {
	class := session.TargetShared
	if r.Kind == machine.KindCella {
		class = session.TargetOwn
	}
	out := tools.Reach{Crosses: session.Crosses{Remote: new(false)}, Target: &tools.ReachTarget{Kind: "path", Class: class}}
	p, ok := tool.(tools.Pather)
	if !ok || r.Workdir == "" {
		return out
	}
	paths := p.Paths(input)
	if len(paths) == 0 {
		return out
	}
	outside := false
	for _, name := range paths {
		if !inside(r.Workdir, name) {
			outside = true
		}
	}
	out.Crosses.Machine = new(outside)
	if !outside && r.Checkpoints {
		out.Undo = session.UndoReversible
	}
	return out
}

// inside reports whether name, as a call's input writes it, resolves
// under workdir.
func inside(workdir, name string) bool {
	if !path.IsAbs(name) {
		name = path.Join(workdir, name)
	}
	name, workdir = path.Clean(name), path.Clean(workdir)
	return name == workdir || strings.HasPrefix(name, strings.TrimSuffix(workdir, "/")+"/")
}

// fetch is the reach of a web_fetch: another system, outside the
// session's network or inside it, outside the organization or inside
// it, never public, and a first contact when no earlier fetch of the
// session reached its host. A boundary the session does not know is
// left out.
func (r Reaching) fetch(input jsontext.Value) tools.Reach {
	out := tools.Reach{Undo: session.UndoNone, Crosses: session.Crosses{Remote: new(true), Public: new(false)}, Target: &tools.ReachTarget{Kind: "host"}}
	host := fetchHost(input)
	if host == "" {
		return out
	}
	if r.Network != nil {
		out.Crosses.Network = new(!r.Network.Inside(host))
	}
	if b := r.Boundary; b != nil {
		out.Crosses.Organization = new(!hostmatch.New(b.Hosts, strings.ToLower).Matches(host))
	}
	out.Contact = session.ContactFirst
	if r.Fetched[host] {
		out.Contact = session.ContactRepeat
	}
	return out
}

// FetchedHosts are the hosts an earlier web_fetch of the session reached:
// a call of any thread whose result is ok. A fetch of any other host is a
// first contact (spec 101).
func FetchedHosts(evs []session.Event) map[string]bool {
	hosts := map[string]string{}
	out := map[string]bool{}
	for _, e := range evs {
		if e.Redacted() {
			continue
		}
		switch e.Type {
		case session.TypeAgentToolUse:
			var u session.AgentToolUse
			if e.Decode(&u) == nil && u.Name == tools.NameWebFetch {
				if h := fetchHost(u.Input); h != "" {
					hosts[u.ToolUseID] = h
				}
			}
		case session.TypeToolResult:
			var r session.ToolResult
			if e.Decode(&r) == nil && r.Outcome == tools.OutcomeOK && hosts[r.ToolUseID] != "" {
				out[hosts[r.ToolUseID]] = true
			}
		}
	}
	return out
}

// repositories is the visibility the git host reported for the session's
// delivered repositories when its first machine opened (spec 101):
// private when every one is private, public when every one is public,
// mixed otherwise, and "" for a session with none or one whose
// visibility is not known.
func repositories(evs []session.Event) string {
	for _, e := range evs {
		var m session.SessionMachine
		if e.Type != session.TypeSessionMachine || e.Subagent != "" || e.Redacted() || e.Decode(&m) != nil || len(m.Repositories) == 0 {
			continue
		}
		seen := map[string]bool{}
		for _, r := range m.Repositories {
			if r.Visibility == "" {
				return ""
			}
			seen[r.Visibility] = true
		}
		switch {
		case len(seen) > 1:
			return RepositoriesMixed
		case seen[RepositoriesPublic]:
			return RepositoriesPublic
		case seen[RepositoriesPrivate]:
			return RepositoriesPrivate
		}
		return ""
	}
	return ""
}

// handoff is what a call of a thread tool hands over (spec 101), nil for
// any other call or one whose subagent the thread cannot find: the tools
// the subagent will hold, and the isolation an agent call asks for.
func (t *turn) handoff(name string, input jsontext.Value) *Handoff {
	var in struct {
		Subagent  string   `json:"subagent"`
		Isolation string   `json:"isolation"`
		Tools     []string `json:"tools"`
		To        string   `json:"to"`
	}
	if json.Unmarshal(input, &in, models.FromProvider) != nil {
		return nil
	}
	var names []string
	h := &Handoff{}
	switch name {
	case ToolAgent:
		sub, ok := t.h.c.Subagents[in.Subagent]
		if !ok {
			return nil
		}
		names = t.handedTools(sub, in.Tools)
		h.Isolation = "shared"
		if in.Isolation == "worktree" {
			h.Isolation = "worktree"
		}
	case ToolSendMessage:
		started, ok := t.spawned(in.To)
		if !ok {
			return nil
		}
		names = started.Tools
	default:
		return nil
	}
	for _, n := range names {
		tool, ok := t.root.Get(n)
		if !ok {
			continue
		}
		switch tool.Properties().Effect {
		case tools.EffectWrite:
			h.Writes = true
		case tools.EffectExternal:
			h.External = true
		}
	}
	return h
}

// denialLimits are the session's limits of refusals (spec 101): the
// installation's, lowered by the ones the session's allow named.
func (t *turn) denialLimits() session.DenialLimits {
	l := session.DenialLimits{Consecutive: session.DefaultDenialsConsecutive, Total: session.DefaultDenialsTotal}
	if t.h.c.Denials != nil {
		l = *t.h.c.Denials
	}
	return l.Lower(t.s.Denials)
}

// checkDenials appends a session.denials_reached when the log's refusals
// reach a limit outside the fallback (spec 101), after the refusal that
// reached it: the effect is ask on an attended turn and stop on an
// unattended one, and on every turn of a rollout's run, which has no
// person to ask either and refuses each ask at once (spec 077). The
// check and the append hold the log's lock, so two threads that refuse
// at once append one.
func (t *turn) checkDenials(ctx context.Context) error {
	limits := t.denialLimits()
	effect := session.DenialsAsk
	if !t.attended() || t.s.Rollout != "" {
		effect = session.DenialsStop
	}
	t.sh.mu.Lock()
	defer t.sh.mu.Unlock()
	r := session.RefusalsOf(t.sh.events)
	limit, ok := r.Reached(limits)
	if !ok {
		return nil
	}
	e, err := t.event(session.TypeDenialsReached, session.DenialsReached{Consecutive: r.Consecutive, Total: r.Total, Limit: limit, Effect: effect})
	if err != nil {
		return err
	}
	return t.appendLocked(ctx, []session.Event{e})
}

// stoppedByDenials reports whether a session.denials_reached that stops
// the turn was appended since the turn began, with no person's message
// after it: every thread of the turn ends at its next step (spec 101).
// It reads the turn's own events alone, newest first.
func (t *turn) stoppedByDenials() bool {
	evs := t.events()
	for i := len(evs) - 1; i >= 0 && evs[i].Seq > t.startSeq; i-- {
		e := evs[i]
		if e.Redacted() {
			continue
		}
		switch e.Type {
		case session.TypeUserMessage:
			var m session.UserMessage
			if e.Decode(&m) == nil && m.Sender.Kind == session.SenderPerson {
				return false
			}
		case session.TypeDenialsReached:
			var d session.DenialsReached
			if e.Decode(&d) == nil {
				return d.Effect == session.DenialsStop
			}
		}
	}
	return false
}

// warnRefused is the result of the call that drove this thread's run,
// warned of the run's refusals.
func (t *turn) warnRefused(res tools.Result) tools.Result {
	return warnRefused(t.events(), t.thread, t.driven, res)
}

// warnRefused is the result of a thread tool's call whose subagent's run,
// the events of thread after the sequence driven, had a refusal (spec
// 101): the fixed warning before the result's text, and the count in its
// meta. A run with none returns res as it is.
func warnRefused(evs []session.Event, thread string, driven uint64, res tools.Result) tools.Result {
	n := session.RefusedIn(evs, thread, driven)
	if n == 0 {
		return res
	}
	warning := lux.Block{Type: ir.BlockText, Text: prompts.Render(prompts.SubagentRefused, prompts.Data{"Count": n})}
	res.Content = append([]lux.Block{warning}, res.Content...)
	if res.Meta == nil {
		res.Meta = &tools.Meta{}
	}
	res.Meta.Refused = n
	return res
}
