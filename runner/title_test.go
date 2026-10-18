// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/manifest"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/dir"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// quick is a model the installation runs beside the agent's own, which a
// title may run on.
const quick = "quick-model"

// titling is the fixture's runner with a title question and the agent's
// latest spec.title, both as a test sets them, recording what is asked
// and logging into logs.
type titling struct {
	f *fixture
	r *Runner

	mu    sync.Mutex
	set   *v1.Title
	asked []string
	allow session.ModelRef
	deny  error
	logs  bytes.Buffer
	heard strings.Builder
}

func (tg *titling) Write(p []byte) (int, error) {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	return tg.logs.Write(p)
}

func (tg *titling) OnDelta(d harness.Delta) {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	tg.heard.WriteString(d.Event.Delta)
}

func (tg *titling) OnReset(string, int, int) {}

func (tg *titling) logged() string {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	return tg.logs.String()
}

func (tg *titling) questions() []string {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	return append([]string(nil), tg.asked...)
}

// title turns titles on for the fixture's sessions, with the question
// allowed and naming no model, and the installation running quick beside
// the agent's model.
func title(t *testing.T, f *fixture, set *v1.Title) *titling {
	t.Helper()
	tg := &titling{f: f, set: set}
	o := f.r.o
	build := o.Harness
	o.Harness = func(ctx context.Context, s session.Session) (harness.Config, error) {
		cfg, err := build(ctx, s)
		cfg.Observer = tg
		// The agent's model is priced, so a session's budget can be spent.
		cfg.Entry.Pricing = &models.Pricing{Input: price("1"), Output: price("5")}
		cfg.Connect = func(_ context.Context, name string) (models.Model, models.Connection, models.Entry, error) {
			if name != quick {
				return nil, models.Connection{}, models.Entry{}, &models.Coded{Code: models.CodeUnknown, Message: name + " is not run here"}
			}
			return nil, models.Connection{BaseURL: f.stub.URL() + "/anthropic", Model: quick, Family: models.FamilyAnthropic}, models.Entry{InputWindow: 100_000, MaxOutputTokens: 8_000}, nil
		}
		return cfg, err
	}
	o.LatestTitle = func(context.Context, string) (*v1.Title, error) {
		tg.mu.Lock()
		defer tg.mu.Unlock()
		return tg.set, nil
	}
	o.Title = func(_ context.Context, id, asked string) (session.ModelRef, error) {
		tg.mu.Lock()
		defer tg.mu.Unlock()
		tg.asked = append(tg.asked, id+" "+asked)
		return tg.allow, tg.deny
	}
	o.Log = slog.New(slog.NewTextHandler(tg, nil))
	r, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	tg.r = r
	return tg
}

func price(s string) *models.Price {
	p, err := models.ParsePrice(s)
	if err != nil {
		panic(err)
	}
	return &p
}

func named(answer string) luxstub.Reply {
	return reply(ir.Block{Type: ir.BlockText, Text: answer})
}

