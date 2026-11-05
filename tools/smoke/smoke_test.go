// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package smoke holds the test for release.sh, the live check the release
// pipeline runs against Topos after a rollout.
package smoke

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func stub(t *testing.T, status int, body string) *httptest.Server {
	return stubVersion(t, status, body, served)
}

// served is a version route that serves the build v1.2.3.
func served(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"version":"v1.2.3","commit":"abc1234","build_time":"2026-11-05T00:00:00Z"}` + "\n"))
}

// stubVersion is stub with the version route a test chooses; nil leaves it
// unserved.
func stubVersion(t *testing.T, status int, body string, version http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	if version != nil {
		mux.HandleFunc("GET /v1/agents/version", version)
	}
	mux.HandleFunc("GET /v1/agents/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// run executes release.sh and skips when the tools it forks are absent, as
// in the gate's hermetic run.
func run(t *testing.T, env ...string) (string, bool) {
	t.Helper()
	for _, c := range []string{"bash", "curl", "grep", "mktemp"} {
		if _, err := exec.LookPath(c); err != nil {
			t.Skipf("%s is not on PATH, so release.sh cannot run here", c)
		}
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	cmd := exec.CommandContext(context.Background(), "bash", filepath.Join(filepath.Dir(file), "release.sh"))
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

func TestSmokePassesAndWritesEvidence(t *testing.T) {
	srv := stub(t, http.StatusOK, "openapi: 3.1.0\n")
	evidence := filepath.Join(t.TempDir(), "smoke.md")
	out, ok := run(t, "BASE_URL="+srv.URL+"/v1/agents", "TAG=v1.2.3", "COMMIT=abc1234", "OUTPUT_MD="+evidence)
	if !ok {
		t.Fatalf("the smoke failed against a healthy service:\n%s", out)
	}
	md, err := os.ReadFile(evidence)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Release evidence", "- Tag: `v1.2.3`", "- Commit: `abc1234`", "Served version: `v1.2.3`, equal to the tag"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("the evidence lacks %q:\n%s", want, md)
		}
	}
}

func TestSmokeFailsWhenTheServedVersionIsNotTheTag(t *testing.T) {
	srv := stub(t, http.StatusOK, "openapi: 3.1.0\n")
	out, ok := run(t, "BASE_URL="+srv.URL+"/v1/agents", "TAG=v1.2.4")
	if ok || !strings.Contains(out, "version mismatch: served v1.2.3, expected v1.2.4") {
		t.Fatalf("the smoke passed or did not name both versions (ok=%v):\n%s", ok, out)
	}
}

func TestSmokeRecordsTheVersionWithNoTag(t *testing.T) {
	srv := stub(t, http.StatusOK, "openapi: 3.1.0\n")
	out, ok := run(t, "BASE_URL="+srv.URL+"/v1/agents")
	if !ok || !strings.Contains(out, "served version recorded (v1.2.3)") {
		t.Fatalf("want a pass that records the version (ok=%v):\n%s", ok, out)
	}
}

func TestSmokeFailsWhenTheVersionRouteIsNotServed(t *testing.T) {
	srv := stubVersion(t, http.StatusOK, "openapi: 3.1.0\n", nil)
	out, ok := run(t, "BASE_URL="+srv.URL+"/v1/agents", "TAG=v1.2.3")
	if ok || !strings.Contains(out, "GET /version: expected 200, got 404") {
		t.Fatalf("the smoke passed or did not name the route (ok=%v):\n%s", ok, out)
	}
}

func TestSmokeFailsOnADownService(t *testing.T) {
	srv := stub(t, http.StatusOK, "openapi: 3.1.0\n")
	url := srv.URL + "/v1/agents"
	srv.Close()
	if out, ok := run(t, "BASE_URL="+url); ok {
		t.Fatalf("the smoke passed against a service that is down:\n%s", out)
	}
}

func TestSmokeFailsOnARouteThatDoesNotAnswer(t *testing.T) {
	srv := stub(t, http.StatusNotFound, "{}")
	out, ok := run(t, "BASE_URL="+srv.URL+"/v1/agents")
	if ok || !strings.Contains(out, "expected 200, got 404") {
		t.Fatalf("want a 404 failure, got ok=%v:\n%s", ok, out)
	}
}

func TestSmokeFailsOnAPageThatIsNotTheDescription(t *testing.T) {
	srv := stub(t, http.StatusOK, "<!doctype html>\n")
	if out, ok := run(t, "BASE_URL="+srv.URL+"/v1/agents"); ok {
		t.Fatalf("the smoke passed on a page:\n%s", out)
	}
}

func TestSmokeRefusesWithNoTarget(t *testing.T) {
	out, ok := run(t, "BASE_URL=")
	if ok || !strings.Contains(out, "BASE_URL is required") {
		t.Fatalf("want a refusal, got ok=%v:\n%s", ok, out)
	}
}
