// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package runnerrole is the runner role's side of the runner protocol
// (spec 016): a session.Store and a runner.Claimer over a toposd's
// internal routes, so the runner package drives a session the same way
// in every role. A claim's lease renews itself every quarter of its
// lifetime and fences every append by its generation; the log is written
// only through it.
package runnerrole

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"latere.ai/x/pkg/httpjson"
	"latere.ai/x/pkg/otel"

	"latere.ai/x/topos/internal/runnerapi"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// ErrUnsupported is a store operation a runner does not make.
var ErrUnsupported = errors.New("runnerrole: a runner does not make this call")

// Client talks to one toposd's internal listener.
type Client struct {
	base  string
	token string
	http  *http.Client
	// renew is how often a claim's lease is renewed; a quarter of the
	// server's lifetime.
	renew time.Duration
	log   *slog.Logger

	mu sync.Mutex
	// leases are the live claims by session, whose generation a delta of
	// the session is sent under.
	leases map[string]*lease
	// deltas wait for the one goroutine that sends them, which runs while
	// any wait.
	deltas  []runnerapi.RunnerDelta
	sending bool
	dropped atomic.Uint64
}

// deltaQueue is how many live deltas wait to be sent at most; the next
// one is dropped until the sender catches up.
const deltaQueue = 1024

// deltaTimeout bounds one send of deltas, so a server that does not
// answer drops them instead of holding the ones behind them.
const deltaTimeout = 5 * time.Second

// New returns the client of the internal listener at base, TOPOS_INTERNAL_URL,
// presenting token, the first of TOPOS_RUNNER_TOKEN.
func New(base, token string, hc *http.Client) (*Client, error) {
	if base == "" || token == "" {
		return nil, errors.New("runnerrole: the internal URL and the runner token are required")
	}
	if hc == nil {
		hc = otel.HTTPClient()
	}
	return &Client{base: strings.TrimRight(base, "/") + runnerapi.Root, token: token, http: hc, renew: runnerapi.DefaultTTL / 4,
		log: slog.New(slog.DiscardHandler), leases: map[string]*lease{}}, nil
}

// SetRenewInterval sets how often a claim's lease is renewed.
func (c *Client) SetRenewInterval(d time.Duration) { c.renew = d }

// SetLog sets where dropped live deltas are logged, at debug level.
func (c *Client) SetLog(l *slog.Logger) { c.log = l }

// PublishDelta sends a live delta of a session the runner holds to the
// server, which publishes it to the streams that follow the session
// (spec 016). It never waits: deltas queue for one sender, which sends
// all that wait in one request, and a delta of a session the runner
// holds no claim on, past the queue, or of a send that failed is
// dropped, counted and logged.
func (c *Client) PublishDelta(id string, d session.Delta) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.leases[id]
	switch {
	case !ok:
		c.drop(1, "the runner holds no claim on the session", nil)
		return
	case len(c.deltas) >= deltaQueue:
		c.drop(1, "the send queue is full", nil)
		return
	}
	c.deltas = append(c.deltas, runnerapi.RunnerDelta{SessionID: id, Generation: l.gen, Delta: d})
	if !c.sending {
		c.sending = true
		go c.sendDeltas()
	}
}

// sendDeltas sends what waits until nothing does.
func (c *Client) sendDeltas() {
	for {
		c.mu.Lock()
		batch := c.deltas
		c.deltas = nil
		if len(batch) == 0 {
			c.sending = false
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), deltaTimeout)
		err := c.json(ctx, http.MethodPost, "/deltas", runnerapi.DeltasRequest{Deltas: batch}, nil)
		cancel()
		if err != nil {
			c.drop(len(batch), "the send failed", err)
		}
	}
}

// drop counts n deltas that were not sent and logs why.
func (c *Client) drop(n int, why string, err error) {
	c.dropped.Add(uint64(n))
	c.log.Debug("dropped live deltas", "deltas", n, "reason", why, "err", err)
}

