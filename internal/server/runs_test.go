// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-yaml"

	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/manifest/trigger"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// runsPage is a page of GET /triggers/runs.
type runsPage struct {
	Items      []triggerRuns `json:"items"`
	NextCursor string        `json:"next_cursor"`
}

// runs reads GET /triggers/runs with query as token and wants a 200.
func (f *triggerFixture) runs(token, query string) runsPage {
	f.t.Helper()
	a := f.do(http.MethodGet, "/v1/triggers/runs"+query, token, "")
	if a.status != http.StatusOK {
		f.t.Fatalf("read the runs: %d %s", a.status, a.body)
	}
	var out runsPage
	a.decode(f.t, &out)
	return out
}

// byName finds the item of the trigger of name in a page.
func (p runsPage) byName(t *testing.T, name string) triggerRuns {
	t.Helper()
	for _, it := range p.Items {
		if it.Name == name {
			return it
		}
	}
	t.Fatalf("the page has no trigger %s: %+v", name, p.Items)
	return triggerRuns{}
}

// turnCosts closes the turn a firing's message began in session id, as
// its runner does, with a model request that cost micro.
func (f *triggerFixture) turnCosts(id string, micro int64) {
	f.t.Helper()
	f.runnerAppends(id, f.status(session.StatusRunning, ""),
		f.event(session.TypeModelRequest, session.ModelRequest{Model: "m", Outcome: "ok", CostUSDMicro: new(micro)}),
		f.status(session.StatusIdle, session.StopEndTurn))
}

// at is a time as a query parameter.
func at(t time.Time) string { return url.QueryEscape(t.UTC().Format(time.RFC3339)) }

