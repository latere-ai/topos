// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package decisions is the harness's client of a decision service (spec
// 037): a program, reached over HTTP, that predicts how likely the person a
// session acts for is to approve a call, suggests a verdict, and learns
// from every decision and answer the harness sends it.
//
// [Decider] is a [harness.Decider] and a [harness.Learner]. The harness
// stays the decision point: in progressive mode the decider composes the
// service's suggestion with the ceiling the rules leave open and with a
// random review draw (latere.ai/x/pkg/verdict.Decide); in confirm mode the
// rules decide and the suggestion is only recorded; in plan mode the service
// is not asked. Every decision is sent, with the probability that a person
// sees the call, so the service's error estimates can weight each answer.
package decisions

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"latere.ai/x/pkg/otel"
	"latere.ai/x/pkg/verdict"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// Timeout bounds every request to the service but a confirm-mode
// prediction.
const Timeout = 2 * time.Second

// ConfirmTimeout bounds the prediction of a call in confirm mode, where
// the rules decide and the suggestion is only recorded: a slow or absent
// service delays such a call by at most this. In progressive mode the
// prediction decides, and it has Timeout. A confirm-mode decision is sent
// in the background, so it delays the call by nothing.
const ConfirmTimeout = 300 * time.Millisecond

// AttributeAttended is the attribute of an action that says whether a
// person began the call's turn (spec 087): false on a turn a trigger's
// message began.
const AttributeAttended = "attended"

// The attributes of an action that say how deep it was delegated and what
// a thread tool's call hands over (spec 101): the subagent depth of the
// thread that made the call, on every call; and, on agent and
// send_message, whether the subagent holds a tool whose effect is write,
// one whose effect is external, and, for agent, its isolation.
const (
	AttributeDepth            = "depth"
	AttributeHandoffWrites    = "handoff.writes"
	AttributeHandoffExternal  = "handoff.external"
	AttributeHandoffIsolation = "handoff.isolation"
)

// The decision sources the harness sends, by mode. A decision's source is
// its mode's and the layer that decided it, as [Source] joins them.
const (
	SourceConfirm     = "topos/confirm"
	SourceProgressive = "topos/progressive"
)

// Source is the source a decision is sent with: the mode's source and the
// layer that decided the call, its agent.tool_use's decided_by, as
// topos/<mode>/<decided_by> (topos/progressive/learned,
// topos/confirm/always_allow), so the service can tell a call its own
// suggestion decided from one a rule or the risk score decided. A decision
// that names no layer is sent with the mode's source alone. A service that
// replays a session's log names each decision the same way.
func Source(mode, decidedBy string) string {
	if decidedBy == "" {
		return mode
	}
	return mode + "/" + decidedBy
}

// Options configure a [Decider].
type Options struct {
	// URL is the service's base URL, required. Token is a fixed bearer sent
	// to it, and Tokens a source asked for the bearer at each request, as a
	// server that holds a token for the service's audience is: exactly one
	// of the two is set.
	URL    string
	Token  string
	Tokens func(ctx context.Context) (string, error)
	// Org is which organization a call is keyed by (spec 100); zero is
	// [OrgInitiator].
	Org OrgSource
	// Client sends the requests; nil is pkg/otel's instrumented client.
	Client *http.Client
	// Timeout bounds each request; zero is [Timeout]. ConfirmTimeout bounds
	// a confirm-mode prediction; zero is [ConfirmTimeout].
	Timeout        time.Duration
	ConfirmTimeout time.Duration
	// Rand draws the review's uniform number; nil is math/rand/v2.
	Rand func() float64
	// Report receives a failure the decider recovered from: a prediction
	// that failed, so the call was asked, or a decision or answer that could
	// not be sent. Nil drops them.
	Report func(error)
	// Mode is how much the service decides in progressive; zero is
	// [ModeDecide]. Confirm and plan are the same in every mode.
	Mode Mode
	// Loosen is when a learned allow may run a call the rules and the
	// risk score would not run without a person (spec 101); zero is
	// [LoosenCovered].
	Loosen Loosen
}

// Loosen is when a learned allow decides a progressive call, in learned
// and in decide (spec 101).
type Loosen string

const (
	// LoosenCovered lets a learned allow decide when its prediction says
	// the person's approvals cover the call, cover.covered; any other
	// allow, and every allow of a service that sends no cover, is left to
	// the rules and the risk score, with the suggestion recorded.
	LoosenCovered Loosen = "covered"
	// LoosenNever lets the stricter of a learned allow and the rules'
	// verdict decide: the service can make a call wait or stop it, and
	// never run one the risk score would ask about.
	LoosenNever Loosen = "never"
)

