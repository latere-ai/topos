// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package triggers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/internal/store/storetest"
	"latere.ai/x/topos/manifest/trigger"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// errBusy is a transient error of the fake actor, as an unavailable
// authorizer is.
var errBusy = errors.New("the authorizer did not answer")

// fakeActor starts and continues sessions in a session store, as the
// server's actor does without the authorizer, and fails as told.
type fakeActor struct {
	mu        sync.Mutex
	sessions  session.Store
	now       func() time.Time
	startErr  error
	sendErr   error
	starts    int
	sends     int
	startWait chan struct{}
}

func (a *fakeActor) Start(ctx context.Context, st Start) (session.Session, error) {
	a.mu.Lock()
	err, wait := a.startErr, a.startWait
	a.starts++
	a.mu.Unlock()
	if wait != nil {
		<-wait
	}
	if err != nil {
		return session.Session{}, err
	}
	s := session.New(session.AgentRef{ID: "agent_01J9Z3Q4W8KX6T0M2V5N7R1B3C", Version: 1}, session.Sender{Subject: st.Trigger.Owner, Kind: session.SenderPerson}, session.RunnerHosted, session.Machine{Kind: session.MachineHost}, a.now())
	s.TriggerID, s.Title = st.Trigger.ID, st.Title
	if err := a.sessions.Create(ctx, s, nil); err != nil {
		return session.Session{}, err
	}
	return s, a.say(ctx, s.ID, st.Trigger, st.FiringID, st.Message)
}

func (a *fakeActor) say(ctx context.Context, id string, t store.Trigger, firing, text string) error {
	ev, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: session.Sender{Subject: session.TriggerSubjectPrefix + t.ID, Kind: session.SenderTrigger},
		Content: []lux.Block{{Type: ir.BlockText, Text: text}}, FiringID: firing}, a.now())
	if err != nil {
		return err
	}
	s, err := a.sessions.Get(ctx, id)
	if err != nil {
		return err
	}
	if s.Status == session.StatusEnded {
		return ErrEnded
	}
	batch := []session.Event{ev}
	session.Stamp(id, s.LastSeq, batch)
	_, err = a.sessions.Append(ctx, id, s.LastSeq, batch)
	return err
}

func (a *fakeActor) Send(ctx context.Context, sd Send) error {
	a.mu.Lock()
	err := a.sendErr
	a.sends++
	a.mu.Unlock()
	if err != nil {
		return err
	}
	return a.say(ctx, sd.SessionID, sd.Trigger, sd.FiringID, sd.Message)
}

func (a *fakeActor) Refusal(err error) (string, bool) {
	if errors.Is(err, errBusy) {
		return "authorizer_unavailable", true
	}
	return "forbidden", false
}

type rig struct {
	t        *testing.T
	clock    *storetest.Clock
	objects  *store.Memory
	sessions session.Store
	actor    *fakeActor
	engine   *Engine
}

func newRig(t *testing.T) *rig {
	t.Helper()
	clock := storetest.NewClock()
	r := &rig{t: t, clock: clock, objects: store.NewMemory(clock.Now), sessions: session.NewMemoryStore()}
	r.actor = &fakeActor{sessions: r.sessions, now: clock.Now}
	r.engine = r.newEngine()
	return r
}

func (r *rig) newEngine() *Engine {
	r.t.Helper()
	e, err := New(Options{Store: r.objects, Sessions: r.sessions, Actor: r.actor, Now: r.clock.Now})
	if err != nil {
		r.t.Fatal(err)
	}
	return e
}

// put stores a trigger of the spec, as JSON, with its next fire time.
func (r *rig) put(name, spec string) store.Trigger {
	r.t.Helper()
	id := session.NewID(session.PrefixTrigger)
	doc := fmt.Sprintf(`{"apiVersion":"topos.latere.ai/v1","kind":"Trigger","metadata":{"name":%q},"spec":%s,"status":{"id":%q,"version":1,"digest":"sha256:1"}}`, name, spec, id)
	t := store.Trigger{ID: id, Name: name, Owner: "alice", AgentID: "agent_01J9Z3Q4W8KX6T0M2V5N7R1B3C", Version: 1, Digest: "sha256:1", Doc: []byte(doc), CreatedAt: r.clock.Now()}
	sp, err := Spec(t)
	if err != nil {
		r.t.Fatal(err)
	}
	if t.NextFireAt, err = NextFire(sp, r.clock.Now()); err != nil {
		r.t.Fatal(err)
	}
	if err := r.objects.PutTrigger(r.t.Context(), t); err != nil {
		r.t.Fatal(err)
	}
	return t
}

