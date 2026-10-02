// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package decisions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// service is a fake decision service: it answers predictions with next,
// records every request, and fails or stalls on demand.
type service struct {
	mu        sync.Mutex
	next      prediction
	status    int
	code      string
	stall     time.Duration
	token     string
	reported  int
	predicted []action
	decided   []decision
	answered  []answer
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
		if dec.Verdict != string(c.want) || dec.ReviewProbability != c.reviewP || dec.Source != SourceProgressive || dec.Action != nil {
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
	_, out, err := newDecider(t, s, 0.01).Decide(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	if out.Verdict != harness.VerdictAsk || out.ReviewProbability != 1 || out.Suggestion == nil || out.Suggestion.Verdict != "allow" || out.Draw != nil {
		t.Errorf("confirm %+v", out)
	}
	if len(s.predicted) != 1 || len(s.decided) != 1 || s.decided[0].Source != SourceConfirm || s.decided[0].Action != nil {
		t.Errorf("confirm sent %d predictions, decisions %+v", len(s.predicted), s.decided)
	}
	// A failing prediction still sends the decision, with the action.
	f := &service{status: http.StatusServiceUnavailable}
	_, out, _ = newDecider(t, f, 0.01).Decide(context.Background(), call)
	if out.Verdict != harness.VerdictAsk || out.Suggestion != nil {
		t.Errorf("confirm with a failing service %+v", out)
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
	} {
		if _, err := New(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	d, err := New(Options{URL: "https://decisions.example", Token: "t", Org: "org_7"})
	if err != nil {
		t.Fatal(err)
	}
	if k := d.key(sess, "toolu_9"); k.Org != "org_7" || k.ActionID != "ses_1/toolu_9" {
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
