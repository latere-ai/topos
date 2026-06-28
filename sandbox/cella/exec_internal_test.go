// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package cella

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/topos/sandbox"
)

// fastProvider returns a provider pointed at srv with a tiny poll interval so
// streaming-exec tests don't pay the production cadence.
func fastProvider(t *testing.T, h http.Handler) *Provider {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	p := New(Options{BaseURL: srv.URL, Token: StaticTokenSource("tok"), HTTPClient: srv.Client()})
	p.pollInterval = time.Millisecond
	return p
}

// execHandler serves the command-start and cursor-log endpoints. logs is the
// ordered sequence of envelopes returned by successive log polls; the last one
// should carry a terminal phase.
func execHandler(t *testing.T, logs []logEnvelope) http.Handler {
	t.Helper()
	var poll atomic.Int64
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/commands"):
			var req createCommandReq
			_ = json.NewDecoder(r.Body).Decode(&req)
			if len(req.Argv) == 0 {
				t.Error("command POST had empty argv")
			}
			_ = json.NewEncoder(w).Encode(commandResp{CommandID: "cmd_1", Phase: "running"})
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/logs"):
			i := int(poll.Add(1)) - 1
			if i >= len(logs) {
				i = len(logs) - 1 // keep serving the terminal frame
			}
			_ = json.NewEncoder(w).Encode(logs[i])
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestExecSuccessCombinesOutput(t *testing.T) {
	code := 0
	p := fastProvider(t, execHandler(t, []logEnvelope{
		{Bytes: "hel", NextCursor: 3, Phase: "running"},
		{Bytes: "lo\n", NextCursor: 6, Phase: "exited", ExitCode: &code},
	}))

	res, err := p.Exec(context.Background(), "sb_1", sandbox.ExecOptions{Argv: []string{"echo", "hello"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if string(res.Stdout) != "hello\n" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "hello\n")
	}
	if res.Phase != "exited" || res.ExitCode != 0 {
		t.Errorf("phase/exit = %q/%d, want exited/0", res.Phase, res.ExitCode)
	}
}

func TestExecNonzeroExit(t *testing.T) {
	code := 42
	p := fastProvider(t, execHandler(t, []logEnvelope{
		{Bytes: "", NextCursor: 0, Phase: "exited", ExitCode: &code},
	}))
	res, err := p.Exec(context.Background(), "sb_1", sandbox.ExecOptions{Argv: []string{"false"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 42 || res.Phase != "exited" {
		t.Errorf("exit/phase = %d/%q, want 42/exited", res.ExitCode, res.Phase)
	}
}

func TestExecLostHasNoExitCode(t *testing.T) {
	p := fastProvider(t, execHandler(t, []logEnvelope{
		{Bytes: "partial", NextCursor: 7, Phase: "lost"},
	}))
	res, err := p.Exec(context.Background(), "sb_1", sandbox.ExecOptions{Argv: []string{"sleep", "100"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Phase != "lost" || res.ExitCode != 0 {
		t.Errorf("phase/exit = %q/%d, want lost/0", res.Phase, res.ExitCode)
	}
	if string(res.Stdout) != "partial" {
		t.Errorf("stdout = %q, want partial", res.Stdout)
	}
}

func TestStreamExecDeliversChunksIncrementally(t *testing.T) {
	code := 0
	p := fastProvider(t, execHandler(t, []logEnvelope{
		{Bytes: "one", NextCursor: 3, Phase: "running"},
		{Bytes: "two", NextCursor: 6, Phase: "running"},
		{Bytes: "three", NextCursor: 11, Phase: "exited", ExitCode: &code},
	}))

	stream, err := p.StreamExec(context.Background(), "sb_1", sandbox.ExecOptions{Argv: []string{"prog"}})
	if err != nil {
		t.Fatalf("StreamExec: %v", err)
	}
	defer stream.Close() //nolint:errcheck

	var chunks []string
	for {
		b, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		chunks = append(chunks, string(b))
	}
	// Chunks may coalesce, but concatenation must be exact and in order.
	joined := ""
	for _, c := range chunks {
		joined += c
	}
	if joined != "onetwothree" {
		t.Errorf("joined = %q, want onetwothree", joined)
	}
	if r := stream.Result(); r.Phase != "exited" || string(r.Stdout) != "onetwothree" {
		t.Errorf("result = %+v", r)
	}
}

func TestStreamExecEmptyArgv(t *testing.T) {
	p := fastProvider(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("no request should be made for empty argv")
	}))
	if _, err := p.StreamExec(context.Background(), "sb_1", sandbox.ExecOptions{}); err == nil {
		t.Fatal("empty argv: want error, got nil")
	}
}

func TestExecPropagatesStartError(t *testing.T) {
	p := fastProvider(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "not_found", "message": "no sandbox"})
	}))
	_, err := p.Exec(context.Background(), "missing", sandbox.ExecOptions{Argv: []string{"x"}})
	if err == nil {
		t.Fatal("want error when the sandbox is missing, got nil")
	}
}
