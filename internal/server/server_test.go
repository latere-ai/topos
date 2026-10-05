// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/bearer"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/store"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/dir"
)

const (
	issuer = "https://login.example"
	alice  = issuer + "|alice"
	bob    = issuer + "|bob"
	root   = issuer + "|root"
)

// tokens verifies a bearer that is the sub of the login.example issuer.
type tokens struct{}

// A bearer of sub@org carries the org_id claim org, the context a token
// of an organization's member names.
func (tokens) Authenticate(r *http.Request) (auth.Caller, error) {
	tok, ok := bearer.FromRequest(r)
	sub, org, _ := strings.Cut(tok, "@")
	if !ok || sub == "" || sub == "forged" {
		return auth.Caller{}, &auth.Error{Code: auth.CodeUnauthenticated, Message: "no bearer"}
	}
	claims := map[string]any{"sub": sub}
	if org != "" {
		claims["org_id"] = org
	}
	return auth.Caller{Subject: authz.Subject(issuer, sub), Issuer: issuer, Sub: sub, Claims: claims}, nil
}

// recording is the owner policy with every question it was asked kept.
type recording struct {
	mu     sync.Mutex
	next   authz.Authorizer
	asked  []string
	answer func(authz.Request) (authz.Decision, error)
}

func (r *recording) Authorize(ctx context.Context, req authz.Request) (authz.Decision, error) {
	r.mu.Lock()
	r.asked = append(r.asked, req.Action)
	answer := r.answer
	r.mu.Unlock()
	if answer != nil {
		return answer(req)
	}
	return r.next.Authorize(ctx, req)
}

func (r *recording) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.asked
	r.asked = nil
	return out
}

type fixture struct {
	t        *testing.T
	sessions session.Store
	objects  *store.Memory
	authz    *recording
	api      *Server
	srv      *httptest.Server
}

func newFixture(t *testing.T, mut ...func(*Options)) *fixture {
	t.Helper()
	f := &fixture{t: t, sessions: session.NewMemoryStore(), objects: store.NewMemory(nil), authz: &recording{next: &auth.OwnerPolicy{Admins: []string{root}}}}
	o := Options{Sessions: f.sessions, Objects: f.objects, Verifier: tokens{}, Guard: auth.Guard{Authorizer: f.authz}, PublicURL: "https://topos.example/", Heartbeat: 20 * time.Millisecond}
	for _, m := range mut {
		m(&o)
	}
	if m, ok := o.Objects.(*store.Memory); ok {
		f.objects = m
	}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	f.api = s
	f.srv = httptest.NewServer(s.Handler())
	t.Cleanup(f.srv.Close)
	return f
}

type answer struct {
	status int
	header http.Header
	body   []byte
}

func (a answer) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(a.body, v); err != nil {
		t.Fatalf("decode %s: %v", a.body, err)
	}
}

func (a answer) code() string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(a.body, &e) != nil {
		return ""
	}
	return e.Error.Code
}

func (f *fixture) do(method, path, token, body string, header ...string) answer {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), method, f.srv.URL+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return answer{status: resp.StatusCode, header: resp.Header, body: b}
}

func agentYAML(name, instructions string) string {
	return fmt.Sprintf("apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: %s\nspec:\n  model: {name: claude-haiku-4-5}\n  instructions: %q\n  machine: {kind: cella}\n", name, instructions)
}

// apply applies an agent as token and returns it.
func (f *fixture) apply(token, name, instructions string) v1.Agent {
	f.t.Helper()
	a := f.do(http.MethodPut, "/v1/agents/"+name, token, agentYAML(name, instructions))
	if a.status != http.StatusCreated && a.status != http.StatusOK {
		f.t.Fatalf("apply %s: %d %s", name, a.status, a.body)
	}
	var out v1.Agent
	a.decode(f.t, &out)
	return out
}

// create creates a session of agent as token with a first message.
func (f *fixture) create(token, agent string) session.Session {
	f.t.Helper()
	a := f.do(http.MethodPost, "/v1/sessions", token, `{"agent":"`+agent+`","message":"Review main.go."}`)
	if a.status != http.StatusCreated {
		f.t.Fatalf("create: %d %s", a.status, a.body)
	}
	var s session.Session
	a.decode(f.t, &s)
	return s
}

