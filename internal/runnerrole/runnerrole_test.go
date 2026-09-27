// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runnerrole

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/internal/runnerapi"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/host"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/dialect"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

type fixture struct {
	st     session.Store
	server *runnerapi.Server
	url    string
	client *Client
}

func setup(t *testing.T, ttl time.Duration) fixture {
	t.Helper()
	st := session.NewMemoryStore()
	srv, err := runnerapi.New(runnerapi.Options{Store: st, Queue: runner.NewQueue(st, 10*time.Millisecond), Tokens: []string{"old", "runner-token"}, TTL: ttl})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	c, err := New(hs.URL, "runner-token", hs.Client())
	if err != nil {
		t.Fatal(err)
	}
	return fixture{st: st, server: srv, url: hs.URL, client: c}
}

func hosted(t *testing.T, st session.Store, text string) session.Session {
	t.Helper()
	s := session.New(session.AgentRef{ID: session.NewID(session.PrefixAgent), Name: "builder", Version: 1},
		session.Sender{Subject: "usr_ada", Kind: session.SenderPerson}, session.RunnerHosted, session.Machine{Kind: machine.KindHost}, time.Now())
	if err := st.Create(t.Context(), s, nil); err != nil {
		t.Fatal(err)
	}
	e, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: text}}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	batch := []session.Event{e}
	session.Stamp(s.ID, 0, batch)
	if _, err := st.Append(t.Context(), s.ID, 0, batch); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestARemoteRunnerRunsASession: a runner in the runner role claims a
