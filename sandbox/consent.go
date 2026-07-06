// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package sandbox

import (
	"context"
	"errors"
	"fmt"
)

// ErrConsentDenied is returned by a Consent-wrapped Provider when the consent
// decider refuses a command. Callers errors.Is it to distinguish a user/policy
// refusal from a backend failure.
var ErrConsentDenied = errors.New("sandbox: exec denied by consent policy")

// ConsentFunc decides whether a command may run. It returns nil to allow, or a
// non-nil error to deny (the reason is wrapped into ErrConsentDenied). It runs on
// the machine that owns the sandbox — for mode 2 (interactive-session-modes trust
// protection #3) that is the laptop, where it prompts the user locally before a
// remote session executes anything on the real machine.
type ConsentFunc func(ctx context.Context, id string, opts ExecOptions) error

// consented wraps a Provider so every Exec and StreamExec first passes the consent
// decider. All other methods pass straight through — consent gates execution, not
// file reads (those are governed by Confine's deny-list + root).
type consented struct {
	inner  Provider
	decide ConsentFunc
}

// Consent wraps inner so each Exec/StreamExec is gated by decide. A nil decider
// allows everything (a no-op wrapper), so callers can compose unconditionally.
// Intended composition (mode 2): Serve(conn, Consent(Confine(host, root), prompt)).
func Consent(inner Provider, decide ConsentFunc) Provider {
	return &consented{inner: inner, decide: decide}
}

// gate runs the decider (if any) and maps a refusal onto ErrConsentDenied.
func (c *consented) gate(ctx context.Context, id string, opts ExecOptions) error {
	if c.decide == nil {
		return nil
	}
	if err := c.decide(ctx, id, opts); err != nil {
		return fmt.Errorf("%w: %w", ErrConsentDenied, err)
	}
	return nil
}

func (c *consented) Create(ctx context.Context, opts CreateOptions) (Sandbox, error) {
	return c.inner.Create(ctx, opts)
}

func (c *consented) Destroy(ctx context.Context, id string) error {
	return c.inner.Destroy(ctx, id)
}

func (c *consented) Exec(ctx context.Context, id string, opts ExecOptions) (ExecResult, error) {
	if err := c.gate(ctx, id, opts); err != nil {
		return ExecResult{}, err
	}
	return c.inner.Exec(ctx, id, opts)
}

func (c *consented) StreamExec(ctx context.Context, id string, opts ExecOptions) (ExecStream, error) {
	if err := c.gate(ctx, id, opts); err != nil {
		return nil, err
	}
	return c.inner.StreamExec(ctx, id, opts)
}

func (c *consented) ReadFile(ctx context.Context, id, path string) ([]byte, error) {
	return c.inner.ReadFile(ctx, id, path)
}

func (c *consented) WriteFile(ctx context.Context, id, path string, data []byte) error {
	return c.inner.WriteFile(ctx, id, path, data)
}

func (c *consented) ListFiles(ctx context.Context, id, path string) ([]FileInfo, error) {
	return c.inner.ListFiles(ctx, id, path)
}

func (c *consented) HealthCheck(ctx context.Context, id string) error {
	return c.inner.HealthCheck(ctx, id)
}
