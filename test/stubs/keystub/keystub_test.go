// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package keystub

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func put(t *testing.T, s *Server, bearer, path, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, s.URL()+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

func codeOf(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

// TestTheStubAnswersTheKeyRoutes: a key's hash registers once per
// session and workload, the same hash renews it, another replaces it; an
// unknown session is session_unknown, an ended one session_ended, a
// wrong bearer and a body that is not one hash are refused, and a delete
// answers 204.
func TestTheStubAnswersTheKeyRoutes(t *testing.T) {
	s := New(t, "keys-token")
	h1, h2 := strings.Repeat("a", 64), strings.Repeat("b", 64)
	body := func(h string) string { return `{"value_sha256":"` + h + `"}` }
	if st, b := put(t, s, "keys-token", "/ses_1/keys/session", body(h1)); st != http.StatusNotFound || codeOf(b) != "session_unknown" {
		t.Fatalf("an unknown session: %d %v", st, b)
	}
	s.AddSession("ses_1")
	if st, b := put(t, s, "wrong", "/ses_1/keys/session", body(h1)); st != http.StatusUnauthorized {
		t.Fatalf("a wrong bearer: %d %v", st, b)
	}
	for _, bad := range []string{`{}`, `{"value_sha256":"x"}`, body(h1) + `x`, `{"value_sha256":"` + h1 + `","value":"lux_x"}`} {
		if st, _ := put(t, s, "keys-token", "/ses_1/keys/session", bad); st != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, st)
		}
	}
	if st, _ := put(t, s, "keys-token", "/ses_1/keys/other", body(h1)); st != http.StatusBadRequest {
		t.Fatalf("an unknown workload: %d", st)
	}
	st, b := put(t, s, "keys-token", "/ses_1/keys/session", body(h1))
	exp, err := time.Parse(time.RFC3339Nano, b["expires_at"].(string))
	if st != http.StatusOK || err != nil || time.Until(exp) < 15*time.Minute {
		t.Fatalf("register: %d %v %v", st, b, err)
	}
	if _, b := put(t, s, "keys-token", "/ses_1/keys/session", body(h1)); b["name"] == "" {
		t.Fatalf("renew: %v", b)
	}
	if k, _ := s.Key("ses_1", "session"); k.Hash != h1 || k.Puts != 2 || len(k.Replaced) != 0 {
		t.Fatalf("after a renewal %+v", k)
	}
	put(t, s, "keys-token", "/ses_1/keys/session", body(h2))
	if k, _ := s.Key("ses_1", "session"); k.Hash != h2 || k.Puts != 1 || len(k.Replaced) != 1 || k.Replaced[0] != h1 {
		t.Fatalf("after a replacement %+v", k)
	}
	if k, ok := s.ByHash(h2); !ok || k.Session != "ses_1" || k.Workload != "session" {
		t.Fatalf("the key by its hash %+v %v", k, ok)
	}
	if _, ok := s.ByHash(h1); ok {
		t.Fatal("a replaced hash was found")
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, s.URL()+"/ses_1/keys/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer keys-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d %v", resp.StatusCode, err)
	}
	if k, ok := s.Key("ses_1", "session"); !ok || !k.Deleted {
		t.Fatalf("after the delete %+v", k)
	}
	if _, ok := s.ByHash(h2); ok {
		t.Fatal("a deleted key was found by its hash")
	}
	if _, ok := s.Key("ses_2", "session"); ok {
		t.Fatal("a key nobody registered")
	}
	s.EndSession("ses_1")
	if st, b := put(t, s, "keys-token", "/ses_1/keys/sandbox", body(h1)); st != http.StatusConflict || codeOf(b) != "session_ended" {
		t.Fatalf("an ended session: %d %v", st, b)
	}
	s.AcceptAll()
	s.SetLifetime(time.Minute)
	if st, b := put(t, s, "keys-token", "/ses_9/keys/sandbox", body(h1)); st != http.StatusOK {
		t.Fatalf("any session: %d %v", st, b)
	}
	if st, b := put(t, s, "keys-token", "/../nothing", body(h1)); st != http.StatusNotFound && codeOf(b) != "not_found" {
		t.Fatalf("an unknown route: %d %v", st, b)
	}
	if len(s.Requests()) < 10 {
		t.Fatalf("%d requests recorded", len(s.Requests()))
	}
}