// session over the internal routes, drives its turn through the client,
// writing only through its claim's lease, and releases it.
func TestARemoteRunnerRunsASession(t *testing.T) {
	f := setup(t, 0)
	s := hosted(t, f.st, "Go.")
	stub := luxstub.New(t)
	stub.Script("builder-model", luxstub.Reply{Response: ir.Response{Model: "builder-model", Blocks: []ir.Block{{Type: ir.BlockText, Text: "done remotely"}}, StopReason: ir.StopEndTurn}})
	base := t.TempDir()
	r, err := runner.New(runner.Options{Store: f.client, ID: "run_remote", Kind: runner.KindRunner,
		Harness: func(ctx context.Context, s session.Session) (harness.Config, error) {
			m, err := host.Open(host.Options{Workdir: base, SpillDir: filepath.Join(base, ".spill"), Environ: []string{"PATH=" + os.Getenv("PATH")}})
			if err != nil {
				return harness.Config{}, err
			}
			return harness.Config{
				Model: &dialect.Model{}, Connection: models.Connection{BaseURL: stub.URL() + "/anthropic", Model: "builder-model", Family: models.FamilyAnthropic},
				Entry: models.Entry{InputWindow: 100_000, MaxOutputTokens: 8_000}, Machine: m, Tools: tools.NewRegistry(),
				Sleep: func(context.Context, time.Duration) error { return nil },
			}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Serve(ctx, f.client, 1, func(id string, err error) { t.Errorf("%s: %v", id, err) }) }()
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		got, err := f.st.Get(t.Context(), s.ID)
		if err == nil && got.Status == session.StatusIdle && got.StopReason == session.StopEndTurn {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the remote runner never answered: %+v, %v", got, err)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	evs, err := f.st.Events(t.Context(), s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var runnerKind string
	for _, e := range evs {
		var p session.SessionStatus
		if e.Type == session.TypeSessionStatus && e.Decode(&p) == nil && p.Runner != nil {
			runnerKind = p.Runner.Kind
		}
	}
	if runnerKind != runner.KindRunner {
		t.Fatalf("the session ran on a %q runner", runnerKind)
	}
	if lease, err := f.st.Acquire(t.Context(), s.ID, session.Holder{Runner: "after"}); err != nil {
		t.Fatalf("the claim was not released: %v", err)
	} else if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestTheProtocolsLeases: a renew and an append carry the claim's
// generation and answer lease_lost for any other; a release ends the
// claim; the reaper frees a claim whose runner stopped renewing.
func TestTheProtocolsLeases(t *testing.T) {
	f := setup(t, 300*time.Millisecond)
	f.client.SetRenewInterval(time.Hour)
	s := hosted(t, f.st, "Go.")
	claims, err := f.client.Claim(t.Context(), session.Holder{Runner: "run_a"}, 2, time.Second)
	if err != nil || len(claims) != 1 || claims[0].ID != s.ID {
		t.Fatalf("claim %+v, %v", claims, err)
	}
	l := claims[0].Lease.(*lease)
	if err := l.Renew(t.Context()); err != nil {
		t.Fatalf("a renew: %v", err)
	}
	e, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusRunning, Runner: &session.RunnerRef{ID: "run_a", Kind: runner.KindRunner}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	batch := []session.Event{e}
	session.Stamp(s.ID, 1, batch)
	if last, err := l.Append(t.Context(), 1, batch); err != nil || last != 2 {
		t.Fatalf("an append under the claim: %d, %v", last, err)
	}
	if _, err := l.Append(t.Context(), 1, batch); err != nil {
		t.Fatalf("a retried append is the same append: %v", err)
	}
	stale := &lease{c: f.client, id: s.ID, gen: l.gen + 99, lost: make(chan struct{}), stop: make(chan struct{})}
	if err := stale.Renew(t.Context()); !errors.Is(err, session.ErrLeaseLost) {
		t.Fatalf("a renew at another generation: %v", err)
	}
	if _, err := stale.Append(t.Context(), 2, batch); !errors.Is(err, session.ErrLeaseLost) {
		t.Fatalf("an append at another generation: %v", err)
	}
	if err := stale.Release(); err != nil {
		t.Fatalf("a release of a lost lease is not an error: %v", err)
	}
	if err := l.Release(); err != nil || l.Release() != nil {
		t.Fatalf("release: %v", err)
	}
	select {
	case <-l.Lost():
	default:
		t.Fatal("a released lease is not lost")
	}

	// A runner that stops renewing loses the claim to the reaper.
	appendTo := func(text string) {
		cur, err := f.st.Get(t.Context(), s.ID)
		if err != nil {
			t.Fatal(err)
		}
		ev, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Content: []lux.Block{{Type: ir.BlockText, Text: text}}}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		b := []session.Event{ev}
		session.Stamp(s.ID, cur.LastSeq, b)
		if _, err := f.st.Append(t.Context(), s.ID, cur.LastSeq, b); err != nil {
			t.Fatal(err)
		}
	}
	appendTo("More.")
	claims, err = f.client.Claim(t.Context(), session.Holder{Runner: "run_a"}, 1, time.Second)
	if err != nil || len(claims) != 1 {
		t.Fatalf("a second claim %+v, %v", claims, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go f.server.Reap(ctx, 50*time.Millisecond)
	time.Sleep(600 * time.Millisecond)
	if err := claims[0].Lease.Renew(t.Context()); !errors.Is(err, session.ErrLeaseLost) {
		t.Fatalf("a renew past the claim's lifetime: %v", err)
	}
	next, err := f.client.Claim(t.Context(), session.Holder{Runner: "run_b"}, 1, time.Second)
	if err != nil || len(next) != 1 {
		t.Fatalf("another runner's claim after the reap: %+v, %v", next, err)
	}
	cancel()
}

// TestTheProtocolsReads: a session, its events, its stream and its blobs
// read through the client as the store holds them; the calls a runner
// does not make are refused.
func TestTheProtocolsReads(t *testing.T) {
	f := setup(t, 0)
	s := hosted(t, f.st, "Go.")
	got, err := f.client.Get(t.Context(), s.ID)
	if err != nil || got.ID != s.ID || got.LastSeq != 1 {
		t.Fatalf("Get %+v, %v", got, err)
	}
	if _, err := f.client.Get(t.Context(), session.NewID(session.PrefixSession)); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("a missing session: %v", err)
	}
	evs, err := f.client.Events(t.Context(), s.ID, 1, 1)
	if err != nil || len(evs) != 1 || evs[0].Type != session.TypeUserMessage {
		t.Fatalf("Events %+v, %v", evs, err)
	}
	d, err := f.client.PutBlob(t.Context(), s.ID, strings.NewReader("blob body"))
	if err != nil || d != session.DigestOf([]byte("blob body")) {
		t.Fatalf("PutBlob %s, %v", d, err)
	}
	rc, err := f.client.Blob(t.Context(), s.ID, d)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	if err := errors.Join(err, rc.Close()); err != nil || string(b) != "blob body" {
		t.Fatalf("Blob %q, %v", b, err)
	}
	if _, err := f.client.Blob(t.Context(), s.ID, session.DigestOf([]byte("none"))); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("a missing blob: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	ch, err := f.client.Watch(ctx, s.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if e := <-ch; e.Seq != 1 {
		t.Fatalf("the stream's first event %+v", e)
	}
	cancel()
	for range ch {
	}
	for name, err := range map[string]error{
		"append": func() error { _, err := f.client.Append(t.Context(), s.ID, 1, nil); return err }(),
		"create": f.client.Create(t.Context(), s, nil),
		"list":   func() error { _, _, err := f.client.List(t.Context(), session.ListOptions{}); return err }(),
		"redact": f.client.Redact(t.Context(), s.ID, "evt_x", session.Sender{}, ""),
		"acquire": func() error {
			_, err := f.client.Acquire(t.Context(), s.ID, session.Holder{})
			return err
		}(),
		"delete": f.client.Delete(t.Context(), s.ID),
	} {
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestTheRoutesNeedTheRunnerToken: a request without a bearer, or with
// one TOPOS_RUNNER_TOKEN does not name, is runner_unauthorized; every
// listed bearer is accepted.
func TestTheRoutesNeedTheRunnerToken(t *testing.T) {
	f := setup(t, 0)
	for _, tok := range []string{"", "wrong"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.url+runnerapi.Root+"/claims", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		if err := errors.Join(err, resp.Body.Close()); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(b), runnerapi.CodeRunnerUnauthorized) {
			t.Fatalf("bearer %q: %d %s", tok, resp.StatusCode, b)
		}
	}
	old, err := New(f.url, "old", nil)
	if err != nil {
		t.Fatal(err)
	}
	if claims, err := old.Claim(t.Context(), session.Holder{Runner: "run_a"}, 1, 0); err != nil || len(claims) != 0 {
		t.Fatalf("the older token: %+v, %v", claims, err)
	}
	if _, err := New("", "t", nil); err == nil {
		t.Fatal("a client with no URL")
	}
}

// TestTheProtocolsRefusals: a malformed claim, a blob that does not hash
// to its path, and a request past the body bound are refused with the
// protocol's codes.
func TestTheProtocolsRefusals(t *testing.T) {
	f := setup(t, 0)
	s := hosted(t, f.st, "Go.")
	post := func(path, body string) (int, string) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.url+runnerapi.Root+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer runner-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		if err := errors.Join(err, resp.Body.Close()); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(b)
	}
	for body, want := range map[string]int{
		`{"runner":"","capacity":1}`:                http.StatusBadRequest,
		`{"runner":"a","capacity":1,"wait":"soon"}`: http.StatusBadRequest,
		`{"runner":"a","capacity":1,"extra":1}`:     http.StatusBadRequest,
	} {
		if code, b := post("/claims", body); code != want {
			t.Errorf("%s: %d %s", body, code, b)
		}
	}
	if code, _ := post("/sessions/"+s.ID+"/events", `{"generation":1,"after_seq":0,"events":[]}`); code != http.StatusConflict {
		t.Fatalf("an append without a claim: %d", code)
	}
	if code, _ := post("/leases/"+s.ID+"/release", `{"generation":1}`); code != http.StatusConflict {
		t.Fatalf("a release without a claim: %d", code)
	}
	if code, _ := post("/claims", `{"runner":"a","capacity":1,"x":"`+strings.Repeat("x", 9<<20)+`"}`); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a body past the bound: %d", code)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, f.url+runnerapi.Root+"/sessions/"+s.ID+"/blobs/"+string(session.DigestOf([]byte("other"))), strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer runner-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a blob that does not hash to its path: %d, %v", resp.StatusCode, err)
	}
	if _, err := f.client.Events(t.Context(), s.ID, 1, -1); err != nil {
		t.Fatalf("a negative limit reads every event: %v", err)
	}
	for _, q := range []string{"from_seq=x", "from_seq=1&limit=x"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.url+runnerapi.Root+"/sessions/"+s.ID+"/events?"+q, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer runner-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil || resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d, %v", q, resp.StatusCode, err)
		}
	}
	if _, err := runnerapi.New(runnerapi.Options{Store: f.st, Queue: runner.NewQueue(f.st, 0)}); err == nil {
		t.Fatal("a server with no token")
	}
	if _, err := runnerapi.New(runnerapi.Options{Tokens: []string{"t"}}); err == nil {
		t.Fatal("a server with no store")
	}
}

// wrapQueue hands out the inner queue's leases wrapped.
type wrapQueue struct {
	inner runner.Claimer
	wrap  func(session.Lease) session.Lease
}

func (q wrapQueue) Claim(ctx context.Context, h session.Holder, n int, wait time.Duration) ([]runner.Claim, error) {
	claims, err := q.inner.Claim(ctx, h, n, wait)
	for i := range claims {
		claims[i].Lease = q.wrap(claims[i].Lease)
	}
	return claims, err
}

// endable is a store lease the test can end or fail.
type endable struct {
	session.Lease
	lost     chan struct{}
	renewErr error
}

func (l *endable) Lost() <-chan struct{} { return l.lost }
func (l *endable) Renew(ctx context.Context) error {
	return errors.Join(l.renewErr, l.Lease.Renew(ctx))
}

// fencing is a store lease whose store fences appends.
type fencing struct {
	session.Lease
	st     session.Store
	id     string
	calls  *int
	refuse bool
}

func (l *fencing) Append(ctx context.Context, after uint64, evs []session.Event) (uint64, error) {
	*l.calls++
	if l.refuse {
		return 0, session.ErrLeaseLost
	}
	return l.st.Append(ctx, l.id, after, evs)
}

// failing is a store whose event reads fail.
type failing struct{ session.Store }

func (failing) Events(context.Context, string, uint64, int) ([]session.Event, error) {
	return nil, errors.New("disk")
}

func serveWith(t *testing.T, st session.Store, q runner.Claimer) *Client {
	t.Helper()
	srv, err := runnerapi.New(runnerapi.Options{Store: st, Queue: q, Tokens: []string{"runner-token"}})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	c, err := New(hs.URL, "runner-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func status(t *testing.T, id string, after uint64) []session.Event {
	t.Helper()
	e, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusRunning, Runner: &session.RunnerRef{ID: "r", Kind: runner.KindRunner}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b := []session.Event{e}
	session.Stamp(id, after, b)
	return b
}

// TestTheServerFollowsTheStoresLease: a claim whose store lease ended
// answers lease_lost to its renew; a store that fences carries the
// runner's appends, and its refusal is lease_lost; a conflicting or
// malformed batch and a failing store answer their codes.
func TestTheServerFollowsTheStoresLease(t *testing.T) {
	st := session.NewMemoryStore()
	var ended *endable
	c := serveWith(t, st, wrapQueue{runner.NewQueue(st, time.Hour), func(l session.Lease) session.Lease {
		ended = &endable{Lease: l, lost: make(chan struct{})}
		return ended
	}})
	a := hosted(t, st, "Go.")
	claims, err := c.Claim(t.Context(), session.Holder{Runner: "run_a"}, 1, 0)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim %v, %v", claims, err)
	}
	l := claims[0].Lease.(*lease)
	if _, err := l.Append(t.Context(), 0, status(t, a.ID, 0)); !errors.Is(err, session.ErrSequenceConflict) {
		t.Fatalf("a batch behind the log: %v", err)
	}
	bad := status(t, a.ID, 1)
	bad[0].ID = "evt_x"
	if _, err := l.Append(t.Context(), 1, bad); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("a malformed batch: %v", err)
	}
	ended.renewErr = errors.New("the store dropped it")
	if err := l.Renew(t.Context()); !errors.Is(err, session.ErrLeaseLost) {
		t.Fatalf("a renew the store refuses: %v", err)
	}

	b := hosted(t, st, "Go.")
	claims, err = c.Claim(t.Context(), session.Holder{Runner: "run_a"}, 1, 0)
	if err != nil || len(claims) != 1 || claims[0].ID != b.ID {
		t.Fatalf("claim %v, %v", claims, err)
	}
	close(ended.lost)
	if err := claims[0].Lease.Renew(t.Context()); !errors.Is(err, session.ErrLeaseLost) {
		t.Fatalf("a renew after the store's lease ended: %v", err)
	}

	calls := 0
	var fenced *fencing
	fc := serveWith(t, st, wrapQueue{runner.NewQueue(st, time.Hour), func(l session.Lease) session.Lease {
		fenced = &fencing{Lease: l, st: st, id: "", calls: &calls}
		return fenced
	}})
	d := hosted(t, st, "Go.")
	claims, err = fc.Claim(t.Context(), session.Holder{Runner: "run_a"}, 1, 0)
	if err != nil || len(claims) != 1 || claims[0].ID != d.ID {
		t.Fatalf("claim %v, %v", claims, err)
	}
	fenced.id = d.ID
	fl := claims[0].Lease.(session.Fence)
	if _, err := fl.Append(t.Context(), 1, status(t, d.ID, 1)); err != nil || calls != 1 {
		t.Fatalf("an append through the store's fence: %v, %d calls", err, calls)
	}
	fenced.refuse = true
	if _, err := fl.Append(t.Context(), 2, status(t, d.ID, 2)); !errors.Is(err, session.ErrLeaseLost) {
		t.Fatalf("the fence's refusal: %v", err)
	}

	broken := serveWith(t, failing{st}, runner.NewQueue(st, time.Hour))
	if _, err := broken.Events(t.Context(), d.ID, 1, 0); err == nil || errors.Is(err, session.ErrNotFound) {
		t.Fatalf("a failing store: %v", err)
	}
	if _, err := c.Watch(t.Context(), session.NewID(session.PrefixSession), 1); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("a stream of no session: %v", err)
	}
	if _, err := c.PutBlob(t.Context(), session.NewID(session.PrefixSession), strings.NewReader("x")); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("a blob of no session: %v", err)
	}
}

