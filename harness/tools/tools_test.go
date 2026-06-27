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

func TestBashToolSuccessful(t *testing.T) {
	p := local.New()
	ctx := context.Background()
	sb, err := p.Create(ctx, sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	bt := &tools.BashTool{}
	input := json.RawMessage(`{"command":"echo hello-world"}`)
	res, err := bt.Invoke(ctx, input, p, sb.ID)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "hello-world") {
		t.Fatalf("output = %q, want 'hello-world'", res.Content)
	}
}

func TestBashToolNonzeroExit(t *testing.T) {
	p := local.New()
	ctx := context.Background()
	sb, err := p.Create(ctx, sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	bt := &tools.BashTool{}
	input := json.RawMessage(`{"command":"exit 1"}`)
	res, err := bt.Invoke(ctx, input, p, sb.ID)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true on non-zero exit")
	}
}

func TestBashToolInvalidJSON(t *testing.T) {
	p := local.New()
	ctx := context.Background()
	sb, err := p.Create(ctx, sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	bt := &tools.BashTool{}
	res, err := bt.Invoke(ctx, json.RawMessage(`not json`), p, sb.ID)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true on invalid JSON")
	}
}

func TestRegistryDefsOrder(t *testing.T) {
	r := tools.Builtins()
	defs := r.Defs()
	if len(defs) == 0 {
		t.Fatal("expected at least one tool def")
	}
	if defs[0].Name != "bash" {
		t.Fatalf("first tool = %q, want bash", defs[0].Name)
	}
}

func TestRegistryGetMissing(t *testing.T) {
	r := tools.NewRegistry()
	if r.Get("no-such") != nil {
		t.Fatal("expected nil for unknown tool")
	}
}

func TestRegistryRegisterDuplicatePanics(t *testing.T) {
	r := tools.NewRegistry()
	r.Register(&tools.BashTool{})
	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatal("expected panic on duplicate tool name")
		}
		if msg, ok := rec.(string); !ok || !strings.Contains(msg, "bash") {
			t.Fatalf("panic = %v, want message naming the duplicate tool 'bash'", rec)
		}
	}()
	r.Register(&tools.BashTool{}) // same Name() → must panic.
}

func TestBashToolEmptyCommand(t *testing.T) {
	p := local.New()
	ctx := context.Background()
	sb, err := p.Create(ctx, sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	bt := &tools.BashTool{}
	// Whitespace-only command is rejected before any sandbox exec.
	res, err := bt.Invoke(ctx, json.RawMessage(`{"command":"   "}`), p, sb.ID)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "empty") {
		t.Fatalf("res = %+v, want IsError with 'empty' message", res)
	}
}

func TestBashToolSandboxExecError(t *testing.T) {
	p := local.New()
	ctx := context.Background()
	bt := &tools.BashTool{}
	// Exec against a sandbox that was never created → provider returns an error,
	// which Invoke surfaces as an error ToolResult (not a Go error).
	res, err := bt.Invoke(ctx, json.RawMessage(`{"command":"echo hi"}`), p, "no-such-sandbox")
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "exec error") {
		t.Fatalf("res = %+v, want IsError with 'exec error' message", res)
	}
}
