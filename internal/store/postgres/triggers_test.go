// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build postgres

package postgres

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/bearer"

	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/server"
	"latere.ai/x/topos/internal/store/storetest"
	"latere.ai/x/topos/session"
)

const loginIssuer = "https://login.example"

// subjects verifies a bearer that is the sub of the login.example issuer.
type subjects struct{}

func (subjects) Authenticate(r *http.Request) (auth.Caller, error) {
	sub, ok := bearer.FromRequest(r)
	if !ok || sub == "" {
		return auth.Caller{}, &auth.Error{Code: auth.CodeUnauthenticated, Message: "no bearer"}
	}
	return auth.Caller{Subject: authz.Subject(loginIssuer, sub), Issuer: loginIssuer, Sub: sub, Claims: map[string]any{"sub": sub}}, nil
}

// replica is one toposd serve over the database: its own store and its
// own server, on the shared clock.
type replica struct {
	api *server.Server
	st  *Store
	srv *httptest.Server
}

func newReplica(t *testing.T, dsn string, clock *storetest.Clock) replica {
	t.Helper()
	st, err := Open(t.Context(), dsn, Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	api, err := server.New(server.Options{Sessions: st, Objects: st, Verifier: subjects{}, Guard: auth.Guard{Authorizer: &auth.OwnerPolicy{}},
		PublicURL: "https://topos.example", Now: clock.Now, HostSessions: true})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return replica{api: api, st: st, srv: srv}
}

func (r replica) do(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, r.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer alice")
	resp, err := r.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

// twoReplicas are two replicas over one fresh database, with alice's
// agent reviewer and a trigger of it applied through the first.
func twoReplicas(t *testing.T, trigger string) (replica, replica, *storetest.Clock, string) {
	t.Helper()
	dsn := database(t)
	clock := storetest.NewClock()
	a, b := newReplica(t, dsn, clock), newReplica(t, dsn, clock)
	storetest.Apply(t, a.st, authz.Subject(loginIssuer, "alice"), "reviewer", "Review.")
	status, body := a.do(t, http.MethodPut, "/v1/triggers/t", "apiVersion: topos.latere.ai/v1\nkind: Trigger\nmetadata: {name: t}\nspec: {agent: reviewer, "+trigger+"}\n")
	if status != http.StatusCreated {
		t.Fatalf("apply: %d %s", status, body)
	}
	var tr struct {
		Status struct {
			ID string `json:"id"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		t.Fatal(err)
	}
	return a, b, clock, tr.Status.ID
}

// triggered lists the sessions a trigger started.
func triggered(t *testing.T, st *Store, id string) []session.Session {
	t.Helper()
	all, _, err := st.List(t.Context(), session.ListOptions{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	var out []session.Session
	for _, s := range all {
		if s.TriggerID == id {
			out = append(out, s)
		}
	}
	return out
}

// TestOneSessionPerFiring: with two serve replicas running the minute
// loop at once on a fake clock, each scheduled firing starts one
// session.
func TestOneSessionPerFiring(t *testing.T) {
	a, b, clock, id := twoReplicas(t, "schedule: '@hourly', skipIfActive: false, session: {message: 'At {{firing.time}}.'}")
	for hour := 1; hour <= 3; hour++ {
		clock.Advance(time.Hour)
		var wg sync.WaitGroup
		for range 3 {
			for _, r := range []replica{a, b} {
				wg.Go(func() {
					if err := r.api.Tick(t.Context()); err != nil {
						t.Error(err)
					}
				})
			}
		}
		wg.Wait()
		if got := triggered(t, a.st, id); len(got) != hour {
			t.Fatalf("after %d scheduled firings, %d sessions", hour, len(got))
		}
	}
	list, _, err := a.st.Firings(t.Context(), id, 0, "")
	if err != nil || len(list) != 3 {
		t.Fatalf("firings %+v, %v", list, err)
	}
}

// TestOneSessionPerKey: two events of one key delivered at once, one to
// each replica, start one session, and the other continues it; a burst
// across both replicas still makes one session of the key.
func TestOneSessionPerKey(t *testing.T) {
	a, b, clock, id := twoReplicas(t, "on: {product: github, verbs: ['*']}, session: {message: '{{event.resource}}', policy: continue}")
	fire := func(r replica, n int) string {
		body := fmt.Sprintf(`{"id":"%d","product":"github","verb":"issue.edited","resource":"o/r#1","time":%q}`, n, clock.Now().Format(time.RFC3339))
		status, out := r.do(t, http.MethodPost, "/v1/triggers/"+id+"/fire", body)
		if status != http.StatusOK {
			t.Errorf("fire %d: %d %s", n, status, out)
			return ""
		}
		var f struct {
			Outcome string `json:"outcome"`
		}
		if err := json.Unmarshal(out, &f); err != nil {
			t.Error(err)
		}
		return f.Outcome
	}
	outcomes := make(chan string, 12)
	var wg sync.WaitGroup
	for n := range 12 {
		wg.Go(func() { outcomes <- fire([]replica{a, b}[n%2], n) })
	}
	wg.Wait()
	close(outcomes)
	count := map[string]int{}
	for o := range outcomes {
		count[o]++
	}
	if count["started"] != 1 || count["continued"] != 11 {
		t.Fatalf("outcomes %v", count)
	}
	ss := triggered(t, b.st, id)
	if len(ss) != 1 {
		t.Fatalf("%d sessions of one key", len(ss))
	}
	evs, err := b.st.Events(t.Context(), ss[0].ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	messages := 0
	for _, e := range evs {
		if e.Type == session.TypeUserMessage {
			messages++
		}
	}
	if messages != 12 {
		t.Fatalf("the key's session holds %d messages", messages)
	}
}