// TestEveryRouteAsksItsAction drives each route of the table once and
// holds the questions it asked to the ones its row names, its own among
// them. A route the table gains without a case here fails the test.
func TestEveryRouteAsksItsAction(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	f.apply("alice", "archivist", "Keep.")
	s := f.create("alice", "reviewer")
	ended := f.create("alice", "reviewer")
	f.turn(ended.ID, 1, "Done.", 1)
	if a := f.do(http.MethodPost, "/v1/sessions/"+ended.ID+"/end", "alice", `{"reason":"completed"}`); a.status != http.StatusOK {
		t.Fatalf("end: %d %s", a.status, a.body)
	}
	doomed := f.create("alice", "reviewer")
	for _, name := range []string{"nightly", "retired"} {
		if a := f.do(http.MethodPut, "/v1/triggers/"+name, "alice", triggerYAML(name, "schedule: '@daily', session: {message: x}")); a.status != http.StatusCreated {
			t.Fatalf("apply trigger %s: %d %s", name, a.status, a.body)
		}
	}
	evs, err := f.sessions.Events(t.Context(), s.ID, 1, 0)
	if err != nil || len(evs) == 0 {
		t.Fatalf("events %v, %v", evs, err)
	}
	cases := map[string]struct{ method, path, body string }{
		"applyAgent":        {http.MethodPut, "/v1/agents/reviewer", agentYAML("reviewer", "Review closely.")},
		"listAgents":        {http.MethodGet, "/v1/agents", ""},
		"getAgent":          {http.MethodGet, "/v1/agents/reviewer", ""},
		"listAgentVersions": {http.MethodGet, "/v1/agents/reviewer/versions", ""},
		"getAgentVersion":   {http.MethodGet, "/v1/agents/reviewer/versions/1", ""},
		"archiveAgent":      {http.MethodPost, "/v1/agents/archivist/archive", `{"permanent":true}`},
		"createSession":     {http.MethodPost, "/v1/sessions", `{"agent":"reviewer"}`},
		"listSessions":      {http.MethodGet, "/v1/sessions", ""},
		"getSessionSummary": {http.MethodGet, "/v1/sessions/summary", ""},
		"searchSessions":    {http.MethodGet, "/v1/sessions/search?q=review", ""},
		"getSession":        {http.MethodGet, "/v1/sessions/" + s.ID, ""},
		"updateSession":     {http.MethodPatch, "/v1/sessions/" + s.ID, `{"model":{"name":"anthropic/claude-sonnet-4-5"}}`},
		"endSession":        {http.MethodPost, "/v1/sessions/" + s.ID + "/end", `{"reason":"canceled"}`},
		"resumeSession":     {http.MethodPost, "/v1/sessions/" + s.ID + "/resume", `{}`},
		"forkSession":       {http.MethodPost, "/v1/sessions/" + ended.ID + "/fork", ""},
		"archiveSession":    {http.MethodPost, "/v1/sessions/" + ended.ID + "/archive", ""},
		"unarchiveSession":  {http.MethodPost, "/v1/sessions/" + ended.ID + "/unarchive", ""},
		"deleteSession":     {http.MethodDelete, "/v1/sessions/" + doomed.ID, ""},
		"listEvents":        {http.MethodGet, "/v1/sessions/" + ended.ID + "/events", ""},
		"sendEvent":         {http.MethodPost, "/v1/sessions/" + ended.ID + "/events", `{"type":"user.message","payload":{"content":[{"type":"text","text":"x"}]}}`},
		"streamEvents":      {http.MethodGet, "/v1/sessions/" + ended.ID + "/stream", ""},
		"getBlob":           {http.MethodGet, "/v1/sessions/" + ended.ID + "/blobs/" + string(ended.Agent.Digest), ""},
		"getFile":           {http.MethodGet, "/v1/sessions/" + ended.ID + "/files?path=a.txt", ""},
		"redactEvent":       {http.MethodPost, "/v1/sessions/" + ended.ID + "/events/" + evs[0].ID + "/redact", `{"reason":"a token"}`},
		"getOpenAPI":        {http.MethodGet, "/v1/openapi.yaml", ""},
		"applyTrigger":      {http.MethodPut, "/v1/triggers/fresh", triggerYAML("fresh", "schedule: '@daily', session: {message: x}")},
		"listTriggers":      {http.MethodGet, "/v1/triggers", ""},
		"getTrigger":        {http.MethodGet, "/v1/triggers/nightly", ""},
		"deleteTrigger":     {http.MethodDelete, "/v1/triggers/retired", ""},
		"fireTrigger":       {http.MethodPost, "/v1/triggers/nightly/fire", ""},
		"listFirings":       {http.MethodGet, "/v1/triggers/nightly/firings", ""},
	}
	var ops []string
	for _, rt := range table() {
		ops = append(ops, rt.op)
		c, ok := cases[rt.op]
		if !ok {
			t.Errorf("route %s %s has no case", rt.method, rt.path)
			continue
		}
		f.authz.take()
		a := f.do(c.method, c.path, "alice", c.body)
		asked := f.authz.take()
		if rt.public {
			if len(asked) != 0 || a.status != rt.status {
				t.Errorf("%s: a public route asked %v, answered %d", rt.op, asked, a.status)
			}
			continue
		}
		if len(asked) == 0 {
			t.Errorf("%s asked nothing (answered %d %s)", rt.op, a.status, a.body)
		}
		for _, q := range asked {
			if !slices.Contains(rt.actions, q) && !strings.HasSuffix(q, ".read") {
				t.Errorf("%s asked %s, which its row does not name", rt.op, q)
			}
		}
		if !slices.Contains(asked, rt.actions[0]) && (rt.op != "applyAgent" || !slices.Contains(asked, rt.actions[1])) {
			t.Errorf("%s asked %v, not its own %s", rt.op, asked, rt.actions[0])
		}
	}
	for op := range cases {
		if !slices.Contains(ops, op) {
			t.Errorf("case %s names no route", op)
		}
	}
}