// TestALeaseRenewsItself: a claim's lease renews on its interval, and
// stops once the server answers lease_lost.
func TestALeaseRenewsItself(t *testing.T) {
	f := setup(t, 200*time.Millisecond)
	f.client.SetRenewInterval(20 * time.Millisecond)
	hosted(t, f.st, "Go.")
	claims, err := f.client.Claim(t.Context(), session.Holder{Runner: "run_a"}, 1, 0)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim %v, %v", claims, err)
	}
	time.Sleep(400 * time.Millisecond)
	if err := claims[0].Lease.Renew(t.Context()); err != nil {
		t.Fatalf("a self-renewing lease past its lifetime: %v", err)
	}
	stale := &lease{c: f.client, id: claims[0].ID, gen: 999, lost: make(chan struct{}), stop: make(chan struct{})}
	go stale.keep(t.Context())
	select {
	case <-stale.Lost():
	case <-time.After(5 * time.Second):
		t.Fatal("a renew loop never saw lease_lost")
	}
	if err := claims[0].Lease.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestReadError(t *testing.T) {
	for body, want := range map[string]error{
		`{"error":{"code":"sequence_conflict","message":"m"}}`: session.ErrSequenceConflict,
		`{"error":{"code":"invalid_request","message":"m"}}`:   session.ErrInvalid,
	} {
		rec := httptest.NewRecorder()
		rec.WriteHeader(http.StatusConflict)
		_, _ = rec.WriteString(body)
		if err := readError(rec.Result()); !errors.Is(err, want) {
			t.Errorf("%s: %v", body, err)
		}
	}
	for _, body := range []string{`{"error":{"code":"internal","message":"m"}}`, `not json`} {
		rec := httptest.NewRecorder()
		rec.WriteHeader(http.StatusInternalServerError)
		_, _ = rec.WriteString(body)
		if err := readError(rec.Result()); err == nil || errors.Is(err, session.ErrInvalid) {
			t.Errorf("%s: %v", body, err)
		}
	}
}
