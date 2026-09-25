// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	cellaclient "latere.ai/x/cella/client"
	v1 "latere.ai/x/cella/manifest/v1"

	"latere.ai/x/topos/sandbox/cella"
)

// basePath is where the fake serves the control plane, the base path the
// hosted deployment serves it under.
const basePath = "/v1/environments"

// createdAt is the creation time the fake stamps on every sandbox.
var createdAt = time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)

// reply is one refusal the fake answers with, in the control plane's error
// envelope.
type reply struct {
	status        int
	code, message string
}

// fakeCore is a Cella control plane under basePath, holding what a test needs
// to assert: the requests in order, the manifests created, the exec bodies and
// the files. A create answers 201 Pending, as the control plane does since
// v0.6.0, unless the request holds it with ?wait=1, when it answers the phase
// the sandbox reached (holdPhase). A read afterwards reports readPhase, the
// phase the scheduler moved the sandbox to meanwhile.
type fakeCore struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	requests  []string
	bearers   []string
	manifests []v1.Sandbox
	createQs  []string
	sandboxes map[string]v1.Sandbox
	execs     []cellaclient.ExecRequest
	files     map[string][]byte
	dirs      map[string][]cellaclient.FileEntry
	next      int

	// holdPhase is what a held create answers, and holdReason its reason.
	holdPhase, holdReason string
	// readPhase and readReason are what a read reports once the sandbox
	// exists.
	readPhase, readReason string
	// admitTTL is a time to live the fake's admission gives a sandbox that
	// asked for none, as the hosted plane's plans do.
	admitTTL v1.Duration
	// createErr, deleteErr and execErr refuse the next such request.
	createErr, deleteErr, execErr *reply
	// execResult is the answer of every exec.
	execResult cellaclient.ExecResult
	// execBlock holds each exec until the request's context ends, so a test
	// can end it from the caller's side.
	execBlock bool
	// execArrived is closed when a blocked exec has reached the fake.
	execArrived chan struct{}
}

func newFakeCore(t *testing.T) *fakeCore {
	t.Helper()
	f := &fakeCore{
		t:           t,
		sandboxes:   map[string]v1.Sandbox{},
		files:       map[string][]byte{},
		dirs:        map[string][]cellaclient.FileEntry{},
		holdPhase:   "Running",
		readPhase:   "Running",
		execArrived: make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+basePath+"/sandboxes", f.create)
	mux.HandleFunc("GET "+basePath+"/sandboxes/{id}", f.get)
	mux.HandleFunc("DELETE "+basePath+"/sandboxes/{id}", f.delete)
	mux.HandleFunc("POST "+basePath+"/sandboxes/{id}/exec", f.exec)
	mux.HandleFunc("GET "+basePath+"/sandboxes/{id}/files/content", f.fileContent)
	mux.HandleFunc("PUT "+basePath+"/sandboxes/{id}/files", f.filePut)
	mux.HandleFunc("GET "+basePath+"/sandboxes/{id}/files/list", f.fileList)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.bearers = append(f.bearers, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// url is the base URL a provider is configured with.
func (f *fakeCore) url() string { return f.srv.URL + basePath }

// provider is a Provider on the fake with a static token and the fake's own
// client.
func (f *fakeCore) provider(t *testing.T) *cella.Provider {
	t.Helper()
	return cella.New(cella.Options{BaseURL: f.url(), Token: cella.StaticTokenSource("test-token"), HTTPClient: f.srv.Client()})
}

func (f *fakeCore) requestLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeCore) lastManifest(t *testing.T) v1.Sandbox {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.manifests) == 0 {
		t.Fatal("the fake received no create")
	}
	return f.manifests[len(f.manifests)-1]
}

func (f *fakeCore) lastExec(t *testing.T) cellaclient.ExecRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.execs) == 0 {
		t.Fatal("the fake received no exec")
	}
	return f.execs[len(f.execs)-1]
}

// refuse writes the control plane's error envelope.
func refuse(w http.ResponseWriter, r reply) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(r.status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": r.code, "message": r.message, "details": map[string]any{"request_id": "req_fake"},
	}})
}

func answer(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode the fake's answer: %v", err)
	}
}

// take returns and clears a one-shot refusal.
func (f *fakeCore) take(r **reply) *reply {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := *r
	*r = nil
	return out
}