// ParseLoosen reads a loosening by its name.
func ParseLoosen(s string) (Loosen, error) {
	switch l := Loosen(s); l {
	case LoosenCovered, LoosenNever:
		return l, nil
	}
	return "", fmt.Errorf("decisions: the loosening %q is not covered or never", s)
}

// Mode is how much a decision service decides in progressive mode.
type Mode string

const (
	// ModeShadow asks and records the service in every mode and lets it
	// decide in none: a progressive call is decided by the rules and the
	// risk score, as without a service, with the service's suggestion
	// recorded beside it, as a confirm-mode call is.
	ModeShadow Mode = "shadow"
	// ModeLearned lets the service decide a progressive call where it has
	// evidence about the person: its allow, as Loosen lets it, its block,
	// and an ask the person's own answers keep for them, of kind split,
	// whose answers disagree, or wider, a call that reaches wider than any
	// of its kind the person allowed (spec 101). Where it has too few
	// answers (unsure), answers that agree but do not settle the call yet
	// (short), approvals that are not enough (few), a kind of ask the
	// harness does not know or none, or where it does not answer, the
	// rules and the risk score decide, as without a service, with the
	// suggestion recorded beside them.
	ModeLearned Mode = "learned"
	// ModeDecide lets the service's suggestion decide every progressive
	// call the rules leave open, and a call it cannot answer waits for a
	// person.
	ModeDecide Mode = "decide"
)

// ParseMode reads a mode by its name.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case ModeShadow, ModeLearned, ModeDecide:
		return m, nil
	}
	return "", fmt.Errorf("decisions: the mode %q is not shadow, learned or decide", s)
}

// OrgSource is which organization a call's key names (spec 100). The
// subject is always the session's initiator.
type OrgSource string

const (
	// OrgInitiator keys every call by the initiator's subject, a person's
	// own context, whoever owns the session's agent.
	OrgInitiator OrgSource = "initiator"
	// OrgAgent keys a call of a session whose agent an organization owns
	// by that organization's id, the session's Org, and every other call
	// as OrgInitiator does.
	OrgAgent OrgSource = "agent"
)

// ParseOrgSource reads an organization source by its name.
func ParseOrgSource(s string) (OrgSource, error) {
	switch o := OrgSource(s); o {
	case OrgInitiator, OrgAgent:
		return o, nil
	}
	return "", fmt.Errorf("decisions: the organization source %q is not initiator or agent", s)
}

// Decider decides calls with a decision service.
type Decider struct {
	base   string
	tokens func(ctx context.Context) (string, error)
	org    OrgSource
	client *http.Client
	limit  time.Duration
	// confirm bounds a confirm-mode prediction.
	confirm time.Duration
	rand    func() float64
	report  func(error)
	// mode is how much the service decides in progressive, and loosen
	// when its allow decides.
	mode   Mode
	loosen Loosen
	// pending holds the confirm-mode decisions still being sent, by key,
	// each closed when its send ends, so an answer is never sent before the
	// decision it answers.
	mu      sync.Mutex
	pending map[key]chan struct{}
}

var (
	_ harness.Decider = (*Decider)(nil)
	_ harness.Learner = (*Decider)(nil)
)

