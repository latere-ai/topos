// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package sandbox_test

import (
	"context"
	"errors"
	"testing"

	"latere.ai/x/topos/sandbox"
)

func TestConsentAllowsWhenApproved(t *testing.T) {
	spy := &spyProvider{}
	var prompted []string
	c := sandbox.Consent(spy, func(_ context.Context, _ string, o sandbox.ExecOptions) error {
		prompted = append(prompted, o.Argv[0])
		return nil // approve
	})
	if _, err := c.Exec(context.Background(), "sb", sandbox.ExecOptions{Argv: []string{"ls"}}); err != nil {
		t.Fatalf("approved exec refused: %v", err)
	}
	if len(spy.execCwds) != 1 {
		t.Fatalf("approved exec did not reach inner provider")
	}
	if len(prompted) != 1 || prompted[0] != "ls" {
		t.Fatalf("decider saw %v, want [ls]", prompted)
	}
}

func TestConsentDeniesWhenRefused(t *testing.T) {
	spy := &spyProvider{}
	refusal := errors.New("user said no")
	c := sandbox.Consent(spy, func(context.Context, string, sandbox.ExecOptions) error {
		return refusal
	})
	_, err := c.Exec(context.Background(), "sb", sandbox.ExecOptions{Argv: []string{"rm", "-rf", "/"}})
	if !errors.Is(err, sandbox.ErrConsentDenied) {
		t.Fatalf("denied exec = %v, want ErrConsentDenied", err)
	}
	if !errors.Is(err, refusal) {
		t.Fatalf("denial did not wrap the decider's reason: %v", err)
	}
	if len(spy.execCwds) != 0 {
		t.Fatalf("a denied exec reached the inner provider")
	}
}

func TestConsentGatesStreamExec(t *testing.T) {
	spy := &spyProvider{}
	c := sandbox.Consent(spy, func(context.Context, string, sandbox.ExecOptions) error {
		return errors.New("no")
	})
	if _, err := c.StreamExec(context.Background(), "sb", sandbox.ExecOptions{Argv: []string{"tail", "-f", "x"}}); !errors.Is(err, sandbox.ErrConsentDenied) {
		t.Fatalf("denied stream exec = %v, want ErrConsentDenied", err)
	}
}

func TestConsentNilDeciderAllowsAll(t *testing.T) {
	spy := &spyProvider{}
	c := sandbox.Consent(spy, nil) // no decider = allow
	if _, err := c.Exec(context.Background(), "sb", sandbox.ExecOptions{Argv: []string{"ls"}}); err != nil {
		t.Fatalf("nil-decider exec refused: %v", err)
	}
	if _, err := c.StreamExec(context.Background(), "sb", sandbox.ExecOptions{Argv: []string{"tail"}}); err != nil {
		t.Fatalf("nil-decider stream exec refused: %v", err)
	}
}

func TestConsentPassesThroughNonExecMethods(t *testing.T) {
	spy := &spyProvider{}
	denyAll := func(context.Context, string, sandbox.ExecOptions) error { return errors.New("no") }
	c := sandbox.Consent(spy, denyAll)
	// Consent gates execution, not file/lifecycle methods.
	if _, err := c.Create(context.Background(), sandbox.CreateOptions{}); err != nil {
		t.Fatalf("Create gated by consent: %v", err)
	}
	if _, err := c.ReadFile(context.Background(), "sb", "a.txt"); err != nil {
		t.Fatalf("ReadFile gated by consent: %v", err)
	}
	if err := c.WriteFile(context.Background(), "sb", "a.txt", nil); err != nil {
		t.Fatalf("WriteFile gated by consent: %v", err)
	}
	if _, err := c.ListFiles(context.Background(), "sb", "."); err != nil {
		t.Fatalf("ListFiles gated by consent: %v", err)
	}
	if err := c.Destroy(context.Background(), "sb"); err != nil {
		t.Fatalf("Destroy gated by consent: %v", err)
	}
	if err := c.HealthCheck(context.Background(), "sb"); err != nil {
		t.Fatalf("HealthCheck gated by consent: %v", err)
	}
	if spy.created != 1 || len(spy.reads) != 1 || len(spy.writes) != 1 || len(spy.lists) != 1 || spy.destroyed != 1 || spy.healths != 1 {
		t.Fatalf("non-exec methods did not all pass through: %+v", spy)
	}
}
