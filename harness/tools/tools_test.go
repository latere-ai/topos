// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/latere-ai/topos/harness/tools"
	"github.com/latere-ai/topos/sandbox"
	"github.com/latere-ai/topos/sandbox/local"
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
