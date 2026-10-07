// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package publish is the publish tool of spec 043: it publishes a folder
// of a hosted session's sandbox as the session's own app at the
// installation's app host, waits for the preview the host builds of it,
// and releases the newest preview to the app's address. The runner
// reaches the app host with the session's own short token, which never
// enters the sandbox; the folder is pushed by the sandbox's own git,
// which reaches the installation's git host with the session's git
// credential (spec 019).
package publish

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"
	"latere.ai/x/pkg/otel"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// Name is the tool's name.
const Name = session.ToolPublish

// The limits of spec 043. Wait is stated in the tool's description, which
// a test holds to it.
const (
	// Wait is the longest a call waits for a preview or a release.
	Wait = 10 * time.Minute
	// PollEvery is how often the app host is read while a call waits.
	PollEvery = 3 * time.Second
	// LogTail is how many lines of a failed build's log the model reads.
	LogTail = 40
	// gitTimeout bounds the commit and the push in the sandbox.
	gitTimeout = 5 * time.Minute
	// requestTimeout bounds one request to the app host.
	requestTimeout = 30 * time.Second
	// maxAnswer bounds what is read of one answer of the app host.
	maxAnswer = 4 << 20
)

// DefaultAudience is TOPOS_APPS_AUDIENCE's default.
const DefaultAudience = "apps"

