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

// The decision sources the harness sends, by mode.
const (
	SourceConfirm     = "topos/confirm"
	SourceProgressive = "topos/progressive"
)

// Options configure a [Decider].
type Options struct {
	// URL is the service's base URL, required. Token is a fixed bearer sent
	// to it, and Tokens a source asked for the bearer at each request, as a
	// server that holds a token for the service's audience is: exactly one
	// of the two is set.
	URL    string
	Token  string
	Tokens func(ctx context.Context) (string, error)
	// Org is the organization the session acts in; empty sends the
	// initiator's subject, a person's own context.
	Org string
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
	// evidence about the person: its allow, its block, and an ask on
	// answers that disagree (ask_kind split). Where it has too few answers
	// (ask_kind unsure), where its prediction carries no ask_kind, or where
	// it does not answer, the rules and the risk score decide, as without a
	// service, with the suggestion recorded beside them.
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

// Decider decides calls with a decision service.
type Decider struct {
	base   string
	tokens func(ctx context.Context) (string, error)
	org    string
	client *http.Client
	limit  time.Duration
	// confirm bounds a confirm-mode prediction.
	confirm time.Duration
	rand    func() float64
	report  func(error)
	// mode is how much the service decides in progressive.
	mode Mode
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
	d := &Decider{
		base: strings.TrimRight(u.String(), "/"), tokens: tokens, org: o.Org,
		client: o.Client, limit: cmp.Or(o.Timeout, Timeout), confirm: cmp.Or(o.ConfirmTimeout, ConfirmTimeout),
		rand: o.Rand, report: o.Report, mode: cmp.Or(o.Mode, ModeDecide), pending: map[key]chan struct{}{},
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
// approvals are learned, and the session-scoped action id.
type key struct {
	Org      string `json:"org"`
	Subject  string `json:"subject"`
	ActionID string `json:"action_id"`
}

func (d *Decider) key(s session.Session, toolUseID string) key {
	return key{Org: cmp.Or(d.org, s.Initiator.Subject), Subject: s.Initiator.Subject, ActionID: s.ID + "/" + toolUseID}
}

type actor struct {
	Kind    string `json:"kind"`
	ID      string `json:"id,omitempty"`
	Session string `json:"session,omitempty"`
}

type environment struct {
	Kind         string `json:"kind"`
	Credentialed bool   `json:"credentialed"`
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
	Environment environment     `json:"environment"`
	// Attributes are observable facts of the call beside its fields:
	// attended, whether a person began its turn (spec 087), so a service
	// can weigh a call nobody watches.
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
}

type decision struct {
	key
	Verdict           string  `json:"verdict"`
	ReviewProbability float64 `json:"review_probability"`
	Source            string  `json:"source"`
	Action            *action `json:"action,omitempty"`
}

type answer struct {
	key
	Approve bool   `json:"approve"`
	By      string `json:"by,omitempty"`
}

func (d *Decider) action(c harness.Call, risk session.Risk) action {
	env := environment{Kind: "host", Credentialed: true}
	if c.MachineKind == machine.KindCella {
		env = environment{Kind: "sandbox"}
	}
	a := action{
		key:         d.key(c.Session, c.ToolUseID),
		Actor:       actor{Kind: "agent", ID: cmp.Or(c.Session.Agent.ID, c.Session.Agent.Name), Session: c.Session.ID},
		Kind:        "tool",
		Name:        c.Name,
		Effect:      string(c.Props.Effect),
		Input:       c.Input,
		Environment: env,
		Attributes:  map[string]any{AttributeAttended: c.Attended},
	}
	if a.Effect == "" {
		a.Effect = "external"
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
	a := d.action(c, risk)
	switch harness.ModeOn(c.Policy.Mode, c.MachineKind) {
	case harness.ModePlan:
		return risk, rules, nil
	case harness.ModeProgressive:
		if d.mode == ModeShadow {
			return risk, d.watch(ctx, a, rules, SourceProgressive), nil
		}
		if settled, ok := c.Policy.Settle(c, risk); ok {
			d.record(ctx, a, settled, SourceProgressive, false)
			return risk, settled, nil
		}
		p, perr := d.predict(ctx, a)
		if d.mode == ModeLearned && (perr != nil || !p.hasEvidence()) {
			if perr != nil {
				d.report(perr)
			}
			return risk, d.byRules(ctx, a, rules, p, perr == nil), nil
		}
		if perr != nil {
			d.report(perr)
			v := verdict.OnFailure(verdict.Allow)
			out := harness.Decision{Verdict: v, Reason: "The decision service did not answer. The call waits for a person.", ReviewProbability: 1, DecidedBy: session.DecidedFallback}
			d.record(ctx, a, out, SourceProgressive, false)
			return risk, out, nil
		}
		u := d.rand()
		v, prob := verdict.Decide(verdict.Verdict(p.Verdict), verdict.Allow, p.AuditRate, u)
		out := harness.Decision{Verdict: v, Reason: reason(p, v), ReviewProbability: prob, Draw: &u, Suggestion: suggestion(p), DecidedBy: session.DecidedLearned, Rule: p.Source}
		d.record(ctx, a, out, SourceProgressive, true)
		return risk, out, nil
	default:
		return risk, d.watch(ctx, a, rules, SourceConfirm), nil
	}
}

// byRules applies the rules' decision of a progressive call in learned
// mode, where the service has no evidence about the person: the risk score
// decides as without a service, an automatic verdict is picked for review
// at the service's audit rate when it answered, and the suggestion is
// recorded beside the decision. decided_by stays the rules' layer.
func (d *Decider) byRules(ctx context.Context, a action, rules harness.Decision, p prediction, predicted bool) harness.Decision {
	if predicted {
		u := d.rand()
		v, prob := verdict.Decide(rules.Verdict, verdict.Allow, p.AuditRate, u)
		rules.Verdict, rules.ReviewProbability, rules.Draw = v, prob, &u
		rules.Suggestion = suggestion(p)
	}
	d.record(ctx, a, rules, SourceProgressive, predicted)
	return rules
}

// hasEvidence is whether a prediction rests on the person's answers: an
// allow, a block, or an ask on answers that disagree. An ask with too few
// answers, or from a service that does not say which ask it is, does not.
func (p prediction) hasEvidence() bool {
	return p.Verdict != string(verdict.Ask) || p.AskKind == AskSplit
}

// The kinds of ask a prediction can carry.
const (
	AskUnsure = "unsure"
	AskSplit  = "split"
)

// watch records the rules' decision with the service's suggestion beside
// it, the prediction bounded by the confirm timeout and the decision sent
// in the background: the service is asked and learns, and decides nothing.
func (d *Decider) watch(ctx context.Context, a action, rules harness.Decision, source string) harness.Decision {
	p, perr := d.predictWithin(ctx, a, d.confirm)
	if perr != nil {
		d.report(perr)
	} else {
		rules.Suggestion = suggestion(p)
	}
	d.recordLater(ctx, a, rules, source, perr == nil)
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
		AuditRate:  p.AuditRate, Reason: p.Reason,
	}
}

// record sends a decision; predicted says the service holds a prediction for
// the key, and otherwise the decision carries the action. A failure is not
// the session's: the call is decided either way.
func (d *Decider) record(ctx context.Context, a action, out harness.Decision, source string, predicted bool) {
	body := decision{key: a.key, Verdict: string(out.Verdict), ReviewProbability: out.ReviewProbability, Source: source}
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
func (d *Decider) recordLater(ctx context.Context, a action, out harness.Decision, source string, predicted bool) {
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
		d.record(context.WithoutCancel(ctx), a, out, source, predicted)
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
// to be sent, so the service never receives an answer before it.
func (d *Decider) Answered(ctx context.Context, s session.Session, toolUseID string, approve bool, by string) error {
	k := d.key(s, toolUseID)
	if err := d.sent(ctx, k); err != nil {
		return err
	}
	return d.post(ctx, "/v1/answers", answer{key: k, Approve: approve, By: by}, nil)
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
