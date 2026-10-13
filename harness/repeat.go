// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// MaxRepeatedCalls is how many times a thread's turn makes a call again
// after it was answered with an error, unchanged and answered with the
// same error each time, before the turn ends (spec 005). The first such
// answer is the call's own; each of the MaxRepeatedCalls repeats after it
// is answered with that error and prompts.CallRepeated, and the turn ends
// with CodeRepeatedCall at the boundary after the step that made the
// last, so a call answered so MaxRepeatedCalls+1 times ends it. A model
// that sends a refused call again unchanged reads the same refusal, so a
// turn left to loop spends a whole request on every repeat until its
// budget runs out.
const MaxRepeatedCalls = 3

// CodeRepeatedCall ends a turn whose thread made one call
// MaxRepeatedCalls times more after it was answered with an error, with
// the same error each time. The session.error carries
// MessageRepeatedCall, and the call's tool, its count, the outcome and
// the last call's tool_use id in its detail; a person's next message
// starts a turn as any does.
const CodeRepeatedCall = "repeated_call"

// MessageRepeatedCall is the one sentence of CodeRepeatedCall, for a
// person.
const MessageRepeatedCall = "The agent kept repeating an action that failed the same way each time, so it stopped. Tell it what to do next."

// repeats is what one thread's turn knows of its calls, to tell a call
// made again unchanged after an error from one that changed. Each thread
// runs its own turn, so each counts its own calls. The count lives with
// the turn, as the count of continuations does: a turn a later claim
// starts counts afresh. Only the turn's own goroutine reads and writes
// it: the results of parallel calls are appended one at a time.
type repeats struct {
	// calls is each call a response of the turn made, and each call the
	// turn settles from an earlier one, by its tool_use id.
	calls map[string]madeCall
	// errors counts the error answers each call got, by the call's
	// identity and then the answer's.
	errors map[[sha256.Size]byte]map[[sha256.Size]byte]int
	// stuck is the detail of the call that reached MaxRepeatedCalls, ""
	// while none did.
	stuck string
}

// madeCall is one call: its tool's name, and its identity, the SHA-256
// of the name and the input in canonical form.
type madeCall struct {
	name     string
	identity [sha256.Size]byte
}

// made records a call, by the input the harness checks it against.
func (r *repeats) made(id, name string, input []byte) {
	if r.calls == nil {
		r.calls = map[string]madeCall{}
	}
	r.calls[id] = madeCall{name: name, identity: sha256.Sum256(slices.Concat([]byte(name), []byte{0}, canonicalInput(input)))}
}

// answered records the answer a call got and returns how many times the
// same call got the same error answer before it in the turn: 0 for the
// first, for a call the turn did not record, and for an answer that is no
// error. An answer that is no error clears the call's count, since a call
// that succeeded may succeed again.
func (r *repeats) answered(id string, res tools.Result) (madeCall, int, error) {
	c, ok := r.calls[id]
	if !ok {
		return madeCall{}, 0, nil
	}
	if !res.IsError() {
		delete(r.errors, c.identity)
		return c, 0, nil
	}
	content, err := json.Marshal(res.Content)
	if err != nil {
		return c, 0, fmt.Errorf("harness: encode the answer of %s: %w", id, err)
	}
	answer := sha256.Sum256(slices.Concat([]byte(res.Outcome), []byte{0}, content))
	if r.errors == nil {
		r.errors = map[[sha256.Size]byte]map[[sha256.Size]byte]int{}
	}
	if r.errors[c.identity] == nil {
		r.errors[c.identity] = map[[sha256.Size]byte]int{}
	}
	n := r.errors[c.identity][answer]
	r.errors[c.identity][answer] = n + 1
	return c, n, nil
}

// canonicalInput is a call's input as one JSON value, compact and with
// its object keys sorted, so a call sent again with other spacing or key
// order is the same call. Input that is not one JSON value, as the text
// of a call whose arguments broke off, is compared as it was sent.
func canonicalInput(input []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return input
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return input
	}
	b, err := json.Marshal(v)
	if err != nil {
		return input
	}
	return b
}

// repeated is the answer a call gets: res, and after it
// prompts.CallRepeated when the same call got the same error earlier in
// the thread's turn. The answer that makes the call's MaxRepeatedCalls-th
// repeat marks the turn to end at the next step boundary, once every call
// of its step is answered.
func (t *turn) repeated(id string, res tools.Result) (tools.Result, error) {
	c, n, err := t.repeats.answered(id, res)
	if err != nil || n == 0 {
		return res, err
	}
	res.Content = append(slices.Clip(res.Content), lux.Block{Type: ir.BlockText, Text: prompts.Text(prompts.CallRepeated)})
	if n >= MaxRepeatedCalls && t.repeats.stuck == "" {
		t.repeats.stuck = fmt.Sprintf("%s was called %d times in the turn with the same input and answered %s with the same content each time; the last call is %s", c.name, n+1, res.Outcome, id)
	}
	return res, nil
}

// stopRepeated ends a turn marked by repeated with CodeRepeatedCall: idle
// with error, as a model's failure ends one.
func (t *turn) stopRepeated(ctx context.Context) error {
	se, err := t.sessionError(CodeRepeatedCall, MessageRepeatedCall, false, t.repeats.stuck)
	if err != nil {
		return err
	}
	return t.finish(ctx, session.StopError, CodeRepeatedCall, se)
}