func (r *rig) env(id, resource string) *trigger.Envelope {
	return &trigger.Envelope{ID: id, Product: "github", Verb: "issue.opened", Resource: resource, Time: r.clock.Now(), Payload: json.RawMessage(`{"n":1}`)}
}

func (r *rig) fire(t store.Trigger, env *trigger.Envelope, outcome string) store.Firing {
	r.t.Helper()
	f, err := r.engine.Fire(r.t.Context(), t, env)
	if err != nil || f.Outcome != outcome {
		r.t.Fatalf("fire: %+v, %v; want %s", f, err, outcome)
	}
	return f
}

const eventContinue = `{"agent":"agent_01J9Z3Q4W8KX6T0M2V5N7R1B3C","timeZone":"UTC","on":{"product":"github","verbs":["*"]},"session":{"message":"{{event.resource}} {{event.payload.n}}","policy":"continue","endOnIdle":false},"skipIfActive":true,"maxAge":"1h","suspend":false}`

func TestNewRefusesAnIncompleteEngine(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("an engine with nothing")
	}
}

// TestTwoEnginesStartOneSessionPerKey: two engines over one store,
// firing events of one key at once, start one session and send it the
// rest, since a trigger's firings act one at a time under its lease.
func TestTwoEnginesStartOneSessionPerKey(t *testing.T) {
	r := newRig(t)
	tr := r.put("triage", eventContinue)
	engines := []*Engine{r.engine, r.newEngine()}
	var wg sync.WaitGroup
	outcomes := make(chan string, 16)
	for i := range 16 {
		wg.Go(func() {
			f, err := engines[i%2].Fire(t.Context(), tr, r.env(fmt.Sprint(i), "o/r#1"))
			if err != nil {
				t.Error(err)
			}
			outcomes <- f.Outcome
		})
	}
	wg.Wait()
	close(outcomes)
	n := map[string]int{}
	for o := range outcomes {
		n[o]++
	}
	if n[store.OutcomeStarted] != 1 || n[store.OutcomeContinued] != 15 {
		t.Fatalf("outcomes %v", n)
	}
	all, _, err := r.sessions.List(t.Context(), session.ListOptions{})
	if err != nil || len(all) != 1 {
		t.Fatalf("%d sessions, %v", len(all), err)
	}
}

// TestAStartThatFailsRunsAgain: a transient error of the actor fails the
// firing, which runs again when it is fired again; a refusal is recorded
// with its reason, and a send the actor refuses is refused.
func TestAStartThatFailsRunsAgain(t *testing.T) {
	r := newRig(t)
	tr := r.put("triage", eventContinue)
	r.actor.startErr = errBusy
	failed := r.fire(tr, r.env("1", "o/r#1"), store.OutcomeFailed)
	if failed.Reason != "authorizer_unavailable" {
		t.Fatalf("the failed firing: %+v", failed)
	}
	r.actor.startErr = errors.New("denied")
	refused := r.fire(tr, r.env("2", "o/r#2"), store.OutcomeRefused)
	if refused.Reason != "forbidden" {
		t.Fatalf("the refused firing: %+v", refused)
	}
	r.actor.startErr = nil
	if again := r.fire(tr, r.env("1", "o/r#1"), store.OutcomeStarted); again.ID != failed.ID {
		t.Fatalf("the rerun is %s, not %s", again.ID, failed.ID)
	}
	r.actor.sendErr = errors.New("denied")
	r.fire(tr, r.env("3", "o/r#1"), store.OutcomeRefused)
}