// TestEveryRouteOfAnObjectAnswersNotFound drives each route of the table
// whose path names a session, an agent or a trigger with one the caller
// cannot see: an id not in the object's form, an id no object has, and
// another subject's object. Each answers not_found with no details,
// whatever store holds the sessions; the directory store, as Postgres
// does, refuses an id not in a session's form where the memory store
// answers it absent. A route the table gains with an object in its path
// is driven here without a case.
func TestEveryRouteOfAnObjectAnswersNotFound(t *testing.T) {
	for _, kind := range []string{"memory", "dir"} {
		t.Run(kind, func(t *testing.T) {
			sessions := session.NewMemoryStore()
			if kind == "dir" {
				d, err := dir.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				sessions = d
			}
			f := newFixture(t, func(o *Options) { o.Sessions = sessions })
			f.sessions = sessions
			agent := f.apply("alice", "reviewer", "Review.")
			s := f.create("alice", "reviewer")
			trigger := f.do(http.MethodPut, "/v1/triggers/nightly", "alice", triggerYAML("nightly", "schedule: '@daily', session: {message: x}"))
			if trigger.status != http.StatusCreated {
				t.Fatalf("apply the trigger: %d %s", trigger.status, trigger.body)
			}
			var tr v1.Trigger
			trigger.decode(t, &tr)
			evs, err := f.sessions.Events(t.Context(), s.ID, 1, 0)
			if err != nil || len(evs) == 0 {
				t.Fatalf("events %v, %v", evs, err)
			}
			bodies := map[string]string{
				"updateSession": `{"model":{"name":"anthropic/claude-sonnet-4-5"}}`, "endSession": `{"reason":"completed"}`, "resumeSession": `{}`,
				"sendEvent": `{"type":"user.message","payload":{"content":[{"type":"text","text":"x"}]}}`, "redactEvent": `{"reason":"a token"}`,
				"archiveAgent": `{"permanent":true}`,
			}
			// Each object a caller cannot see, by the kind of object the
			// route's path names, as the caller who asks.
			type unseen struct{ name, caller, session, agent, trigger string }
			unseens := []unseen{
				{"an id not in the form", "alice", "ses_doesnotexist0000000000000000", "agent_doesnotexist0000000000000000", "trg_doesnotexist0000000000000000"},
				{"an id no object has", "alice", session.NewID(session.PrefixSession), session.NewID(session.PrefixAgent), session.NewID(session.PrefixTrigger)},
				{"another subject's object by id", "bob", s.ID, agent.Status.ID, tr.Status.ID},
				{"another subject's object by name", "bob", s.ID, "reviewer", "nightly"},
			}
			var driven []string
			for _, rt := range table() {
				object := strings.Contains(rt.path, "{id}") || strings.Contains(rt.path, "{ref}")
				if rt.public || !object {
					continue
				}
				driven = append(driven, rt.op)
				for _, u := range unseens {
					ref := u.agent
					if strings.HasPrefix(rt.path, "/triggers/") {
						ref = u.trigger
					}
					path := strings.NewReplacer("{id}", u.session, "{ref}", ref, "{n}", "1", "{digest}", string(s.Agent.Digest), "{event_id}", evs[0].ID).Replace(rt.path)
					a := f.do(rt.method, "/v1"+path, u.caller, bodies[rt.op])
					if a.status != http.StatusNotFound || a.code() != CodeNotFound || strings.Contains(string(a.body), "details") {
						t.Errorf("%s, %s: %s %s: %d %s", rt.op, u.name, rt.method, path, a.status, a.body)
					}
				}
			}
			for _, op := range []string{"getSession", "endSession", "forkSession", "resumeSession", "getAgent", "archiveAgent", "getTrigger", "fireTrigger"} {
				if !slices.Contains(driven, op) {
					t.Errorf("%s was not driven", op)
				}
			}
			if got, err := f.sessions.Get(t.Context(), s.ID); err != nil || got.Status != s.Status || got.LastSeq != s.LastSeq {
				t.Fatalf("a refused route changed the session: %+v, %v", got, err)
			}
		})
	}
}

