// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package cella_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"latere.ai/x/topos/sandbox"
	"latere.ai/x/topos/sandbox/cella"
)

// newProvider wires a cella.Provider to an httptest server with a static token.
func newProvider(t *testing.T, h http.Handler) *cella.Provider {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return cella.New(cella.Options{
		BaseURL:    srv.URL,
		Token:      cella.StaticTokenSource("test-token"),
		HTTPClient: srv.Client(),
	})
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func TestCreateSendsManifestAndParsesResponse(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	var gotBody manifestBody
	p := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(t, w, http.StatusOK, map[string]any{
			"id": "sb_1", "name": "dev", "state": "creating", "tier": "ephemeral",
			"created_at": "2026-06-28T00:00:00Z", "backend": "k8s",
		})
	}))

	callerLabels := map[string]string{"team": "x"}
	sb, err := p.Create(context.Background(), sandbox.CreateOptions{
		Name:   "dev",
		Env:    map[string]string{"FOO": "bar"},
		Labels: callerLabels,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The backend tags every agent sandbox with kind=agent; caller labels are
	// preserved and the caller's map is not mutated.
	if gotBody.Metadata.Labels["kind"] != "agent" || gotBody.Metadata.Labels["team"] != "x" {
		t.Errorf("labels = %v, want kind=agent + team=x", gotBody.Metadata.Labels)
	}
	if _, mutated := callerLabels["kind"]; mutated {
		t.Error("Create mutated the caller's Labels map")
	}

	if gotMethod != "POST" || gotPath != "/v1/sandboxes" {
		t.Errorf("request = %s %s, want POST /v1/sandboxes", gotMethod, gotPath)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("auth = %q, want Bearer test-token", gotAuth)
	}
	if gotBody.APIVersion != "cella.latere.ai/v1" || gotBody.Kind != "Sandbox" {
		t.Errorf("envelope = %+v, want cella.latere.ai/v1 Sandbox", gotBody)
	}
	if gotBody.Metadata.Name != "dev" {
		t.Errorf("metadata.name = %q, want dev", gotBody.Metadata.Name)
	}
	if gotBody.Spec.Image == "" {
		t.Error("spec.image empty; manifest requires a default base image")
	}
	if gotBody.Spec.Tier != "ephemeral" {
		t.Errorf("spec.tier = %q, want ephemeral (default)", gotBody.Spec.Tier)
	}
	if gotBody.Spec.Lifecycle == nil || gotBody.Spec.Lifecycle.AutoStop == "" {
		t.Errorf("spec.lifecycle.autoStop not set; cost backstop missing: %+v", gotBody.Spec)
	}
	if gotBody.Spec.Env["FOO"] != "bar" {
		t.Errorf("spec.env = %v, want FOO=bar", gotBody.Spec.Env)
	}

	if sb.ID != "sb_1" || sb.Name != "dev" || sb.State != sandbox.StateCreating || sb.Tier != "ephemeral" {
		t.Errorf("sandbox = %+v", sb)
	}
}

func TestCreatePropagatesAPIError(t *testing.T) {
	p := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusInternalServerError, map[string]string{
			"code": "internal", "message": "boom", "request_id": "req_9",
		})
	}))
	_, err := p.Create(context.Background(), sandbox.CreateOptions{})
	var apiErr *sandbox.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 500 || apiErr.RequestID != "req_9" {
		t.Fatalf("err = %v, want *APIError 500 req_9", err)
	}
}

func TestCreateConflict(t *testing.T) {
	p := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusConflict, map[string]string{"code": "conflict", "message": "name taken"})
	}))
	_, err := p.Create(context.Background(), sandbox.CreateOptions{Name: "taken"})
	if !errors.Is(err, sandbox.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestTokenFuncPullsCurrentTokenEachRequest(t *testing.T) {
	// The owner rotates the token out of band; TokenFunc returns the current
	// value, so each request carries the latest bearer with no re-wiring.
	var authHeaders []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		writeJSON(t, w, http.StatusOK, map[string]string{"id": "sb_1", "state": "creating", "backend": "k8s", "created_at": "t"})
	}))
	t.Cleanup(srv.Close)

	current := "tok-v1"
	p := cella.New(cella.Options{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Token:      cella.TokenFunc(func(context.Context) (string, error) { return current, nil }),
	})

	if _, err := p.Create(context.Background(), sandbox.CreateOptions{}); err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	current = "tok-v2" // owner refreshes
	if _, err := p.Create(context.Background(), sandbox.CreateOptions{}); err != nil {
		t.Fatalf("Create 2: %v", err)
	}

	want := []string{"Bearer tok-v1", "Bearer tok-v2"}
	if !equalStrings(authHeaders, want) {
		t.Fatalf("auth headers = %v, want %v (refresh did not flow through)", authHeaders, want)
	}
}

func TestTokenFuncErrorPropagates(t *testing.T) {
	p := cella.New(cella.Options{
		BaseURL: "http://unused",
		Token:   cella.TokenFunc(func(context.Context) (string, error) { return "", errors.New("token unavailable") }),
	})
	// The error surfaces before any HTTP request is attempted.
	if _, err := p.Create(context.Background(), sandbox.CreateOptions{}); err == nil {
		t.Fatal("want token error, got nil")
	}
}

