// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"encoding/json"
	"slices"
	"time"
)

// ToolNotify is the name of the harness tool that marks a turn a trigger
// began as one that brought something to the person (spec 086).
const ToolNotify = "notify"

// MaxNoticeTitle bounds a notice's title, in characters: a
// notification's line on a computer and a list row's on a phone.
const MaxNoticeTitle = 80

// The outcomes of a turn a trigger's message began, on the session.status
// that closes it (spec 086).
const (
	// OutcomeNotified is a turn that brought something to the person: its
	// trigger always brings its result, the agent called notify, or the
	// turn waited for the person on the way.
	OutcomeNotified = "notified"
	// OutcomeQuiet is a turn that ended with none of those, which leaves
	// the person undisturbed.
	OutcomeQuiet = "quiet"
)

// Notice is the latest notify of a session as its header folds it: the
// title the agent gave it, and the sequence and time of the call's
// result.
type Notice struct {
	Title string    `json:"title"`
	Seq   uint64    `json:"seq"`
	At    time.Time `json:"at"`
}

// NoticeMeta is a notify call's record in its tool.result's meta.
type NoticeMeta struct {
	Title string `json:"title"`
}

// noticeOf is the notice a tool.result of the session's own thread
// records, nil for any other event.
func noticeOf(e Event) *Notice {
	if e.Type != TypeToolResult || e.Subagent != "" || e.Redacted() {
		return nil
	}
	var p struct {
		Meta struct {
			Notice *NoticeMeta `json:"notice"`
		} `json:"meta"`
	}
	if json.Unmarshal(e.Payload, &p) != nil || p.Meta.Notice == nil {
		return nil
	}
	return &Notice{Title: p.Meta.Notice.Title, Seq: e.Seq, At: e.Time}
}

// Noticed reports whether evs hold a notify's result after the sequence
// after: the call was made in the turn whose events follow it.
func Noticed(evs []Event, after uint64) bool {
	return slices.ContainsFunc(evs, func(e Event) bool { return e.Seq > after && noticeOf(e) != nil })
}

// TriggerTurn is the user.message of a trigger that is the first input of
// the turn whose running status follows the sequence through, and false
// when the turn began with anything else: a person's message, an answer,
// a confirmation or a resume. The turn's input is what the log holds
// after the session's own thread's last status that closed a turn.
func TriggerTurn(evs []Event, through uint64) (UserMessage, bool) {
	start := 0
	for i, e := range slices.Backward(evs) {
		if e.Seq > through || e.Type != TypeSessionStatus || e.Subagent != "" || e.Redacted() {
			continue
		}
		var p SessionStatus
		if e.Decode(&p) == nil && p.Status != StatusRunning {
			start = i + 1
			break
		}
	}
	for _, e := range evs[start:] {
		if e.Seq > through || e.Redacted() {
			continue
		}
		switch e.Type {
		case TypeUserMessage:
			var p UserMessage
			if e.Decode(&p) != nil || p.FiringID == "" {
				return UserMessage{}, false
			}
			return p, true
		case TypeUserToolConfirmation, TypeUserToolResult, TypeUserAnswer, TypeSessionResumed, TypeUserInterrupt:
			return UserMessage{}, false
		}
	}
	return UserMessage{}, false
}

// Judged reports whether a trigger's message leaves it to the agent to
// say whether its turn brought something to the person: a wake's, or one
// of a trigger whose session.notify is judged.
func (m UserMessage) Judged() bool { return m.Wake != nil || m.Notify == NotifyJudged }

// NotifyJudged is UserMessage.Notify for a trigger whose session.notify
// is judged (spec 086).
const NotifyJudged = "judged"