// New checks the options and returns a decider.
func New(o Options) (*Decider, error) {
	u, err := url.Parse(strings.TrimSpace(o.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("decisions: the service URL %q is not an http or https URL", o.URL)
	}
	tokens := o.Tokens
	switch token := strings.TrimSpace(o.Token); {
	case token != "" && tokens != nil:
		return nil, errors.New("decisions: both a fixed token and a token source are set")
	case token != "":
		tokens = func(context.Context) (string, error) { return token, nil }
	case tokens == nil:
		return nil, errors.New("decisions: the service URL is set and its token is not")
	}
	if o.Org != "" {
		if _, err := ParseOrgSource(string(o.Org)); err != nil {
			return nil, err
		}
	}
	if o.Loosen != "" {
		if _, err := ParseLoosen(string(o.Loosen)); err != nil {
			return nil, err
		}
	}
	d := &Decider{
		base: strings.TrimRight(u.String(), "/"), tokens: tokens, org: cmp.Or(o.Org, OrgInitiator),
		client: o.Client, limit: cmp.Or(o.Timeout, Timeout), confirm: cmp.Or(o.ConfirmTimeout, ConfirmTimeout),
		rand: o.Rand, report: o.Report, mode: cmp.Or(o.Mode, ModeDecide), loosen: cmp.Or(o.Loosen, LoosenCovered), pending: map[key]chan struct{}{},
	}
	if d.client == nil {
		d.client = otel.HTTPClient()
	}
	if d.report == nil {
		d.report = func(error) {}
	}
	if d.rand == nil {
		d.rand = rand.Float64
	}
	return d, nil
}

// key is the wire key of a call: the organization, the subject whose
// approvals are learned, and the session-scoped action id. The
// organization is the initiator's subject, a person's own context, or,
// with OrgAgent, the organization that owns the session's agent (spec
// 100).
type key struct {
	Org      string `json:"org"`
	Subject  string `json:"subject"`
	ActionID string `json:"action_id"`
}

func (d *Decider) key(s session.Session, toolUseID string) key {
	org := s.Initiator.Subject
	if d.org == OrgAgent && s.Org != "" {
		org = s.Org
	}
	return key{Org: org, Subject: s.Initiator.Subject, ActionID: s.ID + "/" + toolUseID}
}

type actor struct {
	Kind    string `json:"kind"`
	ID      string `json:"id,omitempty"`
	Session string `json:"session,omitempty"`
}

// environment is where a call runs: a host with credentials or a
// sandbox, the limits it enforces whatever the call does, and the
// visibility of the session's repositories (spec 101).
type environment struct {
	Kind         string   `json:"kind"`
	Credentialed bool     `json:"credentialed"`
	Holds        []string `json:"holds,omitempty"`
	Repositories string   `json:"repositories,omitempty"`
}

// scope is the reach Topos claims for a call (spec 101): whether it can
// be undone, the boundaries it crosses as far as Topos knows, and whether
// it is a first contact. Every value is a word of a closed list.
type scope struct {
	Undo    string           `json:"undo,omitempty"`
	Crosses *session.Crosses `json:"crosses,omitempty"`
	Contact string           `json:"contact,omitempty"`
}

// target is what a call changes: its kind, an opaque id where it has
// one, never a path or a host, and its class.
type target struct {
	Kind  string `json:"kind"`
	ID    string `json:"id,omitempty"`
	Class string `json:"class,omitempty"`
}

type signal struct {
	Source string  `json:"source"`
	Score  float64 `json:"score"`
}

// action is a call as the service reads it.
type action struct {
	key
	Actor       actor           `json:"actor"`
	Kind        string          `json:"kind"`
	Name        string          `json:"name"`
	Effect      string          `json:"effect"`
	Input       json.RawMessage `json:"input,omitempty"`
	Scope       *scope          `json:"scope,omitempty"`
	Target      *target         `json:"target,omitempty"`
	Environment environment     `json:"environment"`
	// Attributes are observable facts of the call beside its fields:
	// attended, whether a person began its turn (spec 087), so a service
	// can weigh a call nobody watches; depth, how deep it was delegated;
	// and what a thread tool's call hands over (spec 101).
	Attributes map[string]any `json:"attributes,omitempty"`
	Signals    []signal       `json:"signals,omitempty"`
}

type thresholds struct {
	AllowAbove float64 `json:"allow_above"`
	BlockBelow float64 `json:"block_below"`
}

// prediction is the service's suggestion.
type prediction struct {
	Verdict     string     `json:"verdict"`
	Approve     float64    `json:"approve"`
	Uncertainty float64    `json:"uncertainty"`
	Thresholds  thresholds `json:"thresholds"`
	AuditRate   float64    `json:"audit_rate"`
	Source      string     `json:"source"`
	Reason      string     `json:"reason"`
	// EvidenceRatio and AskKind say how much the person's answers settle
	// the prediction; a service that predates them sends neither.
	EvidenceRatio *float64 `json:"evidence_ratio,omitempty"`
	AskKind       string   `json:"ask_kind,omitempty"`
	// Scope is the reach the service read, the wider of its reading of
	// the input and what Topos sent, and Cover whether the person's
	// approvals cover the call (spec 101); a service that predates them
	// sends neither.
	Scope *readScope `json:"scope,omitempty"`
	Cover *cover     `json:"cover,omitempty"`
}

// cover says whether the person's approvals cover a call.
type cover struct {
	Covered bool `json:"covered"`
}

// covered reports whether the prediction says the person's approvals
// cover the call.
func (p prediction) covered() bool { return p.Cover != nil && p.Cover.Covered }

// readScope is the scope a prediction carries. Each dimension may be
// unknown, and a boundary of crosses may be a boolean or one of yes, no
// and unknown, so each is read leniently: a value outside a scope's
// lists is not known, and is left out of what the call records.
type readScope struct {
	Undo        string                     `json:"undo"`
	Crosses     map[string]json.RawMessage `json:"crosses"`
	TargetClass string                     `json:"target_class"`
	TargetKind  string                     `json:"target_kind"`
	Contact     string                     `json:"contact"`
}

// record is the scope as a call records it: the words of the lists
// alone, and no boundary that is not known.
func (r readScope) record() session.CallScope {
	known := func(order []string, v string) string {
		if slices.Contains(order, v) {
			return v
		}
		return ""
	}
	out := session.CallScope{
		Undo: known(session.Undos, r.Undo), TargetClass: known(session.TargetClasses, r.TargetClass),
		TargetKind: known(session.TargetKinds, r.TargetKind), Contact: known(session.Contacts, r.Contact),
		Source: session.ScopeFromService,
	}
	c := session.Crosses{
		Machine: boundary(r.Crosses["machine"]), Remote: boundary(r.Crosses["remote"]), Network: boundary(r.Crosses["network"]),
		Organization: boundary(r.Crosses["organization"]), Public: boundary(r.Crosses["public"]),
	}
	if !c.Empty() {
		out.Crosses = &c
	}
	return out
}

// boundary reads one boundary of a prediction's crosses: true or yes,
// false or no, and nil for anything else, unknown included.
func boundary(raw json.RawMessage) *bool {
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return &b
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "yes":
			return new(true)
		case "no":
			return new(false)
		}
	}
	return nil
}

