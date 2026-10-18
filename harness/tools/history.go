// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"fmt"
	"strings"

	"latere.ai/x/topos/prompts"
)

// NameHistory is the tool of spec 084 that reads back the calling
// thread's own log, a built-in an agent holds only when it names it.
const NameHistory = "history"

// historySchema is the history tool's input schema. Every field is
// optional here; which of them a read may name together is checked when
// the call runs, so the answer says what is wrong.
const historySchema = `{
  "type": "object",
  "properties": {
    "from": {"type": "integer", "minimum": 1, "description": "The number of the first event to read; the conversation's first when omitted."},
    "to": {"type": "integer", "minimum": 1, "description": "The number of the last event to read; the one before this call when omitted."},
    "tool_use_id": {"type": "string", "minLength": 1, "description": "The id of one call to read with its result, alone."},
    "query": {"type": "string", "minLength": 1, "description": "Words to find, case ignored; from and to narrow where."}
  },
  "additionalProperties": false
}`

// HistoryRead is one read of the history tool: the events from From to
// To, either 0 for no bound; one call and its result, by ToolUseID; or
// the events whose text holds Query, From and To narrowing them. Its
// JSON is the input that makes the read, which an answer that stops
// early closes with.
type HistoryRead struct {
	From      uint64 `json:"from,omitempty"`
	To        uint64 `json:"to,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	Query     string `json:"query,omitempty"`
}

// HistoryReader answers one read from the log of the thread that made
// the call.
type HistoryReader func(ctx context.Context, c Call, r HistoryRead) (string, error)

// History is the history tool, whose reads read answers. The harness
// gives each thread its own, since the log it reads is the harness's and
// not the machine's: a call changes nothing, never opens a machine, may
// run beside the step's other calls, and may run again after a restart,
// since it answers the same from the same log.
func History(read HistoryReader) Tool {
	return newBuiltin(NameHistory, prompts.Text(prompts.ToolHistory), historySchema, Properties{Parallel: true, Repeatable: true, Effect: EffectNone},
		func(ctx context.Context, b *builtin, c Call) (Result, error) { return runHistory(ctx, b, c, read) })
}

// runHistory checks a read and answers it. Its text is bounded by the
// reader, so it is never capped here: a spill file would open a machine.
func runHistory(ctx context.Context, b *builtin, c Call, read HistoryReader) (Result, error) {
	var in HistoryRead
	if r := b.decode(c, &in); r != nil {
		return *r, nil
	}
	if problem := in.check(); problem != "" {
		return Text(OutcomeInvalidInput, invalidInput(NameHistory, problem)), nil
	}
	in.ToolUseID, in.Query = strings.TrimSpace(in.ToolUseID), strings.TrimSpace(in.Query)
	text, err := read(ctx, c, in)
	if err != nil {
		return Result{}, err
	}
	return Text(OutcomeOK, text), nil
}

// check is the problem of a read that names no one shape, or "": a call
// read with anything else, a blank call or query, and a range whose
// start is past its end.
func (r HistoryRead) check() string {
	switch {
	case r.ToolUseID != "" && (r.From != 0 || r.To != 0 || r.Query != ""):
		return "/tool_use_id: a call is read alone, without from, to or query"
	case r.ToolUseID != "" && strings.TrimSpace(r.ToolUseID) == "":
		return "/tool_use_id: empty"
	case r.Query != "" && strings.TrimSpace(r.Query) == "":
		return "/query: empty"
	case r.To != 0 && r.From > r.To:
		return fmt.Sprintf("/from: %d is past to, %d", r.From, r.To)
	}
	return ""
}
