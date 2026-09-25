// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"context"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"

	"latere.ai/x/topos/sandbox"
)

func TestStateOfEveryPhase(t *testing.T) {
	for phase, want := range map[string]sandbox.State{
		"Pending": sandbox.StateCreating, "Queued": sandbox.StateCreating,
		"Starting": sandbox.StateCreating, "Recovering": sandbox.StateCreating,
		"Running": sandbox.StateRunning, "Stopped": sandbox.StateStopped,
		"Deleting": sandbox.StateDeleting, "Failed": sandbox.StateError,
		"Lost": sandbox.StateError, "": sandbox.StateError,
	} {
		if got := stateOf(phase); got != want {
			t.Errorf("stateOf(%q) = %q, want %q", phase, got, want)
		}
	}
}

func TestTierOf(t *testing.T) {
	for ttl, want := range map[v1.Duration]string{"": "persistent", v1.DurationNever: "persistent", "24h": "ephemeral"} {
		if got := tierOf(v1.Lifecycle{TTL: ttl}); got != want {
			t.Errorf("tierOf(ttl %q) = %q, want %q", ttl, got, want)
		}
	}
}

func TestResolvePath(t *testing.T) {
	for in, want := range map[string]string{
		"": "/workspace", ".": "/workspace", "a/b": "/workspace/a/b", "./a": "/workspace/a",
		"/workspace/a": "/workspace/a", "/tmp/../workspace/b/": "/workspace/b", "../etc": "/etc",
	} {
		if got := resolvePath(in); got != want {
			t.Errorf("resolvePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCreateHoldBounds: a hold never falls below a second, and never exceeds
// the control plane's own default.
func TestCreateHoldBounds(t *testing.T) {
	if got := createHold(context.Background()); got != maxCreateHold {
		t.Errorf("no deadline = %v, want %v", got, maxCreateHold)
	}
	short, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if got := createHold(short); got != time.Second {
		t.Errorf("2s deadline = %v, want 1s", got)
	}
	long, cancel2 := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel2()
	if got := createHold(long); got != maxCreateHold {
		t.Errorf("2h deadline = %v, want %v", got, maxCreateHold)
	}
}

func TestExecTimeoutBounds(t *testing.T) {
	long, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()
	if got, ok := execTimeout(long); !ok || got != maxExecTimeout {
		t.Errorf("3h deadline = %v, %v, want %v", got, ok, maxExecTimeout)
	}
}