func (f *fakeCore) create(w http.ResponseWriter, r *http.Request) {
	if e := f.take(&f.createErr); e != nil {
		refuse(w, *e)
		return
	}
	// The control plane decodes strictly: a field it does not know is
	// refused, so a manifest the fake accepts carries only known fields.
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var obj v1.Sandbox
	if err := dec.Decode(&obj); err != nil {
		refuse(w, reply{http.StatusBadRequest, "invalid_field", err.Error()})
		return
	}
	f.mu.Lock()
	f.manifests = append(f.manifests, obj)
	f.createQs = append(f.createQs, r.URL.RawQuery)
	f.next++
	id := "sbx_" + strconv.Itoa(f.next)
	if obj.Metadata.Name == "" {
		obj.Metadata.Name = "sandbox-" + strconv.Itoa(f.next)
	}
	if obj.Spec.Lifecycle.TTL == "" {
		obj.Spec.Lifecycle.TTL = f.admitTTL
	}
	obj.Status = v1.SandboxStatus{ID: id, Phase: "Pending", CreatedAt: createdAt}
	if r.URL.Query().Get("wait") == "1" {
		obj.Status.Phase, obj.Status.Reason = f.holdPhase, f.holdReason
	}
	f.sandboxes[id] = obj
	f.mu.Unlock()
	w.Header().Set("Location", basePath+"/sandboxes/"+id)
	answer(f.t, w, http.StatusCreated, obj)
}

// lookup finds a sandbox by id or name.
func (f *fakeCore) lookup(ref string) (v1.Sandbox, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if obj, ok := f.sandboxes[ref]; ok {
		return obj, true
	}
	for _, obj := range f.sandboxes {
		if obj.Metadata.Name == ref {
			return obj, true
		}
	}
	return v1.Sandbox{}, false
}

func notFound(w http.ResponseWriter) {
	refuse(w, reply{http.StatusNotFound, "not_found", "The object does not exist."})
}

func (f *fakeCore) get(w http.ResponseWriter, r *http.Request) {
	obj, ok := f.lookup(r.PathValue("id"))
	if !ok {
		notFound(w)
		return
	}
	f.mu.Lock()
	obj.Status.Phase, obj.Status.Reason = f.readPhase, f.readReason
	f.mu.Unlock()
	answer(f.t, w, http.StatusOK, obj)
}

func (f *fakeCore) delete(w http.ResponseWriter, r *http.Request) {
	if e := f.take(&f.deleteErr); e != nil {
		refuse(w, *e)
		return
	}
	obj, ok := f.lookup(r.PathValue("id"))
	if !ok {
		notFound(w)
		return
	}
	f.mu.Lock()
	delete(f.sandboxes, obj.Status.ID)
	f.mu.Unlock()
	obj.Status.Phase = "Deleting"
	answer(f.t, w, http.StatusOK, obj)
}

func (f *fakeCore) exec(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("wait") != "1" {
		refuse(w, reply{http.StatusBadRequest, "invalid_field", "the fake serves the synchronous exec only"})
		return
	}
	if _, ok := f.lookup(r.PathValue("id")); !ok {
		notFound(w)
		return
	}
	var req cellaclient.ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		refuse(w, reply{http.StatusBadRequest, "invalid_field", err.Error()})
		return
	}
	f.mu.Lock()
	f.execs = append(f.execs, req)
	block := f.execBlock
	f.mu.Unlock()
	if block {
		close(f.execArrived)
		<-r.Context().Done()
		return
	}
	if e := f.take(&f.execErr); e != nil {
		refuse(w, *e)
		return
	}
	answer(f.t, w, http.StatusOK, f.execResult)
}

func (f *fakeCore) fileContent(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	data, ok := f.files[r.URL.Query().Get("path")]
	f.mu.Unlock()
	if !ok {
		notFound(w)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(data)
}

func (f *fakeCore) filePut(w http.ResponseWriter, r *http.Request) {
	if _, ok := f.lookup(r.PathValue("id")); !ok {
		notFound(w)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		refuse(w, reply{http.StatusBadRequest, "invalid_field", err.Error()})
		return
	}
	f.mu.Lock()
	f.files[r.URL.Query().Get("path")] = bytes.Clone(data)
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeCore) fileList(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	entries, ok := f.dirs[r.URL.Query().Get("path")]
	f.mu.Unlock()
	if !ok {
		notFound(w)
		return
	}
	sorted := append([]cellaclient.FileEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	answer(f.t, w, http.StatusOK, map[string]any{"items": sorted})
}