// TestTheRunsOfAnOwnersTriggers (spec 088): the read answers each trigger
// the list admits with its fields; a schedule's fire times up to until in
// its zone; an event trigger's runs since since, its own and the wake
// runs of the session it started, the mean of its own; a pending wake's
// runs, its session's earlier wake, and its target naming the trigger
// that started it; a wake of a person's session, its target naming none;
// and an owner the filter leaves out is an empty page.
func TestTheRunsOfAnOwnersTriggers(t *testing.T) {
	f := newTriggerFixture(t)
	t0 := f.clock.Now()
	daily := f.applyTrigger("alice", "daily", "schedule: '0 9 * * *', timeZone: Europe/Berlin, budget: {perWeek: 400000}, session: {message: Look.}")
	watch := f.applyTrigger("alice", "watch", "on: {product: github, verbs: ['*']}, session: {message: 'Look at {{event.resource}}.', policy: new, endOnIdle: false}, skipIfActive: false")
	first := f.fired("alice", "watch", f.envelope("1", "push", "o/r", "a"), store.OutcomeStarted)
	f.turnCosts(first.SessionID, 1200)
	f.clock.Advance(time.Hour)
	second := f.fired("alice", "watch", f.envelope("2", "push", "o/r", "b"), store.OutcomeStarted)
	f.turnCosts(second.SessionID, 800)

	// A wake in the session the event trigger started runs and costs 300;
	// the next one waits.
	reach := f.api.SessionTriggers(first.SessionID)
	if _, err := reach.SetWake(t.Context(), runner.Wake{ToolUseID: "toolu_w", At: f.clock.Now().Add(time.Hour), Title: "Again", Note: "Look again."}); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Hour)
	f.tick()
	f.turnCosts(first.SessionID, 300)
	nextWake := f.clock.Now().Add(24 * time.Hour)
	if _, err := reach.SetWake(t.Context(), runner.Wake{ToolUseID: "toolu_w2", At: nextWake, Title: "Again", Note: "Look again."}); err != nil {
		t.Fatal(err)
	}
	// A wake of a session alice started herself.
	own := f.create("alice", "reviewer")
	f.runnerAppends(own.ID, f.status(session.StatusIdle, session.StopEndTurn))
	ownWake := f.clock.Now().Add(2 * time.Hour)
	if _, err := f.api.SessionTriggers(own.ID).SetWake(t.Context(), runner.Wake{ToolUseID: "toolu_o", At: ownWake, Title: "Refund", Note: "Look for the refund."}); err != nil {
		t.Fatal(err)
	}

	now := f.clock.Now()
	page := f.runs("alice", "?until="+at(now.Add(72*time.Hour)))
	if len(page.Items) != 4 {
		t.Fatalf("the page holds %d triggers: %+v", len(page.Items), page.Items)
	}
	d := page.byName(t, "daily")
	if d.TriggerID != daily.Status.ID || d.FiresOn != firesOnSchedule || d.PerWeekUSDMicro == nil || *d.PerWeekUSDMicro != 400000 || d.Wake || d.Target != nil ||
		len(d.Runs) != 0 || d.PerRunUSDMicro != nil || !d.CreatedAt.Equal(t0) || len(d.Fires) != 3 || d.MoreFires {
		t.Fatalf("the schedule's item: %+v", d)
	}
	for _, fire := range d.Fires {
		berlin, err := time.LoadLocation("Europe/Berlin")
		if err != nil {
			t.Fatal(err)
		}
		if l := fire.In(berlin); l.Hour() != 9 || l.Minute() != 0 || !fire.After(now) {
			t.Errorf("a fire time of the schedule: %v", fire)
		}
	}
	w := page.byName(t, "watch")
	if w.FiresOn != firesOnEvent || w.PerWeekUSDMicro != nil || len(w.Fires) != 0 || len(w.Runs) != 3 || w.PerRunUSDMicro == nil || *w.PerRunUSDMicro != 1000 {
		t.Fatalf("the event trigger's item: %+v", w)
	}
	if r := w.Runs[0]; r.FiringID != first.ID || r.CostUSDMicro != 1200 || !r.Ended || r.Wake || r.Outcome != store.OutcomeStarted || r.SessionID != first.SessionID {
		t.Fatalf("the event trigger's first run: %+v", r)
	}
	if r := w.Runs[2]; !r.Wake || r.CostUSDMicro != 300 || !r.Ended || r.Outcome != store.OutcomeContinued || r.SessionID != first.SessionID {
		t.Fatalf("the wake run of the event trigger's session: %+v", r)
	}
	var wake, refund triggerRuns
	for _, it := range page.Items {
		switch {
		case it.Wake && it.Target != nil && it.Target.SessionID == first.SessionID:
			wake = it
		case it.Wake && it.Target != nil && it.Target.SessionID == own.ID:
			refund = it
		}
	}
	if wake.FiresOn != firesOnAt || wake.Target.TriggerID != watch.Status.ID || len(wake.Runs) != 1 || wake.Runs[0].CostUSDMicro != 300 ||
		wake.PerRunUSDMicro == nil || *wake.PerRunUSDMicro != 300 || len(wake.Fires) != 1 || !wake.Fires[0].Equal(nextWake) {
		t.Fatalf("the pending wake's item: %+v", wake)
	}
	if refund.Target.TriggerID != "" || len(refund.Runs) != 0 || refund.PerRunUSDMicro != nil || len(refund.Fires) != 1 || !refund.Fires[0].Equal(ownWake) {
		t.Fatalf("the person's wake's item: %+v", refund)
	}

	// since keeps the runs from it; until bounds the fire times; a page of
	// one names the next.
	if w := f.runs("alice", "?since="+at(t0.Add(30*time.Minute))).byName(t, "watch"); len(w.Runs) != 2 || *w.PerRunUSDMicro != 1000 {
		t.Fatalf("the event trigger's runs from half an hour on: %+v", w)
	}
	if d := f.runs("alice", "?until="+at(now.Add(time.Minute))).byName(t, "daily"); len(d.Fires) != 0 {
		t.Fatalf("the schedule's fires within a minute: %+v", d.Fires)
	}
	if one := f.runs("alice", "?limit=1"); len(one.Items) != 1 || one.NextCursor == "" {
		t.Fatalf("a page of one: %+v", one)
	}
	// A filter that leaves bob out answers no trigger of his, and alice's
	// own read names her as the owner.
	if other := f.runs("alice", "?owner="+url.QueryEscape(bob)); len(other.Items) != 0 {
		t.Fatalf("bob's runs read by alice: %+v", other.Items)
	}
	if mine := f.runs("alice", "?owner="+url.QueryEscape(alice)); len(mine.Items) != 4 {
		t.Fatalf("alice's runs named by owner: %d", len(mine.Items))
	}
}

// countedSessions counts the reads of each session's log.
type countedSessions struct {
	session.Store
	mu    sync.Mutex
	reads map[string]int
}

func (c *countedSessions) Events(ctx context.Context, id string, from uint64, limit int) ([]session.Event, error) {
	c.mu.Lock()
	c.reads[id]++
	c.mu.Unlock()
	return c.Store.Events(ctx, id, from, limit)
}