// DroppedDeltas is how many live deltas the client did not send.
func (c *Client) DroppedDeltas() uint64 { return c.dropped.Load() }

var _ session.DeltaPublisher = (*Client)(nil)

// hold records a live claim, which a later claim of its session
// replaces.
func (c *Client) hold(l *lease) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.leases[l.id] = l
}

// forget removes a claim that ended, unless a later one replaced it.
func (c *Client) forget(l *lease) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.leases[l.id] == l {
		delete(c.leases, l.id)
	}
}

// do sends one request and hands the answer's body to read, or turns
// an error envelope into the error the runner package reads. The body is
// closed when do returns.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string, read func(io.Reader) error) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	// The body's close error is the connection's, and no answer is lost.
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return readError(resp)
	}
	if read == nil {
		return nil
	}
	return read(resp.Body)
}

func (c *Client) json(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	var read func(io.Reader) error
	if out != nil {
		read = func(r io.Reader) error {
			if err := json.NewDecoder(r).Decode(out); err != nil {
				return fmt.Errorf("runnerrole: %s %s: %w", method, path, err)
			}
			return nil
		}
	}
	return c.do(ctx, method, path, body, "application/json", read)
}

// readError maps the protocol's codes onto the session package's errors.
func readError(resp *http.Response) error {
	var env httpjson.ErrorEnvelope
	b, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if rerr != nil || json.Unmarshal(b, &env) != nil {
		return fmt.Errorf("runnerrole: HTTP %d", resp.StatusCode)
	}
	msg := env.Error.Message
	switch env.Error.Code {
	case runnerapi.CodeLeaseLost:
		return fmt.Errorf("%w: %s", session.ErrLeaseLost, msg)
	case runnerapi.CodeSequenceConflict:
		return fmt.Errorf("%w: %s", session.ErrSequenceConflict, msg)
	case runnerapi.CodeNotFound:
		return fmt.Errorf("%w: %s", session.ErrNotFound, msg)
	case runnerapi.CodeInvalidRequest:
		return fmt.Errorf("%w: %s", session.ErrInvalid, msg)
	case runnerapi.CodeNotMinted:
		return fmt.Errorf("%w: %s", runner.ErrNotMinted, msg)
	case runnerapi.CodeCredentialRefused:
		code, _ := env.Error.Details["code"].(string)
		detail, _ := env.Error.Details["detail"].(string)
		cause := errors.New(detail)
		if code == runner.CodeAgentIdentityMissing {
			cause = fmt.Errorf("%w: %s", runner.ErrNoIdentity, detail)
		}
		return &runner.SetupError{Code: cmp.Or(code, runnerapi.CodeCredentialRefused), Err: cause}
	}
	return fmt.Errorf("runnerrole: HTTP %d %s: %s", resp.StatusCode, env.Error.Code, msg)
}

func sessionPath(id string) string { return "/sessions/" + url.PathEscape(id) }

// Get reads a session.
func (c *Client) Get(ctx context.Context, id string) (session.Session, error) {
	var s session.Session
	return s, c.json(ctx, http.MethodGet, sessionPath(id), nil, &s)
}

// Events reads a session's events from fromSeq, at most limit when it is
// above zero.
func (c *Client) Events(ctx context.Context, id string, fromSeq uint64, limit int) ([]session.Event, error) {
	q := url.Values{"from_seq": {strconv.FormatUint(max(fromSeq, 1), 10)}}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var evs []session.Event
	return evs, c.json(ctx, http.MethodGet, sessionPath(id)+"/events?"+q.Encode(), nil, &evs)
}