func TestNewRefusesAnIncompleteServer(t *testing.T) {
	good := Options{Sessions: session.NewMemoryStore(), Objects: store.NewMemory(nil), Verifier: tokens{}, Guard: auth.Guard{Authorizer: &auth.OwnerPolicy{}}, PublicURL: "https://x"}
	for name, mut := range map[string]func(*Options){
		"no sessions": func(o *Options) { o.Sessions = nil },
		"no verifier": func(o *Options) { o.Verifier = nil },
		"no guard":    func(o *Options) { o.Guard = auth.Guard{} },
		"no url":      func(o *Options) { o.PublicURL = "" },
		// The base path is the public URL's path, or a written URL would
		// carry it twice or not at all.
		"a base path the URL lacks": func(o *Options) { o.BasePath = "/v1/agents" },
	} {
		o := good
		mut(&o)
		if _, err := New(o); err == nil {
			t.Errorf("%s: built", name)
		}
	}
}

// TestWrittenURLsUsePublicURL is spec 030's rule for the URLs an answer
// carries: under a base path each is TOPOS_PUBLIC_URL joined with the
// route's path after the root, whatever Host the request named, and none
// carries the base path twice. The stream and next-page Link headers and
// the served document's server are the three this server writes.
func TestWrittenURLsUsePublicURL(t *testing.T) {
	const public = "https://api.example.com/v1/agents"
	f := newFixture(t, func(o *Options) { o.PublicURL = public + "/"; o.BasePath = "/v1/agents" })
	// Every request names another host than the public URL's, which no
	// written URL may take.
	withHost := func(method, path, body string) answer {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, f.srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "elsewhere.example"
		req.Header.Set("Authorization", "Bearer alice")
		resp, err := f.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return answer{status: resp.StatusCode, header: resp.Header, body: b}
	}
	for _, name := range []string{"a1", "a2"} {
		if a := withHost(http.MethodPut, "/v1/agents/agents/"+name, agentYAML(name, "Review.")); a.status != http.StatusCreated {
			t.Fatalf("apply %s: %d %s", name, a.status, a.body)
		}
	}
	created := withHost(http.MethodPost, "/v1/agents/sessions", `{"agent":"a1","message":"Review main.go."}`)
	var s session.Session
	created.decode(t, &s)
	if link := created.header.Get("Link"); link != "<"+public+"/sessions/"+s.ID+`/stream>; rel="stream"` {
		t.Errorf("the stream's Link is %q", link)
	}
	if link := withHost(http.MethodGet, "/v1/agents/sessions/"+s.ID, "").header.Get("Link"); link != "<"+public+"/sessions/"+s.ID+`/stream>; rel="stream"` {
		t.Errorf("a read session's Link is %q", link)
	}
	page := withHost(http.MethodGet, "/v1/agents/agents?limit=1", "")
	if link := page.header.Get("Link"); !strings.HasPrefix(link, "<"+public+"/agents?") || !strings.HasSuffix(link, `>; rel="next"`) {
		t.Errorf("the next page's Link is %q", link)
	}
	doc := withHost(http.MethodGet, "/v1/agents/openapi.yaml", "")
	if doc.status != http.StatusOK || !strings.Contains(string(doc.body), "url: "+public+"\n") {
		t.Errorf("the served document: %d, its server is not %s", doc.status, public)
	}
	for _, a := range []answer{created, page, doc} {
		if strings.Contains(a.header.Get("Link")+string(a.body), "/v1/agents/v1/agents") {
			t.Errorf("an answer carries the base path twice: %v %s", a.header, a.body)
		}
	}
	// The root /v1 is replaced, not kept beside the base path.
	if a := withHost(http.MethodGet, "/v1/agents/v1/agents", ""); a.status != http.StatusNotFound || a.code() != CodeNotFound {
		t.Errorf("/v1/agents/v1/agents: %d %s", a.status, a.body)
	}
}

