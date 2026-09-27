// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package runnerapi is the runner protocol of spec 016 on toposd's
// internal listener: the routes a runner process claims sessions, keeps
// their leases and writes their logs through, and the wire types both
// ends share. toposd never dials a runner; every connection is the
// runner's.
package runnerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"latere.ai/x/pkg/bearer"
	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// Root is the path the routes answer under.
const Root = "/internal/v1"

// Bounds of spec 016.
const (
	DefaultTTL   = 60 * time.Second
	MaxClaimWait = 20 * time.Second
	maxBody      = 8 << 20
)

// The error codes of the runner protocol.
const (
	CodeLeaseLost          = "lease_lost"
	CodeRunnerUnauthorized = "runner_unauthorized"
	CodeSequenceConflict   = "sequence_conflict"
	CodeNotFound           = "not_found"
	CodeInvalidRequest     = "invalid_request"
	CodeInternal           = "internal"
	// CodeNotMinted is an installation that mints nothing for the
	// audience asked; the runner uses its own credential (spec 018).
	CodeNotMinted = "not_minted"
	// CodeCredentialRefused is a credential the minter could not have:
	// details.code names the setup code the runner closes the turn with.
	CodeCredentialRefused = "credential_refused"
)

// ClaimRequest asks for sessions to run.
type ClaimRequest struct {
	Runner   string `json:"runner"`
	Capacity int    `json:"capacity"`
	// Wait is how long the claim may wait for work, a Go duration, at
	// most MaxClaimWait.
	Wait string `json:"wait,omitempty"`
}

