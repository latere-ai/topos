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
	"time"

	"latere.ai/x/pkg/otel"
	"latere.ai/x/pkg/verdict"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// Timeout bounds every request to the service.
const Timeout = 2 * time.Second

// The decision sources the harness sends, by mode.
const (
	SourceConfirm     = "topos/confirm"
	SourceProgressive = "topos/progressive"
)

// Options configure a [Decider].
type Options struct {
	// URL is the service's base URL; Token the bearer sent to it. Both are
	// required.
	URL   string
	Token string
	// Org is the organization the session acts in; empty sends the
	// initiator's subject, a person's own context.
	Org string
	// Client sends the requests; nil is pkg/otel's instrumented client.
	Client *http.Client
	// Timeout bounds each request; zero is [Timeout].
	Timeout time.Duration
	// Rand draws the review's uniform number; nil is math/rand/v2.
	Rand func() float64
	// Report receives a failure the decider recovered from: a prediction
	// that failed, so the call was asked, or a decision or answer that could
	// not be sent. Nil drops them.
	Report func(error)
}

// Decider decides calls with a decision service.
type Decider struct {
	base   string
	token  string
	org    string
	client *http.Client
	limit  time.Duration
	rand   func() float64
	report func(error)
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
	if strings.TrimSpace(o.Token) == "" {
		return nil, errors.New("decisions: the service URL is set and its token is not")
	}
	d := &Decider{
		base: strings.TrimRight(u.String(), "/"), token: o.Token, org: o.Org,
		client: o.Client, limit: cmp.Or(o.Timeout, Timeout), rand: o.Rand, report: o.Report,
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
	Signals     []signal        `json:"signals,omitempty"`
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
	switch cmp.Or(c.Policy.Mode, harness.ModeConfirm) {
	case harness.ModePlan:
		return risk, rules, nil
	case harness.ModeProgressive:
		if settled, ok := c.Policy.Settle(c, risk); ok {
			d.record(ctx, a, settled, SourceProgressive, false)
			return risk, settled, nil
		}
		p, perr := d.predict(ctx, a)
		if perr != nil {
			d.report(perr)
			v := verdict.OnFailure(verdict.Allow)
			out := harness.Decision{Verdict: v, Reason: "The decision service did not answer. The call waits for a person.", ReviewProbability: 1}
			d.record(ctx, a, out, SourceProgressive, false)
			return risk, out, nil
		}
		u := d.rand()
		v, prob := verdict.Decide(verdict.Verdict(p.Verdict), verdict.Allow, p.AuditRate, u)
		out := harness.Decision{Verdict: v, Reason: reason(p, v), ReviewProbability: prob, Draw: &u, Suggestion: suggestion(p)}
		d.record(ctx, a, out, SourceProgressive, true)
		return risk, out, nil
	default:
		p, perr := d.predict(ctx, a)
		if perr != nil {
			d.report(perr)
		} else {
			rules.Suggestion = suggestion(p)
		}
		d.record(ctx, a, rules, SourceConfirm, perr == nil)
		return risk, rules, nil
	}
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

// Answered implements [harness.Learner].
func (d *Decider) Answered(ctx context.Context, s session.Session, toolUseID string, approve bool, by string) error {
	return d.post(ctx, "/v1/answers", answer{key: d.key(s, toolUseID), Approve: approve, By: by}, nil)
}

func (d *Decider) predict(ctx context.Context, a action) (prediction, error) {
	var p prediction
	if err := d.post(ctx, "/v1/predictions", a, &p); err != nil {
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
	ctx, cancel := context.WithTimeout(ctx, d.limit)
	defer cancel()
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.token)
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
