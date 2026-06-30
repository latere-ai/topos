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
)

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

func TestSearchToolsInBuiltins(t *testing.T) {
	r := tools.Builtins()
	for _, name := range []string{"grep", "glob"} {
		if r.Get(name) == nil {
			t.Errorf("Builtins() missing %q", name)
		}
	}
}