// TestErrorTable: every code of the table has a status and a sentence,
// every error body is the envelope with its code, and an unknown route
// under the API root is not_found.
func TestErrorTable(t *testing.T) {
	for code, row := range codes {
		if row.status < 400 || row.message == "" || strings.Contains(row.message, "—") {
			t.Errorf("%s: %+v", code, row)
		}
	}
	f := newFixture(t)
	for _, c := range []struct {
		method, path, token, body string
		status                    int
		code                      string
	}{
		{http.MethodGet, "/v1/agents", "", "", 401, auth.CodeUnauthenticated},
		{http.MethodGet, "/v1/agents", "forged", "", 401, auth.CodeUnauthenticated},
		{http.MethodGet, "/v1/nothing", "alice", "", 404, CodeNotFound},
		{http.MethodGet, "/v1/agents/nobody", "alice", "", 404, CodeNotFound},
		{http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer","extra":1}`, 400, CodeInvalidRequest},
		{http.MethodPost, "/v1/sessions", "alice", `{"agent":"x"}{"agent":"y"}`, 400, CodeInvalidRequest},
		{http.MethodPost, "/v1/sessions", "alice", ``, 400, CodeInvalidRequest},
		{http.MethodGet, "/v1/agents?limit=0", "alice", "", 400, CodeInvalidRequest},
		{http.MethodGet, "/v1/agents?cursor=%21%21", "alice", "", 400, CodeInvalidRequest},
		{http.MethodPost, "/v1/sessions", "alice", `{"agent":"` + strings.Repeat("x", MaxBody) + `"}`, 413, CodePayloadTooLarge},
	} {
		a := f.do(c.method, c.path, c.token, c.body)
		if a.status != c.status || a.code() != c.code {
			t.Errorf("%s %s: %d %s, want %d %s", c.method, c.path, a.status, a.body, c.status, c.code)
		}
		if a.header.Get("Content-Type") != "application/json" {
			t.Errorf("%s %s: Content-Type %q", c.method, c.path, a.header.Get("Content-Type"))
		}
	}
}

// TestARefusalCarriesTheAuthorizersReason: a forbidden answer keeps its
// code and fixed sentence and carries the authorizer's reason in
// details.reason when the deny gave one, and no reason when it gave
// none. A session create's reason reaches the caller only when the
// caller may read the agent it names; a denied read answers not_found
// with no details at all.
func TestARefusalCarriesTheAuthorizersReason(t *testing.T) {
	f := newFixture(t)
	mine := f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	var reason string
	colleague, closed := false, false
	f.authz.answer = func(r authz.Request) (authz.Decision, error) {
		switch {
		case r.Action == authorizer.ActionSessionCreate, r.Action == authorizer.ActionSessionEnd:
			return authz.Decision{Reason: reason}, nil
		case closed && r.Action == authorizer.ActionAgentRead:
			return authz.Decision{Reason: reason}, nil
		case colleague && r.Action == authorizer.ActionAgentRead:
			return authz.Decision{Allow: true}, nil
		}
		return f.authz.next.Authorize(t.Context(), r)
	}
	type envelope struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	refused := func(token, method, path, body string, status int) envelope {
		t.Helper()
		a := f.do(method, path, token, body)
		var e envelope
		a.decode(t, &e)
		if a.status != status {
			t.Fatalf("%s %s as %s: %d %s", method, path, token, a.status, a.body)
		}
		return e
	}
	create := `{"agent":"` + mine.Status.ID + `"}`
	for _, c := range []struct {
		name, token, reason, method, path, body string
		colleague, closed                       bool
		want                                    map[string]any
	}{
		{"a create with a reason", "alice", "agent_exceeds_initiator", http.MethodPost, "/v1/sessions", create, false, false,
			map[string]any{"detail": "the authorizer denied session.create", "reason": "agent_exceeds_initiator"}},
		{"a create without one", "alice", "", http.MethodPost, "/v1/sessions", create, false, false,
			map[string]any{"detail": "the authorizer denied session.create"}},
		{"the caller's own agent while every agent action is closed to it", "alice", "agents_not_enabled", http.MethodPost, "/v1/sessions", create, false, true,
			map[string]any{"detail": "the authorizer denied session.create", "reason": "agents_not_enabled"}},
		{"a mutation of a readable session", "alice", "plan_suspended", http.MethodPost, "/v1/sessions/" + s.ID + "/end", `{"reason":"canceled"}`, false, false,
			map[string]any{"detail": "the authorizer denied session.end", "reason": "plan_suspended"}},
		{"another subject's agent", "bob", "wrong_context", http.MethodPost, "/v1/sessions", create, false, false,
			map[string]any{"detail": "the authorizer denied session.create"}},
		{"another subject's agent the caller may read", "bob", "agent_budget_unassigned", http.MethodPost, "/v1/sessions", create, true, false,
			map[string]any{"detail": "the authorizer denied session.create", "reason": "agent_budget_unassigned"}},
	} {
		reason, colleague, closed = c.reason, c.colleague, c.closed
		e := refused(c.token, c.method, c.path, c.body, http.StatusForbidden)
		if e.Error.Code != auth.CodeForbidden || e.Error.Message != codes[auth.CodeForbidden].message || !maps.Equal(e.Error.Details, c.want) {
			t.Errorf("%s: %+v, want details %v", c.name, e.Error, c.want)
		}
	}
	reason, colleague, closed = "not_owner", false, false
	for _, path := range []string{"/v1/agents/" + mine.Status.ID, "/v1/sessions/" + s.ID} {
		if a := f.do(http.MethodGet, path, "bob", ""); a.status != http.StatusNotFound || strings.Contains(string(a.body), "details") {
			t.Errorf("bob's denied read of %s: %d %s", path, a.status, a.body)
		}
	}
}

func TestClassify(t *testing.T) {
	for want, err := range map[string]error{
		CodeSequenceConflict:           fmt.Errorf("x: %w", session.ErrSequenceConflict),
		CodeConflict:                   store.ErrConflict,
		CodeInvalidRequest:             session.ErrInvalid,
		CodeInternal:                   errors.New("disk"),
		auth.CodeAuthorizerUnavailable: &auth.Error{Code: auth.CodeAuthorizerUnavailable},
	} {
		if got := classify(err).code; got != want {
			t.Errorf("classify(%v) = %s, want %s", err, got, want)
		}
	}
	w := httptest.NewRecorder()
	writeError(w, nil, &apiError{code: "made_up"})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("an unknown code answered %d", w.Code)
	}
	if (&apiError{code: "x", detail: "d", err: errors.New("e")}).Error() != "x: d: e" || (&apiError{code: "x", detail: "d"}).Error() != "x: d" {
		t.Fatal("apiError.Error")
	}
}