// captureCreate runs Create against a fake that records the manifest body and
// returns it.
func captureCreate(t *testing.T, opts sandbox.CreateOptions) manifestBody {
	t.Helper()
	var got manifestBody
	p := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writeJSON(t, w, http.StatusOK, map[string]string{"id": "sb_1", "state": "creating", "backend": "k8s", "created_at": "t"})
	}))
	if _, err := p.Create(context.Background(), opts); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return got
}

func TestCreateSecretMountsSemantics(t *testing.T) {
	t.Run("nil omits secrets (server applies default_mount)", func(t *testing.T) {
		got := captureCreate(t, sandbox.CreateOptions{SecretMounts: nil})
		if got.Spec.Secrets != nil {
			t.Errorf("secrets = %+v, want omitted for nil SecretMounts", got.Spec.Secrets)
		}
	})

	t.Run("empty slice mounts none (serialises as [])", func(t *testing.T) {
		got := captureCreate(t, sandbox.CreateOptions{SecretMounts: []string{}})
		if got.Spec.Secrets == nil {
			t.Fatal("secrets omitted; an empty SecretMounts must send mount: []")
		}
		if len(got.Spec.Secrets.Mount) != 0 {
			t.Errorf("mount = %v, want empty", got.Spec.Secrets.Mount)
		}
	})

	t.Run("list mounts exactly those names", func(t *testing.T) {
		got := captureCreate(t, sandbox.CreateOptions{SecretMounts: []string{"OPENAI_KEY", "GITHUB_TOKEN"}})
		if got.Spec.Secrets == nil || len(got.Spec.Secrets.Mount) != 2 ||
			got.Spec.Secrets.Mount[0] != "OPENAI_KEY" || got.Spec.Secrets.Mount[1] != "GITHUB_TOKEN" {
			t.Errorf("mount = %+v, want [OPENAI_KEY GITHUB_TOKEN]", got.Spec.Secrets)
		}
	})
}

func TestDestroyDeletesAndIsIdempotent(t *testing.T) {
	var calls int
	p := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "DELETE" || r.URL.Path != "/v1/sandboxes/sb_1" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if calls == 1 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(t, w, http.StatusNotFound, map[string]string{"code": "not_found", "message": "gone"})
	}))

	if err := p.Destroy(context.Background(), "sb_1"); err != nil {
		t.Fatalf("Destroy 1: %v", err)
	}
	// Second destroy hits a 404, which must be swallowed as success.
	if err := p.Destroy(context.Background(), "sb_1"); err != nil {
		t.Fatalf("Destroy 2 (idempotent): %v", err)
	}
}

func TestHealthCheck(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    map[string]string
		wantErr func(error) bool
	}{
		{
			name: "running is healthy", status: 200,
			body:    map[string]string{"id": "sb_1", "state": "running", "backend": "k8s", "created_at": "t"},
			wantErr: func(e error) bool { return e == nil },
		},
		{
			name: "stopped is unhealthy", status: 200,
			body:    map[string]string{"id": "sb_1", "state": "stopped", "backend": "k8s", "created_at": "t"},
			wantErr: func(e error) bool { return e != nil && !errors.Is(e, sandbox.ErrNotFound) },
		},
		{
			name: "missing is ErrNotFound", status: 404,
			body:    map[string]string{"code": "not_found", "message": "gone"},
			wantErr: func(e error) bool { return errors.Is(e, sandbox.ErrNotFound) },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newProvider(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, c.status, c.body)
			}))
			err := p.HealthCheck(context.Background(), "sb_1")
			if !c.wantErr(err) {
				t.Fatalf("HealthCheck err = %v", err)
			}
		})
	}
}

// TestContextBearerThreadsThrough verifies a ContextTokenSource provider sends
// the bearer placed on the context by sandbox.WithBearer.
func TestContextBearerThreadsThrough(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeJSON(t, w, http.StatusOK, map[string]string{"id": "sb_1", "state": "creating", "backend": "k8s", "created_at": "t"})
	}))
	t.Cleanup(srv.Close)
	p := cella.New(cella.Options{BaseURL: srv.URL, Token: cella.ContextTokenSource{}, HTTPClient: srv.Client()})

	ctx := sandbox.WithBearer(context.Background(), "user-jwt-bearer")
	if _, err := p.Create(ctx, sandbox.CreateOptions{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if gotAuth != "Bearer user-jwt-bearer" {
		t.Fatalf("auth = %q, want Bearer user-jwt-bearer", gotAuth)
	}

	// Without a bearer on the context, the token source fails before any request.
	if _, err := p.Create(context.Background(), sandbox.CreateOptions{}); err == nil {
		t.Fatal("missing context bearer: want error, got nil")
	}
}

// manifestBody mirrors the JSON the provider POSTs, for assertions.
type manifestBody struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Image     string            `json:"image"`
		Tier      string            `json:"tier"`
		Env       map[string]string `json:"env"`
		Lifecycle *struct {
			AutoStop string `json:"autoStop"`
		} `json:"lifecycle"`
		Secrets *struct {
			Mount []string `json:"mount"`
		} `json:"secrets"`
	} `json:"spec"`
}
