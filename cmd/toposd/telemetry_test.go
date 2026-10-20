// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/pkg/otel"
)

// collector is an OTLP/HTTP receiver that counts what each signal's path
// was sent.
type collector struct {
	mu    sync.Mutex
	paths map[string]int
}

func newCollector(t *testing.T) (*collector, string) {
	t.Helper()
	c := &collector{paths: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		c.mu.Lock()
		c.paths[r.URL.Path]++
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return c, srv.URL
}

func (c *collector) got(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paths[path]
}

// traceID answers whether a GET of url carried a trace back, which it
// does only when a span recorded the request.
func traceID(t *testing.T, url string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return resp.Header.Get("X-Trace-Id")
}

// TestServeExportsItsTelemetry: with an endpoint set, every request but
// a probe is a span, and the stop flushes the spans and the request
// metrics to the endpoint (spec 023).
func TestServeExportsItsTelemetry(t *testing.T) {
	col, endpoint := newCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1")
	publicURL, internalURL, stop := startServe(t, map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t)})

	if id := traceID(t, publicURL+"/"); id == "" {
		t.Error("GET / carried no trace id: the public listener records no span")
	}
	if id := traceID(t, publicURL+"/v1/agents"); id == "" {
		t.Error("GET /v1/agents carried no trace id: the API records no span")
	}
	for _, base := range []string{publicURL, internalURL} {
		if id := traceID(t, base+"/livez"); id != "" {
			t.Errorf("GET %s/livez carried trace %s: a probe is recorded", base, id)
		}
	}

	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, path := range []string{"/v1/traces", "/v1/metrics"} {
		if col.got(path) == 0 {
			t.Errorf("nothing was sent to %s; the collector got %v", path, col.paths)
		}
	}
}

// TestServeExportsNothingWithoutAnEndpoint: without the endpoint the
// requests are served as before and carry no trace.
func TestServeExportsNothingWithoutAnEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	publicURL, _, stop := startServe(t, map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t)})
	if id := traceID(t, publicURL+"/"); id != "" {
		t.Errorf("GET / carried trace %s with no endpoint set", id)
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// TestTelemetryNamesAnExportThatFails: an export that cannot start, and
// one that cannot flush, is named in the log, and the logger still
// writes to stderr.
func TestTelemetryNamesAnExportThatFails(t *testing.T) {
	orig := bootstrap
	t.Cleanup(func() { bootstrap = orig })
	bootstrap = func(_ context.Context, cfg otel.Config) (*slog.Logger, func(context.Context) error, error) {
		if cfg.ServiceName != "toposd" || cfg.Stdout == nil {
			t.Errorf("bootstrap got %+v; want toposd's name and its stderr handler", cfg)
		}
		return slog.New(cfg.Stdout), func(context.Context) error { return errors.New("flush refused") }, errors.New("log exporter refused")
	}

	var stderr syncBuffer
	log, flush := telemetry(t.Context(), &stderr)
	log.InfoContext(t.Context(), "still logging")
	flush()

	for _, want := range []string{"the log export is not set up", "log exporter refused", "still logging", "the export did not flush", "flush refused"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr misses %q:\n%s", want, stderr.String())
		}
	}
}