type decision struct {
	key
	Verdict           string  `json:"verdict"`
	ReviewProbability float64 `json:"review_probability"`
	Source            string  `json:"source"`
	Action            *action `json:"action,omitempty"`
}

// answer is a person's answer to a call, with how the person answered
// and how long the call waited (spec 101); a field the client did not
// measure is left out.
type answer struct {
	key
	Approve         bool   `json:"approve"`
	By              string `json:"by,omitempty"`
	AnswerMS        *int64 `json:"answer_ms,omitempty"`
	WaitedMS        int64  `json:"waited_ms"`
	DetailsOpened   *bool  `json:"details_opened,omitempty"`
	SuggestionShown *bool  `json:"suggestion_shown,omitempty"`
	Surface         string `json:"surface,omitempty"`
	Remembered      bool   `json:"remembered"`
}

func (d *Decider) action(c harness.Call, risk session.Risk) action {
	env := environment{Kind: "host", Credentialed: true}
	if c.MachineKind == machine.KindCella {
		env = environment{Kind: "sandbox"}
	}
	env.Holds, env.Repositories = c.Holds, c.Repositories
	// A thread tool's input is the agent's own words to a subagent or an
	// advisor, its task or its message, which no part of the contract
	// carries (spec 101).
	input := c.Input
	switch harness.ToolName(c.Name) {
	case harness.ToolAgent, harness.ToolSendMessage, harness.ToolAdvisor:
		input = nil
	}
	a := action{
		key:         d.key(c.Session, c.ToolUseID),
		Actor:       actor{Kind: "agent", ID: cmp.Or(c.Session.Agent.ID, c.Session.Agent.Name), Session: c.Session.ID},
		Kind:        "tool",
		Name:        c.Name,
		Effect:      string(c.Props.Effect),
		Input:       input,
		Environment: env,
		Attributes:  map[string]any{AttributeAttended: c.Attended, AttributeDepth: c.Depth},
	}
	if a.Effect == "" {
		a.Effect = "external"
	}
	if r := c.Reach; r.Undo != "" || r.Contact != "" || !r.Crosses.Empty() {
		a.Scope = &scope{Undo: r.Undo, Contact: r.Contact}
		if !r.Crosses.Empty() {
			crosses := r.Crosses
			a.Scope.Crosses = &crosses
		}
	}
	if t := c.Reach.Target; t != nil && t.Kind != "" {
		a.Target = &target{Kind: t.Kind, ID: t.ID, Class: t.Class}
	}
	// A thread tool's call carries what it hands over and never the task.
	if h := c.Handoff; h != nil {
		a.Attributes[AttributeHandoffWrites] = h.Writes
		a.Attributes[AttributeHandoffExternal] = h.External
		if h.Isolation != "" {
			a.Attributes[AttributeHandoffIsolation] = h.Isolation
		}
	}
	if risk.Source != "" {
		a.Signals = []signal{{Source: risk.Source, Score: risk.Score}}
	}
	return a
}