// TestASessionThatEndsUnderASendStartsTheNext: a send to a session that
// ended between the look and the send starts the key's next session.
func TestASessionThatEndsUnderASendStartsTheNext(t *testing.T) {
	r := newRig(t)
	tr := r.put("triage", eventContinue)
	first := r.fire(tr, r.env("1", "o/r#1"), store.OutcomeStarted)
	r.actor.sendErr = ErrEnded
	next := r.fire(tr, r.env("2", "o/r#1"), store.OutcomeStarted)
	if next.SessionID == first.SessionID {
		t.Fatal("the ended session took the message")
	}
	// A deleted session is a key with no open session too.
	r.actor.sendErr = nil
	if err := r.sessions.Delete(t.Context(), next.SessionID); err != nil {
		t.Fatal(err)
	}
	third := r.fire(tr, r.env("3", "o/r#1"), store.OutcomeStarted)
	if third.SessionID == next.SessionID {
		t.Fatal("a deleted session took the message")
	}
	if open, err := r.objects.OpenFirings(t.Context(), tr.ID); err != nil || len(open) != 1 || open[0].SessionID != third.SessionID {
		t.Fatalf("open firings %+v, %v", open, err)
	}
}

// TestAnActiveSessionIsReadFromItsLog: a session idle with a message
// after its last status is active however long its log, and one whose
// last status follows its input is not.
func TestAnActiveSessionIsReadFromItsLog(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	s := session.New(session.AgentRef{ID: "agent_01J9Z3Q4W8KX6T0M2V5N7R1B3C", Version: 1}, session.Sender{Subject: "alice", Kind: session.SenderPerson}, session.RunnerHosted, session.Machine{Kind: session.MachineHost}, r.clock.Now())
	if err := r.sessions.Create(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	add := func(evs ...session.Event) {
		t.Helper()
		got, err := r.sessions.Get(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		session.Stamp(s.ID, got.LastSeq, evs)
		if _, err := r.sessions.Append(ctx, s.ID, got.LastSeq, evs); err != nil {
			t.Fatal(err)
		}
	}
	ev := func(typ session.Type, p any) session.Event {
		e, err := session.NewEvent(typ, p, r.clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	check := func(want bool) {
		t.Helper()
		got, err := r.sessions.Get(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if busy, err := active(ctx, r.sessions, got); err != nil || busy != want {
			t.Fatalf("active %v, %v; want %v", busy, err, want)
		}
	}
	check(false)
	add(ev(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusRunning}))
	check(true)
	var long []session.Event
	for range 100 {
		long = append(long, ev(session.TypeAgentMessage, map[string]any{"message": map[string]any{"role": "assistant"}}))
	}
	add(long...)
	add(ev(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopEndTurn}))
	check(false)
	add(ev(session.TypeUserMessage, session.UserMessage{Sender: session.Sender{Subject: "alice", Kind: session.SenderPerson}, Content: []lux.Block{{Type: ir.BlockText, Text: "more"}}}))
	add(long...)
	check(true)
	add(ev(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopToolResult}))
	check(true)
	add(ev(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusEnded, StopReason: session.StopCompleted}))
	check(false)
}

// TestAStoredTriggerThatNoLongerReadsIsAnError: a trigger whose stored
// document, schedule, zone or template no longer reads fails its
// firing as an error and fires nothing.
func TestAStoredTriggerThatNoLongerReadsIsAnError(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	bad := store.Trigger{ID: session.NewID(session.PrefixTrigger), Name: "bad", Owner: "alice", Version: 1, Digest: "d", Doc: []byte("{")}
	if _, err := r.engine.Fire(ctx, bad, nil); err == nil {
		t.Fatal("a trigger whose document does not read fired")
	}
	tpl := r.put("tpl", strings.Replace(eventContinue, "{{event.resource}}", "{{nope", 1))
	if _, err := r.engine.Fire(ctx, tpl, r.env("1", "o/r#1")); err == nil {
		t.Fatal("a template that does not parse rendered")
	}
	if _, err := r.engine.Fire(ctx, tpl, nil); err == nil {
		t.Fatal("an event trigger fired with no event")
	}
	sched := r.put("sched", `{"agent":"a","schedule":"@hourly","timeZone":"UTC","session":{"message":"x","endOnIdle":true},"skipIfActive":true,"maxAge":"1h","suspend":false}`)
	if _, err := r.engine.Fire(ctx, sched, r.env("1", "o/r#1")); err == nil {
		t.Fatal("a schedule trigger took an event")
	}
	// A zone the build no longer knows leaves the schedule due.
	sched.Doc = []byte(strings.Replace(string(sched.Doc), `"timeZone":"UTC"`, `"timeZone":"Mars/Olympus"`, 1))
	if err := r.objects.PutTrigger(ctx, sched); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(time.Hour)
	if err := r.engine.Tick(ctx); err == nil || !strings.Contains(err.Error(), "Mars/Olympus") {
		t.Fatalf("a tick of an unknown zone: %v", err)
	}
	if _, err := NextFire(v1.TriggerSpec{Schedule: "nope"}, r.clock.Now()); err == nil {
		t.Fatal("a schedule that does not parse")
	}
	if next, err := NextFire(v1.TriggerSpec{Schedule: "0 0 30 2 *", TimeZone: "UTC"}, r.clock.Now()); err != nil || next != nil {
		t.Fatalf("a schedule that never fires: %v, %v", next, err)
	}
	late := r.put("late", strings.Replace(eventContinue, `"maxAge":"1h"`, `"maxAge":"x"`, 1))
	if _, err := r.engine.Fire(ctx, late, r.env("1", "o/r#1")); err == nil {
		t.Fatal("a maxAge that does not parse")
	}
}

// TestALeaseWaitEndsWithItsContext: a firing that waits on a lease
// another replica holds gives up when its context ends.
func TestALeaseWaitEndsWithItsContext(t *testing.T) {
	r := newRig(t)
	tr := r.put("triage", eventContinue)
	if ok, err := r.objects.LeaseTrigger(t.Context(), tr.ID, "another", r.clock.Now().Add(time.Hour)); err != nil || !ok {
		t.Fatalf("lease: %v, %v", ok, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := r.engine.locked(ctx, tr.ID, func() error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the wait: %v", err)
	}
	if err := r.engine.locked(t.Context(), session.NewID(session.PrefixTrigger), func() error { return nil }); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the lease of no trigger: %v", err)
	}
}

// TestTheSweepSendsHeldFiringsInOrder: held firings of three keys, whose
// sessions wait on a budget, a confirmation and a question, are sent by
// one pass once each key's session runs on, each key in the order its
// firings arrived.
func TestTheSweepSendsHeldFiringsInOrder(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	tr := r.put("triage", eventContinue)
	wait := func(id string, reason session.StopReason) {
		t.Helper()
		s, err := r.sessions.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		ev, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: reason}, r.clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		batch := []session.Event{ev}
		session.Stamp(id, s.LastSeq, batch)
		if _, err := r.sessions.Append(ctx, id, s.LastSeq, batch); err != nil {
			t.Fatal(err)
		}
	}
	a := r.fire(tr, r.env("1", "o/r#1"), store.OutcomeStarted)
	b := r.fire(tr, r.env("2", "o/r#2"), store.OutcomeStarted)
	c := r.fire(tr, r.env("3", "o/r#3"), store.OutcomeStarted)
	wait(a.SessionID, session.StopBudget)
	wait(b.SessionID, session.StopToolConfirmation)
	wait(c.SessionID, session.StopQuestion)
	var held []string
	for i, res := range []string{"o/r#1", "o/r#2", "o/r#1", "o/r#3"} {
		held = append(held, r.fire(tr, r.env(fmt.Sprint(10+i), res), store.OutcomeHeld).ID)
	}
	wait(a.SessionID, session.StopEndTurn)
	wait(b.SessionID, session.StopEndTurn)
	wait(c.SessionID, session.StopEndTurn)
	if err := r.engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	list, _, err := r.objects.Firings(ctx, tr.ID, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range list {
		if f.Outcome == store.OutcomeHeld {
			t.Fatalf("still held: %+v", f)
		}
	}
	evs, err := r.sessions.Events(ctx, a.SessionID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, e := range evs {
		var m session.UserMessage
		if e.Type == session.TypeUserMessage && e.Decode(&m) == nil {
			order = append(order, m.FiringID)
		}
	}
	if len(order) != 3 || order[1] != held[0] || order[2] != held[2] {
		t.Fatalf("the first key's messages %v, want the held %s then %s", order, held[0], held[2])
	}
}

const eventNew = `{"agent":"agent_01J9Z3Q4W8KX6T0M2V5N7R1B3C","timeZone":"UTC","on":{"product":"github","verbs":["issue.*"],"resources":["o/*"]},` +
	`"session":{"message":"{{event.resource}}","title":"{{event.verb}} {{trigger.name}}","resources":[{"type":"repository","url":"https://git.example/{{event.resource}}.git","ref":"{{event.payload.n}}"}],"endOnIdle":true},` +
	`"skipIfActive":true,"maxActive":2,"maxAge":"1h","suspend":false}`

// TestTheNewPolicyInTheEngine: a new firing starts a session titled and
// placed in the repository its templates render, skips a key whose
// session is active, stops at maxActive, and filters an event outside
// on; a suspended trigger fires nothing.
func TestTheNewPolicyInTheEngine(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	tr := r.put("triage", eventNew)
	var started []Start
	starts := &recordingActor{fakeActor: r.actor, started: &started}
	r.engine.o.Actor = starts
	first := r.fire(tr, r.env("1", "o/a"), store.OutcomeStarted)
	if len(started) != 1 || started[0].Title != "issue.opened triage" || len(started[0].Resources) != 1 ||
		started[0].Resources[0].URL != "https://git.example/o/a.git" || started[0].Resources[0].Ref != "1" || started[0].FiringID != first.ID {
		t.Fatalf("the start: %+v", started)
	}
	r.fire(tr, r.env("2", "o/a"), store.OutcomeSkippedActive)
	r.fire(tr, r.env("3", "o/b"), store.OutcomeStarted)
	r.fire(tr, r.env("4", "o/c"), store.OutcomeSkippedBusy)
	other := r.env("5", "x/y")
	if f := r.fire(tr, other, store.OutcomeFiltered); f.ID != "" || len(f.Envelope) == 0 {
		t.Fatalf("a filtered event: %+v", f)
	}
	got, err := r.objects.Trigger(ctx, tr.ID)
	if err != nil || got.Counts.Filtered != 1 || got.Counts.Started != 2 || got.Counts.SkippedActive != 1 || got.Counts.SkippedBusy != 1 {
		t.Fatalf("counts %+v, %v", got.Counts, err)
	}
	paused := r.put("paused", strings.Replace(eventNew, `"suspend":false`, `"suspend":true`, 1))
	if _, err := r.engine.Fire(ctx, paused, r.env("1", "o/a")); !errors.Is(err, ErrSuspended) {
		t.Fatalf("a suspended trigger: %v", err)
	}
	store := r.put("store", strings.Replace(eventNew, `{"type":"repository","url":"https://git.example/{{event.resource}}.git","ref":"{{event.payload.n}}"}`, `{"type":"memoryStore","memoryStore":"mem_01J9Z3Q4W8KX6T0M2V5N7R1B3C","access":"readOnly"}`, 1))
	if _, err := r.engine.Fire(ctx, store, r.env("1", "o/a")); err == nil {
		t.Fatal("a memory store rendered")
	}
}

// recordingActor keeps every start it is asked for.
type recordingActor struct {
	*fakeActor
	started *[]Start
}

func (a *recordingActor) Start(ctx context.Context, st Start) (session.Session, error) {
	*a.started = append(*a.started, st)
	return a.fakeActor.Start(ctx, st)
}

// TestTheMinuteLoopInTheEngine: a due schedule fires each scheduled time
// once, the late ones skipped, and moves its next fire time; a pass of
// another engine in the same minute fires nothing more, and a schedule
// with nothing left to fire keeps none.
func TestTheMinuteLoopInTheEngine(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	tr := r.put("hourly", `{"agent":"agent_01J9Z3Q4W8KX6T0M2V5N7R1B3C","schedule":"@hourly","timeZone":"UTC","session":{"message":"{{firing.time}}","endOnIdle":true},"skipIfActive":false,"maxAge":"1h","suspend":false}`)
	r.clock.Advance(2*time.Hour + time.Minute)
	for _, e := range []*Engine{r.engine, r.newEngine()} {
		if err := e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	list, _, err := r.objects.Firings(ctx, tr.ID, 0, "")
	if err != nil || len(list) != 2 || list[0].Outcome != store.OutcomeStarted || list[1].Outcome != store.OutcomeSkippedLate || list[1].Dedupe != ScheduleDedupe(time.Date(2026, 9, 27, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("firings %+v, %v", list, err)
	}
	got, err := r.objects.Trigger(ctx, tr.ID)
	if err != nil || got.NextFireAt == nil || !got.NextFireAt.Equal(time.Date(2026, 9, 27, 15, 0, 0, 0, time.UTC)) {
		t.Fatalf("next fire %+v, %v", got.NextFireAt, err)
	}
	// A trigger suspended since the store listed it fires nothing.
	got.Suspended = true
	if err := r.objects.PutTrigger(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := r.engine.schedule(ctx, tr.ID, r.clock.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if list, _, err := r.objects.Firings(ctx, tr.ID, 0, ""); err != nil || len(list) != 2 {
		t.Fatalf("a suspended schedule fired: %d, %v", len(list), err)
	}
	if err := r.engine.schedule(ctx, session.NewID(session.PrefixTrigger), r.clock.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the schedule of no trigger: %v", err)
	}
}

var errDisk = errors.New("the disk is gone")

// faultyTriggers fails the one method fail names.
type faultyTriggers struct {
	*store.Memory
	fail string
}

func (f *faultyTriggers) err(name string) error {
	if f.fail == name {
		return errDisk
	}
	return nil
}

func (f *faultyTriggers) Trigger(ctx context.Context, id string) (store.Trigger, error) {
	if err := f.err("Trigger"); err != nil {
		return store.Trigger{}, err
	}
	return f.Memory.Trigger(ctx, id)
}

func (f *faultyTriggers) DueTriggers(ctx context.Context, at time.Time) ([]store.Trigger, error) {
	if err := f.err("DueTriggers"); err != nil {
		return nil, err
	}
	return f.Memory.DueTriggers(ctx, at)
}

func (f *faultyTriggers) SetNextFire(ctx context.Context, id string, at *time.Time) error {
	if err := f.err("SetNextFire"); err != nil {
		return err
	}
	return f.Memory.SetNextFire(ctx, id, at)
}

func (f *faultyTriggers) LeaseTrigger(ctx context.Context, id, holder string, until time.Time) (bool, error) {
	if err := f.err("LeaseTrigger"); err != nil {
		return false, err
	}
	return f.Memory.LeaseTrigger(ctx, id, holder, until)
}

func (f *faultyTriggers) ReleaseTrigger(ctx context.Context, id, holder string) error {
	if err := f.err("ReleaseTrigger"); err != nil {
		return err
	}
	return f.Memory.ReleaseTrigger(ctx, id, holder)
}

func (f *faultyTriggers) ClaimFiring(ctx context.Context, fr store.Firing) (store.Firing, bool, error) {
	if err := f.err("ClaimFiring"); err != nil {
		return store.Firing{}, false, err
	}
	return f.Memory.ClaimFiring(ctx, fr)
}

func (f *faultyTriggers) RecordFiring(ctx context.Context, fr store.Firing, from string) error {
	if err := f.err("RecordFiring"); err != nil {
		return err
	}
	return f.Memory.RecordFiring(ctx, fr, from)
}

func (f *faultyTriggers) CountFiltered(ctx context.Context, id string) error {
	if err := f.err("CountFiltered"); err != nil {
		return err
	}
	return f.Memory.CountFiltered(ctx, id)
}

func (f *faultyTriggers) HeldFirings(ctx context.Context) ([]store.Firing, error) {
	if err := f.err("HeldFirings"); err != nil {
		return nil, err
	}
	return f.Memory.HeldFirings(ctx)
}

func (f *faultyTriggers) OpenFirings(ctx context.Context, id string) ([]store.Firing, error) {
	if err := f.err("OpenFirings"); err != nil {
		return nil, err
	}
	return f.Memory.OpenFirings(ctx, id)
}

func (f *faultyTriggers) CloseSession(ctx context.Context, id, sessionID string) error {
	if err := f.err("CloseSession"); err != nil {
		return err
	}
	return f.Memory.CloseSession(ctx, id, sessionID)
}

func (f *faultyTriggers) TriggerSession(ctx context.Context, id, key string) (string, error) {
	if err := f.err("TriggerSession"); err != nil {
		return "", err
	}
	return f.Memory.TriggerSession(ctx, id, key)
}

func (f *faultyTriggers) SetTriggerSession(ctx context.Context, id, key, sessionID string) error {
	if err := f.err("SetTriggerSession"); err != nil {
		return err
	}
	return f.Memory.SetTriggerSession(ctx, id, key, sessionID)
}

// faultySessions fails the one method fail names, once armed.
type faultySessions struct {
	session.Store
	fail  string
	armed bool
}

func (f *faultySessions) Get(ctx context.Context, id string) (session.Session, error) {
	if f.armed && f.fail == "Get" {
		return session.Session{}, errDisk
	}
	return f.Store.Get(ctx, id)
}

func (f *faultySessions) Events(ctx context.Context, id string, from uint64, limit int) ([]session.Event, error) {
	if f.armed && f.fail == "Events" {
		return nil, errDisk
	}
	return f.Store.Events(ctx, id, from, limit)
}

// TestAStoreThatFailsFailsTheFiring: every store call a firing, a pass
// of the minute loop or the sweep makes returns its error, and the
// engine returns it rather than going on.
func TestAStoreThatFailsFailsTheFiring(t *testing.T) {
	for _, name := range []string{"Trigger", "DueTriggers", "SetNextFire", "LeaseTrigger", "ClaimFiring", "RecordFiring", "CountFiltered",
		"HeldFirings", "OpenFirings", "CloseSession", "TriggerSession", "SetTriggerSession", "Get", "Events", "ReleaseTrigger"} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			ctx := t.Context()
			objects := &faultyTriggers{Memory: r.objects}
			sessions := &faultySessions{Store: r.sessions, fail: name}
			e, err := New(Options{Store: objects, Sessions: sessions, Actor: r.actor, Now: r.clock.Now})
			if err != nil {
				t.Fatal(err)
			}
			cont := r.put("continue", eventContinue)
			fresh := r.put("fresh", eventNew)
			sched := r.put("hourly", `{"agent":"a","schedule":"@hourly","timeZone":"UTC","session":{"message":"x","endOnIdle":true},"skipIfActive":false,"maxAge":"1h","suspend":false}`)
			// A session of each trigger to find, one waiting for a person
			// with a firing held behind it, and one gone.
			first, err := e.Fire(ctx, cont, r.env("1", "o/r#1"))
			if err != nil {
				t.Fatal(err)
			}
			waitOn := func(id string) {
				s, err := r.sessions.Get(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				ev, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopBudget}, r.clock.Now())
				if err != nil {
					t.Fatal(err)
				}
				batch := []session.Event{ev}
				session.Stamp(id, s.LastSeq, batch)
				if _, err := r.sessions.Append(ctx, id, s.LastSeq, batch); err != nil {
					t.Fatal(err)
				}
			}
			waitOn(first.SessionID)
			if _, err := e.Fire(ctx, cont, r.env("2", "o/r#1")); err != nil {
				t.Fatal(err)
			}
			gone, err := e.Fire(ctx, fresh, r.env("1", "o/a"))
			if err != nil {
				t.Fatal(err)
			}
			if err := r.sessions.Delete(ctx, gone.SessionID); err != nil {
				t.Fatal(err)
			}
			objects.fail, sessions.armed = name, true
			r.clock.Advance(time.Hour)
			var errs []error
			for _, call := range []func() error{
				func() error { _, err := e.Fire(ctx, cont, r.env("3", "o/r#1")); return err },
				func() error { _, err := e.Fire(ctx, fresh, r.env("2", "o/a")); return err },
				func() error { _, err := e.Fire(ctx, fresh, r.env("3", "x/y")); return err },
				func() error { _, err := e.Fire(ctx, sched, nil); return err },
				func() error { return e.Tick(ctx) },
			} {
				errs = append(errs, call())
			}
			if name == "ReleaseTrigger" {
				// A lease that cannot be released is logged, and expires.
				if err := errors.Join(errs...); err != nil {
					t.Fatalf("a failed release failed a firing: %v", err)
				}
				return
			}
			if !errors.Is(errors.Join(errs...), errDisk) {
				t.Fatalf("no call returned the store's error: %v", errs)
			}
		})
	}
}
