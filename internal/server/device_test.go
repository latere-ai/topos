// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/session"
)

// deviceBody is a create's machine of the device dev-01jaq7v3k9m2 and the
// folder /Users/ada/code/site on it.
const deviceBody = `"machine":{"kind":"device","environment":"dev-01jaq7v3k9m2","workdir":"/Users/ada/code/site"}`

// TestCreateNamesADevice: a create that names a device records {kind:
// device, environment, workdir} and asks session.create with all three;
// a create naming cella or host, or more of a device than its kind,
// environment and folder, is still refused, and so is a device on a
// server without Cella; an agent's manifest naming device is refused; the
// question of a fork of a device session carries the inherited machine,
// and a fork the authorizer refuses creates nothing (spec 080).
func TestCreateNamesADevice(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.Cella = true })
	f.apply("alice", "reviewer", "Review.")
	var created questions
	created.keep(f, authorizer.ActionSessionCreate)
	a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer","message":"Fix the build.",`+deviceBody+`}`)
	if a.status != http.StatusCreated {
		t.Fatalf("create on a device: %d %s", a.status, a.body)
	}
	var s session.Session
	a.decode(t, &s)
	want := session.Machine{Kind: session.MachineDevice, Environment: "dev-01jaq7v3k9m2", Workdir: "/Users/ada/code/site"}
	if s.Machine != want {
		t.Fatalf("the session's machine is %+v, want %+v", s.Machine, want)
	}
	if stored, err := f.sessions.Get(t.Context(), s.ID); err != nil || stored.Machine != want {
		t.Fatalf("the stored machine is %+v, %v", stored.Machine, err)
	}
	q := created.last(t)
	if q.Resource.String("machine") != "device" || q.Resource.String("environment") != "dev-01jaq7v3k9m2" || q.Resource.String("workdir") != "/Users/ada/code/site" {
		t.Fatalf("session.create asked about %v", q.Resource.Fields)
	}
	// A session of the agent's own machine names no device in its
	// question.
	f.create("alice", "reviewer")
	if q := created.last(t); q.Resource.String("machine") != session.MachineCella || q.Resource.Fields["environment"] != nil || q.Resource.Fields["workdir"] != nil {
		t.Fatalf("a create of the agent's machine asked about %v", q.Resource.Fields)
	}
	for body, why := range map[string]string{
		`{"agent":"reviewer","machine":{"kind":"cella"}}`:                                                              "a cella machine",
		`{"agent":"reviewer","machine":{"kind":"host"}}`:                                                               "a host machine",
		`{"agent":"reviewer","machine":{"kind":"device","environment":"dev-1","workdir":"/w","image":"base"}}`:         "a device with an image",
		`{"agent":"reviewer","machine":{"kind":"device","environment":"dev-1"}}`:                                       "a device with no folder",
		`{"agent":"reviewer","machine":{"kind":"device","environment":"Dev 1","workdir":"/w"}}`:                        "an environment that is no name",
		`{"agent":"reviewer","machine":{"kind":"device","environment":"dev-1","workdir":"w/x"}}`:                       "a relative folder",
		`{"agent":"reviewer","machine":"device"}`:                                                                      "a machine that is no object",
		`{"agent":"reviewer","machine":{"kind":"device","environment":"dev-1","workdir":"/Users/ada/../bob/secrets"}}`: "a folder that climbs",
	} {
		if got := f.do(http.MethodPost, "/v1/sessions", "alice", body); got.code() != CodeInvalidRequest {
			t.Errorf("%s: %d %s, want invalid_request", why, got.status, got.body)
		}
	}
	noCella := newFixture(t)
	noCella.apply("alice", "reviewer", "Review.")
	if got := noCella.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer",`+deviceBody+`}`); got.code() != CodeMachineUnavailable {
		t.Fatalf("a device on a server without Cella: %d %s", got.status, got.body)
	}
	device := strings.Replace(agentYAML("laptop", "x"), "  machine: {kind: cella}\n", "  machine: {kind: device}\n", 1)
	if got := f.do(http.MethodPut, "/v1/agents/laptop", "alice", device); got.code() != "invalid_manifest" || !strings.Contains(string(got.body), "never in an agent's manifest") {
		t.Fatalf("an agent's manifest naming device: %d %s", got.status, got.body)
	}

	f.turn(s.ID, 1, "Fixed.", 1)
	var forked questions
	forked.keep(f, authorizer.ActionSessionFork)
	fa := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/fork", "alice", "")
	if fa.status != http.StatusCreated {
		t.Fatalf("fork: %d %s", fa.status, fa.body)
	}
	var fork session.Session
	fa.decode(t, &fork)
	if fork.Machine != want {
		t.Fatalf("the fork's machine is %+v, want %+v", fork.Machine, want)
	}
	fq := forked.last(t)
	if fq.Resource.String("machine") != "device" || fq.Resource.String("environment") != "dev-01jaq7v3k9m2" || fq.Resource.String("workdir") != "/Users/ada/code/site" ||
		fq.Resource.String("initiator") != alice {
		t.Fatalf("session.fork asked about %v", fq.Resource.Fields)
	}
	before := count(t, f)
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionFork && req.Resource.String("machine") == session.MachineDevice {
			return authz.Decision{Reason: "device_not_owned"}, nil
		}
		return f.authz.next.Authorize(context.Background(), req)
	}
	if got := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/fork", "alice", ""); got.status != http.StatusForbidden {
		t.Fatalf("a refused fork of a device session: %d %s", got.status, got.body)
	}
	if n := count(t, f); n != before {
		t.Fatalf("a refused fork left %d sessions, %d before", n, before)
	}
}