// titles are the session's title changes, by whom and to what.
func (f *fixture) titles(ctx context.Context, id string) []session.TitleChanged {
	f.t.Helper()
	evs, err := f.store.Events(ctx, id, 1, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []session.TitleChanged
	for _, e := range evs {
		var p session.TitleChanged
		if e.Type == session.TypeTitleChanged && e.Decode(&p) == nil {
			out = append(out, p)
		}
	}
	return out
}

// TestATitleFollowsATurnAPersonStarted: after a turn a person started
// ends idle, the drive asks the title question as the session with the
// name the request asks for, the session's model where the agent names
// none, and sends one title request, whose answer is the session's title
// by the harness. The turn's close is in the log before the request, no
// delta of the request reaches the observer, the update time stays where
// the turn left it, and the session stays idle (spec 083).
func TestATitleFollowsATurnAPersonStarted(t *testing.T) {
	f := setup(t)
	tg := title(t, f, &v1.Title{})
	ctx := t.Context()
	f.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: "The plan is written."}), named("Release plan"))
	f.message(ctx, "Write the release plan.")
	out, err := tg.r.Drive(ctx, f.s.ID)
	if err != nil || out.StopReason != session.StopEndTurn {
		t.Fatalf("drive %+v, %v", out, err)
	}
	if got := tg.questions(); len(got) != 1 || got[0] != f.s.ID+" "+model {
		t.Fatalf("the questions %v", got)
	}
	got := f.titles(ctx, f.s.ID)
	if len(got) != 1 || got[0].New != "Release plan" || got[0].By.Subject != session.HarnessSubject || got[0].By.Kind != session.SenderService {
		t.Fatalf("the titles %+v", got)
	}
	evs, err := f.store.Events(ctx, f.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := len(evs)
	var closing session.SessionStatus
	if evs[n-3].Type != session.TypeSessionStatus || evs[n-3].Decode(&closing) != nil || closing.Status != session.StatusIdle || session.Purpose(evs[n-2]) != session.PurposeTitle {
		t.Fatalf("the log ends %s, %s, %s", evs[n-3].Type, evs[n-2].Type, evs[n-1].Type)
	}
	if heard := tg.heard.String(); !strings.Contains(heard, "The plan is written.") || strings.Contains(heard, "Release plan") {
		t.Fatalf("the observer heard %q", heard)
	}
	s, err := f.store.Get(ctx, f.s.ID)
	if err != nil || s.Title != "Release plan" || s.Status != session.StatusIdle || !s.UpdatedAt.Equal(evs[n-3].Time) {
		t.Fatalf("header %+v, %v", s, err)
	}
	if logs := tg.logged(); !strings.Contains(logs, "title: named") || !strings.Contains(logs, f.s.ID) || strings.Contains(logs, "Release plan") {
		t.Fatalf("the log says %q", logs)
	}
}

// TestNoTitleIsAskedWhenItIsNotDue: no question is asked and no title
// written after a trigger's turn, a turn that ended the session, a turn
// whose budget is spent, in a session a person renamed, for an agent
// whose latest version sets no spec.title, and by a runner with no
// question to ask; a turn that left input pending asks after the next
// turn alone (spec 083).
func TestNoTitleIsAskedWhenItIsNotDue(t *testing.T) {
	trigger := session.Sender{Subject: "trigger:trg_1", Kind: session.SenderTrigger}
	for _, c := range []struct {
		name  string
		setup func(*testing.T, *fixture, *titling)
	}{
		{"a trigger's turn", func(t *testing.T, f *fixture, _ *titling) {
			f.send(t.Context(), session.TypeUserMessage, session.UserMessage{Sender: trigger, Content: []lux.Block{{Type: ir.BlockText, Text: "Run the report."}}})
		}},
		{"a turn that ended the session", func(t *testing.T, f *fixture, _ *titling) {
			f.restate(t, func(s *session.Session) { s.EndOnIdle = true })
			f.message(t.Context(), "Do it.")
		}},
		{"a spent budget", func(t *testing.T, f *fixture, _ *titling) {
			f.restate(t, func(s *session.Session) { s.Budget.MaxCostUSDMicro = new(int64(1)) })
			f.message(t.Context(), "Do it.")
		}},
		{"a person's rename", func(t *testing.T, f *fixture, _ *titling) {
			f.send(t.Context(), session.TypeTitleChanged, session.TitleChanged{By: f.s.Initiator, New: "Mine"})
			f.message(t.Context(), "Do it.")
		}},
		{"an agent that sets no title", func(t *testing.T, f *fixture, tg *titling) {
			tg.set = nil
			f.message(t.Context(), "Do it.")
		}},
		{"no question", func(t *testing.T, f *fixture, tg *titling) {
			o := tg.r.o
			o.Title = nil
			r, err := New(o)
			if err != nil {
				t.Fatal(err)
			}
			tg.r = r
			f.message(t.Context(), "Do it.")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			tg := title(t, f, &v1.Title{})
			ctx := t.Context()
			// The model is priced, so a budget of one micro-dollar is spent
			// by the turn.
			f.stub.Script(model, luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{{Type: ir.BlockText, Text: "Done."}}, StopReason: ir.StopEndTurn, Usage: ir.Usage{InputTokens: 1000, OutputTokens: 100}}},
				named("Never"))
			c.setup(t, f, tg)
			if _, err := tg.r.Drive(ctx, f.s.ID); err != nil {
				t.Fatal(err)
			}
			if got := tg.questions(); len(got) != 0 {
				t.Fatalf("asked %v", got)
			}
			for _, tc := range f.titles(ctx, f.s.ID) {
				if tc.By.Subject == session.HarnessSubject {
					t.Fatalf("titled %+v", tc)
				}
			}
			if n := len(f.stub.Requests()); n > 1 {
				t.Fatalf("%d requests", n)
			}
		})
	}

	t.Run("input pending at the turn's end", func(t *testing.T) {
		f := setup(t)
		tg := title(t, f, &v1.Title{})
		ctx := t.Context()
		f.stub.Script(model,
			luxstub.Reply{Response: reply(ir.Block{Type: ir.BlockText, Text: "First."}).Response, Respond: func(*ir.Request, *ir.Response) {
				f.message(ctx, "And another thing.")
			}},
			reply(ir.Block{Type: ir.BlockText, Text: "Second."}),
			named("Two things"),
		)
		f.message(ctx, "One thing.")
		if _, err := tg.r.Drive(ctx, f.s.ID); err != nil {
			t.Fatal(err)
		}
		if got := tg.questions(); len(got) != 1 || f.count(ctx, session.TypeAgentMessage) != 2 {
			t.Fatalf("asked %v over %d answers", got, f.count(ctx, session.TypeAgentMessage))
		}
		if got := f.titles(ctx, f.s.ID); len(got) != 1 || got[0].New != "Two things" {
			t.Fatalf("titles %+v", got)
		}
	})
}