func TestRateLimit(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.PerMinute = 2 })
	for range 2 {
		if a := f.do(http.MethodGet, "/v1/agents", "alice", ""); a.status != http.StatusOK {
			t.Fatalf("within the limit: %d", a.status)
		}
	}
	a := f.do(http.MethodGet, "/v1/agents", "alice", "")
	if a.status != http.StatusTooManyRequests || a.code() != CodeRateLimited || a.header.Get("Retry-After") == "" {
		t.Fatalf("over the limit: %d %s %v", a.status, a.body, a.header)
	}
	if a := f.do(http.MethodGet, "/v1/agents", "bob", ""); a.status != http.StatusOK {
		t.Fatalf("another subject: %d", a.status)
	}
}

func TestRequestIDIsKeptOrMinted(t *testing.T) {
	f := newFixture(t)
	if a := f.do(http.MethodGet, "/v1/agents", "alice", "", "X-Request-Id", "req_mine"); a.header.Get("X-Request-Id") != "req_mine" {
		t.Fatalf("kept %q", a.header.Get("X-Request-Id"))
	}
	if a := f.do(http.MethodGet, "/v1/agents", "alice", ""); !strings.HasPrefix(a.header.Get("X-Request-Id"), "req_") {
		t.Fatalf("minted %q", a.header.Get("X-Request-Id"))
	}
}
