// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// notifySchema is the notify tool's input, its bound read from its
// constant.
var notifySchema = json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "title": {"type": "string", "maxLength": %d, "description": "One line the person reads: what you found."}
  },
  "required": ["title"],
  "additionalProperties": false
}`, session.MaxNoticeTitle))

// notifyTool marks a turn a trigger began as one that brought something
// to the person (spec 086): its result records the notice, which the
// session's header folds and the turn's outcome reads. It is the session's
// own thread's, one call a turn, and changes nothing outside the log.
type notifyTool struct{ t *turn }

func (notifyTool) Definition() tools.Definition {
	return tools.Definition{Name: session.ToolNotify, Description: prompts.Text(prompts.ToolNotify), InputSchema: notifySchema}
}

// Properties: effect none, so every mode runs it without asking.
func (notifyTool) Properties() tools.Properties { return tools.Properties{Effect: tools.EffectNone} }

// noticeTitle reads a call's title and refuses one that is empty, past
// MaxNoticeTitle or more than one line.
func noticeTitle(raw json.RawMessage) (string, error) {
	var in struct {
		Title string `json:"title"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return "", err
	}
	title := strings.TrimSpace(in.Title)
	switch {
	case title == "":
		return "", errors.New("title is required: one line the person reads, what you found")
	case utf8.RuneCountInString(title) > session.MaxNoticeTitle:
		return "", fmt.Errorf("title is longer than %d characters", session.MaxNoticeTitle)
	case strings.ContainsFunc(title, func(r rune) bool { return unicode.IsControl(r) || r == ' ' || r == ' ' }):
		return "", errors.New("title is one line")
	}
	return title, nil
}

// Check refuses a title the tool would refuse, before the call is
// decided.
func (notifyTool) Check(c tools.Call) *tools.Result {
	if _, err := noticeTitle(c.Input); err != nil {
		r := invalidNotice(err)
		return &r
	}
	return nil
}

// invalidNotice is a call answered for what its input got wrong.
func invalidNotice(err error) tools.Result {
	return tools.Text(tools.OutcomeInvalidInput, prompts.Render(prompts.NotifyInvalid, prompts.Data{"Detail": err.Error()}))
}

// Run records the notice, or refuses a second of the turn.
func (n notifyTool) Run(_ context.Context, c tools.Call) (tools.Result, error) {
	title, err := noticeTitle(c.Input)
	if err != nil {
		return invalidNotice(err), nil
	}
	if session.Noticed(n.t.events(), n.t.startSeq) {
		return tools.Text(tools.OutcomeInvalidInput, prompts.Text(prompts.NotifyAgain)), nil
	}
	res := tools.Text(tools.OutcomeOK, prompts.Render(prompts.NotifyNoticed, prompts.Data{"Title": title}))
	res.Meta = &tools.Meta{Notice: &session.NoticeMeta{Title: title}}
	return res, nil
}

// outcome is the outcome the status that closes the turn records (spec
// 086), "" for a turn that records none: one a person began, and one that
// ends on an error, a limit, a budget, an interrupt or a computer that is
// away. A turn a trigger's message began is notified when it waits for
// the person, when its trigger always brings its result and it is no
// wake's, when the agent called notify, when the agent holds no notify
// to call, or when the person was asked on the way, a site or a
// connection; and quiet when it ends with none of those.
func (t *turn) outcome(reason session.StopReason) string {
	if t.thread != "" {
		return ""
	}
	evs := t.events()
	msg, ok := session.TriggerTurn(evs, t.startSeq)
	if !ok {
		return ""
	}
	switch reason {
	case session.StopToolConfirmation, session.StopQuestion, session.StopToolResult:
		return session.OutcomeNotified
	case session.StopEndTurn:
	default:
		return ""
	}
	if !msg.Judged() || !t.notifies() || session.Noticed(evs, t.startSeq) || askedOnTheWay(evs, t.startSeq) {
		return session.OutcomeNotified
	}
	return session.OutcomeQuiet
}

// notifies reports whether the thread holds the notify tool. A client's
// tool of the name, which an agent stored before the name was reserved
// may hold, is that client's.
func (t *turn) notifies() bool {
	if t.reg == nil {
		return false
	}
	tool, ok := t.reg.Get(session.ToolNotify)
	return ok && !tool.Properties().Client
}

// askedOnTheWay reports whether the person was asked during the turn
// whose events follow the sequence after: a site the network asks about,
// or a connection to make.
func askedOnTheWay(evs []session.Event, after uint64) bool {
	for _, e := range evs {
		if e.Seq <= after || e.Redacted() {
			continue
		}
		switch e.Type {
		case session.TypeConnectionRequested:
			return true
		case session.TypeApprovalRequested:
			var p session.ApprovalRequested
			if e.Decode(&p) == nil && p.Verdict == string(VerdictAsk) {
				return true
			}
		}
	}
	return false
}