// Decide implements [harness.Decider].
func (d *Decider) Decide(ctx context.Context, c harness.Call) (session.Risk, harness.Decision, error) {
	risk, rules, err := harness.Rules{}.Decide(ctx, c)
	if err != nil {
		return risk, rules, err
	}
	// Plan mode does not ask the service, and sends it nothing.
	if harness.ModeOn(c.Policy.Mode, c.MachineKind) == harness.ModePlan {
		return risk, rules, nil
	}
	a := d.action(c, risk)
	out := d.decide(ctx, c, risk, a, rules)
	// Every other mode sends the decision under the call's key, whose
	// organization the call records (spec 100).
	out.Org = a.Org
	return risk, out, nil
}

// decide decides a call outside plan mode and sends the decision. Every
// decision records the reach the call was decided on: the service's
// reading joined with Topos's claims when a prediction carried one, and
// Topos's claims alone otherwise (spec 101).
func (d *Decider) decide(ctx context.Context, c harness.Call, risk session.Risk, a action, rules harness.Decision) harness.Decision {
	claimed := harness.ClaimedScope(c.Reach)
	if harness.ModeOn(c.Policy.Mode, c.MachineKind) != harness.ModeProgressive {
		return d.watch(ctx, a, rules, SourceConfirm, claimed)
	}
	if d.mode == ModeShadow {
		return d.watch(ctx, a, rules, SourceProgressive, claimed)
	}
	// While the session asks for every change, the service is asked as in
	// confirm mode: its suggestion is recorded, the decision sent, and the
	// person's answer teaches it (spec 101).
	if out, ok := inFallback(c, risk, rules); ok {
		return d.watch(ctx, a, out, SourceProgressive, claimed)
	}
	if settled, ok := c.Policy.Settle(c, risk); ok {
		settled.Scope = &claimed
		d.record(ctx, a, settled, SourceProgressive, false)
		return settled
	}
	p, perr := d.predict(ctx, a)
	scope := &claimed
	if perr == nil && p.Scope != nil {
		joined := p.Scope.record().Join(claimed)
		scope = &joined
	}
	if d.mode == ModeLearned && (perr != nil || !p.hasEvidence()) {
		if perr != nil {
			d.report(perr)
		}
		return d.byRules(ctx, a, rules, p, perr == nil, scope)
	}
	if perr != nil {
		d.report(perr)
		v := verdict.OnFailure(verdict.Allow)
		out := harness.Decision{Verdict: v, Reason: "The decision service did not answer. The call waits for a person.", ReviewProbability: 1, DecidedBy: session.DecidedFallback, Scope: scope}
		d.record(ctx, a, out, SourceProgressive, false)
		return out
	}
	if p.Verdict == string(verdict.Allow) && !d.loosens(p, rules) {
		return d.byRules(ctx, a, rules, p, true, scope)
	}
	u := d.rand()
	v, prob := verdict.Decide(verdict.Verdict(p.Verdict), verdict.Allow, p.AuditRate, u)
	out := harness.Decision{Verdict: v, Reason: reason(p, v), ReviewProbability: prob, Draw: &u, Suggestion: suggestion(p), DecidedBy: session.DecidedLearned, Rule: p.Source, Scope: scope}
	d.record(ctx, a, out, SourceProgressive, true)
	return out
}

// loosens reports whether a learned allow decides a call under the
// decider's Loosen (spec 101): under covered, when the person's approvals
// cover the call; under never, when the rules' verdict is an allow too.
// No allow lifts a call above the rules' ceiling, which Settle applied
// first.
func (d *Decider) loosens(p prediction, rules harness.Decision) bool {
	if d.loosen == LoosenNever {
		return harness.Stricter(verdict.Allow, rules.Verdict) == verdict.Allow
	}
	return p.covered()
}

