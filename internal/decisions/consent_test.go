// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package decisions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/internal/publish"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// builtinTool is the built-in tool of the name.
func builtinTool(t *testing.T, name string) tools.Tool {
	t.Helper()
	for _, b := range tools.Builtins() {
		if b.Definition().Name == name {
			return b
		}
	}
	t.Fatalf("no built-in %s", name)
	return nil
}

// fakeTool is a tool of a name and an effect that runs nothing.
type fakeTool struct {
	name   string
	effect tools.Effect
}

func (f fakeTool) Definition() tools.Definition { return tools.Definition{Name: f.name} }
func (f fakeTool) Properties() tools.Properties { return tools.Properties{Effect: f.effect} }
func (f fakeTool) Run(context.Context, tools.Call) (tools.Result, error) {
	return tools.Result{}, nil
}

// reaching is the Reaching of a session on a machine of kind, its network
// an allowlist of docs.example.com on Cella, its boundary example.com, its
// checkpoints kept, and docs.example.com fetched before.
func reaching(kind string) harness.Reaching {
	r := harness.Reaching{
		Kind: kind, Workdir: "/work", Checkpoints: kind != machine.KindDevice, ProtectedRefs: true,
		Boundary: &session.Boundary{Hosts: []string{"*.example.com"}}, Fetched: map[string]bool{"docs.example.com": true},
	}
	if kind == machine.KindCella {
		r.Network = &harness.Network{Mode: session.NetworkAllowlist, Hosts: []string{"docs.example.com"}}
	}
	return r
}