// restate changes the fixture's session's header, as its create would
// have set it.
func (f *fixture) restate(t *testing.T, change func(*session.Session)) {
	t.Helper()
	s := session.New(f.s.Agent, f.s.Initiator, f.s.Runner, f.s.Machine, t0)
	change(&s)
	if err := f.store.Create(t.Context(), s, nil); err != nil {
		t.Fatal(err)
	}
	f.s = s
}

// TestATitleThatFailsChangesNothing: a denied question, a request that
// fails, an empty answer and a model the installation does not run write
// no title, no session.error and no status, leave the session idle, and
// are logged with the session and the cause; an agent's own title model
// that runs nowhere sends nothing, not even on the session's model (spec
// 083).
func TestATitleThatFailsChangesNothing(t *testing.T) {
	for _, c := range []struct {
		name     string
		set      *v1.Title
		deny     error
		title    luxstub.Reply
		requests int
		logged   string
	}{
		{"denied", &v1.Title{}, errors.New("forbidden: titles are off"), named("Never"), 1, "titles are off"},
		{"a failed request", &v1.Title{}, nil, luxstub.Reply{Fail: &luxstub.Failure{Status: http.StatusBadRequest}, Response: named("Never").Response}, 2, "title: failed"},
		{"an empty answer", &v1.Title{}, nil, named(" "), 2, "kept=empty"},
		{"an agent's model that runs nowhere", &v1.Title{Model: new("tier/nowhere")}, nil, named("Never"), 1, "tier/nowhere is not run here"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			tg := title(t, f, c.set)
			tg.deny = c.deny
			ctx := t.Context()
			f.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: "Done."}), c.title)
			f.message(ctx, "Do it.")
			out, err := tg.r.Drive(ctx, f.s.ID)
			if err != nil || out.StopReason != session.StopEndTurn {
				t.Fatalf("drive %+v, %v", out, err)
			}
			if n := len(f.stub.Requests()); n != c.requests {
				t.Fatalf("%d requests, want %d", n, c.requests)
			}
			s, err := f.store.Get(ctx, f.s.ID)
			if err != nil || s.Status != session.StatusIdle || s.Title != "" || f.count(ctx, session.TypeSessionError) != 0 || f.count(ctx, session.TypeTitleChanged) != 0 {
				t.Fatalf("the session %+v, %v, %d errors", s, err, f.count(ctx, session.TypeSessionError))
			}
			if logs := tg.logged(); !strings.Contains(logs, c.logged) || !strings.Contains(logs, "session="+f.s.ID) {
				t.Fatalf("the log says %q, want %q", logs, c.logged)
			}
		})
	}
}

