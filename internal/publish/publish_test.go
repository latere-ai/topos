// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// fakeMachine answers Stat for the folders it holds and each Exec with
// the next output, recording the scripts it ran.
type fakeMachine struct {
	machine.Machine
	dirs    map[string]bool
	outputs []string

	mu      sync.Mutex
	scripts []string
}

func (m *fakeMachine) Info() machine.Info {
	return machine.Info{Kind: machine.KindCella, Workdir: "/work"}
}

func (m *fakeMachine) Stat(_ context.Context, p string) (machine.FileInfo, error) {
	isDir, ok := m.dirs[p]
	if !ok {
		return machine.FileInfo{}, fs.ErrNotExist
	}
	return machine.FileInfo{Path: p, IsDir: isDir}, nil
}

func (m *fakeMachine) Exec(_ context.Context, r machine.ExecRequest) (machine.ExecResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scripts = append(m.scripts, r.Command)
	if len(m.outputs) == 0 {
		return machine.ExecResult{}, nil
	}
	out := m.outputs[0]
	m.outputs = m.outputs[1:]
	if code, rest, ok := strings.Cut(out, "!"); ok {
		n, err := strconv.Atoi(code)
		if err != nil {
			return machine.ExecResult{}, err
		}
		return machine.ExecResult{ExitCode: n, Output: []byte(rest)}, nil
	}
	return machine.ExecResult{Output: []byte(out)}, nil
}

// creds mints a token for every audience, or err.
type creds struct{ err error }