// Watch follows a session's events from fromSeq until ctx ends or the
// server hangs up. The stream is read on its own goroutine; an answer
// that is no stream is the error Watch returns.
func (c *Client) Watch(ctx context.Context, id string, fromSeq uint64) (<-chan session.Event, error) {
	out := make(chan session.Event)
	opened := make(chan error, 1)
	go func() {
		defer close(out)
		streaming := false
		err := c.do(ctx, http.MethodGet, sessionPath(id)+"/stream?from_seq="+strconv.FormatUint(max(fromSeq, 1), 10), nil, "", func(r io.Reader) error {
			streaming = true
			opened <- nil
			sc := bufio.NewScanner(r)
			sc.Buffer(make([]byte, 1<<20), 16<<20)
			for sc.Scan() {
				var e session.Event
				if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
					return err
				}
				select {
				case out <- e:
				case <-ctx.Done():
					return nil
				}
			}
			return sc.Err()
		})
		if !streaming {
			opened <- err
		}
	}()
	if err := <-opened; err != nil {
		return nil, err
	}
	return out, nil
}

// PutBlob stores a blob under the digest of its bytes.
func (c *Client) PutBlob(ctx context.Context, id string, r io.Reader) (session.Digest, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	d := session.DigestOf(b)
	if err := c.do(ctx, http.MethodPut, sessionPath(id)+"/blobs/"+string(d), bytes.NewReader(b), "application/octet-stream", nil); err != nil {
		return "", err
	}
	return d, nil
}

// Blob reads a blob, whole: a session's blobs are its agent bundle and
// its captured bodies, each small enough to hold.
func (c *Client) Blob(ctx context.Context, id string, d session.Digest) (io.ReadCloser, error) {
	var buf bytes.Buffer
	err := c.do(ctx, http.MethodGet, sessionPath(id)+"/blobs/"+string(d), nil, "", func(r io.Reader) error {
		_, err := io.Copy(&buf, r)
		return err
	})
	if err != nil {
		return nil, err
	}
	return io.NopCloser(&buf), nil
}

// Append is refused: a runner writes a session only through its claim's
// lease, which fences the batch by its generation.
func (c *Client) Append(context.Context, string, uint64, []session.Event) (uint64, error) {
	return 0, fmt.Errorf("%w: append through the claim's lease", ErrUnsupported)
}

// Create is refused.
func (c *Client) Create(context.Context, session.Session, map[session.Digest][]byte) error {
	return ErrUnsupported
}

// List is refused.
func (c *Client) List(context.Context, session.ListOptions) ([]session.Session, string, error) {
	return nil, "", ErrUnsupported
}

// Redact is refused.
func (c *Client) Redact(context.Context, string, string, session.Sender, string) error {
	return ErrUnsupported
}

// Acquire is refused: a runner's leases come from its claims.
func (c *Client) Acquire(context.Context, string, session.Holder) (session.Lease, error) {
	return nil, fmt.Errorf("%w: claim the session instead", ErrUnsupported)
}

// Delete is refused.
func (c *Client) Delete(context.Context, string) error { return ErrUnsupported }

var _ session.Store = (*Client)(nil)

// Claim asks the server for sessions with work.
func (c *Client) Claim(ctx context.Context, holder session.Holder, n int, wait time.Duration) ([]runner.Claim, error) {
	var got []runnerapi.Claimed
	err := c.json(ctx, http.MethodPost, "/claims", runnerapi.ClaimRequest{Runner: holder.Runner, Capacity: n, Wait: wait.String()}, &got)
	if err != nil {
		return nil, err
	}
	out := make([]runner.Claim, len(got))
	for i, g := range got {
		l := &lease{c: c, id: g.SessionID, gen: g.Generation, lost: make(chan struct{}), stop: make(chan struct{})}
		c.hold(l)
		// The lease outlives the claim's call; its renewals end with it.
		go l.keep(context.WithoutCancel(ctx))
		out[i] = runner.Claim{ID: g.SessionID, Lease: l}
	}
	return out, nil
}

var _ runner.Claimer = (*Client)(nil)