// TestATitleRunsOnTheModelTheAllowNames: the request runs on the model
// the allow names when the installation runs it, on the agent's title
// model when the allow names none, and on the session's model where the
// agent names none and the name asked runs nowhere (spec 083).
func TestATitleRunsOnTheModelTheAllowNames(t *testing.T) {
	for _, c := range []struct {
		name  string
		set   *v1.Title
		allow string
		ran   string
	}{
		{"the allow's model", &v1.Title{Model: new("tier/quick")}, quick, quick},
		{"the agent's model", &v1.Title{Model: new(quick)}, "", quick},
		{"an allow's model that runs nowhere", &v1.Title{Model: new(quick)}, "vendor/gone", quick},
		{"the session's own", &v1.Title{}, "", model},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			tg := title(t, f, c.set)
			tg.allow = session.ModelRef{Name: c.allow, Route: "tier/quick"}
			ctx := t.Context()
			f.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: "Done."}))
			f.stub.Script(c.ran, named("Plan"))
			f.message(ctx, "Do it.")
			if _, err := tg.r.Drive(ctx, f.s.ID); err != nil {
				t.Fatal(err)
			}
			reqs := f.stub.Requests()
			if last := reqs[len(reqs)-1]; last.Model != c.ran || f.titles(ctx, f.s.ID)[0].New != "Plan" {
				t.Fatalf("the title ran on %s", last.Model)
			}
		})
	}
}