// sent is the action the service was sent for a call of tool with input
// on a machine of kind, decided in confirm mode.
func sent(t *testing.T, kind string, tool tools.Tool, input string, state tools.State) action {
	t.Helper()
	s := &service{next: allowSuggestion()}
	d := newDecider(t, s, 0.5)
	r := reaching(kind)
	name := tool.Definition().Name
	c := harness.Call{
		Policy: harness.Policy{Mode: harness.ModeConfirm}, Session: sess, ToolUseID: "toolu_1", Name: name,
		Props: tool.Properties(), Input: json.RawMessage(input), MachineKind: kind,
		Reach: r.Of(tool, name, tools.Call{Input: json.RawMessage(input), State: state}, false), Holds: r.Holds(),
	}
	if _, _, err := d.Decide(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	settle(d)
	if len(s.predicted) != 1 {
		t.Fatalf("%d predictions", len(s.predicted))
	}
	return s.predicted[0]
}

// claims is what an action says of its reach: its scope, its target and
// its holds, as JSON.
func claims(t *testing.T, a action) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"scope": a.Scope, "target": a.Target, "holds": a.Environment.Holds})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestScopeByCall (spec 101): each call of the table is sent with the
// scope, the target and the holds the table gives, on a Cella machine, a
// host and a person's own computer.
func TestScopeByCall(t *testing.T) {
	app := publish.New(publish.Options{URL: "https://apps.example.com", GitURL: "https://git.example.com"}, sess, nil)
	own := tools.State{App: &session.PublishMeta{App: "site", AppID: "app_1", Visibility: session.VisibilityPrivate}}
	hold := map[string]string{
		machine.KindCella:  `["egress","protected_refs","checkpoints"]`,
		machine.KindHost:   `["protected_refs","checkpoints"]`,
		machine.KindDevice: `["protected_refs"]`,
	}
	for _, c := range []struct {
		name  string
		tool  tools.Tool
		input string
		state tools.State
		// want is the scope and the target by kind of machine, "" where
		// they are the same on all three.
		want map[string]string
	}{
		{"read", builtinTool(t, "read"), `{"path":"a.go"}`, tools.State{}, map[string]string{"": `{"scope":{"undo":"none"},"target":null}`}},
		{"todo", builtinTool(t, "todo"), `{"todos":[]}`, tools.State{}, map[string]string{"": `{"scope":{"undo":"none"},"target":null}`}},
		{"agent", fakeTool{harness.ToolAgent, tools.EffectNone}, `{}`, tools.State{}, map[string]string{"": `{"scope":{"undo":"none"},"target":null}`}},
		{"write inside", builtinTool(t, "write"), `{"path":"a.go","content":""}`, tools.State{}, map[string]string{
			machine.KindCella:  `{"scope":{"undo":"reversible","crosses":{"machine":false,"remote":false}},"target":{"kind":"path","class":"own"}}`,
			machine.KindHost:   `{"scope":{"undo":"reversible","crosses":{"machine":false,"remote":false}},"target":{"kind":"path","class":"shared"}}`,
			machine.KindDevice: `{"scope":{"crosses":{"machine":false,"remote":false}},"target":{"kind":"path","class":"shared"}}`,
		}},
		{"edit outside", builtinTool(t, "edit"), `{"path":"/etc/hosts","old":"a","new":"b"}`, tools.State{}, map[string]string{
			machine.KindCella:  `{"scope":{"crosses":{"machine":true,"remote":false}},"target":{"kind":"path","class":"own"}}`,
			machine.KindHost:   `{"scope":{"crosses":{"machine":true,"remote":false}},"target":{"kind":"path","class":"shared"}}`,
			machine.KindDevice: `{"scope":{"crosses":{"machine":true,"remote":false}},"target":{"kind":"path","class":"shared"}}`,
		}},
		{"bash", builtinTool(t, "bash"), `{"command":"git push --force origin main"}`, tools.State{}, map[string]string{"": `{"scope":null,"target":null}`}},
		{"web_fetch inside", builtinTool(t, "web_fetch"), `{"url":"https://docs.example.com/a"}`, tools.State{}, map[string]string{
			machine.KindCella:  `{"scope":{"undo":"none","crosses":{"remote":true,"network":false,"organization":false,"public":false},"contact":"repeat"},"target":{"kind":"host"}}`,
			machine.KindHost:   `{"scope":{"undo":"none","crosses":{"remote":true,"organization":false,"public":false},"contact":"repeat"},"target":{"kind":"host"}}`,
			machine.KindDevice: `{"scope":{"undo":"none","crosses":{"remote":true,"organization":false,"public":false},"contact":"repeat"},"target":{"kind":"host"}}`,
		}},
		{"web_fetch outside", builtinTool(t, "web_fetch"), `{"url":"https://other.org/a"}`, tools.State{}, map[string]string{
			machine.KindCella: `{"scope":{"undo":"none","crosses":{"remote":true,"network":true,"organization":true,"public":false},"contact":"first"},"target":{"kind":"host"}}`,
			machine.KindHost:  `{"scope":{"undo":"none","crosses":{"remote":true,"organization":true,"public":false},"contact":"first"},"target":{"kind":"host"}}`,
		}},
		{"publish a new app's preview", app, `{"path":"site"}`, tools.State{}, map[string]string{
			"": `{"scope":{"undo":"recoverable","crosses":{"remote":true},"contact":"first"},"target":{"kind":"app","class":"own"}}`,
		}},
		{"release a private app", app, `{"release":true}`, own, map[string]string{
			"": `{"scope":{"undo":"recoverable","crosses":{"remote":true,"public":false},"contact":"repeat"},"target":{"kind":"app","id":"app_1","class":"primary"}}`,
		}},
		{"open the app to a link", app, `{"access":"link"}`, own, map[string]string{
			"": `{"scope":{"undo":"irreversible","crosses":{"remote":true,"public":true},"contact":"repeat"},"target":{"kind":"app","id":"app_1","class":"primary"}}`,
		}},
		{"an MCP tool", fakeTool{"mcp__tracker__close", tools.EffectExternal}, `{"id":"7"}`, tools.State{}, map[string]string{"": `{"scope":null,"target":null}`}},
	} {
		for _, kind := range []string{machine.KindCella, machine.KindHost, machine.KindDevice} {
			want, ok := c.want[kind]
			if !ok {
				want, ok = c.want[""]
			}
			if !ok {
				continue
			}
			a := sent(t, kind, c.tool, c.input, c.state)
			got := claims(t, a)
			wantJSON := strings.TrimSuffix(want, "}") + `,"holds":` + hold[kind] + "}"
			var g, w any
			if err := json.Unmarshal([]byte(got), &g); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(wantJSON), &w); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if !reflect.DeepEqual(g, w) {
				t.Errorf("%s on %s:\n got %s\nwant %s", c.name, kind, got, wantJSON)
			}
		}
	}
}