func (c *countedSessions) read(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads[id]
}

// TestTheReadCompletesAndRecordsRuns (spec 088): a run whose turn has not
// ended is completed from its session's log at the read, its log read
// once while it runs and no more once it ended; a firing that has no run
// is recorded; and a run whose session is gone ends with the cost it
// holds.
func TestTheReadCompletesAndRecordsRuns(t *testing.T) {
	var counted *countedSessions
	f := newTriggerFixture(t, func(o *Options) {
		counted = &countedSessions{Store: o.Sessions, reads: map[string]int{}}
		o.Sessions = counted
	})
	f.applyTrigger("alice", "watch", "on: {product: github, verbs: ['*']}, session: {message: 'Look at {{event.resource}}.', policy: new, endOnIdle: false}, skipIfActive: false")
	fired := f.fired("alice", "watch", f.envelope("1", "push", "o/r", "a"), store.OutcomeStarted)
	f.runnerAppends(fired.SessionID, f.status(session.StatusRunning, ""),
		f.event(session.TypeModelRequest, session.ModelRequest{Model: "m", Outcome: "ok", CostUSDMicro: new(int64(250))}))
	before := counted.read(fired.SessionID)
	if r := f.runs("alice", "").byName(t, "watch").Runs; len(r) != 1 || r[0].CostUSDMicro != 250 || r[0].Ended {
		t.Fatalf("a run still running: %+v", r)
	}
	if n := counted.read(fired.SessionID) - before; n != 1 {
		t.Fatalf("the read read the running session's log %d times", n)
	}
	f.runnerAppends(fired.SessionID, f.status(session.StatusIdle, session.StopEndTurn))
	if r := f.runs("alice", "").byName(t, "watch").Runs; r[0].CostUSDMicro != 250 || !r[0].Ended {
		t.Fatalf("a run that ended: %+v", r)
	}
	before = counted.read(fired.SessionID)
	f.runs("alice", "")
	if n := counted.read(fired.SessionID) - before; n != 0 {
		t.Fatalf("the read read an ended run's log %d times", n)
	}
	rs, err := f.objects.Runs(t.Context(), alice, time.Time{})
	if err != nil || len(rs) != 1 || rs[0].CostUSDMicro != 250 || !rs[0].Ended {
		t.Fatalf("the run kept: %+v, %v", rs, err)
	}

	// A firing recorded with no run, as one from before spec 088, gets one
	// at the read, from its session's log; one whose session is gone ends.
	tr, err := f.objects.TriggerByName(t.Context(), alice, "watch")
	if err != nil {
		t.Fatal(err)
	}
	gone := f.create("alice", "reviewer")
	for i, sid := range []string{fired.SessionID, gone.ID} {
		fr := store.Firing{ID: session.NewID(session.PrefixFiring), TriggerID: tr.ID, Dedupe: "manual:old" + string(rune('a'+i)), Origin: store.OriginManual,
			Time: f.clock.Now(), ReceivedAt: f.clock.Now()}
		if _, _, err := f.objects.ClaimFiring(t.Context(), fr); err != nil {
			t.Fatal(err)
		}
		fr.Outcome, fr.SessionID = store.OutcomeContinued, sid
		if err := f.objects.RecordFiring(t.Context(), fr, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.sessions.Delete(t.Context(), gone.ID); err != nil {
		t.Fatal(err)
	}
	w := f.runs("alice", "").byName(t, "watch")
	if len(w.Runs) != 3 {
		t.Fatalf("the runs with the firings that had none: %+v", w.Runs)
	}
	for _, r := range w.Runs[1:] {
		if r.Outcome != store.OutcomeContinued {
			t.Errorf("a recorded run: %+v", r)
		}
		if r.SessionID == gone.ID && !r.Ended {
			t.Errorf("the run of a session that is gone did not end: %+v", r)
		}
	}
	if rs, err := f.objects.Runs(t.Context(), alice, time.Time{}); err != nil || len(rs) != 3 {
		t.Fatalf("the runs kept after the read: %+v, %v", rs, err)
	}
}

// TestTheRunsReadsBounds (spec 088): since and until outside their bounds,
// or not times, are invalid_request, and the bounds themselves are read.
func TestTheRunsReadsBounds(t *testing.T) {
	f := newTriggerFixture(t)
	now := f.clock.Now()
	for _, q := range []string{
		"?since=yesterday",
		"?since=" + at(now.Add(-trigger.RunsKept-time.Minute)),
		"?since=" + at(now.Add(time.Minute)),
		"?until=" + at(now.Add(-time.Minute)),
		"?until=" + at(now.Add(trigger.RunsAhead+time.Minute)),
		"?until=soon",
		"?limit=0",
	} {
		if a := f.do(http.MethodGet, "/v1/triggers/runs"+q, "alice", ""); a.status != http.StatusBadRequest || a.code() != CodeInvalidRequest {
			t.Errorf("%s: %d %s", q, a.status, a.body)
		}
	}
	for _, q := range []string{"?since=" + at(now.Add(-trigger.RunsKept)), "?until=" + at(now.Add(trigger.RunsAhead)), "?since=" + at(now) + "&until=" + at(now)} {
		f.runs("alice", q)
	}
}

// TestATriggersCreatedAtIsItsFirstApply (spec 088): a later apply, such as
// a pause, moves a trigger's version and leaves status.createdAt, its
// first apply, in the read, the list and the read of the runs.
func TestATriggersCreatedAtIsItsFirstApply(t *testing.T) {
	f := newTriggerFixture(t)
	t0 := f.clock.Now()
	f.applyTrigger("alice", "daily", "schedule: '@daily', session: {message: Look.}")
	f.clock.Advance(time.Hour)
	paused := f.applyTrigger("alice", "daily", "schedule: '@daily', session: {message: Look.}, suspend: true")
	if paused.Status.Version != 2 || !paused.Status.CreatedAt.Equal(t0) {
		t.Fatalf("the paused trigger's apply answers version %d, created %v", paused.Status.Version, paused.Status.CreatedAt)
	}
	if got := f.get("alice", "daily"); got.Status.Version != 2 || !got.Status.CreatedAt.Equal(t0) {
		t.Fatalf("the read answers version %d, created %v", got.Status.Version, got.Status.CreatedAt)
	}
	a := f.do(http.MethodGet, "/v1/triggers", "alice", "")
	if a.status != http.StatusOK || !strings.Contains(string(a.body), `"createdAt":"`+t0.UTC().Format(time.RFC3339)+`"`) {
		t.Fatalf("the list: %d %s", a.status, a.body)
	}
	if d := f.runs("alice", "").byName(t, "daily"); !d.CreatedAt.Equal(t0) || !d.Suspended || len(d.Fires) != 0 {
		t.Fatalf("the read of the runs: %+v", d)
	}
}

// TestTheDocumentDescribesSpec088: the OpenAPI document describes the read
// of the runs, its parameters and its answer's fields.
func TestTheDocumentDescribesSpec088(t *testing.T) {
	raw, err := OpenAPI(DefaultBasePath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Description string `yaml:"description"`
			Parameters  []struct {
				Name string `yaml:"name"`
			} `yaml:"parameters"`
		} `yaml:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	op, ok := doc.Paths["/triggers/runs"]["get"]
	if !ok {
		t.Fatal("the document has no GET /triggers/runs")
	}
	var params []string
	for _, p := range op.Parameters {
		params = append(params, p.Name)
	}
	for _, want := range []string{"since", "until", "owner", "limit", "cursor"} {
		if !strings.Contains(strings.Join(params, " "), want) {
			t.Errorf("the read takes no %s: %v", want, params)
		}
	}
	for _, said := range []string{"per_run_usd_micro", "The hold is not applied", "trigger.list"} {
		if !strings.Contains(op.Description, said) {
			t.Errorf("the read's text does not say %q", said)
		}
	}
	for schema, fields := range map[string][]string{
		"TriggerRuns":      {"trigger_id", "name", "created_at", "fires_on", "suspended", "per_week_usd_micro", "wake", "target", "runs", "more_runs", "per_run_usd_micro", "fires", "more_fires"},
		"TriggerRun":       {"firing_id", "at", "outcome", "reason", "session_id", "wake", "cost_usd_micro", "ended"},
		"TriggerRunTarget": {"session_id", "trigger_id"},
	} {
		s, ok := doc.Components.Schemas[schema]
		if !ok {
			t.Errorf("the document has no schema %s", schema)
			continue
		}
		for _, f := range fields {
			if _, ok := s.Properties[f]; !ok {
				t.Errorf("%s has no %s", schema, f)
			}
		}
	}
}
