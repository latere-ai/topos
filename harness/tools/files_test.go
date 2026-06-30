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
	"latere.ai/x/topos/sandbox/local"
)

// fileToolsFixture spins a local sandbox and returns it ready for file-tool tests.
func fileToolsFixture(t *testing.T) (sandbox.Provider, string) {
	t.Helper()
	p := local.New()
	sb, err := p.Create(context.Background(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { _ = p.Destroy(context.Background(), sb.ID) })
	return p, sb.ID
}

func TestWriteThenReadFile(t *testing.T) {
	p, id := fileToolsFixture(t)
	ctx := context.Background()

	w := &tools.WriteFileTool{}
	res, err := w.Invoke(ctx, json.RawMessage(`{"path":"/tmp/note.txt","content":"alpha\nbeta\ngamma"}`), p, id)
	if err != nil || res.IsError {
		t.Fatalf("write: err=%v res=%+v", err, res)
	}

	r := &tools.ReadFileTool{}
	res, err = r.Invoke(ctx, json.RawMessage(`{"path":"/tmp/note.txt"}`), p, id)
	if err != nil || res.IsError {
		t.Fatalf("read: err=%v res=%+v", err, res)
	}
	// Line-numbered output (cat -n style) so the model can reference lines.
	if !strings.Contains(res.Content, "     1\talpha") || !strings.Contains(res.Content, "     3\tgamma") {
		t.Fatalf("read content missing numbered lines:\n%s", res.Content)
	}
}

func TestReadFileOffsetLimit(t *testing.T) {
	p, id := fileToolsFixture(t)
	ctx := context.Background()
	(&tools.WriteFileTool{}).Invoke(ctx, json.RawMessage(`{"path":"/tmp/x","content":"l1\nl2\nl3\nl4\nl5"}`), p, id) //nolint:errcheck

	res, _ := (&tools.ReadFileTool{}).Invoke(ctx, json.RawMessage(`{"path":"/tmp/x","offset":2,"limit":2}`), p, id)
	if !strings.Contains(res.Content, "     2\tl2") || !strings.Contains(res.Content, "     3\tl3") {
		t.Fatalf("offset/limit content wrong:\n%s", res.Content)
	}
	if strings.Contains(res.Content, "     1\tl1") || strings.Contains(res.Content, "     5\tl5") {
		t.Fatalf("offset/limit returned out-of-window lines:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "more lines") {
		t.Fatalf("expected a continuation hint:\n%s", res.Content)
	}
}

func TestReadFileNotFound(t *testing.T) {
	p, id := fileToolsFixture(t)
	res, err := (&tools.ReadFileTool{}).Invoke(context.Background(), json.RawMessage(`{"path":"/tmp/nope"}`), p, id)
	if err != nil {
		t.Fatalf("invoke returned a Go error, want IsError result: %v", err)
	}
	if !res.IsError {
		t.Fatalf("reading a missing file should be an error result: %+v", res)
	}
}

func TestEditFileUnique(t *testing.T) {
	p, id := fileToolsFixture(t)
	ctx := context.Background()
	(&tools.WriteFileTool{}).Invoke(ctx, json.RawMessage(`{"path":"/tmp/c.go","content":"package main\nfunc main(){}\n"}`), p, id) //nolint:errcheck

	res, err := (&tools.EditFileTool{}).Invoke(ctx, json.RawMessage(`{"path":"/tmp/c.go","old_string":"func main(){}","new_string":"func main(){ println(1) }"}`), p, id)
	if err != nil || res.IsError {
		t.Fatalf("edit: err=%v res=%+v", err, res)
	}
	data, _ := p.ReadFile(ctx, id, "/tmp/c.go")
	if !strings.Contains(string(data), "println(1)") {
		t.Fatalf("edit not applied: %s", data)
	}
}

func TestEditFileNotFoundAndNonUnique(t *testing.T) {
	p, id := fileToolsFixture(t)
	ctx := context.Background()
	(&tools.WriteFileTool{}).Invoke(ctx, json.RawMessage(`{"path":"/tmp/d","content":"x\nx\n"}`), p, id) //nolint:errcheck

	// Not found.
	res, _ := (&tools.EditFileTool{}).Invoke(ctx, json.RawMessage(`{"path":"/tmp/d","old_string":"zzz","new_string":"y"}`), p, id)
	if !res.IsError || !strings.Contains(res.Content, "not found") {
		t.Fatalf("expected not-found error, got %+v", res)
	}
	// Non-unique without replace_all.
	res, _ = (&tools.EditFileTool{}).Invoke(ctx, json.RawMessage(`{"path":"/tmp/d","old_string":"x","new_string":"y"}`), p, id)
	if !res.IsError || !strings.Contains(res.Content, "not unique") {
		t.Fatalf("expected not-unique error, got %+v", res)
	}
	// replace_all succeeds.
	res, _ = (&tools.EditFileTool{}).Invoke(ctx, json.RawMessage(`{"path":"/tmp/d","old_string":"x","new_string":"y","replace_all":true}`), p, id)
	if res.IsError {
		t.Fatalf("replace_all should succeed: %+v", res)
	}
	data, _ := p.ReadFile(ctx, id, "/tmp/d")
	if strings.Contains(string(data), "x") {
		t.Fatalf("replace_all left an x: %q", data)
	}
}

func TestFileToolDefsValid(t *testing.T) {
	for _, tl := range []tools.Tool{&tools.ReadFileTool{}, &tools.WriteFileTool{}, &tools.EditFileTool{}} {
		def := tl.Def()
		if def.Name != tl.Name() {
			t.Errorf("Def().Name %q != Name() %q", def.Name, tl.Name())
		}
		var schema map[string]any
		if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
			t.Errorf("%s: invalid InputSchema JSON: %v", tl.Name(), err)
		}
	}
}