// TestScopeHoldsNoInputText (spec 101): no scope, target or attribute
// value holds a path, a host, a URL or a ref name of the call's input.
func TestScopeHoldsNoInputText(t *testing.T) {
	app := publish.New(publish.Options{URL: "https://apps.example.com", GitURL: "https://git.example.com"}, sess, nil)
	for _, c := range []struct {
		tool  tools.Tool
		input string
	}{
		{builtinTool(t, "write"), `{"path":"/work/zzmarker/zzmarker.go","content":"zzmarker"}`},
		{builtinTool(t, "edit"), `{"path":"../zzmarker","old":"zzmarker","new":"zzmarker"}`},
		{builtinTool(t, "web_fetch"), `{"url":"https://zzmarker.example.com/zzmarker?ref=zzmarker"}`},
		{builtinTool(t, "bash"), `{"command":"git push origin zzmarker"}`},
		{app, `{"path":"zzmarker","app":"zzmarker","name":"zzmarker","address":"zzmarker"}`},
	} {
		for _, kind := range []string{machine.KindCella, machine.KindHost} {
			a := sent(t, kind, c.tool, c.input, tools.State{})
			b, err := json.Marshal(map[string]any{"scope": a.Scope, "target": a.Target, "environment": a.Environment, "attributes": a.Attributes})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "zzmarker") {
				t.Errorf("%s on %s sends %s", c.tool.Definition().Name, kind, b)
			}
		}
	}
}

// TestBoundaryDecidesOrganization (spec 101): a fetch crosses the
// organization when its host is outside the boundary an allow named, does
// not inside it, and says nothing of it without one.
func TestBoundaryDecidesOrganization(t *testing.T) {
	fetch := builtinTool(t, "web_fetch")
	of := func(b *session.Boundary, url string) *bool {
		r := harness.Reaching{Kind: machine.KindCella, Boundary: b}
		return r.Of(fetch, "web_fetch", tools.Call{Input: json.RawMessage(`{"url":"` + url + `"}`)}, false).Crosses.Organization
	}
	b := &session.Boundary{Hosts: []string{"*.example.com", "example.org"}}
	if got := of(b, "https://api.example.com/x"); got == nil || *got {
		t.Errorf("inside the boundary: %v", got)
	}
	if got := of(b, "https://example.org/x"); got == nil || *got {
		t.Errorf("an exact host of the boundary: %v", got)
	}
	if got := of(b, "https://evil.org/x"); got == nil || !*got {
		t.Errorf("outside the boundary: %v", got)
	}
	if got := of(&session.Boundary{}, "https://example.com/x"); got == nil || !*got {
		t.Errorf("an empty boundary owns no host: %v", got)
	}
	if got := of(nil, "https://evil.org/x"); got != nil {
		t.Errorf("no boundary: %v", *got)
	}
}

// TestFirstContact (spec 101): the session's first fetch of a host and
// the publish that creates an app are first contacts, and each after is a
// repeat; a fetch that failed reached nothing.
func TestFirstContact(t *testing.T) {
	ev := func(typ session.Type, p any) session.Event {
		e, err := session.NewEvent(typ, p, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	log := []session.Event{
		ev(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: "toolu_1", Name: "web_fetch", Input: json.RawMessage(`{"url":"https://a.example.com/x"}`)}),
		ev(session.TypeToolResult, session.ToolResult{ToolUseID: "toolu_1", Outcome: tools.OutcomeOK}),
		ev(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: "toolu_2", Name: "web_fetch", Input: json.RawMessage(`{"url":"https://b.example.com/x"}`)}),
		ev(session.TypeToolResult, session.ToolResult{ToolUseID: "toolu_2", Outcome: tools.OutcomeError}),
		ev(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: "toolu_3", Name: "bash", Input: json.RawMessage(`{"command":"curl https://c.example.com"}`)}),
		ev(session.TypeToolResult, session.ToolResult{ToolUseID: "toolu_3", Outcome: tools.OutcomeOK}),
	}
	fetched := harness.FetchedHosts(log)
	if !reflect.DeepEqual(fetched, map[string]bool{"a.example.com": true}) {
		t.Fatalf("fetched %v", fetched)
	}
	r := harness.Reaching{Kind: machine.KindCella, Fetched: fetched}
	fetch := builtinTool(t, "web_fetch")
	for url, want := range map[string]string{"https://a.example.com/y": session.ContactRepeat, "https://b.example.com/y": session.ContactFirst, "https://c.example.com": session.ContactFirst} {
		if got := r.Of(fetch, "web_fetch", tools.Call{Input: json.RawMessage(`{"url":"` + url + `"}`)}, false).Contact; got != want {
			t.Errorf("%s: %s, want %s", url, got, want)
		}
	}
	app := publish.New(publish.Options{URL: "https://apps.example.com", GitURL: "https://git.example.com"}, sess, nil)
	first := app.Reach(tools.Call{Input: json.RawMessage(`{"path":"site"}`)})
	again := app.Reach(tools.Call{Input: json.RawMessage(`{"path":"site"}`), State: tools.State{App: &session.PublishMeta{App: "site", AppID: "app_1"}}})
	if first.Contact != session.ContactFirst || first.Target.ID != "" || again.Contact != session.ContactRepeat || again.Target.ID != "app_1" {
		t.Errorf("publish: first %+v, again %+v", first, again)
	}
}