// TestInputDuringTheTitleStartsTheNextTurn: a message sent while the
// title request runs starts the next turn in the same drive, on the same
// lease, and the title is asked again after it (spec 083).
func TestInputDuringTheTitleStartsTheNextTurn(t *testing.T) {
	f := setup(t)
	tg := title(t, f, &v1.Title{})
	ctx := t.Context()
	f.stub.Script(model,
		reply(ir.Block{Type: ir.BlockText, Text: "First."}),
		luxstub.Reply{Response: named("Plans").Response, Respond: func(*ir.Request, *ir.Response) {
			f.message(ctx, "Now the second part.")
		}},
		reply(ir.Block{Type: ir.BlockText, Text: "Second."}),
		named("KEEP"),
	)
	f.message(ctx, "The first part.")
	out, err := tg.r.Drive(ctx, f.s.ID)
	if err != nil || out.StopReason != session.StopEndTurn {
		t.Fatalf("drive %+v, %v", out, err)
	}
	if f.count(ctx, session.TypeAgentMessage) != 2 || len(tg.questions()) != 2 || len(f.stub.Requests()) != 4 {
		t.Fatalf("%d answers, %d questions, %d requests", f.count(ctx, session.TypeAgentMessage), len(tg.questions()), len(f.stub.Requests()))
	}
	evs, err := f.store.Events(ctx, f.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var runners []string
	for _, e := range evs {
		var p session.SessionStatus
		if e.Type == session.TypeSessionStatus && e.Decode(&p) == nil && p.Status == session.StatusRunning {
			runners = append(runners, p.Runner.ID)
		}
	}
	if len(runners) != 2 || runners[0] != runners[1] {
		t.Fatalf("the turns ran on %v", runners)
	}
}

// TestTitleNames: the name asked is the agent's title model, else the
// name the session's model was asked by, else its model, else the
// agent's own; the session's model is a fallback only where the agent
// names no title model.
func TestTitleNames(t *testing.T) {
	routed := session.Session{Model: &session.ModelRef{Name: "vendor/b", Via: "tier/auto"}}
	for _, c := range []struct {
		set    *v1.Title
		s      session.Session
		asked  string
		models string
	}{
		{&v1.Title{}, session.Session{}, "agent/a", "agent/a"},
		{&v1.Title{}, session.Session{Model: &session.ModelRef{Name: "vendor/b"}}, "vendor/b", "vendor/b"},
		{&v1.Title{}, routed, "tier/auto", "tier/auto vendor/b"},
		{&v1.Title{Model: new(" tier/quick ")}, routed, "tier/quick", "tier/quick"},
	} {
		asked, names := titleNames(c.set, c.s, "agent/a")
		if asked != c.asked || strings.Join(names, " ") != c.models {
			t.Errorf("%+v: asked %q on %v", c.s.Model, asked, names)
		}
	}
}

// TestBlockedAndStartedByPerson: a session is not named while it is not
// idle, holds pending input, was renamed by a person, a fork's copied
// rename included, or spent its budget; a turn was started by a person
// when its input, after the turn before it closed, held their message.
func TestBlockedAndStartedByPerson(t *testing.T) {
	ev := func(typ session.Type, p any) session.Event {
		t.Helper()
		e, err := session.NewEvent(typ, p, t0)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	person := session.Sender{Subject: "usr_ada", Kind: session.SenderPerson}
	said := func(by session.Sender) session.Event {
		return ev(session.TypeUserMessage, session.UserMessage{Sender: by, Content: []lux.Block{{Type: ir.BlockText, Text: "hi"}}})
	}
	status := func(st session.Status) session.Event {
		return ev(session.TypeSessionStatus, session.SessionStatus{Status: st, StopReason: session.StopEndTurn})
	}
	trigger := session.Sender{Subject: "trigger:trg_1", Kind: session.SenderTrigger}
	asked := func(fold uint64, purpose string) session.Event {
		return ev(session.TypeModelRequest, session.ModelRequest{Model: "m", Outcome: "ok", FoldSeq: fold, Purpose: purpose})
	}
	numbered := func(evs ...session.Event) []session.Event {
		for i := range evs {
			evs[i].Seq = uint64(i + 1)
		}
		return evs
	}
	run, stop := status(session.StatusRunning), status(session.StatusIdle)
	for _, c := range []struct {
		name string
		evs  []session.Event
		want bool
	}{
		{"a first turn", numbered(said(person), run, asked(2, ""), stop), true},
		{"a trigger's turn", numbered(said(person), run, asked(2, ""), stop, said(trigger), run, asked(6, ""), stop), false},
		{"a turn after a title", numbered(said(trigger), run, asked(2, ""), stop, asked(4, session.PurposeTitle), said(person), run, asked(7, ""), stop), true},
		{"a message the turn before saw", numbered(said(person), run, asked(2, ""), stop, run, asked(5, ""), stop), false},
		{"a message after the turn before's last request", numbered(said(trigger), run, asked(2, ""), said(person), stop, run, asked(6, ""), stop), true},
		{"a failed request's own place", numbered(said(person), run, asked(0, ""), stop, said(trigger), run, stop), false},
		{"a running turn", numbered(said(person), run), false},
		{"nothing", nil, false},
	} {
		if got := startedByPerson(c.evs); got != c.want {
			t.Errorf("%s: startedByPerson = %v", c.name, got)
		}
	}
	turn := numbered(said(person), run, asked(2, ""), stop)
	cost := int64(10)
	paid := ev(session.TypeModelRequest, session.ModelRequest{Model: "m", CostUSDMicro: &cost})
	idle := session.Session{Status: session.StatusIdle}
	renamed := ev(session.TypeTitleChanged, session.TitleChanged{By: person, New: "Mine"})
	renamed.Seq = 1
	fork := idle
	fork.Parent = &session.Parent{SessionID: "ses_parent", Seq: 1}
	capped := idle
	capped.Budget.MaxCostUSDMicro = new(int64(10))
	for _, c := range []struct {
		s    session.Session
		evs  []session.Event
		want string
	}{
		{idle, turn, ""},
		{session.Session{Status: session.StatusRunning}, turn, "running"},
		{idle, append(turn[:4:4], said(person)), "pending"},
		{fork, []session.Event{renamed, status(session.StatusIdle)}, "a person named"},
		{capped, []session.Event{paid, status(session.StatusIdle)}, "budget"},
	} {
		if got := blocked(c.s, c.evs); (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("blocked = %q, want %q", got, c.want)
		}
	}
}

// TestTheTitleSettingIsTheAgentsLatest: the setting is read from the
// agent's latest version where the runner reaches it, and from the
// session's own bundle where it does not, or cannot answer.
func TestTheTitleSettingIsTheAgentsLatest(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	rs, err := manifest.Resolve(ctx, []byte(reviewer+"  title: {model: tier/quick}\n"), manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ref, blobs, err := AgentRef(rs[0])
	if err != nil {
		t.Fatal(err)
	}
	s := session.New(ref, f.s.Initiator, session.RunnerHosted, f.s.Machine, t0)
	if err := f.store.Create(ctx, s, blobs); err != nil {
		t.Fatal(err)
	}
	if set, err := f.r.titleSetting(ctx, s); err != nil || set == nil || *set.Model != "tier/quick" {
		t.Fatalf("the bundle's setting %+v, %v", set, err)
	}
	o := f.r.o
	o.LatestTitle = func(context.Context, string) (*v1.Title, error) { return nil, nil }
	latest, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	if set, err := latest.titleSetting(ctx, s); err != nil || set != nil {
		t.Fatalf("the latest version turned titles off and reads %+v, %v", set, err)
	}
	o.LatestTitle = func(context.Context, string) (*v1.Title, error) { return nil, errors.New("the store is down") }
	unreachable, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	if set, err := unreachable.titleSetting(ctx, s); err != nil || set == nil {
		t.Fatalf("an unreachable store reads %+v, %v", set, err)
	}
	if set, err := f.r.titleSetting(ctx, f.s); err != nil || set != nil {
		t.Fatalf("a session of no bundle reads %+v, %v", set, err)
	}
}

// hosted creates a hosted session of the fixture's agent that ran one
// turn its first message started, sent by by, and appends after it the
// events of more.
func (f *fixture) hosted(t *testing.T, by session.Sender, more ...func(id string)) string {
	t.Helper()
	return f.hostedAs(t, by, nil, more...)
}

// hostedAs is hosted with the session's header changed by header before
// it is created.
func (f *fixture) hostedAs(t *testing.T, by session.Sender, header func(*session.Session), more ...func(id string)) string {
	t.Helper()
	ctx := t.Context()
	s := session.New(f.s.Agent, f.s.Initiator, session.RunnerHosted, session.Machine{Kind: session.MachineHost}, t0)
	if header != nil {
		header(&s)
	}
	if err := f.store.Create(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	f.sendTo(ctx, s.ID, session.TypeUserMessage, session.UserMessage{Sender: by, Content: []lux.Block{{Type: ir.BlockText, Text: "Plan the release."}}})
	f.sendTo(ctx, s.ID, session.TypeSessionStatus, session.SessionStatus{Status: session.StatusRunning})
	f.sendTo(ctx, s.ID, session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockText, Text: "Planned."}}}, StopReason: ir.StopEndTurn})
	f.sendTo(ctx, s.ID, session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopEndTurn})
	for _, m := range more {
		m(s.ID)
	}
	return s.ID
}

