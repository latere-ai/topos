// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package keystub is a stub of the authorizer's session key routes of
// spec 018: an HTTP server on loopback where toposd registers the
// SHA-256 of each Lux key it generates for a session and a workload,
// renews it by sending the same hash again, and replaces it by sending
// another. It knows the sessions a test adds, refuses an unknown one
// session_unknown and an ended one session_ended, records every hash and
// every request, and answers in the envelope of latere.ai/x/pkg/httpjson.
// It is a test artifact.
package keystub

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/httpjson"
)

// DefaultLifetime is a key's life from its registration or renewal: the
// runner's 60 second lease plus 15 minutes.
const DefaultLifetime = 60*time.Second + 15*time.Minute

// Key is one session key the stub holds.
type Key struct {
	Session  string
	Workload string
	Hash     string
	// Puts counts the registrations and renewals of this hash.
	Puts      int
	ExpiresAt time.Time
	// Replaced are the hashes this key replaced, oldest first.
	Replaced []string
	Deleted  bool
}

// Recorded is one request the stub received.
type Recorded struct {
	Method string
	Path   string
	Header http.Header
}

// Server is the stub.
type Server struct {
	srv   *httptest.Server
	token string

	mu       sync.Mutex
	sessions map[string]bool
	ended    map[string]bool
	keys     map[string]*Key
	lifetime time.Duration
	requests []Recorded
	now      func() time.Time
}

// New starts a stub that wants token as its bearer and closes it when
// the test ends.
func New(t testing.TB, token string) *Server {
	s := &Server{token: token, sessions: map[string]bool{}, ended: map[string]bool{}, keys: map[string]*Key{}, lifetime: DefaultLifetime, now: time.Now}
	s.srv = httptest.NewServer(s.routes())
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the base the routes answer under, /sessions on the stub.
func (s *Server) URL() string { return s.srv.URL + "/sessions" }

// AddSession makes a session known, as the authorizer knows the sessions
// it allowed. AcceptAll makes every session known.
func (s *Server) AddSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = true
}

// AcceptAll makes every session known.
func (s *Server) AcceptAll() { s.AddSession("*") }

// EndSession ends a known session: its keys are refused session_ended.
func (s *Server) EndSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ended[id] = true
}

// SetLifetime sets a key's life from each registration.
func (s *Server) SetLifetime(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lifetime = d
}

// Key is the session's key for a workload.
func (s *Server) Key(session, workload string) (Key, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[session+"/"+workload]
	if !ok {
		return Key{}, false
	}
	out := *k
	out.Replaced = slices.Clone(k.Replaced)
	return out, true
}

// Requests returns every request the stub received, in order.
func (s *Server) Requests() []Recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// hashRE is a SHA-256 in lower-case hex.
var hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /sessions/{id}/keys/{workload}", s.admit(s.put))
	mux.HandleFunc("DELETE /sessions/{id}/keys/{workload}", s.admit(s.remove))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		refuse(w, http.StatusNotFound, "not_found", "the stub serves no "+r.Method+" "+r.URL.Path)
	})
	return mux
}

func (s *Server) admit(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, Recorded{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone()})
		s.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+s.token {
			refuse(w, http.StatusUnauthorized, "unauthenticated", "the bearer is not the stub's")
			return
		}
		next(w, r)
	}
}

func (s *Server) put(w http.ResponseWriter, r *http.Request) {
	id, workload := r.PathValue("id"), r.PathValue("workload")
	if workload != "session" && workload != "sandbox" {
		refuse(w, http.StatusBadRequest, "invalid_request", "the workload is session or sandbox")
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		refuse(w, http.StatusBadRequest, "invalid_request", "the body could not be read")
		return
	}
	var in struct {
		ValueSHA256 string `json:"value_sha256"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || dec.Decode(&struct{}{}) != io.EOF || !hashRE.MatchString(in.ValueSHA256) {
		refuse(w, http.StatusBadRequest, "invalid_request", "the body is {\"value_sha256\": \"<64 hex>\"} and nothing else")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case !s.sessions[id] && !s.sessions["*"]:
		refuse(w, http.StatusNotFound, "session_unknown", "the authorizer has no record of the session")
		return
	case s.ended[id]:
		refuse(w, http.StatusConflict, "session_ended", "the session ended")
		return
	}
	k := s.keys[id+"/"+workload]
	switch {
	case k == nil:
		k = &Key{Session: id, Workload: workload, Hash: in.ValueSHA256}
		s.keys[id+"/"+workload] = k
	case k.Hash != in.ValueSHA256:
		k.Replaced = append(k.Replaced, k.Hash)
		k.Hash, k.Puts = in.ValueSHA256, 0
	}
	k.Puts++
	k.Deleted = false
	k.ExpiresAt = s.now().Add(s.lifetime).UTC()
	httpjson.Write(w, http.StatusOK, map[string]any{"name": "session-" + workload, "expires_at": k.ExpiresAt, "budget": "session"})
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if k := s.keys[r.PathValue("id")+"/"+r.PathValue("workload")]; k != nil {
		k.Deleted = true
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func refuse(w http.ResponseWriter, status int, code, msg string) {
	httpjson.WriteError(w, status, httpjson.Error{Code: code, Message: msg})
}