// inFallback is the decision of a progressive call that changes something
// while the session asks for every one (spec 101), and false for any
// other call: a call the rules' ceiling blocks or asks about keeps that
// decision, and every other asks, decided_by denials, whatever an
// always_allow or a remembered pattern would allow.
func inFallback(c harness.Call, risk session.Risk, rules harness.Decision) (harness.Decision, bool) {
	if !c.Fallback || c.Memory || !harness.Changes(c.Props) {
		return rules, false
	}
	out := rules
	if settled, ok := c.Policy.Settle(c, risk); ok && harness.Stricter(settled.Verdict, verdict.Ask) == settled.Verdict {
		out = settled
		out.Scope = rules.Scope
	}
	return harness.InFallback(c, out), true
}

// byRules applies the rules' decision of a progressive call the service
// does not decide: in learned mode, where it has no evidence about the
// person, and for an allow its Loosen leaves to the rules. The risk score
// decides as without a service, an automatic verdict is picked for review
// at the service's audit rate when it answered, and the suggestion is
// recorded beside the decision. decided_by stays the rules' layer.
func (d *Decider) byRules(ctx context.Context, a action, rules harness.Decision, p prediction, predicted bool, scope *session.CallScope) harness.Decision {
	if predicted {
		u := d.rand()
		v, prob := verdict.Decide(rules.Verdict, verdict.Allow, p.AuditRate, u)
		rules.Verdict, rules.ReviewProbability, rules.Draw = v, prob, &u
		rules.Suggestion = suggestion(p)
	}
	rules.Scope = scope
	d.record(ctx, a, rules, SourceProgressive, predicted)
	return rules
}

// hasEvidence is whether a prediction rests on the person's answers in
// learned mode: an allow, a block, or an ask the person's answers keep
// for them, split or wider (spec 101). An unsure, a short or a few ask,
// and an ask from a service that does not say which ask it is or names
// one the harness does not know, do not.
func (p prediction) hasEvidence() bool {
	return p.Verdict != string(verdict.Ask) || p.AskKind == AskSplit || p.AskKind == AskWider
}

// The kinds of ask a prediction can carry (spec 101).
const (
	// AskUnsure is an ask with an evidence ratio below 0.5: the person's
	// answers say little about the call yet, and more answers can move it.
	AskUnsure = "unsure"
	// AskSplit is an ask with an evidence ratio of 0.5 or more whose
	// answers disagree: the person allowed and denied calls like it, and
	// the approval probability is still between the thresholds.
	AskSplit = "split"
	// AskShort is an ask with an evidence ratio of 0.5 or more whose
	// answers agree, with the approval probability still in the ask band:
	// more answers that agree can move it past a threshold.
	AskShort = "short"
	// AskWider is an ask for a call that reaches wider than any call of
	// its kind the person allowed.
	AskWider = "wider"
	// AskFew is an ask for a call of a reach the person allowed, but not
	// with enough considered answers or in enough sessions.
	AskFew = "few"
)

// watch records the rules' decision with the service's suggestion beside
// it, the prediction bounded by the confirm timeout and the decision sent
// in the background: the service is asked and learns, and decides nothing.
// The decision records the reach the service read joined with claimed,
// or claimed alone.
func (d *Decider) watch(ctx context.Context, a action, rules harness.Decision, mode string, claimed session.CallScope) harness.Decision {
	p, perr := d.predictWithin(ctx, a, d.confirm)
	rules.Scope = &claimed
	if perr != nil {
		d.report(perr)
	} else {
		rules.Suggestion = suggestion(p)
		if p.Scope != nil {
			joined := p.Scope.record().Join(claimed)
			rules.Scope = &joined
		}
	}
	d.recordLater(ctx, a, rules, mode, perr == nil)
	return rules
}

// reason is the sentence the person reads for a verdict the service
// suggested. A block's reason is also the model's result, so it names the
// decision, not the person's history.
func reason(p prediction, v verdict.Verdict) string {
	switch v {
	case verdict.Allow:
		return "The decision service expects that the person approves this call."
	case verdict.Flag:
		return "The decision service expects that the person approves this call. The call runs, and the random audit selected it for review."
	case verdict.Block:
		return "The decision service expects that the person denies this call."
	}
	if p.Verdict == string(verdict.Block) {
		return "The decision service expects that the person denies this call. The random audit selected the call for review. The call waits for the person."
	}
	return "The decision service is not sure that the person approves this call. The call waits for the person."
}