// TestThePassNamesEachDueSessionOnce: the pass names the idle hosted
// sessions of the agents it is given that were never named, one at a
// time, under each one's lease; it skips a session a person renamed, one
// already named, one whose lease another holder has, one no person wrote
// to and one that is running, counts a session it could not build a
// harness for as failed, and names nothing new when it runs again but
// the session whose lease was held then (spec 083).
func TestThePassNamesEachDueSessionOnce(t *testing.T) {
	f := setup(t)
	tg := title(t, f, &v1.Title{})
	ctx := t.Context()
	person, trigger := f.s.Initiator, session.Sender{Subject: "trigger:trg_1", Kind: session.SenderTrigger}
	due := f.hosted(t, person)
	f.hosted(t, person, func(id string) {
		f.sendTo(ctx, id, session.TypeTitleChanged, session.TitleChanged{By: person, New: "Mine"})
	})
	f.hosted(t, person, func(id string) {
		f.sendTo(ctx, id, session.TypeModelRequest, session.ModelRequest{Model: model, Outcome: "ok", Purpose: session.PurposeTitle})
	})
	held := f.hosted(t, person)
	f.hosted(t, trigger)
	f.hosted(t, person, func(id string) {
		f.sendTo(ctx, id, session.TypeSessionStatus, session.SessionStatus{Status: session.StatusRunning})
	})
	// A hosted session handed to an external writer is the writer's to
	// append to.
	f.hostedAs(t, person, func(s *session.Session) {
		s.Writer = &session.Writer{Kind: session.RunnerExternal, Subject: person.Subject, Since: t0}
	})
	broken := f.hosted(t, person)
	f.works[broken] = filepath.Join(t.TempDir(), "gone")
	lease, err := f.store.Acquire(ctx, held, session.Holder{Runner: "another", AcquiredAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	f.stub.Script(model, named("Release plan"), named("Release plan"))
	c, err := tg.r.TitlePass(ctx, []string{f.s.Agent.ID, "agent_other"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if c != (TitleCounts{Named: 1, Skipped: 5, Failed: 1}) {
		t.Fatalf("the first pass %+v", c)
	}
	if got := f.titles(ctx, due); len(got) != 1 || got[0].New != "Release plan" || got[0].By.Subject != session.HarnessSubject {
		t.Fatalf("the due session's titles %+v", got)
	}
	if got := tg.questions(); len(got) != 1 || got[0] != due+" "+model {
		t.Fatalf("the questions %v", got)
	}
	if !strings.Contains(tg.logged(), "session="+broken) {
		t.Fatalf("the failed session is not logged: %s", tg.logged())
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if again, err := f.store.Acquire(ctx, due, session.Holder{Runner: "check", AcquiredAt: t0}); err != nil {
		t.Fatalf("the pass kept the lease of %s: %v", due, err)
	} else if err := again.Release(); err != nil {
		t.Fatal(err)
	}
	f.works[broken] = f.work
	f.stub.Script(model, named("Second plan"))
	c, err = tg.r.TitlePass(ctx, []string{f.s.Agent.ID}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.Named != 2 || c.Failed != 0 || len(f.titles(ctx, due)) != 1 {
		t.Fatalf("the second pass %+v, the named session's titles %+v", c, f.titles(ctx, due))
	}
}

// TestThePassWaitsBetweenSessions: the pass waits its interval after a
// session it asked about, and stops when its context ends.
func TestThePassWaitsBetweenSessions(t *testing.T) {
	f := setup(t)
	tg := title(t, f, &v1.Title{})
	f.hosted(t, f.s.Initiator)
	f.hosted(t, f.s.Initiator)
	f.stub.Script(model, named("One"), named("Two"))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	c, err := tg.r.TitlePass(ctx, []string{f.s.Agent.ID}, time.Hour)
	if !errors.Is(err, context.DeadlineExceeded) || c.Named != 1 {
		t.Fatalf("the pass %+v, %v", c, err)
	}
}

// lateStore is the fixture's store, which appends a person's message
// right after the batch that closes a turn, as a client's message
// arrives the moment the turn ends, before the drive reads the log again.
type lateStore struct {
	*dir.Store
	f    *fixture
	once sync.Once
}

func (l *lateStore) Append(ctx context.Context, id string, after uint64, evs []session.Event) (uint64, error) {
	last, err := l.Store.Append(ctx, id, after, evs)
	var p session.SessionStatus
	if err == nil && evs[len(evs)-1].Type == session.TypeSessionStatus && evs[len(evs)-1].Decode(&p) == nil && p.Status == session.StatusIdle {
		l.once.Do(func() { l.f.message(ctx, "One more thing.") })
	}
	return last, err
}

// TestInputAtTheTurnsEndStartsTheNextTurn: a message appended once the
// turn closed, which the harness did not see pending, starts the next
// turn in the same drive with no title asked between, and the title is
// asked after the turn it started; an agent's setting that cannot be read
// is logged and names nothing (spec 083).
func TestInputAtTheTurnsEndStartsTheNextTurn(t *testing.T) {
	f := setup(t)
	tg := title(t, f, &v1.Title{})
	o := tg.r.o
	o.Store = &lateStore{Store: f.store, f: f}
	r, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	f.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: "First."}), reply(ir.Block{Type: ir.BlockText, Text: "Second."}), named("Two things"))
	f.message(ctx, "One thing.")
	if _, err := r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	if f.count(ctx, session.TypeAgentMessage) != 2 || len(tg.questions()) != 1 || len(f.titles(ctx, f.s.ID)) != 1 {
		t.Fatalf("%d answers, %d questions, %d titles", f.count(ctx, session.TypeAgentMessage), len(tg.questions()), len(f.titles(ctx, f.s.ID)))
	}

	g := setup(t)
	gone := title(t, g, &v1.Title{})
	o = gone.r.o
	o.LatestTitle = func(context.Context, string) (*v1.Title, error) { return nil, errors.New("the store is down") }
	if r, err = New(o); err != nil {
		t.Fatal(err)
	}
	g.restate(t, func(s *session.Session) { s.Agent.Bundle = session.DigestOf([]byte("never stored")) })
	g.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: "Done."}))
	g.message(ctx, "Do it.")
	if _, err := r.Drive(ctx, g.s.ID); err != nil {
		t.Fatal(err)
	}
	if len(gone.questions()) != 0 || !strings.Contains(gone.logged(), "the agent's setting could not be read") {
		t.Fatalf("asked %v; the log says %s", gone.questions(), gone.logged())
	}
}
