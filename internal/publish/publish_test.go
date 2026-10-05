// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

	readyState := tools.State{App: &session.PublishMeta{App: "a-poem"}, Ready: &session.PublishMeta{App: "a-poem", Commit: sha1, Status: session.PublishReady, Preview: "https://d1--a-poem.apps.example"}}
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

// TestRefusals: a path that is no folder, a release with no ready
// preview, a push URL on another host, an app host that refuses, a git
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
		"no preview":     {`{"release":true}`, tools.State{}, prompts.Text(prompts.PublishNoPreview)},
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

	ready := tools.State{Ready: &session.PublishMeta{App: "a-poem", Commit: sha1, Status: session.PublishReady}}
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