func suggestion(p prediction) *session.Suggestion {
	return &session.Suggestion{
		Source: p.Source, Verdict: p.Verdict, Approve: p.Approve, Uncertainty: p.Uncertainty,
		Thresholds: session.SuggestionThresholds{AllowAbove: p.Thresholds.AllowAbove, BlockBelow: p.Thresholds.BlockBelow},
		AuditRate:  p.AuditRate, Reason: p.Reason, EvidenceRatio: p.EvidenceRatio, AskKind: p.AskKind,
	}
}

// record sends a decision under the mode's source and the layer that
// decided it; predicted says the service holds a prediction for the key,
// and otherwise the decision carries the action. A failure is not the
// session's: the call is decided either way.
func (d *Decider) record(ctx context.Context, a action, out harness.Decision, mode string, predicted bool) {
	body := decision{key: a.key, Verdict: string(out.Verdict), ReviewProbability: out.ReviewProbability, Source: Source(mode, out.DecidedBy)}
	if !predicted {
		body.Action = &a
	}
	if err := d.post(ctx, "/v1/decisions", body, nil); err != nil {
		d.report(err)
	}
}

// recordLater sends a decision in the background, under the context's
// values but not its cancellation, so the call it records is not held
// while it is sent. An answer to the same call waits for it.
func (d *Decider) recordLater(ctx context.Context, a action, out harness.Decision, mode string, predicted bool) {
	done := make(chan struct{})
	d.mu.Lock()
	d.pending[a.key] = done
	d.mu.Unlock()
	go func() {
		defer func() {
			d.mu.Lock()
			if d.pending[a.key] == done {
				delete(d.pending, a.key)
			}
			d.mu.Unlock()
			close(done)
		}()
		d.record(context.WithoutCancel(ctx), a, out, mode, predicted)
	}()
}

// sent waits until no decision for k is being sent, or ctx ends.
func (d *Decider) sent(ctx context.Context, k key) error {
	d.mu.Lock()
	done := d.pending[k]
	d.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Answered implements [harness.Learner]. It waits for the call's decision
// to be sent, so the service never receives an answer before it. The
// answer carries how the person answered and how long the call waited
// (spec 101), never the confirmation's note or pattern.
func (d *Decider) Answered(ctx context.Context, s session.Session, toolUseID string, a harness.Answer) error {
	k := d.key(s, toolUseID)
	if err := d.sent(ctx, k); err != nil {
		return err
	}
	return d.post(ctx, "/v1/answers", answer{
		key: k, Approve: a.Approve, By: a.By, AnswerMS: a.AnswerMS, WaitedMS: a.WaitedMS,
		DetailsOpened: a.DetailsOpened, SuggestionShown: a.SuggestionShown, Surface: a.Surface, Remembered: a.Remembered,
	}, nil)
}

func (d *Decider) predict(ctx context.Context, a action) (prediction, error) {
	return d.predictWithin(ctx, a, d.limit)
}

// predictWithin asks the service for a prediction under limit.
func (d *Decider) predictWithin(ctx context.Context, a action, limit time.Duration) (prediction, error) {
	var p prediction
	if err := d.postWithin(ctx, limit, "/v1/predictions", a, &p); err != nil {
		return prediction{}, err
	}
	if v, ok := verdict.Parse(p.Verdict); !ok || v == verdict.Flag {
		return prediction{}, fmt.Errorf("decisions: the service suggested %q, not allow, ask or block", p.Verdict)
	}
	return p, nil
}

// errorBody is the service's error envelope.
type errorBody struct {
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

// post sends one request under the timeout and decodes a 200's body into
// out. A refusal of a decision already recorded counts as recorded: the
// first decision for a key is the one the service keeps.
func (d *Decider) post(ctx context.Context, path string, body, out any) error {
	return d.postWithin(ctx, d.limit, path, body, out)
}

// postWithin is post under limit.
func (d *Decider) postWithin(ctx context.Context, limit time.Duration, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	token, err := d.tokens(ctx)
	if err != nil {
		return fmt.Errorf("decisions: POST %s: the token for the service: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("decisions: POST %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("decisions: POST %s: read: %w", path, err)
	}
	if resp.StatusCode/100 != 2 {
		var e errorBody
		_ = json.Unmarshal(raw, &e)
		if path == "/v1/decisions" && e.Error.Code == "already_decided" {
			return nil
		}
		return fmt.Errorf("decisions: POST %s: status %d, code %q", path, resp.StatusCode, e.Error.Code)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decisions: POST %s: decode: %w", path, err)
	}
	return nil
}
