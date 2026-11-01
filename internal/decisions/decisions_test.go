// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package decisions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/verdict"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// service is a fake decision service: it answers predictions with next,
// records every request, and fails or stalls on demand: status fails
// every request, and unpredicted fails only the predictions.
type service struct {
	mu          sync.Mutex
	next        prediction
	status      int
	unpredicted bool
	code        string
	stall       time.Duration
	token       string
	reported    int
	predicted   []action
	decided     []decision
	answered    []answer
}

func (s *service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stall > 0 {
		s.mu.Unlock()
		time.Sleep(s.stall)
		s.mu.Lock()
	}
	s.token = r.Header.Get("Authorization")
	if s.status != 0 {
		w.WriteHeader(s.status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": s.code}})
		return
	}
	switch r.URL.Path {
	case "/v1/predictions":
		var a action
		_ = json.NewDecoder(r.Body).Decode(&a)
		s.predicted = append(s.predicted, a)
		if s.unpredicted {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(s.next)
	case "/v1/decisions":
		var d decision
		_ = json.NewDecoder(r.Body).Decode(&d)
		s.decided = append(s.decided, d)
		w.WriteHeader(http.StatusNoContent)
	case "/v1/answers":
		var a answer
		_ = json.NewDecoder(r.Body).Decode(&a)
		s.answered = append(s.answered, a)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// settle waits until every decision d sends in the background is sent.
func settle(d *Decider) {
	for {
		d.mu.Lock()
		var done chan struct{}
		for _, c := range d.pending {
			done = c
			break
		}
		d.mu.Unlock()
		if done == nil {
			return
		}
		<-done
	}
}

func newDecider(t *testing.T, s *service, draw float64) *Decider {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	d, err := New(Options{URL: srv.URL + "/", Token: "tok", Rand: func() float64 { return draw }, Timeout: 200 * time.Millisecond,
		Report: func(err error) { s.mu.Lock(); s.reported++; s.mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

var sess = session.Session{
	ID:        "ses_1",
	Agent:     session.AgentRef{ID: "agt_1", Name: "coder"},
	Initiator: session.Sender{Subject: "https://issuer.example|usr_42", Kind: session.SenderPerson},
}

func bash(mode harness.Mode, command string) harness.Call {
	return harness.Call{
		Policy:      harness.Policy{Mode: mode},
		Session:     sess,
		ToolUseID:   "toolu_1",
		Name:        "bash",
		Props:       tools.Properties{Effect: tools.EffectWrite},
		Input:       json.RawMessage(`{"command":"` + command + `"}`),
		MachineKind: machine.KindCella,
	}
}

func allowSuggestion() prediction {
	return prediction{Verdict: "allow", Approve: 0.97, Uncertainty: 0.2, Thresholds: thresholds{AllowAbove: 0.9, BlockBelow: 0.25}, AuditRate: 0.05, Source: "learned/1", Reason: "approved often"}
}

func TestProgressiveTakesTheSuggestion(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name    string
		draw    float64
		want    harness.Verdict
		reviewP float64
	}{
		{"not drawn", 0.5, harness.VerdictAllow, 0.05},
		{"drawn for review", 0.01, harness.VerdictFlag, 0.05},
	} {
		s := &service{next: allowSuggestion()}
		d := newDecider(t, s, c.draw)
		risk, out, err := d.Decide(ctx, bash(harness.ModeProgressive, "go test ./..."))
		if err != nil {
			t.Fatal(err)
		}
		if out.Verdict != c.want || out.ReviewProbability != c.reviewP || out.Draw == nil || *out.Draw != c.draw {
			t.Errorf("%s: %+v", c.name, out)
		}
		if out.Suggestion == nil || out.Suggestion.Source != "learned/1" || out.Suggestion.Approve != 0.97 || out.Suggestion.Thresholds.AllowAbove != 0.9 {
			t.Errorf("%s: suggestion %+v", c.name, out.Suggestion)
		}
		if risk.Source != harness.RiskSource {
			t.Errorf("%s: risk %+v", c.name, risk)
		}
		if len(s.predicted) != 1 || len(s.decided) != 1 {
			t.Fatalf("%s: %d predictions, %d decisions", c.name, len(s.predicted), len(s.decided))
		}
		a, dec := s.predicted[0], s.decided[0]
		if a.Org != sess.Initiator.Subject || a.Subject != sess.Initiator.Subject || a.ActionID != "ses_1/toolu_1" ||
			a.Kind != "tool" || a.Name != "bash" || a.Effect != "write" || a.Actor.Kind != "agent" || a.Actor.ID != "agt_1" ||
			a.Environment.Kind != "sandbox" || len(a.Signals) != 1 || a.Signals[0].Source != harness.RiskSource {
			t.Errorf("%s: action %+v", c.name, a)
		}
		if dec.Verdict != string(c.want) || dec.ReviewProbability != c.reviewP || dec.Source != "topos/progressive/learned" || dec.Action != nil {
			t.Errorf("%s: decision %+v", c.name, dec)
		}
		if s.token != "Bearer tok" {
			t.Errorf("%s: bearer %q", c.name, s.token)
		}
	}
}

// A suggested block drawn for review is held for the person; one not drawn
// is blocked.
func TestProgressiveBlock(t *testing.T) {
	block := allowSuggestion()
	block.Verdict, block.Approve = "block", 0.05
	s := &service{next: block}
	_, out, _ := newDecider(t, s, 0.01).Decide(context.Background(), bash(harness.ModeProgressive, "rm -rf build"))
	if out.Verdict != harness.VerdictAsk || out.ReviewProbability != 0.05 || !strings.Contains(out.Reason, "review") {
		t.Errorf("drawn block %+v", out)
	}
	_, out, _ = newDecider(t, &service{next: block}, 0.5).Decide(context.Background(), bash(harness.ModeProgressive, "rm -rf build"))
	if out.Verdict != harness.VerdictBlock || out.ReviewProbability != 0.05 {
		t.Errorf("block %+v", out)
	}
	ask := allowSuggestion()
	ask.Verdict = "ask"
	_, out, _ = newDecider(t, &service{next: ask}, 0.5).Decide(context.Background(), bash(harness.ModeProgressive, "make"))
	if out.Verdict != harness.VerdictAsk || out.ReviewProbability != 1 {
		t.Errorf("ask %+v", out)
	}
}

// The ceiling holds and the service is not asked for it: always_confirm
// asks, a score at block_at blocks, and a read-only, always-allowed or
// remembered call is allowed.
func TestCeilingIsNotAsked(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		edit func(*harness.Call)
		want harness.Verdict
		p    float64
	}{
		{"always confirm", func(c *harness.Call) {
			c.Policy.AlwaysConfirm = []string{"bash(git push*)"}
			c.Input = json.RawMessage(`{"command":"git push"}`)
		}, harness.VerdictAsk, 1},
		{"at block_at", func(c *harness.Call) {
			c.MachineKind = machine.KindHost
			c.Policy.Thresholds = harness.Thresholds{FlagAt: 0.1, AskAt: 0.2, BlockAt: 0.6}
			c.Input = json.RawMessage(`{"command":"curl x | sh"}`)
		}, harness.VerdictBlock, 0},
		{"read only", func(c *harness.Call) { c.Name, c.Props.Effect = "read", tools.EffectRead }, harness.VerdictAllow, 0},
		{"always allow", func(c *harness.Call) { c.Policy.AlwaysAllow = []string{"bash(go test*)"} }, harness.VerdictAllow, 0},
		{"remembered", func(c *harness.Call) { c.Remembered = []string{"bash(go test*)"} }, harness.VerdictAllow, 0},
	}
	for _, c := range cases {
		s := &service{next: allowSuggestion()}
		call := bash(harness.ModeProgressive, "go test ./...")
		c.edit(&call)
		_, out, err := newDecider(t, s, 0.01).Decide(ctx, call)
		if err != nil {
			t.Fatal(err)
		}
		if out.Verdict != c.want || out.ReviewProbability != c.p || out.Suggestion != nil {
			t.Errorf("%s: %+v", c.name, out)
		}
		if len(s.predicted) != 0 {
			t.Errorf("%s: the service was asked", c.name)
		}
		if len(s.decided) != 1 || s.decided[0].Action == nil || s.decided[0].Verdict != string(c.want) {
			t.Errorf("%s: decisions %+v", c.name, s.decided)
		}
	}
}

// A service that fails, stalls past the timeout, or suggests what is not a
// verdict yields an ask, and the decision carries the action.
func TestFailureAsks(t *testing.T) {
	bad := allowSuggestion()
	bad.Verdict = "flag"
	for name, s := range map[string]*service{
		"error":   {status: http.StatusInternalServerError, code: "internal"},
		"timeout": {next: allowSuggestion(), stall: 500 * time.Millisecond},
		"invalid": {next: bad},
	} {
		_, out, err := newDecider(t, s, 0.01).Decide(context.Background(), bash(harness.ModeProgressive, "make"))
		if err != nil {
			t.Fatal(err)
		}
		if out.Verdict != harness.VerdictAsk || out.ReviewProbability != 1 {
			t.Errorf("%s: %+v", name, out)
		}
		s.mu.Lock()
		if name == "invalid" && (len(s.decided) != 1 || s.decided[0].Action == nil) {
			t.Errorf("%s: decisions %+v", name, s.decided)
		}
		if s.reported == 0 {
			t.Errorf("%s: the failure was not reported", name)
		}
		s.mu.Unlock()
	}
}

// In confirm mode the rules decide, the suggestion is recorded, not
// applied, and the decision is sent.
func TestConfirmModeTeaches(t *testing.T) {
	s := &service{next: allowSuggestion()}
	call := bash(harness.ModeConfirm, "make")
	call.MachineKind = machine.KindHost
	d := newDecider(t, s, 0.01)
	_, out, err := d.Decide(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	if out.Verdict != harness.VerdictAsk || out.ReviewProbability != 1 || out.Suggestion == nil || out.Suggestion.Verdict != "allow" || out.Draw != nil {
		t.Errorf("confirm %+v", out)
	}
	settle(d)
	if len(s.predicted) != 1 || len(s.decided) != 1 || s.decided[0].Source != "topos/confirm/confirm" || s.decided[0].Action != nil {
		t.Errorf("confirm sent %d predictions, decisions %+v", len(s.predicted), s.decided)
	}
	// A failing prediction still sends the decision, with the action.
	f := &service{status: http.StatusServiceUnavailable}
	_, out, _ = newDecider(t, f, 0.01).Decide(context.Background(), call)
	if out.Verdict != harness.VerdictAsk || out.Suggestion != nil {
		t.Errorf("confirm with a failing service %+v", out)
	}
}

// TestADeviceIsDecidedAsConfirm: on a person's own computer a call under
// progressive is decided as confirm: the service's allow is a suggestion
// and the call asks, recorded as a confirm decision (spec 080).
func TestADeviceIsDecidedAsConfirm(t *testing.T) {
	s := &service{next: allowSuggestion()}
	call := bash(harness.ModeProgressive, "make")
	call.MachineKind = machine.KindDevice
	d := newDecider(t, s, 0.5)
	_, out, err := d.Decide(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	if out.Verdict != harness.VerdictAsk || out.ReviewProbability != 1 || out.Suggestion == nil || out.Suggestion.Verdict != "allow" || out.Draw != nil {
		t.Errorf("a device under progressive %+v", out)
	}
	settle(d)
	if len(s.decided) != 1 || s.decided[0].Source != "topos/confirm/confirm" {
		t.Errorf("a device under progressive recorded %+v", s.decided)
	}
}

func TestPlanModeAsksNothing(t *testing.T) {
	s := &service{next: allowSuggestion()}
	_, out, err := newDecider(t, s, 0.01).Decide(context.Background(), bash(harness.ModePlan, "make"))
	if err != nil || out.Verdict != harness.VerdictBlock {
		t.Fatalf("plan %+v, %v", out, err)
	}
	if len(s.predicted)+len(s.decided) != 0 {
		t.Errorf("plan sent %d predictions and %d decisions", len(s.predicted), len(s.decided))
	}
}

func TestAnswered(t *testing.T) {
	s := &service{}
	d := newDecider(t, s, 0.5)
	if err := d.Answered(context.Background(), sess, "toolu_1", false, "https://issuer.example|usr_lead"); err != nil {
		t.Fatal(err)
	}
	if len(s.answered) != 1 || s.answered[0].ActionID != "ses_1/toolu_1" || s.answered[0].Approve || s.answered[0].By != "https://issuer.example|usr_lead" {
		t.Errorf("answers %+v", s.answered)
	}
	f := &service{status: http.StatusBadGateway}
	if err := newDecider(t, f, 0.5).Answered(context.Background(), sess, "toolu_1", true, ""); err == nil {
		t.Error("a failing answer reported no error")
	}
}

// A decision the service already holds counts as recorded.
func TestAlreadyDecidedIsRecorded(t *testing.T) {
	s := &service{status: http.StatusConflict, code: "already_decided"}
	d := newDecider(t, s, 0.5)
	if err := d.post(context.Background(), "/v1/decisions", decision{}, nil); err != nil {
		t.Errorf("already decided: %v", err)
	}
	s.code = "action_conflict"
	if err := d.post(context.Background(), "/v1/decisions", decision{}, nil); err == nil {
		t.Error("a conflicting decision reported no error")
	}
}

func TestNewRefuses(t *testing.T) {
	for name, o := range map[string]Options{
		"no url":     {Token: "t"},
		"not http":   {URL: "ftp://x", Token: "t"},
		"no host":    {URL: "https://", Token: "t"},
		"no token":   {URL: "https://decisions.example"},
		"bad syntax": {URL: "https://a b", Token: "t"},
		"both tokens": {URL: "https://decisions.example", Token: "t",
			Tokens: func(context.Context) (string, error) { return "u", nil }},
	} {
		if _, err := New(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := New(Options{URL: "https://decisions.example", Token: "t", Org: "org_7"}); err == nil {
		t.Error("an organization source that is neither initiator nor agent was accepted")
	}
	d, err := New(Options{URL: "https://decisions.example", Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if k := d.key(sess, "toolu_9"); k.Org != sess.Initiator.Subject || k.ActionID != "ses_1/toolu_9" {
		t.Errorf("key %+v", k)
	}
	a := d.action(harness.Call{Session: sess, ToolUseID: "x", Name: "mcp__x", MachineKind: machine.KindHost}, session.Risk{})
	if a.Effect != "external" || a.Environment.Kind != "host" || !a.Environment.Credentialed || a.Signals != nil {
		t.Errorf("action %+v", a)
	}
}

// A decoding failure or an unreachable service is an error from post.
func TestPostErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("not json")) }))
	defer srv.Close()
	d, _ := New(Options{URL: srv.URL, Token: "t"})
	var p prediction
	if err := d.post(context.Background(), "/v1/predictions", action{}, &p); err == nil {
		t.Error("a body that is not JSON decoded")
	}
	srv.Close()
	if err := d.post(context.Background(), "/v1/answers", answer{}, nil); err == nil {
		t.Error("an unreachable service reported no error")
	}
	if err := d.post(context.Background(), "/v1/answers", func() {}, nil); err == nil {
		t.Error("an unencodable body reported no error")
	}
}

// TestTheServiceIsToldWhetherAnyoneWatches (spec 087): every action the
// service reads carries attended among its attributes, true on a turn a
// person began and false on one a trigger's message began, in a
// prediction and in a decision that carries its action.
func TestTheServiceIsToldWhetherAnyoneWatches(t *testing.T) {
	for _, attended := range []bool{true, false} {
		s := &service{next: allowSuggestion()}
		d := newDecider(t, s, 0.5)
		open := bash(harness.ModeProgressive, "make")
		open.Attended = attended
		if _, _, err := d.Decide(context.Background(), open); err != nil {
			t.Fatal(err)
		}
		settled := bash(harness.ModeProgressive, "ls")
		settled.ToolUseID, settled.Props, settled.Attended = "toolu_2", tools.Properties{Effect: tools.EffectRead}, attended
		if _, _, err := d.Decide(context.Background(), settled); err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		if len(s.predicted) != 1 || s.predicted[0].Attributes[AttributeAttended] != attended {
			t.Errorf("attended %v: predictions %+v", attended, s.predicted)
		}
		if len(s.decided) != 2 || s.decided[1].Action == nil || s.decided[1].Action.Attributes[AttributeAttended] != attended {
			t.Errorf("attended %v: decisions %+v", attended, s.decided)
		}
		s.mu.Unlock()
	}
}

// A token source is asked at each request, so a server sends the token it
// holds for the service's audience as that token is renewed; a source that
// fails makes the call wait for a person, as any failure to reach the
// service does.
func TestTokenSource(t *testing.T) {
	s := &service{next: allowSuggestion()}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	n := 0
	var fail error
	d, err := New(Options{URL: srv.URL, Rand: func() float64 { return 0.5 }, Timeout: 200 * time.Millisecond,
		Tokens: func(context.Context) (string, error) { n++; return fmt.Sprintf("svc-%d", n), fail },
		Report: func(error) { s.mu.Lock(); s.reported++; s.mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	if _, out, _ := d.Decide(context.Background(), bash(harness.ModeProgressive, "go test ./...")); out.Verdict != harness.VerdictAllow {
		t.Fatalf("allowed suggestion: %+v", out)
	}
	if s.token != "Bearer svc-2" || len(s.predicted) != 1 || len(s.decided) != 1 {
		t.Errorf("bearer %q after %d predictions and %d decisions", s.token, len(s.predicted), len(s.decided))
	}
	fail = errors.New("the identity provider is down")
	if _, out, _ := d.Decide(context.Background(), bash(harness.ModeProgressive, "go test ./...")); out.Verdict != harness.VerdictAsk || s.reported == 0 {
		t.Errorf("a failing token source: %+v, %d reported", out, s.reported)
	}
}

// In confirm mode a service that hangs delays a call by no more than
// ConfirmTimeout: the prediction gives up at that bound, and the decision
// is sent in the background, whose Timeout still applies to it. An answer
// to the call waits for its decision, so it is never sent first.
func TestConfirmModeBoundsTheDelay(t *testing.T) {
	s := &service{next: allowSuggestion(), stall: 400 * time.Millisecond}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	d, err := New(Options{URL: srv.URL, Token: "tok", Timeout: 2 * time.Second, ConfirmTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	call := bash(harness.ModeConfirm, "make")
	start := time.Now()
	_, out, err := d.Decide(context.Background(), call)
	if took := time.Since(start); err != nil || took > 300*time.Millisecond {
		t.Fatalf("a hanging service held a confirm-mode call %v: %v", took, err)
	}
	// The rules' verdict stands, with no suggestion recorded.
	if out.Verdict != harness.VerdictAllow || out.Suggestion != nil {
		t.Errorf("confirm with a hanging service %+v", out)
	}
	if err := d.Answered(context.Background(), call.Session, call.ToolUseID, true, ""); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.decided) != 1 || s.decided[0].Action == nil || len(s.answered) != 1 {
		t.Errorf("after the answer: %d decisions %+v, %d answers", len(s.decided), s.decided, len(s.answered))
	}
	if ConfirmTimeout >= Timeout {
		t.Errorf("ConfirmTimeout %v is not below Timeout %v", ConfirmTimeout, Timeout)
	}
}

// An answer whose context ends while its decision is still being sent is
// not sent.
func TestAnswerWaitsForItsDecision(t *testing.T) {
	s := &service{stall: 300 * time.Millisecond}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	d, err := New(Options{URL: srv.URL, Token: "tok", ConfirmTimeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	call := bash(harness.ModeConfirm, "make")
	if _, _, err := d.Decide(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := d.Answered(ctx, call.Session, call.ToolUseID, true, ""); err == nil {
		t.Error("an answer was sent before its decision")
	}
	settle(d)
}

// In shadow, a progressive call is decided by the rules and the risk score
// as without a service, whatever the service suggests; the suggestion is
// recorded beside the decision and the service still learns.
func TestShadowLeavesProgressiveToTheRules(t *testing.T) {
	call := bash(harness.ModeProgressive, "make")
	_, want, err := harness.Rules{}.Decide(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	for _, suggested := range []string{"allow", "ask", "block"} {
		p := allowSuggestion()
		p.Verdict = suggested
		s := &service{next: p}
		srv := httptest.NewServer(s)
		t.Cleanup(srv.Close)
		d, err := New(Options{URL: srv.URL, Token: "tok", Mode: ModeShadow, Rand: func() float64 { return 0.01 }})
		if err != nil {
			t.Fatal(err)
		}
		_, out, err := d.Decide(context.Background(), call)
		if err != nil {
			t.Fatal(err)
		}
		if out.Verdict != want.Verdict || out.ReviewProbability != want.ReviewProbability || out.Draw != nil {
			t.Errorf("suggested %s: decided %+v, want the rules' %+v", suggested, out, want)
		}
		if out.Suggestion == nil || out.Suggestion.Verdict != suggested {
			t.Errorf("suggested %s: the suggestion is not recorded: %+v", suggested, out.Suggestion)
		}
		settle(d)
		if len(s.predicted) != 1 || len(s.decided) != 1 || s.decided[0].Source != "topos/progressive/score" || s.decided[0].Verdict != string(want.Verdict) {
			t.Errorf("suggested %s: sent %d predictions, decisions %+v", suggested, len(s.predicted), s.decided)
		}
	}
}

// The decision service's suggestion is recorded as having decided only
// when it did: in progressive, outside shadow. A failure is a fallback,
// and in shadow and confirm the rules' layer stands.
func TestDecidedByTheService(t *testing.T) {
	ctx := context.Background()
	call := bash(harness.ModeProgressive, "make")
	_, out, err := newDecider(t, &service{next: allowSuggestion()}, 0.99).Decide(ctx, call)
	if err != nil || out.DecidedBy != session.DecidedLearned || out.Rule != allowSuggestion().Source {
		t.Errorf("progressive: %+v, %v", out, err)
	}
	_, out, _ = newDecider(t, &service{status: http.StatusServiceUnavailable}, 0.99).Decide(ctx, call)
	if out.DecidedBy != session.DecidedFallback || out.Verdict != harness.VerdictAsk {
		t.Errorf("a failing service: %+v", out)
	}
	_, want, _ := harness.Rules{}.Decide(ctx, call)
	s := &service{next: allowSuggestion()}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	d, err := New(Options{URL: srv.URL, Token: "tok", Mode: ModeShadow})
	if err != nil {
		t.Fatal(err)
	}
	if _, out, _ = d.Decide(ctx, call); out.DecidedBy != want.DecidedBy || out.DecidedBy == session.DecidedLearned {
		t.Errorf("shadow: decided by %q, want the rules' %q", out.DecidedBy, want.DecidedBy)
	}
	settle(d)
	confirm := bash(harness.ModeConfirm, "make")
	_, wantConfirm, _ := harness.Rules{}.Decide(ctx, confirm)
	d2 := newDecider(t, &service{next: allowSuggestion()}, 0.99)
	if _, out, _ = d2.Decide(ctx, confirm); out.DecidedBy != wantConfirm.DecidedBy || out.DecidedBy == session.DecidedLearned {
		t.Errorf("confirm: decided by %q, want the rules' %q", out.DecidedBy, wantConfirm.DecidedBy)
	}
	settle(d2)
}

// In learned mode the service decides a progressive call where it has
// evidence about the person, and the rules and the risk score decide where
// it has too few answers, says no kind of ask, or does not answer.
func TestLearnedDecidesWhereItHasEvidence(t *testing.T) {
	ctx := context.Background()
	call := bash(harness.ModeProgressive, "make")
	_, rules, err := harness.Rules{}.Decide(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	learned := func(p prediction, status int) (*service, harness.Decision) {
		t.Helper()
		s := &service{next: p, status: status}
		srv := httptest.NewServer(s)
		t.Cleanup(srv.Close)
		d, err := New(Options{URL: srv.URL, Token: "tok", Mode: ModeLearned, Rand: func() float64 { return 0.5 },
			Timeout: 200 * time.Millisecond, Report: func(error) { s.mu.Lock(); s.reported++; s.mu.Unlock() }})
		if err != nil {
			t.Fatal(err)
		}
		_, out, err := d.Decide(ctx, call)
		if err != nil {
			t.Fatal(err)
		}
		settle(d)
		return s, out
	}
	with := func(v, kind string) prediction {
		p := allowSuggestion()
		p.Verdict, p.AskKind = v, kind
		return p
	}

	// Evidence: the suggestion decides, recorded as learned.
	for _, c := range []struct{ verdict, kind string }{{"allow", ""}, {"block", ""}, {"ask", AskSplit}} {
		s, out := learned(with(c.verdict, c.kind), 0)
		if string(out.Verdict) != c.verdict || out.DecidedBy != session.DecidedLearned || out.Suggestion == nil {
			t.Errorf("%s %s: %+v", c.verdict, c.kind, out)
		}
		if len(s.decided) != 1 || s.decided[0].Source != "topos/progressive/learned" {
			t.Errorf("%s %s: decisions %+v", c.verdict, c.kind, s.decided)
		}
	}
	// No evidence: the rules decide, with the suggestion beside them.
	for _, kind := range []string{AskUnsure, ""} {
		s, out := learned(with("ask", kind), 0)
		if out.Verdict != rules.Verdict || out.DecidedBy != rules.DecidedBy || out.DecidedBy == session.DecidedLearned {
			t.Errorf("ask %q: decided %+v, want the rules' %+v", kind, out, rules)
		}
		if out.Suggestion == nil || out.Suggestion.Verdict != "ask" || out.Draw == nil {
			t.Errorf("ask %q: the suggestion and the draw are recorded: %+v", kind, out)
		}
		if len(s.decided) != 1 || s.decided[0].Verdict != string(rules.Verdict) || s.decided[0].Source != SourceProgressive+"/"+rules.DecidedBy {
			t.Errorf("ask %q: decisions %+v", kind, s.decided)
		}
	}
	// A service that does not answer: the rules decide, not a fallback ask.
	s, out := learned(allowSuggestion(), http.StatusServiceUnavailable)
	if out.Verdict != rules.Verdict || out.DecidedBy != rules.DecidedBy || out.Suggestion != nil || s.reported == 0 {
		t.Errorf("a failing service in learned mode: %+v (reported %d)", out, s.reported)
	}
}

// In learned mode an automatic verdict the rules decide is still picked
// for review at the service's audit rate.
func TestLearnedAuditsTheRules(t *testing.T) {
	call := bash(harness.ModeProgressive, "make")
	_, rules, _ := harness.Rules{}.Decide(context.Background(), call)
	p := allowSuggestion()
	p.Verdict, p.AskKind, p.AuditRate = "ask", AskUnsure, 0.5
	s := &service{next: p}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	d, err := New(Options{URL: srv.URL, Token: "tok", Mode: ModeLearned, Rand: func() float64 { return 0.1 }})
	if err != nil {
		t.Fatal(err)
	}
	_, out, _ := d.Decide(context.Background(), call)
	want, prob := verdict.Decide(rules.Verdict, verdict.Allow, 0.5, 0.1)
	if out.Verdict != want || out.ReviewProbability != prob {
		t.Errorf("audited %+v, want %s at %v", out, want, prob)
	}
}

// TestTheOrganizationOfACallsKey (spec 100): with OrgInitiator, the
// default, every call is keyed by the initiator's own context; with
// OrgAgent, a call of a session whose agent an organization owns is keyed
// by the organization, every other call by the initiator; the subject is
// the initiator either way, and an answer has the key of its decision.
func TestTheOrganizationOfACallsKey(t *testing.T) {
	ctx := context.Background()
	orgs := sess
	orgs.Org = "org_acme"
	for _, c := range []struct {
		name   string
		source OrgSource
		s      session.Session
		want   string
	}{
		{"default, a person's agent", "", sess, sess.Initiator.Subject},
		{"default, an organization's agent", "", orgs, sess.Initiator.Subject},
		{"initiator, an organization's agent", OrgInitiator, orgs, sess.Initiator.Subject},
		{"agent, a person's agent", OrgAgent, sess, sess.Initiator.Subject},
		{"agent, an organization's agent", OrgAgent, orgs, "org_acme"},
	} {
		s := &service{next: allowSuggestion()}
		srv := httptest.NewServer(s)
		d, err := New(Options{URL: srv.URL, Token: "tok", Org: c.source, Rand: func() float64 { return 0.5 }})
		if err != nil {
			t.Fatal(err)
		}
		call := bash(harness.ModeProgressive, "go test ./...")
		call.Session = c.s
		if _, _, err := d.Decide(ctx, call); err != nil {
			t.Fatal(err)
		}
		if err := d.Answered(ctx, c.s, "toolu_1", true, ""); err != nil {
			t.Fatal(err)
		}
		srv.Close()
		if len(s.predicted) != 1 || len(s.decided) != 1 || len(s.answered) != 1 {
			t.Fatalf("%s: %d predictions, %d decisions, %d answers", c.name, len(s.predicted), len(s.decided), len(s.answered))
		}
		for what, k := range map[string]key{"prediction": s.predicted[0].key, "decision": s.decided[0].key, "answer": s.answered[0].key} {
			if k.Org != c.want || k.Subject != sess.Initiator.Subject || k.ActionID != "ses_1/toolu_1" {
				t.Errorf("%s: the %s is keyed %+v, want the organization %q", c.name, what, k, c.want)
			}
		}
	}
}

// TestTheDecisionNamesItsOrganization (spec 100): every decision the
// decider sends carries the organization of its key back to the harness,
// in confirm and in progressive, decided by the service, by the rules, or
// with the service down; plan sends nothing and names none.
func TestTheDecisionNamesItsOrganization(t *testing.T) {
	ctx := context.Background()
	orgs := sess
	orgs.Org = "org_acme"
	for _, c := range []struct {
		name string
		mode harness.Mode
		dm   Mode
		down bool
		want string
	}{
		{"confirm", harness.ModeConfirm, ModeDecide, false, "org_acme"},
		{"progressive, decided by the service", harness.ModeProgressive, ModeDecide, false, "org_acme"},
		{"progressive, shadow", harness.ModeProgressive, ModeShadow, false, "org_acme"},
		{"progressive, learned with too few answers", harness.ModeProgressive, ModeLearned, false, "org_acme"},
		{"progressive, the service down", harness.ModeProgressive, ModeDecide, true, "org_acme"},
		{"plan", harness.ModePlan, ModeDecide, false, ""},
	} {
		s := &service{next: prediction{Verdict: "ask", Approve: 0.5, Thresholds: thresholds{AllowAbove: 0.9, BlockBelow: 0.25}, AuditRate: 0.05, Source: "learned/1", AskKind: AskUnsure}}
		if c.down {
			s.status = http.StatusServiceUnavailable
		}
		srv := httptest.NewServer(s)
		d, err := New(Options{URL: srv.URL, Token: "tok", Org: OrgAgent, Mode: c.dm, Rand: func() float64 { return 0.5 }})
		if err != nil {
			t.Fatal(err)
		}
		call := bash(c.mode, "make deploy")
		call.Session = orgs
		_, out, err := d.Decide(ctx, call)
		settle(d)
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if out.Org != c.want {
			t.Errorf("%s: the decision names %q, want %q", c.name, out.Org, c.want)
		}
	}
}

func TestParseOrgSource(t *testing.T) {
	for _, o := range []OrgSource{OrgInitiator, OrgAgent} {
		if got, err := ParseOrgSource(string(o)); err != nil || got != o {
			t.Errorf("%s: %v %v", o, got, err)
		}
	}
	if _, err := ParseOrgSource("owner"); err == nil {
		t.Error("an unknown organization source is refused")
	}
}

func TestParseMode(t *testing.T) {
	for _, m := range []Mode{ModeShadow, ModeLearned, ModeDecide} {
		if got, err := ParseMode(string(m)); err != nil || got != m {
			t.Errorf("%s: %v %v", m, got, err)
		}
	}
	if _, err := ParseMode("sometimes"); err == nil {
		t.Error("an unknown mode is refused")
	}
}

// The suggestion recorded on a call keeps the prediction's evidence ratio
// and ask kind in every mode that asks the service, so a client can show
// why an ask asks; a prediction from a service that predates them records
// neither, and the event's JSON leaves them out.
func TestTheSuggestionKeepsItsEvidence(t *testing.T) {
	ctx := context.Background()
	split := allowSuggestion()
	split.Verdict, split.AskKind, split.EvidenceRatio = "ask", AskSplit, new(0.8)
	unsure := allowSuggestion()
	unsure.Verdict, unsure.AskKind, unsure.EvidenceRatio = "ask", AskUnsure, new(0.1)
	for _, c := range []struct {
		name  string
		mode  Mode
		call  harness.Mode
		next  prediction
		kind  string
		ratio float64
	}{
		{"decide", ModeDecide, harness.ModeProgressive, split, AskSplit, 0.8},
		{"learned with evidence", ModeLearned, harness.ModeProgressive, split, AskSplit, 0.8},
		{"learned without evidence", ModeLearned, harness.ModeProgressive, unsure, AskUnsure, 0.1},
		{"shadow", ModeShadow, harness.ModeProgressive, unsure, AskUnsure, 0.1},
		{"confirm", ModeDecide, harness.ModeConfirm, split, AskSplit, 0.8},
	} {
		s := &service{next: c.next}
		srv := httptest.NewServer(s)
		t.Cleanup(srv.Close)
		d, err := New(Options{URL: srv.URL, Token: "tok", Mode: c.mode, Rand: func() float64 { return 0.5 }, Timeout: 200 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		_, out, err := d.Decide(ctx, bash(c.call, "make"))
		if err != nil {
			t.Fatal(err)
		}
		settle(d)
		sg := out.Suggestion
		if sg == nil || sg.AskKind != c.kind || sg.EvidenceRatio == nil || *sg.EvidenceRatio != c.ratio {
			t.Errorf("%s: suggestion %+v, want ask kind %q and evidence ratio %v", c.name, sg, c.kind, c.ratio)
			continue
		}
		raw, err := json.Marshal(session.AgentToolUse{ToolUseID: "toolu_1", Name: "bash", Verdict: string(out.Verdict), Suggestion: sg})
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf(`"evidence_ratio":%v,"ask_kind":"%s"`, c.ratio, c.kind); !strings.Contains(string(raw), want) {
			t.Errorf("%s: the event %s does not carry %s", c.name, raw, want)
		}
	}

	// A service that predates the fields: the suggestion has neither, and
	// the event's JSON names neither.
	d := newDecider(t, &service{next: allowSuggestion()}, 0.5)
	_, out, err := d.Decide(ctx, bash(harness.ModeProgressive, "make"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Suggestion == nil || out.Suggestion.AskKind != "" || out.Suggestion.EvidenceRatio != nil {
		t.Fatalf("an older prediction: suggestion %+v", out.Suggestion)
	}
	raw, err := json.Marshal(out.Suggestion)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "ask_kind") || strings.Contains(string(raw), "evidence_ratio") {
		t.Errorf("an older prediction's suggestion %s names a field it does not have", raw)
	}
}

// Each decision is sent with its mode's source and the layer that decided
// the call, topos/<mode>/<decided_by>, the source a replay of the session's
// log names it with: the service tells a call its suggestion decided from
// one a rule, the risk score or a failure decided. The layer is the one the
// call's agent.tool_use records.
func TestTheSourceNamesTheLayer(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name        string
		mode        Mode
		call        harness.Call
		unpredicted bool
		want        string
	}{
		{"learned in decide", ModeDecide, bash(harness.ModeProgressive, "make"), false, "topos/progressive/learned"},
		{"a failure in decide", ModeDecide, bash(harness.ModeProgressive, "make"), true, "topos/progressive/fallback"},
		{"a failure in learned", ModeLearned, bash(harness.ModeProgressive, "make"), true, "topos/progressive/score"},
		{"a rule that settles", ModeDecide, func() harness.Call {
			c := bash(harness.ModeProgressive, "make")
			c.Policy.AlwaysAllow = []string{"bash(make)"}
			return c
		}(), false, "topos/progressive/always_allow"},
		{"the score in shadow", ModeShadow, bash(harness.ModeProgressive, "make"), false, "topos/progressive/score"},
		{"confirm", ModeDecide, func() harness.Call {
			c := bash(harness.ModeConfirm, "make")
			c.MachineKind = machine.KindHost
			return c
		}(), false, "topos/confirm/confirm"},
		{"a sandbox write in confirm", ModeDecide, bash(harness.ModeConfirm, "make"), false, "topos/confirm/sandbox"},
	}
	for _, c := range cases {
		s := &service{next: allowSuggestion(), unpredicted: c.unpredicted}
		srv := httptest.NewServer(s)
		t.Cleanup(srv.Close)
		d, err := New(Options{URL: srv.URL, Token: "tok", Mode: c.mode, Rand: func() float64 { return 0.5 }, Timeout: 200 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		_, out, err := d.Decide(ctx, c.call)
		if err != nil {
			t.Fatal(err)
		}
		settle(d)
		if len(s.decided) != 1 || s.decided[0].Source != c.want {
			t.Errorf("%s: decisions %+v, want the source %s", c.name, s.decided, c.want)
			continue
		}
		if !strings.HasSuffix(s.decided[0].Source, "/"+out.DecidedBy) {
			t.Errorf("%s: sent %s, and the call records decided_by %q", c.name, s.decided[0].Source, out.DecidedBy)
		}
	}
}

// A decision that names no layer is sent with its mode's source alone.
func TestSource(t *testing.T) {
	for _, c := range []struct{ mode, by, want string }{
		{SourceProgressive, session.DecidedLearned, "topos/progressive/learned"},
		{SourceConfirm, session.DecidedAlwaysAllow, "topos/confirm/always_allow"},
		{SourceProgressive, "", "topos/progressive"},
		{SourceConfirm, "", "topos/confirm"},
	} {
		if got := Source(c.mode, c.by); got != c.want {
			t.Errorf("Source(%q, %q) = %q, want %q", c.mode, c.by, got, c.want)
		}
	}
}
