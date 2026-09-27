// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"errors"
	"fmt"

	"latere.ai/x/cella/client"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/models"
)

// The failures of spec 009's error codes. Every error the machine returns
// for one of them wraps it, so Code names the code.
var (
	// ErrUnavailable is a machine that cannot be had: Cella is not
	// configured, or it refused the sandbox, its Environment, its image
	// or a secret it names. It is not retryable.
	ErrUnavailable = errors.New("machine: the cella machine is unavailable")
	// ErrLost is a sandbox that is gone. The machine answers it for every
	// call until Recreate.
	ErrLost = errors.New("machine: the session's sandbox is gone")
)

// Code is the spec 009 code of an error: machine_unavailable,
// machine_lost, or empty for any other failure, a transient one included.
func Code(err error) string {
	switch {
	case errors.Is(err, ErrLost):
		return machine.CodeLost
	case errors.Is(err, ErrUnavailable):
		return machine.CodeUnavailable
	}
	return ""
}

// refused names a failed call to Cella. A refusal for spend is a
// models.SpendError whatever its status, which stops the turn with
// budget (spec 007). Any other refusal, any 4xx but a rate limit, is
// ErrUnavailable: the same request would be refused again. A server
// that could not answer, a 5xx, a 429 or no answer at all, is transient
// and carries no code.
func refused(what string, err error) error {
	if se, ok := spent(what, err); ok {
		return se
	}
	var ce *client.Error
	if errors.As(err, &ce) && ce.Status >= 400 && ce.Status < 500 && ce.Status != 429 {
		return fmt.Errorf("%w: Cella refused to %s: %w", ErrUnavailable, what, refusal{ce})
	}
	return fmt.Errorf("machine: %s: %w", what, err)
}

// spent is err as a models.SpendError when Cella refused the call
// because the allowance the installation's authorizer gave the session
// for sandbox time is spent: budget_exhausted or spend_exceeded, which
// no retry and no other sandbox brings back.
func spent(what string, err error) (error, bool) {
	var ce *client.Error
	if !errors.As(err, &ce) || !models.SpendCode(ce.Code) {
		return nil, false
	}
	return &models.SpendError{Core: machine.KindCella, Code: ce.Code, Err: fmt.Errorf("Cella refused to %s: %w", what, refusal{ce})}, true
}

// refusal renders Cella's refusal with its code and the developer
// sentence, which say more than the fixed user sentence alone, and keeps
// the refusal in the chain.
type refusal struct{ e *client.Error }

func (r refusal) Error() string {
	s := r.e.Message
	if r.e.Code != "" {
		s += " (" + r.e.Code
		if r.e.Detail != "" {
			s += ": " + r.e.Detail
		}
		s += ")"
	}
	return s
}

func (r refusal) Unwrap() error { return r.e }
