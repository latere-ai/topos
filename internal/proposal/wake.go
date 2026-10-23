// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package proposal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/manifest/trigger"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// WakeName is the wake tool's name.
const WakeName = session.ToolWake

// The wake tool's actions.
const (
	WakeSet    = "set"
	WakeCancel = "cancel"
)

// wakeSchema is the wake tool's input, its bounds read from their
// constants.
var wakeSchema = json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": [%q, %q], "description": "set to set this conversation's next wake, replacing a pending one; cancel to cancel it."},
    "at": {"type": "string", "description": "For set, in place of after: an RFC 3339 time with an offset, such as 2026-10-24T09:00:00+02:00."},
    "after": {"type": "string", "description": "For set, in place of at: how long from now, in hours and minutes, such as 90m or 36h."},
    "title": {"type": "string", "maxLength": %d, "description": "For set: one line the person reads, what you will look for."},
    "note": {"type": "string", "maxLength": %d, "description": "For set: what you tell yourself, what to look at, what you knew and what to do with each answer."}
  },
  "required": ["action"],
  "additionalProperties": false
}`, WakeSet, WakeCancel, trigger.MaxWakeTitle, trigger.MaxWakeNote))

// Wake is the wake tool of one hosted session (spec 086): it sets the
// session's next wake, a one-shot trigger that sends the note the agent
// wrote back into the session at the time it chose, and cancels it. The
// bounds are manifest/trigger's, checked here before anything is decided
// and again by the server.
type Wake struct {
	t runner.Triggers
	// expires is when the session's life ends, which a wake lives inside.
	expires time.Time
	now     func() time.Time
}

// NewWake is the wake tool over t, the session's reach to its server's
// triggers, for a session whose life ends at expires, on the clock now.
func NewWake(t runner.Triggers, expires time.Time, now func() time.Time) *Wake {
	if now == nil {
		now = time.Now
	}
	return &Wake{t: t, expires: expires, now: now}
}

// Definition is what the model sees.
func (*Wake) Definition() tools.Definition {
	return tools.Definition{Name: WakeName, Description: prompts.Text(prompts.ToolWake), InputSchema: wakeSchema}
}

// Properties: one call at a time, a write.
func (*Wake) Properties() tools.Properties {
	return tools.Properties{Effect: tools.EffectWrite}
}

// wakeInput is a call's input.
type wakeInput struct {
	Action string `json:"action"`
	At     string `json:"at"`
	After  string `json:"after"`
	Title  string `json:"title"`
	Note   string `json:"note"`
}

func decodeWake(raw json.RawMessage) (wakeInput, error) {
	var in wakeInput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	err := dec.Decode(&in)
	return in, err
}

// when is the time a set asks for, from now: its at, or now and its
// after.
func (in wakeInput) when(now time.Time) (time.Time, error) {
	switch {
	case (in.At == "") == (in.After == ""):
		return time.Time{}, errors.New("a set names at or after, one of them")
	case in.At != "":
		at, err := time.Parse(time.RFC3339, in.At)
		if err != nil {
			return time.Time{}, fmt.Errorf("at %q is not an RFC 3339 time with an offset, such as 2026-10-24T09:00:00+02:00", in.At)
		}
		return at, nil
	}
	d, err := time.ParseDuration(in.After)
	if err != nil || d <= 0 || d%time.Minute != 0 {
		return time.Time{}, fmt.Errorf("after %q is not a time from now in hours and minutes, such as 90m or 36h", in.After)
	}
	return now.Add(d), nil
}

// check refuses a set's title and note past their bounds.
func (in wakeInput) check() error {
	title := strings.TrimSpace(in.Title)
	switch {
	case title == "":
		return errors.New("title is required: one line the person reads, what you will look for")
	case utf8.RuneCountInString(title) > trigger.MaxWakeTitle:
		return fmt.Errorf("title is longer than %d characters", trigger.MaxWakeTitle)
	case strings.ContainsFunc(title, func(r rune) bool { return unicode.IsControl(r) || r == ' ' || r == ' ' }):
		return errors.New("title is one line")
	case strings.TrimSpace(in.Note) == "":
		return errors.New("note is required: what you tell yourself, what to look at and what to do with each answer")
	case utf8.RuneCountInString(in.Note) > trigger.MaxWakeNote:
		return fmt.Errorf("note is longer than %d characters", trigger.MaxWakeNote)
	}
	return nil
}

// invalidWake is a call answered without running it.
func invalidWake(detail string) *tools.Result {
	r := tools.Text(tools.OutcomeInvalidInput, prompts.Render(prompts.WakeInvalid, prompts.Data{"Detail": detail}))
	return &r
}

// outOfBounds is a set outside its bounds, answered with the bound it
// passed.
func outOfBounds(err error) *tools.Result {
	we, ok := errors.AsType[*trigger.WakeError](err)
	if !ok {
		return invalidWake(err.Error())
	}
	var r tools.Result
	if we.Code == trigger.CodeTooSoon {
		r = tools.Text(tools.OutcomeInvalidInput, prompts.Render(prompts.WakeTooSoon, prompts.Data{"Code": we.Code, "Min": minutes(trigger.MinWakeDelay), "Earliest": stamp(we.Bound)}))
	} else {
		r = tools.Text(tools.OutcomeInvalidInput, prompts.Render(prompts.WakeTooLate, prompts.Data{"Code": we.Code, "Max": hours(trigger.MaxWakeAhead), "Latest": stamp(we.Bound)}))
	}
	return &r
}

// Check refuses, from its input, the session's life and the clock, a
// call that names no action, a set whose time, title or note is past its
// bounds, before anyone is asked to allow it.
func (w *Wake) Check(c tools.Call) *tools.Result {
	in, err := decodeWake(c.Input)
	if err != nil {
		return invalidWake(err.Error())
	}
	switch in.Action {
	case WakeCancel:
		return nil
	case WakeSet:
	default:
		return invalidWake(fmt.Sprintf("action is %q; it is %s or %s", in.Action, WakeSet, WakeCancel))
	}
	now := w.now()
	at, err := in.when(now)
	if err != nil {
		return invalidWake(err.Error())
	}
	if err := in.check(); err != nil {
		return invalidWake(err.Error())
	}
	if err := trigger.CheckWake(at, now, w.expires); err != nil {
		return outOfBounds(err)
	}
	return nil
}

// Confirm holds a set past MaxWakesWithoutPerson wakes in a row with no
// word from the person for that person (spec 086): their allow is their
// word, so the count starts again, and their deny leaves the session
// without a wake.
func (*Wake) Confirm(c tools.Call) string {
	in, err := decodeWake(c.Input)
	if err != nil || in.Action != WakeSet || c.State.Wakes < trigger.MaxWakesWithoutPerson {
		return ""
	}
	return prompts.Render(prompts.WakeMany, prompts.Data{"Count": c.State.Wakes + 1, "Max": trigger.MaxWakesWithoutPerson})
}

// Run sets or cancels the session's wake, and names it in the result for
// the harness to record.
func (w *Wake) Run(ctx context.Context, c tools.Call) (tools.Result, error) {
	in, err := decodeWake(c.Input)
	if err != nil {
		return *invalidWake(err.Error()), nil
	}
	if in.Action == WakeCancel {
		return w.cancel(ctx, c.ID)
	}
	now := w.now()
	at, err := in.when(now)
	if err != nil {
		return *invalidWake(err.Error()), nil
	}
	if err := in.check(); err != nil {
		return *invalidWake(err.Error()), nil
	}
	if err := trigger.CheckWake(at, now, w.expires); err != nil {
		return *outOfBounds(err), nil
	}
	title := strings.TrimSpace(in.Title)
	set, err := w.t.SetWake(ctx, runner.Wake{ToolUseID: c.ID, At: at.UTC(), Title: title, Note: in.Note})
	if re, ok := errors.AsType[*runner.TriggerRefusal](err); ok {
		return tools.Text(tools.OutcomeError, prompts.Render(prompts.WakeNotSet, prompts.Data{"Code": re.Code, "Message": re.Message})), nil
	}
	if err != nil {
		return tools.Result{}, err
	}
	count := c.State.Wakes + 1
	earliest, latest := trigger.WakeWindow(now, w.expires)
	res := tools.Text(tools.OutcomeOK, prompts.Render(prompts.WakeIsSet, prompts.Data{
		"At": stamp(at), "Count": count, "Max": trigger.MaxWakesWithoutPerson, "Earliest": stamp(earliest), "Latest": stamp(latest), "Held": false,
	}))
	res.WakeSet = &session.WakeSet{TriggerID: set.ID, At: at.UTC(), Title: title, Note: in.Note, Count: count}
	return res, nil
}

// cancel deletes the session's pending wake.
func (w *Wake) cancel(ctx context.Context, id string) (tools.Result, error) {
	canceled, err := w.t.CancelWake(ctx, id)
	if re, ok := errors.AsType[*runner.TriggerRefusal](err); ok {
		return tools.Text(tools.OutcomeError, prompts.Render(prompts.WakeNotSet, prompts.Data{"Code": re.Code, "Message": re.Message})), nil
	}
	if err != nil {
		return tools.Result{}, err
	}
	if canceled == "" {
		return tools.Text(tools.OutcomeOK, prompts.Text(prompts.WakeNone)), nil
	}
	res := tools.Text(tools.OutcomeOK, prompts.Text(prompts.WakeCanceled))
	res.WakeCleared = &session.WakeCleared{TriggerID: canceled, Reason: session.WakeCanceled}
	return res, nil
}

// stamp is a time as the model reads it, in UTC.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// minutes is a duration of whole minutes as words, such as 10 minutes.
func minutes(d time.Duration) string { return strconv.Itoa(int(d/time.Minute)) + " minutes" }

// hours is a duration of whole hours as words, such as 168 hours.
func hours(d time.Duration) string { return strconv.Itoa(int(d/time.Hour)) + " hours" }