// Claimed is one session a claim handed out.
type Claimed struct {
	SessionID  string    `json:"session_id"`
	Generation int64     `json:"generation"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// LeaseRequest names the lease a renew or a release is about.
type LeaseRequest struct {
	Generation int64 `json:"generation"`
}

// Renewed is a renewed lease.
type Renewed struct {
	ExpiresAt time.Time `json:"expires_at"`
}

// AppendRequest is one batch a runner appends under its lease.
type AppendRequest struct {
	Generation int64           `json:"generation"`
	AfterSeq   uint64          `json:"after_seq"`
	Events     []session.Event `json:"events"`
}

// TokenRequest asks for one credential of the session under the lease:
// a token for the audience, or the session's Lux key for the audience
// lux, for the runner's own calls or the session's sandbox.
type TokenRequest struct {
	Generation int64  `json:"generation"`
	Audience   string `json:"audience"`
	Workload   string `json:"workload"`
}

// Token is one credential and when it expires. It is held in memory and
// never appended.
type Token struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Appended is the log's last sequence after an append.
type Appended struct {
	LastSeq uint64 `json:"last_seq"`
}

// Options configure the server side.
type Options struct {
	Store session.Store
	// Queue hands out the sessions with work; the same queue serve's own
	// runners claim from.
	Queue runner.Claimer
	// Tokens are TOPOS_RUNNER_TOKEN's bearers; every one is accepted.
	Tokens []string
	// TTL is how long a claim holds without a renew; DefaultTTL when
	// zero.
	TTL time.Duration
	// Credentials answers the session's credential for audience and
	// workload to the lease named lease (spec 018), which the route has
	// checked the runner holds; nil mints nothing.
	Credentials func(ctx context.Context, id, lease, audience, workload string) (runner.Credential, error)
	Now         func() time.Time
	Log         *slog.Logger
}

// Server holds the claims of remote runners. A claim holds the store's
// lease on the session for as long as its runner renews it: the store's
// own lease may renew itself, so the claim's expiry here is what a
// runner that went away loses.
type Server struct {
	o      Options
	mu     sync.Mutex
	claims map[string]*claim
	gen    int64
}

type claim struct {
	runner  string
	gen     int64
	lease   session.Lease
	expires time.Time
}

// New builds the server side.
func New(o Options) (*Server, error) {
	switch {
	case o.Store == nil || o.Queue == nil:
		return nil, errors.New("runnerapi: a store and a queue are required")
	case len(o.Tokens) == 0:
		return nil, errors.New("runnerapi: no runner token")
	}
	if o.TTL <= 0 {
		o.TTL = DefaultTTL
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Server{o: o, claims: map[string]*claim{}}, nil
}

// Handler serves the routes under Root.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, h func(w http.ResponseWriter, r *http.Request) error) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			tok, ok := bearer.FromRequest(r)
			if !ok || !s.accepts(tok) {
				write(w, CodeRunnerUnauthorized, http.StatusUnauthorized, "no bearer, or one TOPOS_RUNNER_TOKEN does not name")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
			if err := h(w, r); err != nil && !errors.Is(err, errHungUp) {
				s.fail(w, r, err)
			}
		})
	}
	route("POST "+Root+"/claims", s.claim)
	route("POST "+Root+"/leases/{session}/renew", s.renew)
	route("POST "+Root+"/leases/{session}/release", s.release)
	route("POST "+Root+"/leases/{session}/tokens", s.tokens)
	route("POST "+Root+"/sessions/{session}/events", s.append)
	route("GET "+Root+"/sessions/{session}", s.get)
	route("GET "+Root+"/sessions/{session}/events", s.events)
	route("GET "+Root+"/sessions/{session}/stream", s.stream)
	route("PUT "+Root+"/sessions/{session}/blobs/{digest}", s.putBlob)
	route("GET "+Root+"/sessions/{session}/blobs/{digest}", s.blob)
	return mux
}

func (s *Server) accepts(tok string) bool {
	for _, t := range s.o.Tokens {
		if bearer.Equal(tok, t) {
			return true
		}
	}
	return false
}

// Reap releases, every interval until ctx ends, the claims whose runner
// stopped renewing them, so their sessions are claimable again. It
// releases every claim when ctx ends.
func (s *Server) Reap(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.releaseAll(func(*claim) bool { return true })
			return
		case <-t.C:
			now := s.o.Now()
			s.releaseAll(func(c *claim) bool { return now.After(c.expires) })
		}
	}
}

func (s *Server) releaseAll(expired func(*claim) bool) {
	s.mu.Lock()
	var gone []*claim
	for id, c := range s.claims {
		if expired(c) {
			gone = append(gone, c)
			delete(s.claims, id)
		}
	}
	s.mu.Unlock()
	for _, c := range gone {
		if err := c.lease.Release(); err != nil {
			s.o.Log.Error("release an expired claim", "err", err)
		}
	}
}

func (s *Server) claim(w http.ResponseWriter, r *http.Request) error {
	var req ClaimRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Runner == "" || req.Capacity < 1 {
		return &wireError{CodeInvalidRequest, http.StatusBadRequest, "a claim names its runner and a capacity of at least one"}
	}
	var wait time.Duration
	if req.Wait != "" {
		d, err := time.ParseDuration(req.Wait)
		if err != nil || d < 0 {
			return &wireError{CodeInvalidRequest, http.StatusBadRequest, "wait is not a duration"}
		}
		wait = min(d, MaxClaimWait)
	}
	got, err := s.o.Queue.Claim(r.Context(), session.Holder{Runner: req.Runner, AcquiredAt: s.o.Now()}, req.Capacity, wait)
	if err != nil {
		return err
	}
	out := make([]Claimed, 0, len(got))
	s.mu.Lock()
	for _, c := range got {
		s.gen++
		cl := &claim{runner: req.Runner, gen: s.gen, lease: c.Lease, expires: s.o.Now().Add(s.o.TTL)}
		s.claims[c.ID] = cl
		out = append(out, Claimed{SessionID: c.ID, Generation: cl.gen, ExpiresAt: cl.expires.UTC()})
	}
	s.mu.Unlock()
	return reply(w, http.StatusOK, out)
}

// held is the live claim of a session at gen, or lease_lost.
func (s *Server) held(id string, gen int64) (*claim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.claims[id]
	if !ok || c.gen != gen || s.o.Now().After(c.expires) {
		return nil, &wireError{CodeLeaseLost, http.StatusConflict, "the generation is not the session's current one"}
	}
	return c, nil
}

func (s *Server) renew(w http.ResponseWriter, r *http.Request) error {
	var req LeaseRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	c, err := s.held(r.PathValue("session"), req.Generation)
	if err != nil {
		return err
	}
	select {
	case <-c.lease.Lost():
		s.drop(r.PathValue("session"), c)
		return &wireError{CodeLeaseLost, http.StatusConflict, "the store's lease on the session ended"}
	default:
	}
	if err := c.lease.Renew(r.Context()); err != nil {
		s.drop(r.PathValue("session"), c)
		return &wireError{CodeLeaseLost, http.StatusConflict, "the store's lease on the session ended"}
	}
	s.mu.Lock()
	c.expires = s.o.Now().Add(s.o.TTL)
	exp := c.expires
	s.mu.Unlock()
	return reply(w, http.StatusOK, Renewed{ExpiresAt: exp.UTC()})
}

// drop forgets a claim whose lease ended.
func (s *Server) drop(id string, c *claim) {
	s.mu.Lock()
	if s.claims[id] == c {
		delete(s.claims, id)
	}
	s.mu.Unlock()
}

func (s *Server) release(w http.ResponseWriter, r *http.Request) error {
	var req LeaseRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	id := r.PathValue("session")
	c, err := s.held(id, req.Generation)
	if err != nil {
		return err
	}
	s.drop(id, c)
	if err := c.lease.Release(); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// tokens answers one of the session's credentials to the runner that
// holds its lease at the current generation, and to no other: a stale
// generation, an expired claim or a store lease that ended is lease_lost
// before anything is minted.
func (s *Server) tokens(w http.ResponseWriter, r *http.Request) error {
	var req TokenRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Audience == "" || (req.Workload != runner.WorkloadSession && req.Workload != runner.WorkloadSandbox) {
		return &wireError{CodeInvalidRequest, http.StatusBadRequest, "a token request names an audience and a workload of session or sandbox"}
	}
	id := r.PathValue("session")
	c, err := s.held(id, req.Generation)
	if err != nil {
		return err
	}
	select {
	case <-c.lease.Lost():
		s.drop(id, c)
		return &wireError{CodeLeaseLost, http.StatusConflict, "the store's lease on the session ended"}
	default:
	}
	if s.o.Credentials == nil {
		return &wireError{CodeNotMinted, http.StatusNotFound, "this installation mints no credential for " + req.Audience}
	}
	cred, err := s.o.Credentials(r.Context(), id, "claim:"+strconv.FormatInt(c.gen, 10), req.Audience, req.Workload)
	if err != nil {
		return err
	}
	return reply(w, http.StatusOK, Token{Token: cred.Value, ExpiresAt: cred.ExpiresAt.UTC()})
}

func (s *Server) append(w http.ResponseWriter, r *http.Request) error {
	var req AppendRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	id := r.PathValue("session")
	c, err := s.held(id, req.Generation)
	if err != nil {
		return err
	}
	appendTo := s.o.Store.Append
	if f, ok := c.lease.(session.Fence); ok {
		appendTo = func(ctx context.Context, _ string, after uint64, evs []session.Event) (uint64, error) {
			return f.Append(ctx, after, evs)
		}
	}
	last, err := appendTo(r.Context(), id, req.AfterSeq, req.Events)
	if err != nil {
		return err
	}
	return reply(w, http.StatusOK, Appended{LastSeq: last})
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) error {
	sess, err := s.o.Store.Get(r.Context(), r.PathValue("session"))
	if err != nil {
		return err
	}
	return reply(w, http.StatusOK, sess)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) error {
	from, limit, err := seqQuery(r)
	if err != nil {
		return err
	}
	evs, err := s.o.Store.Events(r.Context(), r.PathValue("session"), from, limit)
	if err != nil {
		return err
	}
	if evs == nil {
		evs = []session.Event{}
	}
	return reply(w, http.StatusOK, evs)
}

func seqQuery(r *http.Request) (uint64, int, error) {
	from, limit := uint64(1), 0
	if v := r.URL.Query().Get("from_seq"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil || n < 1 {
			return 0, 0, &wireError{CodeInvalidRequest, http.StatusBadRequest, "from_seq is not a sequence number"}
		}
		from = n
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, 0, &wireError{CodeInvalidRequest, http.StatusBadRequest, "limit is not a count"}
		}
		limit = n
	}
	return from, limit, nil
}

// stream is the session's events from from_seq, then each new one, one
// JSON event per line, until the runner hangs up.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) error {
	from, _, err := seqQuery(r)
	if err != nil {
		return err
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("runnerapi: the response writer cannot flush")
	}
	evs, err := s.o.Store.Watch(r.Context(), r.PathValue("session"), from)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	for ev := range evs {
		b, err := session.Marshal(ev)
		if err != nil {
			return err
		}
		if _, werr := w.Write(append(b, '\n')); werr != nil {
			return fmt.Errorf("%w: %w", errHungUp, werr)
		}
		flusher.Flush()
	}
	return nil
}

// errHungUp is a stream whose runner went away: there is nobody left to
// answer, so no error response is written.
var errHungUp = errors.New("runnerapi: the runner hung up")

// putBlob stores a blob the runner names by its digest, which the
// bytes must hash to.
func (s *Server) putBlob(w http.ResponseWriter, r *http.Request) error {
	want := session.Digest(r.PathValue("digest"))
	if !want.Valid() {
		return &wireError{CodeInvalidRequest, http.StatusBadRequest, "the path names no sha256 digest"}
	}
	d, err := s.o.Store.PutBlob(r.Context(), r.PathValue("session"), r.Body)
	if err != nil {
		return err
	}
	if d != want {
		return fmt.Errorf("%w: the bytes hash to %s, the path names %s", session.ErrBlobMismatch, d, want)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) blob(w http.ResponseWriter, r *http.Request) error {
	rc, err := s.o.Store.Blob(r.Context(), r.PathValue("session"), session.Digest(r.PathValue("digest")))
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, err = io.Copy(w, rc)
	return errors.Join(err, rc.Close())
}

// wireError is a refusal with its code and status.
type wireError struct {
	code   string
	status int
	msg    string
}

func (e *wireError) Error() string { return e.code + ": " + e.msg }

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var we *wireError
	if se, ok := errors.AsType[*runner.SetupError](err); ok {
		detail := ""
		if se.Err != nil {
			detail = se.Err.Error()
		}
		httpjson.WriteError(w, http.StatusBadGateway, httpjson.Error{Code: CodeCredentialRefused, Message: "the session's credential could not be had", Details: map[string]any{"code": se.Code, "detail": detail}})
		return
	}
	switch {
	case errors.As(err, &we):
	case errors.Is(err, runner.ErrNotMinted):
		we = &wireError{CodeNotMinted, http.StatusNotFound, err.Error()}
	case errors.Is(err, session.ErrLeaseLost):
		we = &wireError{CodeLeaseLost, http.StatusConflict, err.Error()}
	case errors.Is(err, session.ErrSequenceConflict):
		we = &wireError{CodeSequenceConflict, http.StatusConflict, err.Error()}
	case errors.Is(err, session.ErrNotFound):
		we = &wireError{CodeNotFound, http.StatusNotFound, err.Error()}
	case errors.Is(err, session.ErrInvalid), errors.Is(err, session.ErrBlobMismatch):
		we = &wireError{CodeInvalidRequest, http.StatusBadRequest, err.Error()}
	default:
		if mbe, ok := errors.AsType[*http.MaxBytesError](err); ok {
			we = &wireError{CodeInvalidRequest, http.StatusRequestEntityTooLarge, fmt.Sprintf("the body is past %d bytes", mbe.Limit)}
			break
		}
		s.o.Log.ErrorContext(r.Context(), "runner route", "path", r.URL.Path, "err", err)
		we = &wireError{CodeInternal, http.StatusInternalServerError, "the server failed to answer"}
	}
	write(w, we.code, we.status, we.msg)
}

func write(w http.ResponseWriter, code string, status int, msg string) {
	httpjson.WriteError(w, status, httpjson.Error{Code: code, Message: msg})
}

func decode(r *http.Request, v any) error {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &wireError{CodeInvalidRequest, http.StatusBadRequest, "the body does not decode: " + err.Error()}
	}
	return nil
}

func reply(w http.ResponseWriter, status int, v any) error {
	b, err := session.Marshal(v)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, err = w.Write(append(b, '\n'))
	return err
}
