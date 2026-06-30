// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package topos

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"latere.ai/x/topos/sandbox"
	"latere.ai/x/topos/sandbox/local"
)

// TestProviderHonorsWorkdir verifies Options.Workdir roots the default local
// provider at the given directory, so a run's tools execute there instead of a
// temp dir. This is the zero-import seam an embedding host uses to run in a git
// worktree without depending on the sandbox subpackage.
func TestProviderHonorsWorkdir(t *testing.T) {
	root := t.TempDir()
	r, err := NewRunner(Options{SessionID: "wd", Model: ModelOptions{Kind: ModelFake}, Workdir: root})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	ctx := context.Background()
	p := r.provider()
	sb, err := p.Create(ctx, sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := p.Exec(ctx, sb.ID, sandbox.ExecOptions{Argv: []string{"sh", "-c", "echo hi > wd_marker.txt"}}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "wd_marker.txt")); err != nil {
		t.Errorf("Workdir not honored: marker not written under root: %v", err)
	}
}

// TestProviderInjectedSandboxWinsOverWorkdir verifies an explicitly injected
// Sandbox backend takes precedence over Workdir (the backend owns execution).
func TestProviderInjectedSandboxWinsOverWorkdir(t *testing.T) {
	sentinel := local.New()
	r, err := NewRunner(Options{
		SessionID: "s", Model: ModelOptions{Kind: ModelFake},
		Workdir: "/tmp/ignored", Sandbox: sentinel,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if r.provider() != sentinel {
		t.Error("injected Sandbox must win over Workdir")
	}
}

// TestProviderDefaultWhenNeither verifies the zero-config path: no Sandbox and no
// Workdir yields a working temp-dir provider.
func TestProviderDefaultWhenNeither(t *testing.T) {
	r, err := NewRunner(Options{SessionID: "d", Model: ModelOptions{Kind: ModelFake}})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if r.provider() == nil {
		t.Error("provider() must never be nil")
	}
}