// lease is a claim's lease on the server.
type lease struct {
	c    *Client
	id   string
	gen  int64
	lost chan struct{}
	stop chan struct{}
	once sync.Once
	mu   sync.Mutex
	done bool
}

// keep renews the lease until it is released or lost.
func (l *lease) keep(ctx context.Context) {
	t := time.NewTicker(l.c.renew)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			if err := l.Renew(ctx); errors.Is(err, session.ErrLeaseLost) {
				return
			}
		}
	}
}

func (l *lease) end() {
	l.once.Do(func() {
		l.c.forget(l)
		close(l.lost)
	})
}

// Renew extends the lease; lease_lost ends it.
func (l *lease) Renew(ctx context.Context) error {
	var r runnerapi.Renewed
	err := l.c.json(ctx, http.MethodPost, "/leases/"+url.PathEscape(l.id)+"/renew", runnerapi.LeaseRequest{Generation: l.gen}, &r)
	if errors.Is(err, session.ErrLeaseLost) {
		l.end()
	}
	return err
}

// Release gives the lease up; a second Release is a no-op.
func (l *lease) Release() error {
	l.mu.Lock()
	if l.done {
		l.mu.Unlock()
		return nil
	}
	l.done = true
	close(l.stop)
	l.mu.Unlock()
	defer l.end()
	err := l.c.json(context.Background(), http.MethodPost, "/leases/"+url.PathEscape(l.id)+"/release", runnerapi.LeaseRequest{Generation: l.gen}, nil)
	if errors.Is(err, session.ErrLeaseLost) {
		return nil
	}
	return err
}

// Lost is closed once the lease ends.
func (l *lease) Lost() <-chan struct{} { return l.lost }

// Append writes a batch under the lease; lease_lost ends it.
func (l *lease) Append(ctx context.Context, afterSeq uint64, events []session.Event) (uint64, error) {
	var a runnerapi.Appended
	err := l.c.json(ctx, http.MethodPost, sessionPath(l.id)+"/events", runnerapi.AppendRequest{Generation: l.gen, AfterSeq: afterSeq, Events: events}, &a)
	if errors.Is(err, session.ErrLeaseLost) {
		l.end()
	}
	return a.LastSeq, err
}

var _ session.Fence = (*lease)(nil)

// Credential asks the server for one of the session's credentials under
// the lease's generation (spec 018); lease_lost ends the lease.
func (l *lease) Credential(ctx context.Context, audience, workload string) (runner.Credential, error) {
	var t runnerapi.Token
	err := l.c.json(ctx, http.MethodPost, "/leases/"+url.PathEscape(l.id)+"/tokens", runnerapi.TokenRequest{Generation: l.gen, Audience: audience, Workload: workload}, &t)
	if errors.Is(err, session.ErrLeaseLost) {
		l.end()
	}
	if err != nil {
		return runner.Credential{}, err
	}
	return runner.Credential{Value: t.Token, ExpiresAt: t.ExpiresAt}, nil
}

var _ runner.Credentials = (*lease)(nil)

// Failover asks the server which model the session's turn continues on
// when failed could not serve now (spec 051), standing the model the turn
// runs and detail the developer detail of the failure, under the lease's
// generation; lease_lost ends the lease.
func (l *lease) Failover(ctx context.Context, standing, failed session.ModelRef, detail string) (session.ModelRef, error) {
	q := runnerapi.FailoverRequest{Generation: l.gen, Failed: failed, Detail: detail}
	if standing != failed {
		q.Standing = &standing
	}
	var a runnerapi.FailoverAnswer
	err := l.c.json(ctx, http.MethodPost, "/leases/"+url.PathEscape(l.id)+"/failover", q, &a)
	if errors.Is(err, session.ErrLeaseLost) {
		l.end()
	}
	if err != nil {
		return session.ModelRef{}, err
	}
	return a.Model, nil
}

var _ runner.Failover = (*lease)(nil)