// count is how many sessions f's store holds.
func count(t *testing.T, f *fixture) int {
	t.Helper()
	page, _, err := f.sessions.List(t.Context(), session.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return len(page)
}

// TestATriggerNamesADevice: a trigger names a device in spec.session.
// machine, and the session its firing starts works on it, records what
// the trigger fires on, and asks session.create with the device beside
// the trigger; a device the manifest names wrongly, and a trigger naming
// any other machine, are refused (spec 080).
func TestATriggerNamesADevice(t *testing.T) {
	f := newTriggerFixture(t, func(o *Options) { o.Cella = true })
	var created questions
	created.keep(f.fixture, authorizer.ActionSessionCreate)
	tr := f.applyTrigger("alice", "nightly", "schedule: '@daily', session: {message: 'Tidy up.', machine: {kind: device, environment: dev-01jaq7v3k9m2, workdir: /Users/ada/code/site}}")
	started := f.fired("alice", "nightly", "", "started")
	s, err := f.sessions.Get(t.Context(), started.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	want := session.Machine{Kind: session.MachineDevice, Environment: "dev-01jaq7v3k9m2", Workdir: "/Users/ada/code/site"}
	if s.Machine != want || s.TriggerID != tr.Status.ID || s.TriggerFiresOn != session.TriggerSchedule {
		t.Fatalf("the firing's session: machine %+v, trigger %s fires on %q", s.Machine, s.TriggerID, s.TriggerFiresOn)
	}
	q := created.last(t)
	if q.Resource.String("machine") != "device" || q.Resource.String("environment") != "dev-01jaq7v3k9m2" || q.Resource.String("workdir") != "/Users/ada/code/site" ||
		q.Resource.String("trigger_id") != tr.Status.ID {
		t.Fatalf("session.create asked about %v", q.Resource.Fields)
	}
	f.applyTrigger("alice", "triage", eventSpec(""))
	ev := f.fired("alice", "triage", f.envelope("1", "issue.opened", "o/r#1", "x"), "started")
	if s, err := f.sessions.Get(t.Context(), ev.SessionID); err != nil || s.TriggerFiresOn != session.TriggerEvents || s.Machine.Kind == session.MachineDevice {
		t.Fatalf("an event trigger's session: %+v, %v", s, err)
	}
	for spec, why := range map[string]string{
		"schedule: '@daily', session: {message: x, machine: {kind: device, workdir: /w}}":                                  "a device with no environment",
		"schedule: '@daily', session: {message: x, machine: {kind: device, environment: dev-1}}":                           "a device with no folder",
		"schedule: '@daily', session: {message: x, machine: {kind: device, environment: dev-1, workdir: w}}":               "a relative folder",
		"schedule: '@daily', session: {message: x, machine: {kind: device, environment: dev-1, workdir: /w, image: base}}": "a device with an image",
		"schedule: '@daily', session: {message: x, machine: {kind: cella, workdir: /w}}":                                   "a folder on another kind",
		"schedule: '@daily', session: {message: x, machine: {kind: host}}":                                                 "a host machine",
	} {
		if got := f.do(http.MethodPut, "/v1/triggers/bad", "alice", triggerYAML("bad", spec)); got.status < 400 {
			t.Errorf("%s: %d %s", why, got.status, got.body)
		}
	}
}

// TestResumeAMachineWaitingSession: a session waiting for its machine
// resumes with no message and the budget it has, which session.resumed
// carries so the fold keeps it; a resume that asks for another budget is
// refused; and a session stopped for any reason but its budget or its
// machine is still refused (spec 080).
func TestResumeAMachineWaitingSession(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.Cella = true })
	f.apply("alice", "reviewer", "Review.")
	a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer","message":"Fix the build.","budget":{"max_cost_usd_micro":900000},`+deviceBody+`}`)
	if a.status != http.StatusCreated {
		t.Fatalf("create: %d %s", a.status, a.body)
	}
	var s session.Session
	a.decode(t, &s)
	resume := func(body string) answer {
		return f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/resume", "alice", body)
	}
	for _, stop := range []session.StopReason{session.StopEndTurn, session.StopError, session.StopToolConfirmation} {
		f.appendTo(s.ID, 1, session.SessionStatus{Status: session.StatusRunning}, session.SessionStatus{Status: session.StatusIdle, StopReason: stop})
		if got := resume(`{}`); got.code() != CodeConflict {
			t.Fatalf("a resume of a session stopped with %s: %d %s", stop, got.status, got.body)
		}
	}
	f.appendTo(s.ID, 2, session.SessionStatus{Status: session.StatusRunning}, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopMachineWaiting, Detail: session.MachineOffline})
	if got := resume(`{"max_cost_usd_micro":5000000}`); got.code() != CodeInvalidRequest {
		t.Fatalf("a resume of a waiting session that asks for a budget: %d %s", got.status, got.body)
	}
	got := resume(`{"reason":"device_back"}`)
	if got.status != http.StatusOK {
		t.Fatalf("resume: %d %s", got.status, got.body)
	}
	var resumed session.Session
	got.decode(t, &resumed)
	if resumed.Budget.MaxCostUSDMicro == nil || *resumed.Budget.MaxCostUSDMicro != 900000 {
		t.Fatalf("the resumed session's budget: %+v", resumed.Budget)
	}
	evs, err := f.sessions.Events(t.Context(), s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	last := evs[len(evs)-1]
	var p session.SessionResumed
	if last.Type != session.TypeSessionResumed || last.Decode(&p) != nil || p.Reason != "device_back" || p.MaxCostUSDMicro == nil || *p.MaxCostUSDMicro != 900000 {
		t.Fatalf("the last event %s %s", last.Type, last.Payload)
	}
	if !session.HasPendingInput(evs) {
		t.Fatal("a resumed session holds no pending input")
	}
	for _, e := range evs[1:] {
		if e.Type == session.TypeUserMessage {
			t.Fatal("the resume added a message")
		}
	}
}
