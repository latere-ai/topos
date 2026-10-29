// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-yaml"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/manifest/trigger"
	"latere.ai/x/topos/session"
)

// TestASplitTriggerEndToEnd (spec 095): applied at the route, a trigger's
// create carries its reach; a tick of a split trigger asks trigger.runs as
// the owner with the trigger, its reach, the firing and each scope's last
// started run. The owner policy, which knows no kind, allows with no
// limits, and the firing is read as skipped_nothing_new and starts
// nothing. An authorizer that names two scopes starts a session for each,
// its create carrying the reach with its scope and since, and the runs
// read answers each run's scope; the next ask names each scope's last.
func TestASplitTriggerEndToEnd(t *testing.T) {
	f := newTriggerFixture(t)
	var mu sync.Mutex
	asked := map[string][]authz.Resource{}
	var due *authorizer.WireRuns
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		mu.Lock()
		asked[req.Action] = append(asked[req.Action], req.Resource)
		answer := due
		mu.Unlock()
		if req.Action == authorizer.ActionTriggerRuns && answer != nil {
			raw, err := json.Marshal(answer)
			if err != nil {
				return authz.Decision{}, err
			}
			return authz.Decision{Allow: true, Limits: raw}, nil
		}
		return f.authz.next.Authorize(context.Background(), req)
	}
	take := func(action string) []authz.Resource {
		mu.Lock()
		defer mu.Unlock()
		out := asked[action]
		delete(asked, action)
		return out
	}
	tr := f.applyTrigger("alice", "tidy", "schedule: '0 3 * * *', timeZone: UTC, reach: {kind: memory_tidy, split: scope}, session: {message: Tidy.}")
	if r := tr.Spec.Reach; r == nil || r.Kind != "memory_tidy" || r.Split != "scope" {
		t.Fatalf("the trigger answers its reach as %+v", tr.Spec.Reach)
	}
	if creates := take(authorizer.ActionTriggerCreate); len(creates) != 1 || !sameJSON(t, creates[0].Fields["reach"], `{"kind":"memory_tidy","split":"scope"}`) {
		t.Fatalf("the create: %+v", creates)
	}

	// 03:01 the next day: the owner policy names no runs.
	f.clock.Advance(15*time.Hour + time.Minute)
	f.tick()
	ask := take(authorizer.ActionTriggerRuns)
	if len(ask) != 1 || ask[0].Kind != authorizer.KindTrigger || ask[0].ID != tr.Status.ID {
		t.Fatalf("the first tick asked %+v", ask)
	}
	fs := f.firings("alice", "tidy")
	if len(fs) != 1 || fs[0].Outcome != store.OutcomeSkippedNothingNew {
		t.Fatalf("the first tick's firings: %+v", fs)
	}
	want := `{"owner":"` + alice + `","name":"tidy","agent":"` + f.reviewer() + `","reach":{"kind":"memory_tidy","split":"scope"},` +
		`"firing":{"id":"` + fs[0].ID + `","at":"2026-09-28T03:00:00Z","origin":"schedule"},"last":{}}`
	if !sameJSON(t, ask[0].Fields, want) {
		t.Fatalf("the ask's fields: %+v", ask[0].Fields)
	}
	if n := len(f.sessionsOf(tr.Status.ID)); n != 0 || len(take(authorizer.ActionSessionCreate)) != 0 {
		t.Fatalf("nothing due started %d sessions", n)
	}
	if c := f.counts("tidy"); c.SkippedNothingNew != 1 || c.Started != 0 {
		t.Fatalf("the counts: %+v", c)
	}
	item := f.runs("alice", "").byName(t, "tidy")
	if len(item.Runs) != 1 || item.Runs[0].Outcome != store.OutcomeSkippedNothingNew || item.Runs[0].Scope != "" || !item.Runs[0].Ended ||
		item.Runs[0].CostUSDMicro != 0 || item.Runs[0].SessionID != "" {
		t.Fatalf("the runs read after nothing due: %+v", item.Runs)
	}

	// The next night two scopes are due.
	since := time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC)
	mu.Lock()
	due = &authorizer.WireRuns{Runs: []authorizer.WireRun{{Scope: "you", Since: &since}, {Scope: "projects/prj_01JA4R"}}}
	mu.Unlock()
	f.clock.Advance(24 * time.Hour)
	f.tick()
	// A night with nothing new leaves no scope a last run.
	if ask := take(authorizer.ActionTriggerRuns); len(ask) != 1 || !sameJSON(t, ask[0].Fields["last"], `{}`) {
		t.Fatalf("the second ask: %+v", ask)
	}
	creates := take(authorizer.ActionSessionCreate)
	if len(creates) != 2 || !sameJSON(t, creates[0].Fields["reach"], `{"kind":"memory_tidy","scope":"you","since":"2026-09-27T03:00:00Z"}`) ||
		!sameJSON(t, creates[1].Fields["reach"], `{"kind":"memory_tidy","scope":"projects/prj_01JA4R"}`) {
		t.Fatalf("the creates of two scopes: %+v", creates)
	}
	fs = f.firings("alice", "tidy")
	for _, c := range creates {
		if c.Fields["trigger_id"] != tr.Status.ID || c.Fields["firing_id"] != fs[0].ID {
			t.Fatalf("a scope's create names %v and %v", c.Fields["trigger_id"], c.Fields["firing_id"])
		}
	}
	sessions := f.sessionsOf(tr.Status.ID)
	if len(sessions) != 2 || fs[0].Outcome != store.OutcomeStarted || !slices.ContainsFunc(sessions, func(s session.Session) bool { return s.ID == fs[0].SessionID }) {
		t.Fatalf("two scopes: the firing %+v, the sessions %+v", fs[0], sessions)
	}
	item = f.runs("alice", "").byName(t, "tidy")
	if len(item.Runs) != 3 {
		t.Fatalf("the runs read after two scopes: %+v", item.Runs)
	}
	scoped := item.Runs[1:]
	if scoped[0].Scope != "projects/prj_01JA4R" || scoped[1].Scope != "you" || scoped[1].SessionID != fs[0].SessionID || scoped[0].SessionID == "" ||
		scoped[0].SessionID == scoped[1].SessionID || scoped[0].FiringID != fs[0].ID || scoped[0].Outcome != store.OutcomeStarted {
		t.Fatalf("the scopes' runs: %+v", scoped)
	}

	// The third night asks with each scope's last started run.
	for _, s := range sessions {
		f.turnCosts(s.ID, 10)
	}
	mu.Lock()
	due = &authorizer.WireRuns{}
	mu.Unlock()
	f.clock.Advance(24 * time.Hour)
	f.tick()
	ask = take(authorizer.ActionTriggerRuns)
	if len(ask) != 1 || !sameJSON(t, ask[0].Fields["last"], `{"you":"2026-09-29T03:00:00Z","projects/prj_01JA4R":"2026-09-29T03:00:00Z"}`) {
		t.Fatalf("the third ask: %+v", ask)
	}
	if fs := f.firings("alice", "tidy"); fs[0].Outcome != store.OutcomeSkippedNothingNew {
		t.Fatalf("an allow naming no runs: %+v", fs[0])
	}

	// A manual fire asks as the fire route's own question, an invalid
	// answer refusing the firing.
	mu.Lock()
	due = &authorizer.WireRuns{Runs: []authorizer.WireRun{{Scope: "you"}, {Scope: "you"}}}
	mu.Unlock()
	if out := f.fired("alice", "tidy", "", store.OutcomeRefused); out.Reason != "runs_invalid" {
		t.Fatalf("an invalid answer: %+v", out)
	}
	// A deny is the firing's refusal, with the deny's reason.
	mu.Lock()
	due = nil
	mu.Unlock()
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		mu.Lock()
		asked[req.Action] = append(asked[req.Action], req.Resource)
		mu.Unlock()
		if req.Action == authorizer.ActionTriggerRuns {
			return authz.Decision{Reason: "share_spent"}, nil
		}
		return f.authz.next.Authorize(context.Background(), req)
	}
	if out := f.fired("alice", "tidy", "", store.OutcomeRefused); out.Reason != "share_spent" {
		t.Fatalf("a deny: %+v", out)
	}
	if n := len(take(authorizer.ActionSessionCreate)); n != 0 {
		t.Fatalf("a deny started %d sessions", n)
	}

	// An update carries the reach as applied, split left out when absent,
	// and a firing of a trigger with no split asks nothing and carries its
	// kind alone.
	f.applyTrigger("alice", "tidy", "schedule: '0 3 * * *', timeZone: UTC, reach: {kind: memory_tidy}, session: {message: Tidy.}")
	if updates := take(authorizer.ActionTriggerUpdate); len(updates) != 1 || !sameJSON(t, updates[0].Fields["reach"], `{"kind":"memory_tidy"}`) {
		t.Fatalf("the update: %+v", updates)
	}
	take(authorizer.ActionTriggerRuns)
	take(authorizer.ActionSessionCreate)
	f.fired("alice", "tidy", "", store.OutcomeStarted)
	if creates := take(authorizer.ActionSessionCreate); len(creates) != 1 || !sameJSON(t, creates[0].Fields["reach"], `{"kind":"memory_tidy"}`) {
		t.Fatalf("the create of a trigger with no split: %+v", creates)
	}
	if ask := take(authorizer.ActionTriggerRuns); len(ask) != 0 {
		t.Fatalf("a trigger with no split asked %+v", ask)
	}
}