func (c creds) Credential(_ context.Context, audience, workload string) (runner.Credential, error) {
	if c.err != nil {
		return runner.Credential{}, c.err
	}
	return runner.Credential{Value: audience + "-" + workload, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// host is a stub app host whose answers each test sets by route.
type host struct {
	t   *testing.T
	srv *httptest.Server
	mu  sync.Mutex
	// answer maps "METHOD /path" to a function of the call's count there.
	answer map[string]func(n int) (int, any)
	calls  map[string]int
}

func newHost(t *testing.T, answer map[string]func(n int) (int, any)) *host {
	h := &host{t: t, answer: answer, calls: map[string]int{}}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer apps-session" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		key := r.Method + " " + r.URL.Path
		h.mu.Lock()
		h.calls[key]++
		n := h.calls[key]
		f := h.answer[key]
		h.mu.Unlock()
		if f == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		code, body := f(n)
		if s, ok := body.(string); ok {
			w.WriteHeader(code)
			if _, err := w.Write([]byte(s)); err != nil {
				t.Errorf("write: %v", err)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *host) count(key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[key]
}

const gitURL = "https://git.example"

func app(slug string) func(int) (int, any) {
	return func(int) (int, any) {
		return http.StatusOK, map[string]any{"slug": slug, "name": "A poem", "url": "https://" + slug + ".apps.example", "repository": map[string]string{"push_url": gitURL + "/ada/" + slug + ".git"}}
	}
}

func created(slug string) func(int) (int, any) {
	return func(n int) (int, any) {
		_, body := app(slug)(n)
		return http.StatusCreated, body
	}
}

func deploys(list ...map[string]any) func(int) (int, any) {
	return func(int) (int, any) { return http.StatusOK, map[string]any{"deploys": list} }
}

func refused(status int, code string) func(int) (int, any) {
	return func(int) (int, any) {
		return status, map[string]any{"error": map[string]string{"code": code, "message": "No."}}
	}
}

func newTool(t *testing.T, h *host, c runner.Credentials) *Tool {
	t.Helper()
	s := session.Session{ID: "ses_1", Title: "A poem", Agent: session.AgentRef{ID: "agent_1", Name: "latere", Version: 3}}
	return New(Options{URL: h.srv.URL, Audience: "apps", GitURL: gitURL, Wait: 50 * time.Millisecond, PollEvery: 5 * time.Millisecond}, s, runner.NewTokenSource(c, nil, nil))
}

func call(input string, m machine.Machine, st tools.State) tools.Call {
	return tools.Call{ID: "toolu_1", Input: json.RawMessage(input), Machine: m, State: st}
}

func resultText(r tools.Result) string {
	if len(r.Content) == 0 {
		return ""
	}
	return r.Content[0].Text
}

func metaOf(r tools.Result) session.PublishMeta {
	if r.Meta == nil || r.Meta.Publish == nil {
		return session.PublishMeta{}
	}
	return *r.Meta.Publish
}

const sha1, sha2 = "1111111aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "2222222bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// TestOutcomes: a failed build answers its code, its message and the end
// of its log; a canceled one its status; a wait that runs out with no
// deploy, or with one still building, answers building; a folder with
// nothing changed since a failed build is committed again; and a
// release answers refused with its reason, failed with its deploy's
// error, or pending when the host has not done it in time.
func TestOutcomes(t *testing.T) {
	ctx := t.Context()
	site := map[string]bool{"/work/site": true}
	var log strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&log, "{\"seq\":%d,\"line\":\"line %d\"}\n", i, i)
	}
	failed := map[string]any{"id": "d1", "status": "failed", "preview": true, "commit_sha": sha1, "preview_url": "https://d1--a-poem.apps.example",
		"error": map[string]string{"code": "build_failed", "message": "The build command exited with an error."}}

	h := newHost(t, map[string]func(int) (int, any){
		"POST /apps":                       created("a-poem"),
		"GET /apps/a-poem/deploys":         deploys(failed),
		"GET /apps/a-poem/deploys/d1/logs": func(int) (int, any) { return http.StatusOK, log.String() },
	})
	m := &fakeMachine{dirs: site, outputs: []string{"1 " + sha1 + "\n"}}
	res, err := newTool(t, h, creds{}).Run(ctx, call(`{"path":"site"}`, m, tools.State{}))
	got := metaOf(res)
	if err != nil || !res.IsError() || got.Status != session.PublishFailed || got.Error == nil || got.Error.Code != "build_failed" || got.Deploy != "d1" || got.Commit != sha1 {
		t.Fatalf("failed: %+v %+v %v", res, got, err)
	}
	if text := resultText(res); !strings.Contains(text, "build_failed: The build command exited with an error.") || !strings.Contains(text, "line 50") || !strings.Contains(text, "line 11\n") || strings.Contains(text, "line 10\n") {
		t.Fatalf("failed text: %s", text)
	}
	if !strings.Contains(m.scripts[0], "Topos-Session: ses_1") || !strings.Contains(m.scripts[0], "Topos-Agent: agent_1@3") || !strings.Contains(m.scripts[0], "agents/latere/ses_1") {
		t.Fatalf("the push script:\n%s", m.scripts[0])
	}

	// Nothing changed since the failed build: an empty commit builds again.
	ready := map[string]any{"id": "d2", "status": "ready", "preview": true, "commit_sha": sha2, "preview_url": "https://d2--a-poem.apps.example"}
	h = newHost(t, map[string]func(int) (int, any){
		"GET /apps/a-poem":         app("a-poem"),
		"GET /apps/a-poem/deploys": func(n int) (int, any) { return deploys(ready, failed)(n) },
	})
	m = &fakeMachine{dirs: site, outputs: []string{"0 " + sha1 + "\n", "1 " + sha2 + "\n"}}
	res, err = newTool(t, h, creds{}).Run(ctx, call(`{"path":"site"}`, m, tools.State{App: &session.PublishMeta{App: "a-poem"}}))
	if got := metaOf(res); err != nil || res.IsError() || got.Status != session.PublishReady || got.Commit != sha2 || len(m.scripts) != 2 || !strings.Contains(m.scripts[1], "[ 1 = 1 ]") || h.count("POST /apps") != 0 {
		t.Fatalf("retry: %+v %+v %v %q", res, got, err, m.scripts)
	}

	for name, c := range map[string]struct {
		deploys []map[string]any
		status  string
		preview string
		text    string
	}{
		"canceled":   {[]map[string]any{{"id": "d1", "status": "canceled", "preview": true, "commit_sha": sha1}}, session.PublishCanceled, "", "failed: canceled."},
		"no build":   {nil, session.PublishBuilding, "", "has not started a build of site"},
		"still busy": {[]map[string]any{{"id": "d1", "status": "building", "preview": true, "commit_sha": sha1, "preview_url": "https://d1--a-poem.apps.example"}}, session.PublishBuilding, "https://d1--a-poem.apps.example", "still building"},
	} {
		h := newHost(t, map[string]func(int) (int, any){"POST /apps": created("a-poem"), "GET /apps/a-poem/deploys": deploys(c.deploys...)})
		m := &fakeMachine{dirs: site, outputs: []string{"1 " + sha1 + "\n"}}
		res, err := newTool(t, h, creds{}).Run(ctx, call(`{"path":"site"}`, m, tools.State{}))
		got := metaOf(res)
		if err != nil || got.Status != c.status || got.Preview != c.preview || !strings.Contains(resultText(res), c.text) {
			t.Errorf("%s: %+v %+v %v", name, res, got, err)
		}
		if res.IsError() != (c.status == session.PublishCanceled) {
			t.Errorf("%s: outcome %s", name, res.Outcome)
		}
	}

	// A thread whose results record no folder, as before results did,
	// releases its standing preview as it is (spec 043).
	standing := &session.PublishMeta{App: "a-poem", Commit: sha1, Status: session.PublishReady, Preview: "https://d1--a-poem.apps.example"}
	readyState := tools.State{App: standing, Apps: map[string]*session.PublishMeta{"a-poem": standing}, Standing: map[string]*session.PublishMeta{"a-poem": standing}}
	reason := "agents_may_not_release"
	for name, c := range map[string]struct {
		release func(int) (int, any)
		status  string
		text    string
	}{
		"refused": {func(int) (int, any) {
			return http.StatusOK, map[string]any{"release": map[string]any{"tag": "v3", "commit_sha": sha1, "status": "refused", "reason": reason}}
		}, session.PublishRefused, "refused the release v3: agents_may_not_release"},
		"failed": {func(n int) (int, any) {
			if n == 1 {
				return http.StatusOK, map[string]any{"release": map[string]any{"tag": "v3", "commit_sha": sha1, "status": "pending"}}
			}
			return http.StatusOK, map[string]any{"release": map[string]any{"tag": "v3", "commit_sha": sha1, "status": "failed"}, "deploy": map[string]any{"id": "d9", "error": map[string]string{"code": "build_internal", "message": "The platform failed."}}}
		}, session.PublishFailed, "The release v3 failed: build_internal: The platform failed."},
		"pending": {refused(http.StatusNotFound, "release_not_found"), session.PublishPending, "not done after"},
	} {
		h := newHost(t, map[string]func(int) (int, any){
			"GET /apps/a-poem": app("a-poem"),
			"GET /apps/a-poem/releases": func(int) (int, any) {
				return http.StatusOK, map[string]any{"releases": []map[string]any{{"tag": "v2", "commit_sha": sha2, "status": "released"}}}
			},
			"GET /apps/a-poem/releases/v3": c.release,
		})
		m := &fakeMachine{}
		res, err := newTool(t, h, creds{}).Run(ctx, call(`{"release":true}`, m, readyState))
		got := metaOf(res)
		if err != nil || got.Status != c.status || got.Release != "v3" || got.Commit != sha1 || !strings.Contains(resultText(res), c.text) {
			t.Errorf("%s: %+v %+v %v", name, res, got, err)
		}
		if len(m.scripts) != 1 || !strings.Contains(m.scripts[0], "refs/tags/\"'v3'") {
			t.Errorf("%s: the tag script %q", name, m.scripts)
		}
		if res.IsError() != (c.status != session.PublishPending) {
			t.Errorf("%s: outcome %s", name, res.Outcome)
		}
	}

	// A preview still building is released: its commit is tagged, and the
	// host releases it once it is ready.
	still := &session.PublishMeta{App: "a-poem", Commit: sha2, Status: session.PublishBuilding}
	building := tools.State{App: still, Apps: map[string]*session.PublishMeta{"a-poem": still}, Standing: map[string]*session.PublishMeta{"a-poem": still}}
	h = newHost(t, map[string]func(int) (int, any){
		"GET /apps/a-poem":          app("a-poem"),
		"GET /apps/a-poem/releases": func(int) (int, any) { return http.StatusOK, map[string]any{"releases": []map[string]any{}} },
		"GET /apps/a-poem/releases/v1": func(n int) (int, any) {
			if n == 1 {
				return http.StatusOK, map[string]any{"release": map[string]any{"tag": "v1", "commit_sha": sha2, "status": "pending"}}
			}
			return http.StatusOK, map[string]any{"release": map[string]any{"tag": "v1", "commit_sha": sha2, "status": "released"}}
		},
	})
	m = &fakeMachine{}
	res, err = newTool(t, h, creds{}).Run(ctx, call(`{"release":true}`, m, building))
	if got := metaOf(res); err != nil || got.Status != session.PublishReleased || got.Commit != sha2 || len(m.scripts) != 1 || !strings.Contains(m.scripts[0], sha2) {
		t.Fatalf("a release of a preview still building: %+v %+v %v %q", res, got, err, m.scripts)
	}

	// A release the host already holds for the commit is waited on, not
	// tagged again.
	h = newHost(t, map[string]func(int) (int, any){
		"GET /apps/a-poem": app("a-poem"),
		"GET /apps/a-poem/releases": func(int) (int, any) {
			return http.StatusOK, map[string]any{"releases": []map[string]any{{"tag": "v1", "commit_sha": sha1, "status": "released"}}}
		},
		"GET /apps/a-poem/releases/v1": func(int) (int, any) {
			return http.StatusOK, map[string]any{"release": map[string]any{"tag": "v1", "commit_sha": sha1, "status": "released"}}
		},
	})
	m = &fakeMachine{}
	res, err = newTool(t, h, creds{}).Run(ctx, call(`{"release":true}`, m, readyState))
	if got := metaOf(res); err != nil || got.Status != session.PublishReleased || got.Release != "v1" || len(m.scripts) != 0 {
		t.Fatalf("a release already made: %+v %+v %v %q", res, got, err, m.scripts)
	}
}

// TestRefusals: a path that is no folder, a release of an app whose
// results record no folder and that has no preview, a push URL on
// another host, an app host that refuses, a git
// push that fails, and an installation that mints no token for the host
// each answer an error the model reads; a lost lease stops the turn; and
// an app the host no longer knows is created again.
func TestRefusals(t *testing.T) {
	ctx := t.Context()
	site := map[string]bool{"/work/site": true, "/work/page.html": false}
	h := newHost(t, map[string]func(int) (int, any){"POST /apps": created("a-poem")})
	for name, c := range map[string]struct {
		input string
		st    tools.State
		want  string
	}{
		"no such folder": {`{"path":"nope"}`, tools.State{}, "nope is not a folder of the machine"},
		"a file":         {`{"path":"page.html"}`, tools.State{}, "page.html is not a folder of the machine"},
		"no preview":     {`{"release":true}`, tools.State{App: &session.PublishMeta{App: "a-poem"}, Apps: map[string]*session.PublishMeta{"a-poem": {App: "a-poem"}}}, prompts.Text(prompts.PublishNoPreview)},
		"no folder":      {`{"release":true}`, tools.State{}, ". is not a folder of the machine"},
	} {
		res, err := newTool(t, h, creds{}).Run(ctx, call(c.input, &fakeMachine{dirs: site}, c.st))
		if err != nil || !res.IsError() || !strings.Contains(resultText(res), c.want) {
			t.Errorf("%s: %+v %v", name, res, err)
		}
	}

	elsewhere := newHost(t, map[string]func(int) (int, any){"POST /apps": func(int) (int, any) {
		return http.StatusCreated, map[string]any{"slug": "a-poem", "repository": map[string]string{"push_url": "https://elsewhere.example/a-poem.git"}}
	}})
	m := &fakeMachine{dirs: site}
	res, err := newTool(t, elsewhere, creds{}).Run(ctx, call(`{"path":"site"}`, m, tools.State{}))
	if err != nil || !res.IsError() || !strings.Contains(resultText(res), "is not on the git host git.example") || len(m.scripts) != 0 {
		t.Fatalf("another host: %+v %v %q", res, err, m.scripts)
	}

	forbidden := newHost(t, map[string]func(int) (int, any){"POST /apps": refused(http.StatusForbidden, "forbidden")})
	res, err = newTool(t, forbidden, creds{}).Run(ctx, call(`{"path":"site"}`, &fakeMachine{dirs: site}, tools.State{}))
	if err != nil || !res.IsError() || !strings.Contains(resultText(res), "the app host refused with forbidden: No") {
		t.Fatalf("a refused create: %+v %v", res, err)
	}

	m = &fakeMachine{dirs: site, outputs: []string{"128!fatal: unable to access the repository\n"}}
	res, err = newTool(t, h, creds{}).Run(ctx, call(`{"path":"site"}`, m, tools.State{}))
	if err != nil || !res.IsError() || !strings.Contains(resultText(res), "the push of the folder failed: exit 128: fatal: unable to access") || metaOf(res).App != "a-poem" {
		t.Fatalf("a failed push: %+v %v", res, err)
	}

	gone := newHost(t, map[string]func(int) (int, any){
		"GET /apps/old":            refused(http.StatusNotFound, "app_not_found"),
		"POST /apps":               created("a-poem"),
		"GET /apps/a-poem/deploys": deploys(map[string]any{"id": "d1", "status": "ready", "preview": true, "commit_sha": sha1}),
	})
	res, err = newTool(t, gone, creds{}).Run(ctx, call(`{"path":"site"}`, &fakeMachine{dirs: site, outputs: []string{"1 " + sha1}}, tools.State{App: &session.PublishMeta{App: "old"}}))
	if got := metaOf(res); err != nil || res.IsError() || got.App != "a-poem" || gone.count("POST /apps") != 1 {
		t.Fatalf("an app the host lost: %+v %+v %v", res, got, err)
	}

	res, err = newTool(t, h, creds{err: runner.ErrNotMinted}).Run(ctx, call(`{"path":"site"}`, &fakeMachine{dirs: site}, tools.State{}))
	if err != nil || !res.IsError() || !strings.Contains(resultText(res), "mints no credential for its app host") {
		t.Fatalf("not minted: %+v %v", res, err)
	}
	none := New(Options{URL: h.srv.URL, GitURL: gitURL}, session.Session{}, nil)
	if res, err := none.Run(ctx, call(`{}`, &fakeMachine{dirs: site}, tools.State{})); err != nil || !res.IsError() {
		t.Fatalf("no tokens: %+v %v", res, err)
	}
	if _, err := newTool(t, h, creds{err: runner.ErrLeaseLost}).Run(ctx, call(`{"path":"site"}`, &fakeMachine{dirs: site}, tools.State{})); !errors.Is(err, runner.ErrLeaseLost) {
		t.Fatalf("a lost lease: %v", err)
	}
}

// TestTheDescriptionStatesTheWait holds the tool's description to Wait.
func TestTheDescriptionStatesTheWait(t *testing.T) {
	d := (&Tool{}).Definition()
	if want := fmt.Sprintf("at most %d minutes", int(Wait/time.Minute)); !strings.Contains(d.Description, want) {
		t.Fatalf("the description does not say %q:\n%s", want, d.Description)
	}
	if d.Name != Name || (&Tool{}).Properties().Effect != tools.EffectExternal {
		t.Fatalf("definition %+v", d)
	}
	if _, err := tools.CompileSchema(d.InputSchema); err != nil {
		t.Fatal(err)
	}
}

func TestNextTag(t *testing.T) {
	for _, c := range []struct {
		tags []string
		want string
	}{
		{nil, "v1"},
		{[]string{"v1", "v3", "v2.0.0", "release", "v10x"}, "v4"},
	} {
		var rs []hostRelease
		for _, tag := range c.tags {
			rs = append(rs, hostRelease{Tag: tag})
		}
		if got := nextTag(rs); got != c.want {
			t.Errorf("%v: %s, want %s", c.tags, got, c.want)
		}
	}
}

// timedOut answers every command as one that passed its timeout, or with
// err.
type timedOut struct {
	fakeMachine
	err error
}

func (m *timedOut) Exec(context.Context, machine.ExecRequest) (machine.ExecResult, error) {
	return machine.ExecResult{TimedOut: true}, m.err
}

// TestTheHostsAnswers: what the tool makes of an app host that answers
// without its envelope, with what it does not read, or not at all, a
// build log it cannot read, and a git command that runs out of time.
func TestTheHostsAnswers(t *testing.T) {
	ctx := t.Context()
	if (Options{}).Configured() || !(Options{URL: "https://apps.example", GitURL: gitURL}).Configured() {
		t.Fatal("Configured")
	}
	for _, c := range []struct {
		e    hostError
		want string
	}{
		{hostError{Status: 502}, "the app host answered 502"},
		{hostError{Status: 403, Code: "forbidden"}, "the app host refused with forbidden"},
		{hostError{Status: 409, Code: "slug_taken", Message: "This slug is already in use."}, "the app host refused with slug_taken: This slug is already in use"},
	} {
		if got := c.e.Error(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.e, got, c.want)
		}
	}
	if deref(nil) != "" {
		t.Fatal("deref(nil)")
	}
	site := map[string]bool{"/work/site": true}
	for name, c := range map[string]struct {
		answer map[string]func(int) (int, any)
		m      machine.Machine
		want   string
	}{
		"plain text": {map[string]func(int) (int, any){"POST /apps": func(int) (int, any) { return http.StatusBadGateway, "upstream down" }}, &fakeMachine{dirs: site}, "the app host answered 502"},
		"not json":   {map[string]func(int) (int, any){"POST /apps": func(int) (int, any) { return http.StatusCreated, "[" }}, &fakeMachine{dirs: site}, "not what the tool reads"},
		"no slug":    {map[string]func(int) (int, any){"POST /apps": func(int) (int, any) { return http.StatusCreated, map[string]any{} }}, &fakeMachine{dirs: site}, "without a slug or a push URL"},
		"deploys down": {map[string]func(int) (int, any){"POST /apps": created("a-poem"), "GET /apps/a-poem/deploys": refused(http.StatusServiceUnavailable, "unavailable")},
			&fakeMachine{dirs: site, outputs: []string{"1 " + sha1}}, "refused with unavailable"},
		"push timeout": {map[string]func(int) (int, any){"POST /apps": created("a-poem")}, &timedOut{dirs: site}, "passed its timeout"},
		"push error":   {map[string]func(int) (int, any){"POST /apps": created("a-poem")}, &timedOut{dirs: site, err: errors.New("the sandbox is gone")}, "the sandbox is gone"},
		"odd output":   {map[string]func(int) (int, any){"POST /apps": created("a-poem")}, &fakeMachine{dirs: site, outputs: []string{"nothing\n"}}, "answered \"nothing\""},
	} {
		h := newHost(t, c.answer)
		res, err := newTool(t, h, creds{}).Run(ctx, call(`{"path":"site"}`, c.m, tools.State{}))
		if err != nil || !res.IsError() || !strings.Contains(resultText(res), c.want) {
			t.Errorf("%s: %+v %v", name, res, err)
		}
	}
	if _, err := newTool(t, newHost(t, nil), creds{}).Run(ctx, call(`{"path":`, &fakeMachine{dirs: site}, tools.State{})); err == nil {
		t.Fatal("an input that is no JSON object ran")
	}

	failed := map[string]any{"id": "d1", "status": "failed", "preview": true, "commit_sha": sha1}
	h := newHost(t, map[string]func(int) (int, any){
		"POST /apps":                       created("a-poem"),
		"GET /apps/a-poem/deploys":         deploys(failed),
		"GET /apps/a-poem/deploys/d1/logs": refused(http.StatusInternalServerError, "internal"),
	})
	res, err := newTool(t, h, creds{}).Run(ctx, call(`{"path":"site"}`, &fakeMachine{dirs: site, outputs: []string{"1 " + sha1}}, tools.State{}))
	if err != nil || !strings.Contains(resultText(res), "[the build log could not be read: the app host answered 500]") || metaOf(res).Error.Code != session.PublishFailed {
		t.Fatalf("an unreadable log: %+v %v", res, err)
	}

	last := &session.PublishMeta{App: "a-poem", Commit: sha1, Status: session.PublishReady}
	ready := tools.State{App: last, Apps: map[string]*session.PublishMeta{"a-poem": last}, Standing: map[string]*session.PublishMeta{"a-poem": last}}
	for name, c := range map[string]struct {
		answer map[string]func(int) (int, any)
		want   string
	}{
		"gone":          {map[string]func(int) (int, any){"GET /apps/a-poem": refused(http.StatusNotFound, "app_not_found")}, "no longer exists at the app host"},
		"releases down": {map[string]func(int) (int, any){"GET /apps/a-poem": app("a-poem"), "GET /apps/a-poem/releases": refused(http.StatusServiceUnavailable, "unavailable")}, "refused with unavailable"},
		"release down": {map[string]func(int) (int, any){"GET /apps/a-poem": app("a-poem"), "GET /apps/a-poem/releases": func(int) (int, any) { return http.StatusOK, map[string]any{} },
			"GET /apps/a-poem/releases/v1": refused(http.StatusServiceUnavailable, "unavailable")}, "refused with unavailable"},
		"other host": {map[string]func(int) (int, any){"GET /apps/a-poem": func(int) (int, any) {
			return http.StatusOK, map[string]any{"slug": "a-poem", "repository": map[string]string{"push_url": "https://elsewhere.example/a.git"}}
		}}, "is not on the git host"},
	} {
		res, err := newTool(t, newHost(t, c.answer), creds{}).Run(ctx, call(`{"release":true}`, &fakeMachine{}, ready))
		if err != nil || !res.IsError() || !strings.Contains(resultText(res), c.want) {
			t.Errorf("%s: %+v %v", name, res, err)
		}
	}
	res, err = newTool(t, newHost(t, map[string]func(int) (int, any){
		"GET /apps/a-poem": app("a-poem"), "GET /apps/a-poem/releases": func(int) (int, any) { return http.StatusOK, map[string]any{} },
	}), creds{}).Run(ctx, call(`{"release":true}`, &fakeMachine{outputs: []string{"1!fatal: refused\n"}}, ready))
	if err != nil || !res.IsError() || !strings.Contains(resultText(res), "the push of the tag v1 failed: exit 1: fatal: refused") {
		t.Fatalf("a refused tag: %+v %v", res, err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if sleep(canceled, time.Now().Add(time.Hour), time.Millisecond) {
		t.Fatal("a canceled wait slept")
	}
}

// TestServedReadsTheCommitsAnAppServes: the runner's reader answers the
// app's current deploy's commit as Live and its newest ready preview's
// as Preview, read with the session's token for the app host; a host
// that names neither answers none; a host that refuses, and a drive with
// no token source, are errors.
func TestServedReadsTheCommitsAnAppServes(t *testing.T) {
	h := newHost(t, map[string]func(int) (int, any){
		"GET /apps/tide": func(int) (int, any) {
			_, body := app("tide")(1)
			m := body.(map[string]any)
			m["current_deploy"] = map[string]any{"id": "d4", "commit_sha": sha1}
			m["latest_preview"] = map[string]any{"id": "d5", "commit_sha": sha2}
			return http.StatusOK, m
		},
		"GET /apps/fresh": app("fresh"),
		"GET /apps/draft": func(int) (int, any) {
			_, body := app("draft")(1)
			m := body.(map[string]any)
			m["current_deploy"], m["latest_preview"] = nil, map[string]any{"id": "d1", "commit_sha": sha2}
			return http.StatusOK, m
		},
		"GET /apps/gone": refused(http.StatusNotFound, "not_found"),
	})
	read := Served(Options{URL: h.srv.URL, Audience: "apps", GitURL: gitURL})
	tokens := runner.NewTokenSource(creds{}, nil, nil)
	for _, c := range []struct {
		slug string
		want runner.AppCommits
	}{
		{"tide", runner.AppCommits{Live: sha1, Preview: sha2}},
		{"fresh", runner.AppCommits{}},
		{"draft", runner.AppCommits{Preview: sha2}},
	} {
		got, err := read(t.Context(), tokens, c.slug)
		if err != nil || got != c.want {
			t.Fatalf("%s: %+v, %v; want %+v", c.slug, got, err, c.want)
		}
	}
	if _, err := read(t.Context(), tokens, "gone"); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("a refused read: %v", err)
	}
	if _, err := read(t.Context(), nil, "tide"); err == nil {
		t.Fatal("a drive with no token source read the host")
	}
}

// attachedSession is newTool's session attached the app tide, whose
// checkout is /work/tide, beside a repository of its request in the
// working directory.
func attachedTool(t *testing.T, h *host) *Tool {
	t.Helper()
	s := session.Session{ID: "ses_1", Title: "A poem", Agent: session.AgentRef{ID: "agent_1", Name: "builder", Version: 3}, Resources: []session.Resource{
		{Type: session.ResourceRepository, URL: gitURL + "/acme/web.git"},
		{Type: session.ResourceRepository, URL: gitURL + "/r/tide.git", App: &session.ResourceApp{Slug: "tide", Name: "Tide", URL: "https://tide.apps.example"}, Attached: true},
	}}
	return New(Options{URL: h.srv.URL, Audience: "apps", GitURL: gitURL, Wait: 50 * time.Millisecond, PollEvery: 5 * time.Millisecond}, s, runner.NewTokenSource(creds{}, nil, nil))
}

// liveApp answers the app slug with its current deploy at live, none
// when live is "".
func liveApp(slug, live string) func(int) (int, any) {
	return func(n int) (int, any) {
		_, body := app(slug)(n)
		m := body.(map[string]any)
		m["current_deploy"] = nil
		if live != "" {
			m["current_deploy"] = map[string]any{"id": "dlive", "commit_sha": live}
		}
		return http.StatusOK, m
	}
}

// TestWhichApp: app naming an attached app publishes its checkout from
// the checkout, pushing the session's branch to the repository the
// session was attached; naming an app the thread made publishes that one
// through the session's git directory; any other slug is
// app_not_attached with no request and no app in its meta; a folder
// outside the named app's checkout is not_in_checkout; with no app a
// folder inside a checkout publishes that app and one outside it the
// thread's own; a release with standing previews of several apps and no
// app is app_required, naming them.
func TestWhichApp(t *testing.T) {
	ctx := t.Context()
	ready := map[string]any{"id": "d2", "status": "ready", "preview": true, "commit_sha": sha2, "preview_url": "https://d2--tide.apps.example"}
	dirs := map[string]bool{"/work": true, "/work/tide": true, "/work/tide/site": true, "/work/site": true}

	h := newHost(t, map[string]func(int) (int, any){"GET /apps/tide": app("tide"), "GET /apps/tide/deploys": deploys(ready)})
	m := &fakeMachine{dirs: dirs, outputs: []string{"1 " + sha2 + "\n"}}
	res, err := attachedTool(t, h).Run(ctx, call(`{"app":"tide"}`, m, tools.State{}))
	got := metaOf(res)
	if err != nil || res.IsError() || got.App != "tide" || !got.Attached || got.Status != session.PublishReady || got.Commit != sha2 || h.count("POST /apps") != 0 {
		t.Fatalf("an attached app: %+v %+v %v", res, got, err)
	}
	script := m.scripts[0]
	for _, want := range []string{"cd '/work/tide'", "url='" + gitURL + "/r/tide.git'", `git push --quiet "$url" "HEAD:refs/heads/$branch"`, "Publish tide", "Topos-Session: ses_1", "node_modules/ lost+found/"} {
		if !strings.Contains(script, want) {
			t.Fatalf("the checkout's push lacks %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, ".topos/publish") || strings.Contains(script, "origin") {
		t.Fatalf("an attached app's push used the session's git directory or the checkout's remote:\n%s", script)
	}
	if !strings.Contains(resultText(res), "Published tide as a preview") {
		t.Fatalf("the result names %q", resultText(res))
	}

	// A folder inside the checkout publishes the app with no app named.
	m = &fakeMachine{dirs: dirs, outputs: []string{"1 " + sha2 + "\n"}}
	if res, err := attachedTool(t, h).Run(ctx, call(`{"path":"tide/site"}`, m, tools.State{})); err != nil || !metaOf(res).Attached || metaOf(res).App != "tide" {
		t.Fatalf("a folder inside the checkout: %+v %v", res, err)
	}

	// The thread's own app, named by its slug.
	own := map[string]any{"id": "d3", "status": "ready", "preview": true, "commit_sha": sha1, "preview_url": "https://d3--a-poem.apps.example"}
	h = newHost(t, map[string]func(int) (int, any){"GET /apps/a-poem": app("a-poem"), "GET /apps/a-poem/deploys": deploys(own)})
	m = &fakeMachine{dirs: dirs, outputs: []string{"1 " + sha1 + "\n"}}
	st := tools.State{App: &session.PublishMeta{App: "a-poem"}, Apps: map[string]*session.PublishMeta{"a-poem": {App: "a-poem"}}}
	res, err = attachedTool(t, h).Run(ctx, call(`{"app":"a-poem","path":"site"}`, m, st))
	if got := metaOf(res); err != nil || res.IsError() || got.App != "a-poem" || got.Attached || !strings.Contains(m.scripts[0], ".topos/publish") {
		t.Fatalf("the thread's own app: %+v %+v %v %q", res, got, err, m.scripts)
	}
	// With no app, a folder outside every checkout is the thread's own.
	m = &fakeMachine{dirs: dirs, outputs: []string{"1 " + sha1 + "\n"}}
	if res, err := attachedTool(t, h).Run(ctx, call(`{"path":"site"}`, m, st)); err != nil || metaOf(res).App != "a-poem" || metaOf(res).Attached {
		t.Fatalf("a folder outside every checkout: %+v %v", res, err)
	}

	for name, c := range map[string]struct {
		input string
		st    tools.State
		code  string
		text  string
	}{
		"another slug":           {`{"app":"other"}`, st, session.PublishAppNotAttached, "other is not an app this session may publish to. It may publish to a-poem, tide."},
		"an attached app's slug": {`{"app":"tide-2"}`, tools.State{}, session.PublishAppNotAttached, "It may publish to tide."},
		"an attached app as own": {`{"app":"web"}`, tools.State{Apps: map[string]*session.PublishMeta{"web": {App: "web", Attached: true}}}, session.PublishAppNotAttached, "web is not an app"},
		"a folder outside":       {`{"app":"tide","path":"site"}`, tools.State{}, session.PublishNotInCheckout, "site is outside tide/, the checkout of tide."},
		"several standing": {`{"release":true}`, tools.State{Standing: map[string]*session.PublishMeta{
			"tide": {App: "tide", Commit: sha2, Status: session.PublishReady, Attached: true}, "a-poem": {App: "a-poem", Commit: sha1, Status: session.PublishReady},
		}}, session.PublishAppRequired, "app set to one of a-poem, tide, or with path"},
		"several published": {`{"release":true}`, tools.State{Apps: map[string]*session.PublishMeta{
			"tide": {App: "tide", Status: session.PublishFailed, Attached: true}, "a-poem": {App: "a-poem", Status: session.PublishReleased},
		}}, session.PublishAppRequired, "app set to one of a-poem, tide, or with path"},
		"none published, apps attached": {`{"release":true}`, tools.State{}, session.PublishAppRequired, "app set to one of tide, or with path"},
		"a release outside":             {`{"app":"tide","path":"site","release":true}`, tools.State{}, session.PublishNotInCheckout, "site is outside tide/"},
	} {
		h := newHost(t, nil)
		m := &fakeMachine{dirs: dirs}
		res, err := attachedTool(t, h).Run(ctx, call(c.input, m, c.st))
		got := metaOf(res)
		if err != nil || !res.IsError() || got.Error == nil || got.Error.Code != c.code || got.App != "" || !strings.Contains(resultText(res), c.text) {
			t.Errorf("%s: %+v %+v %v", name, res, got, err)
		}
		h.mu.Lock()
		calls := len(h.calls)
		h.mu.Unlock()
		if calls != 0 || len(m.scripts) != 0 {
			t.Errorf("%s: %d requests and %d scripts before the refusal", name, calls, len(m.scripts))
		}
	}
}

// TestAReleaseIsCheckedAgainstTheLiveVersion: a release of an attached
// app pushes its checkout and waits for the build, then is tagged from
// the checkout when its commit descends from the live one; one that does
// not is refused behind_live with no tag, its text naming the merge and
// one more release; nothing live tags at once without a check; an answer
// of git that is neither yes nor no is the call's error and pushes no
// tag; and an own app is checked in the session's git directory.
func TestAReleaseIsCheckedAgainstTheLiveVersion(t *testing.T) {
	ctx := t.Context()
	const live = "3333333ccccccccccccccccccccccccccccccccc"
	dirs := map[string]bool{"/work/tide": true}
	st := tools.State{Standing: map[string]*session.PublishMeta{"tide": {App: "tide", Commit: sha2, Status: session.PublishReady, Attached: true}}}
	host := func(current string) *host {
		return newHost(t, map[string]func(int) (int, any){
			"GET /apps/tide":         liveApp("tide", current),
			"GET /apps/tide/deploys": deploys(map[string]any{"id": "d2", "status": "ready", "preview": true, "commit_sha": sha2, "preview_url": "https://d2--tide.apps.example"}),
			"GET /apps/tide/releases": func(int) (int, any) {
				return http.StatusOK, map[string]any{"releases": []map[string]any{{"tag": "v1", "commit_sha": live, "status": "released"}}}
			},
			"GET /apps/tide/releases/v2": func(int) (int, any) {
				return http.StatusOK, map[string]any{"release": map[string]any{"tag": "v2", "commit_sha": sha2, "status": "released"}}
			},
		})
	}

	m := &fakeMachine{dirs: dirs, outputs: []string{"0 " + sha2 + "\n", "ancestry 0\n", ""}}
	res, err := attachedTool(t, host(live)).Run(ctx, call(`{"release":true}`, m, st))
	if got := metaOf(res); err != nil || res.IsError() || got.Status != session.PublishReleased || got.Release != "v2" || !got.Attached || got.Commit != sha2 || got.Folder != "tide" || len(m.scripts) != 3 {
		t.Fatalf("a release that holds the live one: %+v %+v %v %q", res, got, err, m.scripts)
	}
	if !strings.Contains(m.scripts[0], "cd '/work/tide'") || !strings.Contains(m.scripts[0], `git push --quiet "$url" "HEAD:refs/heads/$branch"`) {
		t.Fatalf("the push of the checkout:\n%s", m.scripts[0])
	}
	for _, want := range []string{"cd '/work/tide'", "live='" + live + "'", `git merge-base --is-ancestor "$live" "$commit"`, `git fetch --quiet "$url" "+refs/tags/*:refs/tags/*"`} {
		if !strings.Contains(m.scripts[1], want) {
			t.Fatalf("the check lacks %q:\n%s", want, m.scripts[1])
		}
	}
	if !strings.Contains(m.scripts[2], "cd '/work/tide'") || !strings.Contains(m.scripts[2], "refs/tags/\"'v2'") {
		t.Fatalf("the tag script:\n%s", m.scripts[2])
	}

	m = &fakeMachine{dirs: dirs, outputs: []string{"0 " + sha2 + "\n", "ancestry 1\n"}}
	res, err = attachedTool(t, host(live)).Run(ctx, call(`{"app":"tide","release":true}`, m, st))
	got := metaOf(res)
	if err != nil || !res.IsError() || got.Status != session.PublishRefused || got.Error == nil || got.Error.Code != session.PublishBehindLive || !got.Attached || got.Release != "" || len(m.scripts) != 2 {
		t.Fatalf("a release behind the live one: %+v %+v %v %q", res, got, err, m.scripts)
	}
	if text := resultText(res); !strings.Contains(text, "tide is live at 3333333") || !strings.Contains(text, "git -C tide merge 3333333") || !strings.Contains(text, `call publish with app "tide" and release set to true again`) {
		t.Fatalf("the refusal reads %q", text)
	}

	m = &fakeMachine{dirs: dirs, outputs: []string{"0 " + sha2 + "\n", ""}}
	if res, err := attachedTool(t, host("")).Run(ctx, call(`{"release":true}`, m, st)); err != nil || res.IsError() || len(m.scripts) != 2 || strings.Contains(m.scripts[1], "merge-base") {
		t.Fatalf("nothing live: %+v %v %q", res, err, m.scripts)
	}

	m = &fakeMachine{dirs: dirs, outputs: []string{"0 " + sha2 + "\n", "ancestry 128\nfatal: Not a valid commit name\n"}}
	if res, err := attachedTool(t, host(live)).Run(ctx, call(`{"release":true}`, m, st)); err != nil || !res.IsError() || metaOf(res).Error != nil || !strings.Contains(resultText(res), "Not a valid commit name") || len(m.scripts) != 2 {
		t.Fatalf("an odd answer of git: %+v %v %q", res, err, m.scripts)
	}

	ownState := tools.State{App: &session.PublishMeta{App: "tide"}, Apps: map[string]*session.PublishMeta{"tide": {App: "tide"}}, Standing: map[string]*session.PublishMeta{"tide": {App: "tide", Commit: sha2, Status: session.PublishReady}}}
	m = &fakeMachine{outputs: []string{"ancestry 1\n"}}
	res, err = newTool(t, host(live), creds{}).Run(ctx, call(`{"release":true}`, m, ownState))
	if got := metaOf(res); err != nil || got.Error == nil || got.Error.Code != session.PublishBehindLive || got.Attached || !strings.Contains(m.scripts[0], ".topos/publish") {
		t.Fatalf("an own app behind the live one: %+v %+v %v %q", res, got, err, m.scripts)
	}
	if text := resultText(res); !strings.Contains(text, `--git-dir="$HOME/.topos/publish/tide.git"`) || !strings.Contains(text, gitURL+"/ada/tide.git") {
		t.Fatalf("an own app's refusal reads %q", text)
	}
}

// TestARefusalOpensNoMachine: a call refused for the app it names opens
// no machine that has not opened, and a call that publishes an attached
// app opens it to find the checkout.
func TestARefusalOpensNoMachine(t *testing.T) {
	ctx := t.Context()
	var opens int
	deferred := func(m *fakeMachine) machine.Machine {
		return machine.Defer(ctx, machine.KindCella, func(context.Context) (machine.Machine, error) {
			opens++
			return m, nil
		})
	}
	h := newHost(t, nil)
	res, err := attachedTool(t, h).Run(ctx, call(`{"app":"other"}`, deferred(&fakeMachine{}), tools.State{}))
	if err != nil || metaOf(res).Error == nil || metaOf(res).Error.Code != session.PublishAppNotAttached || opens != 0 {
		t.Fatalf("a refused app: %+v %v, %d opens", res, err, opens)
	}
	ready := map[string]any{"id": "d2", "status": "ready", "preview": true, "commit_sha": sha2, "preview_url": "https://d2--tide.apps.example"}
	h = newHost(t, map[string]func(int) (int, any){"GET /apps/tide": app("tide"), "GET /apps/tide/deploys": deploys(ready)})
	m := &fakeMachine{dirs: map[string]bool{"/work/tide": true}, outputs: []string{"1 " + sha2 + "\n"}}
	res, err = attachedTool(t, h).Run(ctx, call(`{"app":"tide"}`, deferred(m), tools.State{}))
	if err != nil || res.IsError() || opens != 1 || !strings.Contains(m.scripts[0], "cd '/work/tide'") {
		t.Fatalf("an attached app on a machine not yet open: %+v %v, %d opens, %q", res, err, opens, m.scripts)
	}
}

// releases answers an app's releases as none, and its release tag as
// released on commit.
func releases(slug, tag, commit string) map[string]func(int) (int, any) {
	return map[string]func(int) (int, any){
		"GET /apps/" + slug + "/releases": func(int) (int, any) { return http.StatusOK, map[string]any{"releases": []map[string]any{}} },
		"GET /apps/" + slug + "/releases/" + tag: func(int) (int, any) {
			return http.StatusOK, map[string]any{"release": map[string]any{"tag": tag, "commit_sha": commit, "status": "released"}, "deploy": map[string]any{"id": "drel"}}
		},
	}
}

// with is answer and more, more winning.
func with(answer map[string]func(int) (int, any), more map[string]func(int) (int, any)) map[string]func(int) (int, any) {
	out := map[string]func(int) (int, any){}
	maps.Copy(out, answer)
	maps.Copy(out, more)
	return out
}

// TestAReleaseBuildsWhatItReleases: release with no preview standing
// publishes and releases in one call (spec 059): an attached app's
// checkout is pushed, its build waited for, and the commit it built
// tagged; an own app's folder is published through the session's git
// directory, the app created at once, and its folder recorded, which a
// later release that names no path publishes again; a build still
// running when the wait ends is tagged for the host to release once it
// is ready; and a build the host has not started by then releases
// nothing.
func TestAReleaseBuildsWhatItReleases(t *testing.T) {
	ctx := t.Context()
	ready := map[string]any{"id": "d2", "status": "ready", "preview": true, "commit_sha": sha2, "preview_url": "https://d2--tide.apps.example"}
	h := newHost(t, with(releases("tide", "v1", sha2), map[string]func(int) (int, any){
		"GET /apps/tide": liveApp("tide", ""), "GET /apps/tide/deploys": deploys(ready),
	}))
	m := &fakeMachine{dirs: map[string]bool{"/work/tide": true}, outputs: []string{"1 " + sha2 + "\n", ""}}
	res, err := attachedTool(t, h).Run(ctx, call(`{"app":"tide","path":"tide","release":true}`, m, tools.State{}))
	got := metaOf(res)
	if err != nil || res.IsError() || got.Status != session.PublishReleased || got.Release != "v1" || got.Commit != sha2 || !got.Attached || got.Folder != "tide" ||
		got.Preview != "https://d2--tide.apps.example" || got.Deploy != "drel" {
		t.Fatalf("an attached app released in one call: %+v %+v %v", res, got, err)
	}
	if len(m.scripts) != 2 || !strings.Contains(m.scripts[0], "Publish tide") || !strings.Contains(m.scripts[1], sha2) || !strings.Contains(m.scripts[1], "refs/tags/\"'v1'") {
		t.Fatalf("the push and the tag: %q", m.scripts)
	}
	if !strings.Contains(resultText(res), "Released v1") || h.count("GET /apps/tide/deploys") == 0 {
		t.Fatalf("the result reads %q, %d reads of the deploys", resultText(res), h.count("GET /apps/tide/deploys"))
	}

	own := map[string]any{"id": "d1", "status": "ready", "preview": true, "commit_sha": sha1, "preview_url": "https://d1--a-poem.apps.example"}
	h = newHost(t, with(releases("a-poem", "v1", sha1), map[string]func(int) (int, any){
		"POST /apps": created("a-poem"), "GET /apps/a-poem": app("a-poem"), "GET /apps/a-poem/deploys": deploys(own),
		// v1 is listed once it is made, so a release of the same commit
		// waits on it rather than tagging again.
		"GET /apps/a-poem/releases": func(n int) (int, any) {
			if n == 1 {
				return http.StatusOK, map[string]any{"releases": []map[string]any{}}
			}
			return http.StatusOK, map[string]any{"releases": []map[string]any{{"tag": "v1", "commit_sha": sha1, "status": "released"}}}
		},
	}))
	site := map[string]bool{"/work": true, "/work/site": true}
	m = &fakeMachine{dirs: site, outputs: []string{"1 " + sha1 + "\n", ""}}
	res, err = newTool(t, h, creds{}).Run(ctx, call(`{"path":"site","release":true}`, m, tools.State{}))
	first := metaOf(res)
	if err != nil || res.IsError() || first.Status != session.PublishReleased || first.Release != "v1" || first.Commit != sha1 || first.Attached || first.Folder != "site" || h.count("POST /apps") != 1 {
		t.Fatalf("an own app released in one call: %+v %+v %v", res, first, err)
	}
	if len(m.scripts) != 2 || !strings.Contains(m.scripts[0], "GIT_WORK_TREE='/work/site'") || !strings.Contains(m.scripts[1], ".topos/publish") {
		t.Fatalf("the own app's push and tag: %q", m.scripts)
	}
	// The thread's state now holds the release, whose folder a release
	// that names no path publishes again.
	st := tools.State{App: &first, Apps: map[string]*session.PublishMeta{"a-poem": &first}}
	m = &fakeMachine{dirs: site, outputs: []string{"0 " + sha1 + "\n"}}
	res, err = newTool(t, h, creds{}).Run(ctx, call(`{"release":true}`, m, st))
	if got := metaOf(res); err != nil || res.IsError() || got.Release != "v1" || got.Folder != "site" || len(m.scripts) != 1 || !strings.Contains(m.scripts[0], "GIT_WORK_TREE='/work/site'") {
		t.Fatalf("a release of the folder released last: %+v %+v %v %q", res, got, err, m.scripts)
	}

	// Still building when the wait ends: tagged, and the host releases it
	// once it is ready.
	building := map[string]any{"id": "d2", "status": "building", "preview": true, "commit_sha": sha2, "preview_url": "https://d2--tide.apps.example"}
	h = newHost(t, with(releases("tide", "v1", sha2), map[string]func(int) (int, any){"GET /apps/tide": liveApp("tide", ""), "GET /apps/tide/deploys": deploys(building)}))
	m = &fakeMachine{dirs: map[string]bool{"/work/tide": true}, outputs: []string{"1 " + sha2 + "\n", ""}}
	res, err = attachedTool(t, h).Run(ctx, call(`{"app":"tide","release":true}`, m, tools.State{}))
	if got := metaOf(res); err != nil || res.IsError() || got.Status != session.PublishReleased || got.Release != "v1" || len(m.scripts) != 2 {
		t.Fatalf("a build still running: %+v %+v %v %q", res, got, err, m.scripts)
	}

	// No build started by the end of the wait: nothing is tagged.
	h = newHost(t, with(releases("tide", "v1", sha2), map[string]func(int) (int, any){"GET /apps/tide": liveApp("tide", ""), "GET /apps/tide/deploys": deploys()}))
	m = &fakeMachine{dirs: map[string]bool{"/work/tide": true}, outputs: []string{"1 " + sha2 + "\n"}}
	res, err = attachedTool(t, h).Run(ctx, call(`{"app":"tide","release":true}`, m, tools.State{}))
	if got := metaOf(res); err != nil || res.IsError() || got.Status != session.PublishBuilding || got.Release != "" || len(m.scripts) != 1 || h.count("GET /apps/tide/releases") != 0 ||
		!strings.Contains(resultText(res), "Nothing was released yet: the app host has not started a build of tide") {
		t.Fatalf("no build started: %+v %+v %v %q", res, got, err, m.scripts)
	}
}

// TestAFailedBuildReleasesNothing: a release whose build fails, or is
// canceled, pushes no tag and reads no release; its result is an error
// that says nothing was released, with the build's code, its sentence
// and the end of its log, and its meta records the failed build.
func TestAFailedBuildReleasesNothing(t *testing.T) {
	ctx := t.Context()
	var log strings.Builder
	for i := 1; i <= 3; i++ {
		fmt.Fprintf(&log, "{\"seq\":%d,\"line\":\"npm ERR! line %d\"}\n", i, i)
	}
	for name, c := range map[string]struct {
		deploy map[string]any
		code   string
		text   []string
	}{
		"failed": {map[string]any{"id": "d2", "status": "failed", "preview": true, "commit_sha": sha2,
			"error": map[string]string{"code": "build_failed", "message": "The build command exited with an error."}}, "build_failed",
			[]string{"Nothing was released: the build of tide failed: build_failed: The build command exited with an error.", "npm ERR! line 3", "call publish with release set to true again"}},
		"canceled": {map[string]any{"id": "d2", "status": "canceled", "preview": true, "commit_sha": sha2}, "canceled",
			[]string{"Nothing was released: the build of tide failed: canceled."}},
	} {
		h := newHost(t, with(releases("tide", "v1", sha2), map[string]func(int) (int, any){
			"GET /apps/tide": liveApp("tide", ""), "GET /apps/tide/deploys": deploys(c.deploy),
			"GET /apps/tide/deploys/d2/logs": func(int) (int, any) { return http.StatusOK, log.String() },
		}))
		m := &fakeMachine{dirs: map[string]bool{"/work/tide": true}, outputs: []string{"1 " + sha2 + "\n"}}
		res, err := attachedTool(t, h).Run(ctx, call(`{"app":"tide","release":true}`, m, tools.State{}))
		got := metaOf(res)
		if err != nil || !res.IsError() || got.Status != c.deploy["status"] || got.Error == nil || got.Error.Code != c.code || got.Release != "" || got.Commit != sha2 || got.Deploy != "d2" {
			t.Errorf("%s: %+v %+v %v", name, res, got, err)
		}
		for _, want := range c.text {
			if !strings.Contains(resultText(res), want) {
				t.Errorf("%s: the result lacks %q:\n%s", name, want, resultText(res))
			}
		}
		if len(m.scripts) != 1 || h.count("GET /apps/tide/releases") != 0 {
			t.Errorf("%s: %d scripts and %d reads of the releases after a failed build", name, len(m.scripts), h.count("GET /apps/tide/releases"))
		}
	}
}

// TestCheckRefusesBeforeTheCallIsDecided: Check answers what Run would
// refuse on the input alone, with the same result, before any request and
// without opening the machine: an app not attached, a release that could
// mean several apps, a folder outside the named checkout whether or not
// the machine has reported its working directory, and a drive with no
// token source. Every other call, an absolute folder on a machine not yet
// open among them, is left to Run.
func TestCheckRefusesBeforeTheCallIsDecided(t *testing.T) {
	ctx := t.Context()
	var opens int
	unopened := machine.Defer(ctx, machine.KindCella, func(context.Context) (machine.Machine, error) {
		opens++
		return &fakeMachine{}, nil
	})
	two := tools.State{Standing: map[string]*session.PublishMeta{"tide": {App: "tide", Commit: sha2, Status: session.PublishReady, Attached: true}, "a-poem": {App: "a-poem", Commit: sha1, Status: session.PublishReady}}}
	for name, c := range map[string]struct {
		input string
		m     machine.Machine
		st    tools.State
		code  string
	}{
		"not attached":                 {`{"app":"other","release":true}`, unopened, tools.State{}, session.PublishAppNotAttached},
		"several apps":                 {`{"release":true}`, unopened, two, session.PublishAppRequired},
		"outside, machine not open":    {`{"app":"tide","path":"site","release":true}`, unopened, tools.State{}, session.PublishNotInCheckout},
		"climbing out, machine open":   {`{"app":"tide","path":"tide/../site"}`, &fakeMachine{}, tools.State{}, session.PublishNotInCheckout},
		"absolute outside, open":       {`{"app":"tide","path":"/work/site"}`, &fakeMachine{}, tools.State{}, session.PublishNotInCheckout},
		"absolute, machine not open":   {`{"app":"tide","path":"/work/site"}`, unopened, tools.State{}, ""},
		"climbing out, machine closed": {`{"app":"tide","path":"../work/site"}`, unopened, tools.State{}, ""},
		"inside":                       {`{"app":"tide","path":"tide/site","release":true}`, unopened, tools.State{}, ""},
		"no app":                       {`{"path":"site"}`, unopened, tools.State{}, ""},
		"one app":                      {`{"release":true}`, unopened, tools.State{Standing: map[string]*session.PublishMeta{"tide": two.Standing["tide"]}}, ""},
		"not json":                     {`{"app":`, unopened, tools.State{}, ""},
	} {
		h := newHost(t, nil)
		tool := attachedTool(t, h)
		got := tool.Check(tools.Call{ID: "toolu_1", Input: json.RawMessage(c.input), Machine: c.m, State: c.st})
		switch {
		case c.code == "" && got != nil:
			t.Errorf("%s: refused %+v", name, *got)
		case c.code != "" && (got == nil || metaOf(*got).Error == nil || metaOf(*got).Error.Code != c.code || metaOf(*got).App != ""):
			t.Errorf("%s: %+v, want %s", name, got, c.code)
		case c.code != "":
			// Run refuses the same call with the same result.
			m := &fakeMachine{dirs: map[string]bool{"/work/tide": true, "/work/site": true}}
			res, err := tool.Run(ctx, call(c.input, m, c.st))
			if err != nil || resultText(res) != resultText(*got) || metaOf(res).Error == nil || metaOf(res).Error.Code != c.code {
				t.Errorf("%s: Run answered %+v %v, Check %+v", name, res, err, *got)
			}
		}
		h.mu.Lock()
		calls := len(h.calls)
		h.mu.Unlock()
		if calls != 0 {
			t.Errorf("%s: %d requests", name, calls)
		}
	}
	if opens != 0 {
		t.Fatalf("Check opened the machine %d times", opens)
	}
	none := New(Options{URL: "https://apps.example", GitURL: gitURL}, session.Session{}, nil)
	if got := none.Check(tools.Call{Input: json.RawMessage(`{"path":"site"}`), Machine: unopened}); got == nil || !strings.Contains(resultText(*got), "mints no credential for its app host") {
		t.Fatalf("no token source: %+v", got)
	}
	var _ tools.Checker = (*Tool)(nil)
}