// schema is the tool's input.
var schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "The folder to publish; a relative path resolves against the working directory. Default: the checkout of the app that app names, else the working directory."},
    "app": {"type": "string", "description": "The slug of the app to publish to or release: one the session was given, or one this session created. Default: the app whose checkout path is in, else this session's own app."},
    "release": {"type": "boolean", "description": "True to release the app's newest preview to its own address instead of publishing a folder."}
  },
  "additionalProperties": false
}`)

// Options configure the tool of every session of an installation.
type Options struct {
	// URL is TOPOS_APPS_URL, the app host's root.
	URL string
	// Audience is TOPOS_APPS_AUDIENCE; empty is DefaultAudience.
	Audience string
	// GitURL is TOPOS_ORIGO_URL, the git host the sandbox's git sends
	// the session's credential to; an app's repository must be on it.
	GitURL string
	// HTTP reaches the app host; nil is the instrumented client.
	HTTP *http.Client
	// Wait and PollEvery replace the limits of the same names when set,
	// for a test.
	Wait, PollEvery time.Duration
}

// Configured reports whether the installation has an app host to publish
// to.
func (o Options) Configured() bool { return o.URL != "" && o.GitURL != "" }

// Tool is the publish tool of one session's drive.
type Tool struct {
	o      Options
	s      session.Session
	tokens *runner.TokenSource
}

// New is the tool of session s, reaching the app host with the drive's
// tokens; nil tokens answers every call that the installation mints no
// credential for its app host.
func New(o Options, s session.Session, tokens *runner.TokenSource) *Tool {
	o.Audience = cmp.Or(o.Audience, DefaultAudience)
	o.Wait = cmp.Or(o.Wait, Wait)
	o.PollEvery = cmp.Or(o.PollEvery, PollEvery)
	if o.HTTP == nil {
		o.HTTP = otel.HTTPClient()
	}
	o.URL = strings.TrimRight(o.URL, "/")
	return &Tool{o: o, s: s, tokens: tokens}
}

// Definition is what the model sees.
func (t *Tool) Definition() tools.Definition {
	return tools.Definition{Name: Name, Description: prompts.Text(prompts.ToolPublish), InputSchema: schema}
}

// Properties: one call at a time, and an effect outside the machine, so
// a call asks in confirm and progressive alike (spec 012).
func (t *Tool) Properties() tools.Properties {
	return tools.Properties{Effect: tools.EffectExternal}
}

type input struct {
	Path    string `json:"path"`
	App     string `json:"app"`
	Release bool   `json:"release"`
}

// target is the app a call publishes to or releases (spec 059): an
// attached app, with its repository and the absolute path of its
// checkout, or the thread's own, with the thread's last result for it.
type target struct {
	// slug is the app's slug, "" for the thread's own app when the call
	// names none, which ensureApp finds or creates.
	slug string
	// repo is an attached app's repository, nil for an own app, and dir
	// its checkout's absolute path and rel that path under the working
	// directory.
	repo     *session.Resource
	dir, rel string
	// last is the thread's last result for an own app.
	last *session.PublishMeta
}

func (g target) attached() bool { return g.repo != nil }

// meta is a result's meta for the app a, marked attached for an
// attached app, so the thread's state never folds an attached app as its
// own.
func (g target) meta(a hostApp) *session.PublishMeta {
	return &session.PublishMeta{App: a.Slug, Name: a.Name, URL: a.URL, Attached: g.attached()}
}

// Run publishes the folder the call names, or releases the newest
// preview of the app it names. The app is resolved before any request,
// and a call that names an app the session may not publish to is
// refused then. A failure of the app host, the git host or the machine
// is the call's error result, which the model reads; a Go error is a
// lost lease or a released machine, which stop the turn.
func (t *Tool) Run(ctx context.Context, c tools.Call) (tools.Result, error) {
	var in input
	if len(bytes.TrimSpace(c.Input)) > 0 {
		// The registry checked the input against the schema, so a failure
		// here is the harness's.
		if err := json.Unmarshal(c.Input, &in); err != nil {
			return tools.Result{}, fmt.Errorf("publish: read the input: %w", err)
		}
	}
	g, refused, err := t.which(ctx, c, in)
	switch {
	case err != nil:
		return failure(cmp.Or(in.App, in.Path, "."), err, nil)
	case refused != nil:
		return *refused, nil
	}
	if t.tokens == nil {
		return unavailable("this server mints no credential for its app host"), nil
	}
	if _, err := t.bearer(ctx); err != nil {
		switch {
		case errors.Is(err, runner.ErrNotMinted):
			return unavailable("this server mints no credential for its app host"), nil
		case errors.Is(err, runner.ErrLeaseLost):
			return tools.Result{}, err
		}
		return unavailable("the session's credential for the app host could not be had: " + err.Error()), nil
	}
	if in.Release {
		return t.release(ctx, c, g)
	}
	return t.preview(ctx, c, g, in.Path)
}

// which resolves the app a call publishes to or releases, before any
// request (spec 059): the app the call names, one the session was
// attached or one the thread created; else, for a folder, the attached
// app whose checkout holds it, or the thread's own; else, for a release,
// the one app whose preview stands. A call that names any other app, a
// folder outside the named attached app's checkout, and a release that
// names no app while previews of several stand are refused, each with a
// result whose meta names no app, so the thread's state folds none.
//
// The machine's working directory is read only where a checkout's path
// is, opening a machine that has not opened, so a call refused for its
// app opens none.
func (t *Tool) which(ctx context.Context, c tools.Call, in input) (target, *tools.Result, error) {
	workdir := func() (string, error) {
		if w := c.Machine.Info().Workdir; w != "" {
			return w, nil
		}
		if err := machine.Open(ctx, c.Machine); err != nil {
			return "", err
		}
		return c.Machine.Info().Workdir, nil
	}
	if in.App != "" {
		if _, _, ok := session.App(t.s, in.App); ok {
			w, err := workdir()
			if err != nil {
				return target{}, nil, err
			}
			g, _ := t.attachedApp(w, in.App)
			if p := in.Path; !in.Release && p != "" && !inside(abs(w, p), g.dir) {
				return g, refusal(session.PublishNotInCheckout, prompts.Render(prompts.PublishNotInCheckout, prompts.Data{"Path": p, "Dir": g.rel, "App": g.slug})), nil
			}
			return g, nil, nil
		}
		if m := c.State.Apps[in.App]; m != nil && !m.Attached {
			return target{slug: in.App, last: m}, nil, nil
		}
		return target{}, refusal(session.PublishAppNotAttached, prompts.Render(prompts.PublishNotAttached, prompts.Data{"App": in.App, "Apps": strings.Join(t.publishable(c.State), ", ")})), nil
	}
	if in.Release {
		slugs := slices.Sorted(maps.Keys(c.State.Standing))
		switch len(slugs) {
		case 0:
			return target{}, nil, nil
		case 1:
			if _, _, ok := session.App(t.s, slugs[0]); !ok {
				return target{slug: slugs[0], last: c.State.Apps[slugs[0]]}, nil, nil
			}
			w, err := workdir()
			if err != nil {
				return target{}, nil, err
			}
			g, _ := t.attachedApp(w, slugs[0])
			return g, nil, nil
		}
		return target{}, refusal(session.PublishAppRequired, prompts.Render(prompts.PublishAppRequired, prompts.Data{"Apps": strings.Join(slugs, ", ")})), nil
	}
	if !slices.ContainsFunc(session.Repositories(t.s), func(r session.Resource) bool { return r.App != nil }) {
		return target{last: c.State.App}, nil, nil
	}
	w, err := workdir()
	if err != nil {
		return target{}, nil, err
	}
	folder := abs(w, cmp.Or(in.Path, "."))
	for _, r := range session.Repositories(t.s) {
		if r.App == nil {
			continue
		}
		if g, ok := t.attachedApp(w, r.App.Slug); ok && inside(folder, g.dir) {
			return g, nil, nil
		}
	}
	return target{last: c.State.App}, nil, nil
}

// attachedApp is the attached app of slug, with its checkout under
// workdir, and whether the session was attached one.
func (t *Tool) attachedApp(workdir, slug string) (target, bool) {
	r, dir, ok := session.App(t.s, slug)
	if !ok {
		return target{}, false
	}
	return target{slug: slug, repo: &r, dir: path.Join(workdir, dir), rel: dir}, true
}

// publishable are the slugs of the apps the session may publish to: the
// attached ones and those the thread created, sorted.
func (t *Tool) publishable(st tools.State) []string {
	var out []string
	for _, r := range session.Repositories(t.s) {
		if r.App != nil {
			out = append(out, r.App.Slug)
		}
	}
	for slug, m := range st.Apps {
		if !m.Attached && !slices.Contains(out, slug) {
			out = append(out, slug)
		}
	}
	slices.Sort(out)
	return out
}

// refusal is a call's error result refused with code before any
// request: its meta names no app, so the thread's state folds none.
func refusal(code, s string) *tools.Result {
	r := text(tools.OutcomeError, s, &session.PublishMeta{Error: &session.PublishError{Code: code}})
	return &r
}

// abs is p resolved against workdir and cleaned.
func abs(workdir, p string) string {
	if !path.IsAbs(p) {
		p = path.Join(workdir, p)
	}
	return path.Clean(p)
}

// inside reports whether the folder p is dir or a folder under it.
func inside(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// harnessError reports the errors that stop the turn instead of being
// the call's result.
func harnessError(err error) bool {
	return errors.Is(err, runner.ErrLeaseLost) || errors.Is(err, machine.ErrReleased)
}

func unavailable(reason string) tools.Result {
	return text(tools.OutcomeError, prompts.Render(prompts.PublishUnavailable, prompts.Data{"Reason": reason}), nil)
}

func text(outcome, s string, meta *session.PublishMeta) tools.Result {
	res := tools.Result{Outcome: outcome, Content: []lux.Block{{Type: ir.BlockText, Text: s}}}
	if meta != nil {
		res.Meta = &tools.Meta{Publish: meta}
	}
	return res
}

// failure is the call's error result for err while publishing shown, or
// err itself when it stops the turn.
func failure(shown string, err error, meta *session.PublishMeta) (tools.Result, error) {
	if harnessError(err) {
		return tools.Result{}, err
	}
	return text(tools.OutcomeError, prompts.Render(prompts.PublishError, prompts.Data{"Path": shown, "Error": err.Error()}), meta), nil
}

// preview publishes the folder p of the machine to g and waits for its
// preview: the thread's own app's folder through the session's git
// directory (spec 043), or an attached app's checkout, committed and
// pushed on the session's branch (spec 059).
func (t *Tool) preview(ctx context.Context, c tools.Call, g target, p string) (tools.Result, error) {
	shown, dir := cmp.Or(p, "."), abs(c.Machine.Info().Workdir, cmp.Or(p, "."))
	if g.attached() && p == "" {
		shown, dir = g.rel, g.dir
	}
	if fi, err := c.Machine.Stat(ctx, dir); err != nil || !fi.IsDir {
		if harnessError(err) {
			return tools.Result{}, err
		}
		return text(tools.OutcomeError, prompts.Render(prompts.PublishNotFolder, prompts.Data{"Path": shown}), nil), nil
	}
	var a hostApp
	var meta *session.PublishMeta
	var push func(empty bool) (pushed, error)
	if g.attached() {
		var err error
		if a, err = t.app(ctx, g.slug); err != nil {
			return failure(shown, err, &session.PublishMeta{App: g.slug, Attached: true})
		}
		meta = g.meta(a)
		if err := errors.Join(t.onGitHost(a.Repository.PushURL), t.onGitHost(g.repo.URL)); err != nil {
			return failure(shown, err, meta)
		}
		push = func(empty bool) (pushed, error) { return t.pushCheckout(ctx, c.Machine, g, empty) }
	} else {
		var err error
		if a, err = t.ensureApp(ctx, g.last); err != nil {
			return failure(shown, err, nil)
		}
		meta = g.meta(a)
		if err := t.onGitHost(a.Repository.PushURL); err != nil {
			return failure(shown, err, meta)
		}
		push = func(empty bool) (pushed, error) { return t.push(ctx, c.Machine, a, dir, empty) }
	}
	pushed, err := push(false)
	if err != nil {
		return failure(shown, err, meta)
	}
	d, found, done, err := t.waitDeploy(ctx, a.Slug, pushed.commit)
	if err == nil && !pushed.changed && done && (d.Status == session.PublishFailed || d.Status == session.PublishCanceled) {
		// Nothing changed since a build that failed or was replaced: an
		// empty commit builds the same files again.
		if pushed, err = push(true); err != nil {
			return failure(shown, err, meta)
		}
		d, found, done, err = t.waitDeploy(ctx, a.Slug, pushed.commit)
	}
	meta.Commit = pushed.commit
	if err != nil {
		return failure(shown, err, meta)
	}
	if found {
		meta.Deploy, meta.Preview = d.ID, d.PreviewURL
	}
	wait := t.o.Wait.String()
	switch {
	case !done:
		meta.Status = session.PublishBuilding
		return text(tools.OutcomeOK, prompts.Render(prompts.PublishBuilding, prompts.Data{"Path": shown, "Wait": wait, "Preview": meta.Preview}), meta), nil
	case d.Status == session.PublishReady:
		meta.Status = session.PublishReady
		return text(tools.OutcomeOK, prompts.Render(prompts.PublishReady, prompts.Data{"Path": shown, "Preview": meta.Preview, "URL": a.URL}), meta), nil
	}
	meta.Status = d.Status
	e := cmp.Or(d.Error, &session.PublishError{Code: d.Status})
	meta.Error = e
	log := ""
	if d.Status == session.PublishFailed {
		log = t.logTail(ctx, a.Slug, d.ID)
	}
	return text(tools.OutcomeError, prompts.Render(prompts.PublishFailed, prompts.Data{"Path": shown, "Code": e.Code, "Message": e.Message, "Log": log}), meta), nil
}

// release puts the commit of g's newest preview that stands in the
// thread, ready or still building, at the app's address with the next
// version tag, and waits for the release: the host releases a preview
// still building once it is ready. Before it tags, it requires the commit
// the app serves to be an ancestor of the one it releases, and refuses
// behind_live otherwise (spec 059).
func (t *Tool) release(ctx context.Context, c tools.Call, g target) (tools.Result, error) {
	var ready *session.PublishMeta
	if g.slug != "" {
		ready = c.State.Standing[g.slug]
	}
	if ready == nil {
		return text(tools.OutcomeError, prompts.Text(prompts.PublishNoPreview), nil), nil
	}
	a, err := t.app(ctx, ready.App)
	if err != nil {
		if isNotFound(err) {
			err = fmt.Errorf("the app %s no longer exists at the app host; publish the folder again", ready.App)
		}
		var named *session.PublishMeta
		if g.attached() {
			named = &session.PublishMeta{App: g.slug, Attached: true}
		}
		return failure(ready.App, err, named)
	}
	meta := g.meta(a)
	meta.Commit, meta.Preview = ready.Commit, ready.Preview
	if err := t.onGitHost(a.Repository.PushURL); err != nil {
		return failure(a.Slug, err, meta)
	}
	repoURL := a.Repository.PushURL
	if g.attached() {
		if err := t.onGitHost(g.repo.URL); err != nil {
			return failure(a.Slug, err, meta)
		}
		repoURL = g.repo.URL
	}
	var list struct {
		Releases []hostRelease `json:"releases"`
	}
	if err := t.do(ctx, http.MethodGet, "/apps/"+url.PathEscape(a.Slug)+"/releases", nil, &list); err != nil {
		return failure(a.Slug, err, meta)
	}
	tag := ""
	for _, r := range list.Releases {
		if r.CommitSHA == ready.Commit && (r.Status == session.PublishReleased || r.Status == session.PublishPending) {
			tag = r.Tag
			break
		}
	}
	if tag == "" {
		if live := a.live(); live != "" && live != ready.Commit {
			holds, err := t.holds(ctx, c.Machine, g, a.Slug, repoURL, live, ready.Commit)
			if err != nil {
				return failure(a.Slug, err, meta)
			}
			if !holds {
				meta.Status, meta.Error = session.PublishRefused, &session.PublishError{Code: session.PublishBehindLive}
				return text(tools.OutcomeError, prompts.Render(prompts.PublishBehindLive, prompts.Data{
					"App": a.Slug, "Live": live[:min(len(live), 7)], "Attached": g.attached(), "Dir": g.rel, "URL": repoURL,
				}), meta), nil
			}
		}
		tag = nextTag(list.Releases)
		if err := t.pushTag(ctx, c.Machine, g, a.Slug, repoURL, ready.Commit, tag); err != nil {
			return failure(a.Slug, err, meta)
		}
	}
	meta.Release = tag
	r, done, err := t.waitRelease(ctx, a.Slug, tag)
	if err != nil {
		return failure(a.Slug, err, meta)
	}
	if r.Deploy != nil {
		meta.Deploy = r.Deploy.ID
	}
	switch {
	case !done:
		meta.Status = session.PublishPending
		return text(tools.OutcomeOK, prompts.Render(prompts.PublishPending, prompts.Data{"Tag": tag, "Wait": t.o.Wait.String()}), meta), nil
	case r.Release.Status == session.PublishReleased || r.Release.Status == "superseded":
		meta.Status = session.PublishReleased
		return text(tools.OutcomeOK, prompts.Render(prompts.PublishReleased, prompts.Data{"Tag": tag, "URL": a.URL}), meta), nil
	case r.Release.Status == session.PublishRefused:
		meta.Status = session.PublishRefused
		reason := cmp.Or(deref(r.Release.Reason), "no reason given")
		meta.Error = &session.PublishError{Code: reason}
		return text(tools.OutcomeError, prompts.Render(prompts.PublishRefused, prompts.Data{"Tag": tag, "Reason": reason}), meta), nil
	}
	meta.Status = session.PublishFailed
	e := &session.PublishError{Code: session.PublishFailed}
	if r.Deploy != nil && r.Deploy.Error != nil {
		e = r.Deploy.Error
	}
	meta.Error = e
	return text(tools.OutcomeError, prompts.Render(prompts.PublishReleaseFailed, prompts.Data{"Tag": tag, "Code": e.Code, "Message": e.Message}), meta), nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// versionTag is a release tag this tool counts: v and a number.
var versionTag = regexp.MustCompile(`^v([0-9]{1,9})$`)

// nextTag is one past the highest v<n> among the releases, v1 for none.
func nextTag(releases []hostRelease) string {
	n := 0
	for _, r := range releases {
		if m := versionTag.FindStringSubmatch(r.Tag); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil && v > n {
				n = v
			}
		}
	}
	return "v" + strconv.Itoa(n+1)
}

// The app host's objects, as much of them as the tool reads.
type (
	hostApp struct {
		Slug       string `json:"slug"`
		Name       string `json:"name"`
		URL        string `json:"url"`
		Repository struct {
			PushURL string `json:"push_url"`
		} `json:"repository"`
		// CurrentDeploy is the deploy at the app's address and
		// LatestPreview its newest ready preview, each nil when it has
		// none or the host does not name it (spec 058).
		CurrentDeploy *hostServed `json:"current_deploy"`
		LatestPreview *hostServed `json:"latest_preview"`
	}
	// hostServed is a deploy as the app resource names it, with the
	// commit it serves.
	hostServed struct {
		ID        string `json:"id"`
		CommitSHA string `json:"commit_sha"`
	}
	hostDeploy struct {
		ID         string                `json:"id"`
		Status     string                `json:"status"`
		Preview    bool                  `json:"preview"`
		CommitSHA  string                `json:"commit_sha"`
		PreviewURL string                `json:"preview_url"`
		Error      *session.PublishError `json:"error"`
	}
	hostRelease struct {
		Tag       string  `json:"tag"`
		CommitSHA string  `json:"commit_sha"`
		Status    string  `json:"status"`
		Reason    *string `json:"reason"`
	}
	hostReleaseDetail struct {
		Release hostRelease `json:"release"`
		Deploy  *hostDeploy `json:"deploy"`
	}
)

// hostError is a refusal of the app host in its envelope.
type hostError struct {
	Status  int
	Code    string
	Message string
}

func (e *hostError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("the app host answered %d", e.Status)
	}
	if e.Message == "" {
		return fmt.Sprintf("the app host refused with %s", e.Code)
	}
	return fmt.Sprintf("the app host refused with %s: %s", e.Code, strings.TrimSuffix(e.Message, "."))
}

func isNotFound(err error) bool {
	he, ok := errors.AsType[*hostError](err)
	return ok && he.Status == http.StatusNotFound
}

// bearer is the session's token for the app host.
func (t *Tool) bearer(ctx context.Context) (string, error) {
	c, err := t.tokens.Token(ctx, t.o.Audience, runner.WorkloadSession)
	return c.Value, err
}

// do sends one request to the app host as the session and decodes the
// answer into out, or answers the host's refusal.
func (t *Tool) do(ctx context.Context, method, p string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.o.URL+p, rd)
	if err != nil {
		return err
	}
	token, err := t.bearer(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.o.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("the app host could not be reached: %w", err)
	}
	b, rerr := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if cerr := resp.Body.Close(); rerr == nil {
		rerr = cerr
	}
	if rerr != nil {
		return fmt.Errorf("read the app host's answer: %w", rerr)
	}
	if resp.StatusCode >= 300 {
		he := &hostError{Status: resp.StatusCode}
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(b, &env) == nil {
			he.Code, he.Message = env.Error.Code, env.Error.Message
		}
		return he
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("the app host's answer is not what the tool reads: %w", err)
	}
	return nil
}

// Served is the runner's reader of the commits an app is served at
// (spec 058): the app host's answer for the app, read with the drive's
// token for the app host, its current deploy's commit as Live and its
// newest ready preview's as Preview, each "" when the host names none.
func Served(o Options) func(ctx context.Context, tokens *runner.TokenSource, slug string) (runner.AppCommits, error) {
	return func(ctx context.Context, tokens *runner.TokenSource, slug string) (runner.AppCommits, error) {
		if tokens == nil {
			return runner.AppCommits{}, errors.New("this server mints no credential for its app host")
		}
		a, err := New(o, session.Session{}, tokens).app(ctx, slug)
		if err != nil {
			return runner.AppCommits{}, err
		}
		var c runner.AppCommits
		if d := a.CurrentDeploy; d != nil {
			c.Live = d.CommitSHA
		}
		if d := a.LatestPreview; d != nil {
			c.Preview = d.CommitSHA
		}
		return c, nil
	}
}

// live is the commit the app's address serves, "" when nothing is live
// or the host names no commit.
func (a hostApp) live() string {
	if a.CurrentDeploy == nil {
		return ""
	}
	return a.CurrentDeploy.CommitSHA
}

// app reads one app by its slug.
func (t *Tool) app(ctx context.Context, slug string) (hostApp, error) {
	var a hostApp
	err := t.do(ctx, http.MethodGet, "/apps/"+url.PathEscape(slug), nil, &a)
	return a, err
}

// ensureApp is the session's app: the one the thread last published to,
// or a new one named after the session when there is none or the host no
// longer knows it.
func (t *Tool) ensureApp(ctx context.Context, last *session.PublishMeta) (hostApp, error) {
	if last != nil {
		a, err := t.app(ctx, last.App)
		if err == nil || !isNotFound(err) {
			return a, err
		}
	}
	body := map[string]string{"visibility": "public"}
	if name := strings.TrimSpace(t.s.Title); name != "" {
		body["name"] = name
	}
	var a hostApp
	if err := t.do(ctx, http.MethodPost, "/apps", body, &a); err != nil {
		return hostApp{}, err
	}
	if a.Slug == "" || a.Repository.PushURL == "" {
		return hostApp{}, errors.New("the app host created an app without a slug or a push URL")
	}
	return a, nil
}

// onGitHost refuses a push URL on another host than the git host's,
// since the sandbox's git sends the session's credential there alone.
func (t *Tool) onGitHost(pushURL string) error {
	want, err := url.Parse(t.o.GitURL)
	if err != nil {
		return fmt.Errorf("the git host %q: %w", t.o.GitURL, err)
	}
	got, err := url.Parse(pushURL)
	if err != nil || got.Scheme != want.Scheme || !strings.EqualFold(got.Host, want.Host) {
		return fmt.Errorf("the app's repository %q is not on the git host %s", pushURL, want.Host)
	}
	return nil
}

// gitDir is the session's git directory for the app, in the sandbox's
// own home, outside the workspace.
func gitDir(slug string) string { return `"$HOME/.topos/publish/"` + quote(slug+".git") }

// pushed is what a push left: the commit at the session's branch, and
// whether the call made it.
type pushed struct {
	commit  string
	changed bool
}

// push commits the folder dir as the session's work on the app's
// repository and pushes it to the session's branch. The git directory
// builds on the branch as the git host holds it, so a lost directory or
// a push from another drive never forks it. empty commits even when
// nothing changed.
func (t *Tool) push(ctx context.Context, m machine.Machine, a hostApp, dir string, empty bool) (pushed, error) {
	agent := t.s.Agent.ID + "@" + strconv.Itoa(t.s.Agent.Version)
	msg := "Publish " + path.Base(dir) + "\n\n" + runner.TrailerSession + ": " + t.s.ID + "\n" + runner.TrailerAgent + ": " + agent + "\n"
	force := "0"
	if empty {
		force = "1"
	}
	var b strings.Builder
	w := func(line string) { b.WriteString(line + "\n") }
	w("set -e")
	w("url=" + quote(a.Repository.PushURL))
	w("branch=" + quote(session.Branch(t.s)))
	w("export GIT_DIR=" + gitDir(a.Slug) + " GIT_WORK_TREE=" + quote(dir))
	w(`if [ ! -f "$GIT_DIR/HEAD" ]; then`)
	w(`  mkdir -p "$GIT_DIR"`)
	w(`  git init --quiet`)
	w(`  mkdir -p "$GIT_DIR/info"`)
	w(`  printf 'node_modules/\nlost+found/\n' >"$GIT_DIR/info/exclude"`)
	w(`fi`)
	// The branch as the git host holds it is the base: a directory that
	// lost its history, or one behind the host, starts from it.
	w(`if git fetch --quiet "$url" "+refs/heads/$branch:refs/remotes/published" 2>/dev/null; then`)
	w(`  if ! git rev-parse --verify --quiet HEAD >/dev/null || ! git merge-base --is-ancestor refs/remotes/published HEAD; then`)
	w(`    git reset --quiet --soft refs/remotes/published`)
	w(`  fi`)
	w(`fi`)
	w(`git add --all`)
	w(`changed=1`)
	w(`if git rev-parse --verify --quiet HEAD >/dev/null && git diff --cached --quiet HEAD; then changed=0; fi`)
	w(`if [ "$changed" = 1 ] || [ ` + force + ` = 1 ]; then`)
	w(`  git -c user.name=` + quote(t.s.AgentName()) + ` -c user.email=` + quote(t.s.AgentName()+"@agents.topos.invalid") + ` commit --quiet --allow-empty -m ` + quote(msg))
	w(`  changed=1`)
	w(`fi`)
	w(`git push --quiet "$url" "HEAD:refs/heads/$branch"`)
	w(`printf '%s %s\n' "$changed" "$(git rev-parse HEAD)"`)
	out, err := run(ctx, m, b.String())
	if err != nil {
		return pushed{}, fmt.Errorf("the push of the folder failed: %w", err)
	}
	fields := strings.Fields(lastLine(out))
	if len(fields) != 2 || len(fields[1]) < 7 {
		return pushed{}, fmt.Errorf("the push of the folder answered %q", strings.TrimSpace(out))
	}
	return pushed{commit: fields[1], changed: fields[0] == "1"}, nil
}

// pushCheckout commits the checkout of the attached app g on the
// session's branch and pushes the branch to the repository the session
// was attached, never to a remote the checkout names, which a command in
// the machine may have changed (spec 059). node_modules/ and lost+found/
// are excluded beside the repository's own .gitignore. empty commits
// even when nothing changed.
func (t *Tool) pushCheckout(ctx context.Context, m machine.Machine, g target, empty bool) (pushed, error) {
	agent := t.s.Agent.ID + "@" + strconv.Itoa(t.s.Agent.Version)
	msg := "Publish " + g.slug + "\n\n" + runner.TrailerSession + ": " + t.s.ID + "\n" + runner.TrailerAgent + ": " + agent + "\n"
	force := "0"
	if empty {
		force = "1"
	}
	var b strings.Builder
	w := func(line string) { b.WriteString(line + "\n") }
	w("set -e")
	w("cd " + quote(g.dir))
	w("url=" + quote(g.repo.URL))
	w("branch=" + quote(session.Branch(t.s)))
	w(`exclude=$(git rev-parse --git-path info/exclude)`)
	w(`mkdir -p "$(dirname "$exclude")"`)
	w(`for p in node_modules/ lost+found/; do grep -qxF "$p" "$exclude" 2>/dev/null || printf '%s\n' "$p" >>"$exclude"; done`)
	w(`git add --all`)
	w(`changed=1`)
	w(`if git rev-parse --verify --quiet HEAD >/dev/null && git diff --cached --quiet HEAD; then changed=0; fi`)
	w(`if [ "$changed" = 1 ] || [ ` + force + ` = 1 ]; then`)
	w(`  git -c user.name=` + quote(t.s.AgentName()) + ` -c user.email=` + quote(t.s.AgentName()+"@agents.topos.invalid") + ` commit --quiet --allow-empty -m ` + quote(msg))
	w(`  changed=1`)
	w(`fi`)
	w(`git push --quiet "$url" "HEAD:refs/heads/$branch"`)
	w(`printf '%s %s\n' "$changed" "$(git rev-parse HEAD)"`)
	out, err := run(ctx, m, b.String())
	if err != nil {
		return pushed{}, fmt.Errorf("the push of %s failed: %w", g.rel, err)
	}
	fields := strings.Fields(lastLine(out))
	if len(fields) != 2 || len(fields[1]) < 7 {
		return pushed{}, fmt.Errorf("the push of %s answered %q", g.rel, strings.TrimSpace(out))
	}
	return pushed{commit: fields[1], changed: fields[0] == "1"}, nil
}

// gitPlace is the lines that put a script's git in g's repository: an
// attached app's checkout, or the session's git directory of its own
// app slug, made bare when it is not there.
func gitPlace(g target, slug string) []string {
	if g.attached() {
		return []string{"cd " + quote(g.dir)}
	}
	return []string{"export GIT_DIR=" + gitDir(slug), `if [ ! -f "$GIT_DIR/HEAD" ]; then mkdir -p "$GIT_DIR" && git init --quiet --bare; fi`}
}

// pushTag pushes tag on commit to the app's repository at url, from g's
// repository, which fetches the session's branch first when it no longer
// holds the commit.
func (t *Tool) pushTag(ctx context.Context, m machine.Machine, g target, slug, url, commit, tag string) error {
	var b strings.Builder
	w := func(line string) { b.WriteString(line + "\n") }
	w("set -e")
	for _, l := range gitPlace(g, slug) {
		w(l)
	}
	w("url=" + quote(url))
	w("branch=" + quote(session.Branch(t.s)))
	w("commit=" + quote(commit))
	w(`if ! git cat-file -e "$commit^{commit}" 2>/dev/null; then git fetch --quiet "$url" "+refs/heads/$branch:refs/remotes/published"; fi`)
	w(`git push --quiet "$url" "$commit:refs/tags/"` + quote(tag))
	if _, err := run(ctx, m, b.String()); err != nil {
		return fmt.Errorf("the push of the tag %s failed: %w", tag, err)
	}
	return nil
}

// ancestryMark begins the line the ancestry script answers with.
const ancestryMark = "ancestry "

// holds reports whether commit holds live, the commit the app serves:
// in g's repository it fetches the commit when it is gone, the
// repository's tags and the live commit when no tag brought it, and asks
// git whether live is an ancestor of commit (spec 059). A fetch that
// fails, or an answer of git other than yes or no, is an error with
// git's output.
func (t *Tool) holds(ctx context.Context, m machine.Machine, g target, slug, url, live, commit string) (bool, error) {
	var b strings.Builder
	w := func(line string) { b.WriteString(line + "\n") }
	w("set -e")
	for _, l := range gitPlace(g, slug) {
		w(l)
	}
	w("url=" + quote(url))
	w("branch=" + quote(session.Branch(t.s)))
	w("live=" + quote(live))
	w("commit=" + quote(commit))
	w(`git cat-file -e "$commit^{commit}" 2>/dev/null || git fetch --quiet "$url" "+refs/heads/$branch:refs/remotes/published"`)
	// The tags by refspec, so a repository whose HEAD names no branch, as
	// an app's that only sessions pushed to, still answers.
	w(`git fetch --quiet "$url" "+refs/tags/*:refs/tags/*"`)
	w(`git cat-file -e "$live^{commit}" 2>/dev/null || git fetch --quiet "$url" "$live"`)
	w("set +e")
	w(`git merge-base --is-ancestor "$live" "$commit"`)
	w(`printf '` + ancestryMark + `%s\n' "$?"`)
	out, err := run(ctx, m, b.String())
	if err != nil {
		return false, fmt.Errorf("the check of the live release %s failed: %w", live, err)
	}
	switch strings.TrimSpace(lastLine(out)) {
	case ancestryMark + "0":
		return true, nil
	case ancestryMark + "1":
		return false, nil
	}
	return false, fmt.Errorf("the check of the live release %s answered: %s", live, strings.TrimSpace(tail(out, 20)))
}

// run runs a script in the machine and answers its output, or why it
// failed with the output's end.
func run(ctx context.Context, m machine.Machine, script string) (string, error) {
	res, err := m.Exec(ctx, machine.ExecRequest{Command: script, Timeout: gitTimeout})
	switch {
	case err != nil:
		return "", err
	case res.TimedOut:
		return "", fmt.Errorf("it passed its timeout of %s", gitTimeout)
	case res.ExitCode != 0:
		return "", fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(tail(string(res.Output), 20)))
	}
	return string(res.Output), nil
}

// waitDeploy reads the app's deploys until the preview deploy of commit
// is done or the wait passes. found reports a deploy of the commit, and
// done that it is ready, failed or canceled.
func (t *Tool) waitDeploy(ctx context.Context, slug, commit string) (d hostDeploy, found, done bool, err error) {
	deadline := time.Now().Add(t.o.Wait)
	for {
		var list struct {
			Deploys []hostDeploy `json:"deploys"`
		}
		if err := t.do(ctx, http.MethodGet, "/apps/"+url.PathEscape(slug)+"/deploys", nil, &list); err != nil {
			return hostDeploy{}, false, false, err
		}
		for _, x := range list.Deploys {
			if x.Preview && x.CommitSHA == commit {
				d, found = x, true
				break
			}
		}
		if found {
			switch d.Status {
			case session.PublishReady, "live":
				d.Status = session.PublishReady
				return d, true, true, nil
			case session.PublishFailed, session.PublishCanceled:
				return d, true, true, nil
			}
		}
		if !sleep(ctx, deadline, t.o.PollEvery) {
			return d, found, false, ctx.Err()
		}
	}
}

// waitRelease reads the release of tag until it is released, refused or
// failed, or the wait passes. A tag the host has not seen yet is not
// found, and read again.
func (t *Tool) waitRelease(ctx context.Context, slug, tag string) (r hostReleaseDetail, done bool, err error) {
	deadline := time.Now().Add(t.o.Wait)
	for {
		err := t.do(ctx, http.MethodGet, "/apps/"+url.PathEscape(slug)+"/releases/"+url.PathEscape(tag), nil, &r)
		switch {
		case err == nil && r.Release.Status != "" && r.Release.Status != session.PublishPending:
			return r, true, nil
		case err != nil && !isNotFound(err):
			return r, false, err
		}
		if !sleep(ctx, deadline, t.o.PollEvery) {
			return r, false, ctx.Err()
		}
	}
}

// sleep waits every, and reports false once the deadline has passed
// or ctx ended; an error of ctx is the caller's to read.
func sleep(ctx context.Context, deadline time.Time, every time.Duration) bool {
	if !time.Now().Add(every).Before(deadline) {
		return false
	}
	timer := time.NewTimer(every)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// logTail is the last LogTail lines of a deploy's build log, or a line
// saying why it could not be read.
func (t *Tool) logTail(ctx context.Context, slug, id string) string {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.o.URL+"/apps/"+url.PathEscape(slug)+"/deploys/"+url.PathEscape(id)+"/logs", nil)
	if err != nil {
		return "[the build log could not be read: " + err.Error() + "]"
	}
	token, err := t.bearer(ctx)
	if err != nil {
		return "[the build log could not be read: " + err.Error() + "]"
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := t.o.HTTP.Do(req)
	if err != nil {
		return "[the build log could not be read: " + err.Error() + "]"
	}
	b, rerr := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if cerr := resp.Body.Close(); rerr == nil {
		rerr = cerr
	}
	switch {
	case rerr != nil:
		return "[the build log could not be read: " + rerr.Error() + "]"
	case resp.StatusCode != http.StatusOK:
		return fmt.Sprintf("[the build log could not be read: the app host answered %d]", resp.StatusCode)
	}
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var l struct {
			Line string `json:"line"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		lines = append(lines, strings.TrimRight(l.Line, "\n"))
		if len(lines) > LogTail {
			lines = lines[1:]
		}
	}
	if err := sc.Err(); err != nil {
		return "[the build log could not be read: " + err.Error() + "]"
	}
	return strings.Join(lines, "\n")
}

// quote is s as one word of the shell.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// lastLine is the last line of s that holds anything.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

// tail is the last n lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}
