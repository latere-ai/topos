// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"fmt"
)

// The reasons a machine waits (spec 080), which session.machine_waiting
// names: the person's computer it runs on is offline, or its owner has
// not allowed the session on it yet.
const (
	WaitOffline  = "offline"
	WaitApproval = "approval"
)

// CodeRefused is the error code of a machine whose computer's owner did
// not allow the session on it (spec 080).
const CodeRefused = "machine_refused"

// Waiting is a machine that cannot act now because something outside the
// session has to happen first (spec 080): the person's own computer it
// runs on is offline or asleep, or its owner has not allowed the session
// on it yet. Nothing of the machine is given up: a turn that meets it
// stops to wait, and the next message or a resume opens the machine
// again.
type Waiting struct {
	// Reason is WaitOffline or WaitApproval.
	Reason string
	// Environment is the Cella environment the computer serves.
	Environment string
	// Err is what said so, for a developer.
	Err error
}

func (w *Waiting) Error() string {
	what := "the computer is offline"
	if w.Reason == WaitApproval {
		what = "the computer's owner has not allowed the session yet"
	}
	if w.Err == nil {
		return fmt.Sprintf("machine: %s (%s)", what, w.Environment)
	}
	return fmt.Sprintf("machine: %s (%s): %v", what, w.Environment, w.Err)
}

func (w *Waiting) Unwrap() error { return w.Err }

// Refused is a machine whose computer's owner did not allow the session
// on it (spec 080): they refused it, or did not answer before the
// computer stopped waiting. Detail is the computer's own words.
type Refused struct {
	Environment string
	Detail      string
	// Timeout is set when the owner did not answer in time.
	Timeout bool
}

func (r *Refused) Error() string {
	what := "refused the session"
	if r.Timeout {
		what = "did not allow the session in time"
	}
	if r.Detail == "" {
		return fmt.Sprintf("machine: the computer's owner %s (%s)", what, r.Environment)
	}
	return fmt.Sprintf("machine: the computer's owner %s (%s): %s", what, r.Environment, r.Detail)
}

// Absent is the optional interface of a machine that can be away rather
// than gone, a person's own computer (spec 080). Absent reports why the
// machine cannot act now, nil while it can: after a call found it offline
// or lost it under a command, the turn stops to wait for it rather than
// going on without it.
type Absent interface {
	Absent() *Waiting
}

// AbsentOf is m's Absent, nil for a machine that is never away.
func AbsentOf(m Machine) *Waiting {
	if a, ok := m.(Absent); ok {
		return a.Absent()
	}
	return nil
}

// AsksAgain is the optional interface of a machine opened on demand that
// remembers what a person's computer answered (spec 080): after a refusal,
// or a wait, every open answers it again without asking the computer,
// and after the computer was lost a call answers it absent, until
// AskAgain lets the next operation open it anew.
type AsksAgain interface {
	AskAgain()
}

type waitingKey struct{}

// WithWaiting returns ctx carrying tell, which an open that begins to
// wait on something outside the session calls once it does (spec 080):
// the runner records it in the session, so a client shows the person what
// the machine waits for while the call is still held.
func WithWaiting(ctx context.Context, tell func(context.Context, Waiting) error) context.Context {
	return context.WithValue(ctx, waitingKey{}, tell)
}

// TellWaiting hands w to the function ctx carries, and does nothing when
// it carries none.
func TellWaiting(ctx context.Context, w Waiting) error {
	tell, _ := ctx.Value(waitingKey{}).(func(context.Context, Waiting) error)
	if tell == nil {
		return nil
	}
	return tell(ctx, w)
}
