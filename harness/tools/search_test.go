// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/sandbox"
)

type rgAbsentProvider struct{ sandbox.Provider }

func (p rgAbsentProvider) Exec(ctx context.Context, id string, opts sandbox.ExecOptions) (sandbox.ExecResult, error) {
	if len(opts.Argv) > 0 && opts.Argv[0] == "rg" {
		return sandbox.ExecResult{Phase: "exited", ExitCode: 127}, nil
	}
	return p.Provider.Exec(ctx, id, opts)
}

func TestGrepFindsMatches(t *testing.T) {
	p, id := fileToolsFixture(t)
	ctx := context.Background()
	w := &tools.WriteFileTool{}
	w.Invoke(ctx, json.RawMessage(`{"path":"a.go","content":"package a\n// TODO: fix\nfunc A(){}\n"}`), p, id) //nolint:errcheck
	w.Invoke(ctx, json.RawMessage(`{"path":"b.txt","content":"nothing here\n"}`), p, id)                       //nolint:errcheck

	res, err := (&tools.GrepTool{}).Invoke(ctx, json.RawMessage(`{"pattern":"TODO","path":"."}`), p, id)
	if err != nil || res.IsError {
		t.Fatalf("grep: err=%v res=%+v", err, res)
	}
	if !strings.Contains(res.Content, "a.go") || !strings.Contains(res.Content, "TODO") {
		t.Fatalf("grep content should reference a.go and TODO:\n%s", res.Content)
	}
}

func TestGrepNoMatches(t *testing.T) {
	p, id := fileToolsFixture(t)
	ctx := context.Background()
	(&tools.WriteFileTool{}).Invoke(ctx, json.RawMessage(`{"path":"a.txt","content":"hello\n"}`), p, id) //nolint:errcheck

	res, err := (&tools.GrepTool{}).Invoke(ctx, json.RawMessage(`{"pattern":"zzzznotfound","path":"."}`), p, id)
	if err != nil || res.IsError {
		t.Fatalf("grep no-match should not error: err=%v res=%+v", err, res)
	}
	if !strings.Contains(res.Content, "no matches") {
		t.Fatalf("expected a no-matches message, got:\n%s", res.Content)
	}
}

func TestGrepEmptyPattern(t *testing.T) {
	p, id := fileToolsFixture(t)
	res, _ := (&tools.GrepTool{}).Invoke(context.Background(), json.RawMessage(`{"pattern":"  "}`), p, id)
	if !res.IsError {
		t.Fatalf("empty pattern should be an error: %+v", res)
	}
}

func TestSearchToolsNonExitedCommandIsError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tool  tools.Tool
		input json.RawMessage
	}{
		{name: "grep", tool: &tools.GrepTool{}, input: json.RawMessage(`{"pattern":"needle"}`)},
		{name: "glob", tool: &tools.GlobTool{}, input: json.RawMessage(`{"pattern":"*.go"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixedExecProvider{result: sandbox.ExecResult{Phase: "killed", ExitCode: 0, Stdout: []byte("partial")}}
			res, err := tc.tool.Invoke(context.Background(), tc.input, p, "sandbox")
			if err != nil {
				t.Fatalf("invoke: %v", err)
			}
			if !res.IsError || !strings.Contains(res.Content, "killed") {
				t.Fatalf("result = %+v, want an error naming the killed phase", res)
			}
		})
	}
}

func TestGlobFindsFiles(t *testing.T) {
	p, id := fileToolsFixture(t)
	ctx := context.Background()
	w := &tools.WriteFileTool{}
	w.Invoke(ctx, json.RawMessage(`{"path":"main.go","content":"package main\n"}`), p, id) //nolint:errcheck
	w.Invoke(ctx, json.RawMessage(`{"path":"readme.md","content":"# hi\n"}`), p, id)       //nolint:errcheck

	res, err := (&tools.GlobTool{}).Invoke(ctx, json.RawMessage(`{"pattern":"*.go","path":"."}`), p, id)
	if err != nil || res.IsError {
		t.Fatalf("glob: err=%v res=%+v", err, res)
	}
	if !strings.Contains(res.Content, "main.go") {
		t.Fatalf("glob should list main.go:\n%s", res.Content)
	}
	if strings.Contains(res.Content, "readme.md") {
		t.Fatalf("glob *.go should not list readme.md:\n%s", res.Content)
	}
}

func TestGlobNoMatch(t *testing.T) {
	p, id := fileToolsFixture(t)
	res, err := (&tools.GlobTool{}).Invoke(context.Background(), json.RawMessage(`{"pattern":"*.nonesuch","path":"."}`), p, id)
	if err != nil || res.IsError {
		t.Fatalf("glob no-match should not error: err=%v res=%+v", err, res)
	}
	if !strings.Contains(res.Content, "no files") {
		t.Fatalf("expected a no-files message, got:\n%s", res.Content)
	}
}

func TestGlobFindFallbackTreatsExitOneAsError(t *testing.T) {
	base, id := fileToolsFixture(t)
	p := rgAbsentProvider{Provider: base}

	res, err := (&tools.GlobTool{}).Invoke(context.Background(), json.RawMessage(`{"pattern":"*.go","path":"does-not-exist-xyz"}`), p, id)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !res.IsError {
		t.Fatalf("find diagnostic was returned as a successful match: %q", res.Content)
	}
	if !strings.Contains(res.Content, "does-not-exist-xyz") {
		t.Fatalf("error does not identify the invalid path: %q", res.Content)
	}
}

func TestSearchToolsInBuiltins(t *testing.T) {
	r := tools.Builtins()
	for _, name := range []string{"grep", "glob"} {
		if r.Get(name) == nil {
			t.Errorf("Builtins() missing %q", name)
		}
	}
}