// sameJSON reports whether v renders as the JSON want does.
func sameJSON(t *testing.T, v any, want string) bool {
	t.Helper()
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatal(err)
	}
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	return string(ra) == string(rb)
}

// TestTheDocumentDescribesSpec095: the OpenAPI document gives a trigger's
// spec its reach and a run its scope, and the texts of the apply route,
// the fire route and the runs read say how a split firing runs and what
// skipped_nothing_new is.
func TestTheDocumentDescribesSpec095(t *testing.T) {
	raw, err := OpenAPI(DefaultBasePath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths      map[string]map[string]struct{ Description string } `yaml:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for schema, field := range map[string]string{"TriggerSpec": "reach", "TriggerReach": "split", "TriggerRun": "scope", "TriggerCounts": "skippedNothingNew"} {
		if _, ok := doc.Components.Schemas[schema].Properties[field]; !ok {
			t.Errorf("%s has no %s", schema, field)
		}
	}
	for path, wants := range map[string][]string{
		"/triggers/{name}": {"spec.reach {kind, split}", "trigger.runs", "skipped_nothing_new", "at most " + strconv.Itoa(trigger.MaxRunsPerFiring), "runs_invalid", "maxActive bounds no scope",
			"reach {kind, scope, since}"},
		"/triggers/{ref}/fire": {"skipped_nothing_new", "trigger.runs"},
		"/triggers/runs":       {"skipped_nothing_new", "each naming its scope"},
	} {
		method := "put"
		switch path {
		case "/triggers/runs":
			method = "get"
		case "/triggers/{ref}/fire":
			method = "post"
		}
		text := doc.Paths[path][method].Description
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not say %q", path, want)
			}
		}
	}
}