// TestScopeSource (spec 101): a call decided with a prediction that
// carries a scope records the service's reading joined with Topos's
// claims, source service, and reads each dimension leniently; one whose
// prediction carries none, failed or was not asked records Topos's
// claims, source topos.
func TestScopeSource(t *testing.T) {
	ctx := context.Background()
	write := harness.Call{
		Policy: harness.Policy{Mode: harness.ModeProgressive}, Session: sess, ToolUseID: "toolu_1", Name: "write",
		Props: tools.Properties{Effect: tools.EffectWrite}, Input: json.RawMessage(`{"path":"a"}`), MachineKind: machine.KindCella,
		Reach: tools.Reach{Undo: session.UndoReversible, Crosses: session.Crosses{Machine: new(false), Remote: new(false)}, Target: &tools.ReachTarget{Kind: "path", Class: session.TargetOwn}},
	}
	claimed := harness.ClaimedScope(write.Reach)
	p := allowSuggestion()
	p.Scope = &readScope{
		Undo: "irreversible", TargetClass: "unknown", TargetKind: "zz-main", Contact: "first",
		Crosses: map[string]json.RawMessage{"machine": json.RawMessage(`"unknown"`), "remote": json.RawMessage(`"yes"`), "public": json.RawMessage(`false`), "network": json.RawMessage(`"no"`), "organization": json.RawMessage(`7`)},
	}
	want := session.CallScope{
		Undo: session.UndoIrreversible, Crosses: &session.Crosses{Machine: new(false), Remote: new(true), Network: new(false), Public: new(false)},
		TargetClass: session.TargetOwn, TargetKind: "path", Contact: session.ContactFirst, Source: session.ScopeFromService,
	}
	// A progressive call the service decides, and one the rules decide in
	// confirm mode with the suggestion beside them.
	for _, mode := range []harness.Mode{harness.ModeProgressive, harness.ModeConfirm} {
		c := write
		c.Policy.Mode = mode
		_, out, err := newDecider(t, &service{next: p}, 0.5).Decide(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		if out.Scope == nil || !reflect.DeepEqual(*out.Scope, want) {
			t.Errorf("%s: recorded %+v, want %+v", mode, out.Scope, want)
		}
	}
	for name, s := range map[string]*service{
		"no scope":    {next: allowSuggestion()},
		"no answer":   {unpredicted: true},
		"plan":        {next: p},
		"the ceiling": {next: p},
	} {
		c := write
		switch name {
		case "plan":
			c.Policy.Mode = harness.ModePlan
		case "the ceiling":
			c.Props.Effect = tools.EffectRead
		}
		_, out, err := newDecider(t, s, 0.5).Decide(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		if out.Scope == nil || !reflect.DeepEqual(*out.Scope, claimed) {
			t.Errorf("%s: recorded %+v, want Topos's claims %+v", name, out.Scope, claimed)
		}
	}
}

// TestAnswerCarriesHowThePersonAnswered (spec 101): an answer carries how
// the person answered and how long the call waited, each left out when
// unknown, and never a note or a pattern.
func TestAnswerCarriesHowThePersonAnswered(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		bodies = append(bodies, m)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	d, err := New(Options{URL: srv.URL, Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []harness.Answer{
		{Approve: true, By: "usr_ada", AnswerMS: new(int64(1800)), WaitedMS: 4000, DetailsOpened: new(false), SuggestionShown: new(true), Surface: session.SurfaceQueue, Remembered: true},
		{By: "usr_ada", WaitedMS: 20},
	} {
		if err := d.Answered(context.Background(), sess, "toolu_1", a); err != nil {
			t.Fatal(err)
		}
	}
	strip := func(m map[string]any) map[string]any {
		delete(m, "org")
		delete(m, "subject")
		delete(m, "action_id")
		return m
	}
	want := []map[string]any{
		{"approve": true, "by": "usr_ada", "answer_ms": 1800.0, "waited_ms": 4000.0, "details_opened": false, "suggestion_shown": true, "surface": "queue", "remembered": true},
		{"approve": false, "by": "usr_ada", "waited_ms": 20.0, "remembered": false},
	}
	if len(bodies) != 2 || !reflect.DeepEqual(strip(bodies[0]), want[0]) || !reflect.DeepEqual(strip(bodies[1]), want[1]) {
		t.Fatalf("answers %v, want %v", bodies, want)
	}
}

// TestWiderIsKeptAndFewIsLeft (spec 101): in learned mode a wider ask is
// kept for the person, decided_by learned with review probability 1, as a
// split ask is; a few, a short and an unsure ask, and one of a kind the
// harness does not know, are left to the rules and the risk score.
func TestWiderIsKeptAndFewIsLeft(t *testing.T) {
	ctx := context.Background()
	call := bash(harness.ModeProgressive, "make")
	_, rules, err := harness.Rules{}.Decide(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		kind string
		kept bool
	}{{AskWider, true}, {AskSplit, true}, {AskFew, false}, {AskShort, false}, {AskUnsure, false}, {"later", false}} {
		p := allowSuggestion()
		p.Verdict, p.AskKind = "ask", c.kind
		s := &service{next: p}
		srv := httptest.NewServer(s)
		t.Cleanup(srv.Close)
		d, err := New(Options{URL: srv.URL, Token: "tok", Mode: ModeLearned, Rand: func() float64 { return 0.5 }})
		if err != nil {
			t.Fatal(err)
		}
		_, out, err := d.Decide(ctx, call)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case c.kept && (out.Verdict != harness.VerdictAsk || out.DecidedBy != session.DecidedLearned || out.ReviewProbability != 1):
			t.Errorf("%s: %+v, want kept for the person", c.kind, out)
		case !c.kept && (out.Verdict != rules.Verdict || out.DecidedBy != rules.DecidedBy || out.Suggestion == nil):
			t.Errorf("%s: %+v, want the rules' %+v", c.kind, out, rules)
		}
	}
}

// TestLoosen (spec 101): under covered a learned allow decides only with
// cover.covered, and otherwise the rules decide with the suggestion
// recorded; under never the stricter of the allow and the rules' verdict
// decides, so an allow never runs a call the risk score asks about; in
// learned and decide alike.
func TestLoosen(t *testing.T) {
	ctx := context.Background()
	// make on a host scores 0.5: the risk score asks.
	asks := bash(harness.ModeProgressive, "make")
	asks.MachineKind = machine.KindHost
	// make on Cella scores 0.1: the risk score allows.
	runs := bash(harness.ModeProgressive, "make")
	uncovered := allowSuggestion()
	uncovered.Cover = nil
	notCovered := allowSuggestion()
	notCovered.Cover = &cover{Covered: false}
	for _, c := range []struct {
		name   string
		loosen Loosen
		p      prediction
		call   harness.Call
		want   harness.Verdict
		by     string
	}{
		{"covered: an allow the approvals cover", LoosenCovered, allowSuggestion(), asks, harness.VerdictAllow, session.DecidedLearned},
		{"covered: an allow with no cover", LoosenCovered, uncovered, asks, harness.VerdictAsk, session.DecidedScore},
		{"covered: an allow not covered", "", notCovered, asks, harness.VerdictAsk, session.DecidedScore},
		{"never: the score asks", LoosenNever, allowSuggestion(), asks, harness.VerdictAsk, session.DecidedScore},
		{"never: the score allows", LoosenNever, uncovered, runs, harness.VerdictAllow, session.DecidedLearned},
	} {
		for _, mode := range []Mode{ModeLearned, ModeDecide} {
			s := &service{next: c.p}
			srv := httptest.NewServer(s)
			t.Cleanup(srv.Close)
			d, err := New(Options{URL: srv.URL, Token: "tok", Mode: mode, Loosen: c.loosen, Rand: func() float64 { return 0.5 }})
			if err != nil {
				t.Fatal(err)
			}
			_, out, err := d.Decide(ctx, c.call)
			if err != nil {
				t.Fatal(err)
			}
			if out.Verdict != c.want || out.DecidedBy != c.by || out.Suggestion == nil {
				t.Errorf("%s in %s: %+v", c.name, mode, out)
			}
			if len(s.decided) != 1 || s.decided[0].Source != Source(SourceProgressive, c.by) {
				t.Errorf("%s in %s: decisions %+v", c.name, mode, s.decided)
			}
		}
	}
	if _, err := ParseLoosen("always"); err == nil {
		t.Error("an unknown loosening parsed")
	}
	if l, err := ParseLoosen("never"); err != nil || l != LoosenNever {
		t.Errorf("never: %q %v", l, err)
	}
	if _, err := New(Options{URL: "https://d.example.com", Token: "t", Loosen: "sometimes"}); err == nil {
		t.Error("an unknown loosening was accepted")
	}
}

// TestTheFallbackIsAskedAsConfirm (spec 101): while the session asks for
// every change, a progressive call the service would allow asks with
// decided_by denials, its suggestion recorded and the decision sent; a
// call the rules' ceiling blocks or asks about keeps that decision.
func TestTheFallbackIsAskedAsConfirm(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		mut  func(*harness.Call)
		want harness.Verdict
		by   string
	}{
		{"a change", func(*harness.Call) {}, harness.VerdictAsk, session.DecidedDenials},
		{"always_allow", func(c *harness.Call) { c.Policy.AlwaysAllow = []string{"bash"} }, harness.VerdictAsk, session.DecidedDenials},
		{"always_confirm", func(c *harness.Call) { c.Policy.AlwaysConfirm = []string{"bash"} }, harness.VerdictAsk, session.DecidedAlwaysConfirm},
		{"block_at", func(c *harness.Call) { c.Policy.Thresholds = harness.Thresholds{FlagAt: 0, AskAt: 0, BlockAt: 0.05} }, harness.VerdictBlock, session.DecidedScore},
		{"a read", func(c *harness.Call) { c.Props.Effect = tools.EffectRead }, harness.VerdictAllow, session.DecidedReadOnly},
	} {
		s := &service{next: allowSuggestion()}
		d := newDecider(t, s, 0.5)
		call := bash(harness.ModeProgressive, "make")
		call.Fallback = true
		c.mut(&call)
		_, out, err := d.Decide(ctx, call)
		if err != nil {
			t.Fatal(err)
		}
		settle(d)
		if out.Verdict != c.want || out.DecidedBy != c.by {
			t.Errorf("%s: %+v", c.name, out)
		}
		if c.by == session.DecidedDenials && (out.Suggestion == nil || out.ReviewProbability != 1 || len(s.decided) != 1 || s.decided[0].Source != "topos/progressive/denials") {
			t.Errorf("%s: asked as confirm: %+v, decisions %+v", c.name, out, s.decided)
		}
	}
}

// TestHandoffAttributes (spec 101): agent and send_message carry what
// they hand over, every call its depth, and no task text reaches the
// decision service.
func TestHandoffAttributes(t *testing.T) {
	ctx := context.Background()
	s := &service{next: allowSuggestion()}
	d := newDecider(t, s, 0.5)
	for _, c := range []harness.Call{
		{Name: harness.ToolAgent, Input: json.RawMessage(`{"subagent":"reviewer","task":"zzmarker"}`), Depth: 1, Handoff: &harness.Handoff{Writes: true, Isolation: "worktree"}},
		{Name: harness.ToolSendMessage, Input: json.RawMessage(`{"to":"evt_1","message":"zzmarker"}`), Handoff: &harness.Handoff{External: true}},
		{Name: harness.ToolMessage, Input: json.RawMessage(`{"thread":"evt_1","content":"zzmarker"}`)},
		{Name: "bash", Input: json.RawMessage(`{"command":"ls"}`), Depth: 2},
	} {
		c.Policy, c.Session, c.ToolUseID, c.MachineKind = harness.Policy{Mode: harness.ModeConfirm}, sess, "toolu_1", machine.KindCella
		c.Props = tools.Properties{Effect: tools.EffectNone}
		if _, _, err := d.Decide(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	settle(d)
	want := []map[string]any{
		{"attended": false, "depth": 1.0, "handoff.writes": true, "handoff.external": false, "handoff.isolation": "worktree"},
		{"attended": false, "depth": 0.0, "handoff.writes": false, "handoff.external": true},
		{"attended": false, "depth": 0.0},
		{"attended": false, "depth": 2.0},
	}
	if len(s.predicted) != len(want) {
		t.Fatalf("%d predictions", len(s.predicted))
	}
	for i, a := range s.predicted {
		if !reflect.DeepEqual(a.Attributes, want[i]) {
			t.Errorf("%s: attributes %v, want %v", a.Name, a.Attributes, want[i])
		}
		b, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "zzmarker") {
			t.Errorf("%s sends its task: %s", a.Name, b)
		}
	}
}
