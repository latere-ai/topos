// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"time"

	"latere.ai/x/topos/session"
)

// SendNowGrace is how long a step's calls have to return once a message
// sent to be read now asked every running command to move to the
// background; the calls still running then are cut (spec 098).
const SendNowGrace = time.Second

// markReads appends the messages.read that names the waiting messages
// the step's request is the first to read (spec 098): every person's,
// and every other one whose turn closed before this one. The step folds
// the log the append returned, so a message withdrawn before it is left
// out, and one appended after it waits for the next step.
func (t *turn) markReads(ctx context.Context) error {
	var ids []string
	for _, w := range session.Unread(t.events()) {
		if w.Person() || w.Closed != 0 {
			ids = append(ids, w.Event.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	e, err := t.event(session.TypeMessagesRead, session.MessagesRead{EventIDs: ids})
	if err != nil {
		return err
	}
	return t.commit(ctx, e)
}

// sendsNow reports whether a message the runner signaled as sent to be
// read now is one the step acts on: appended after the turn began, and
// neither read by a request of the turn nor withdrawn. A message the
// turn's log does not hold yet came after the step's request was built.
func (t *turn) sendsNow(e session.Event) bool {
	if e.Seq <= t.startSeq {
		return false
	}
	for _, w := range session.WaitingMessages(t.events()) {
		if w.Event.ID == e.ID {
			return w.State == session.MessageWaiting
		}
	}
	return true
}

// cutDetail is the detail of the interrupted status of a step a cut
// ended: send_now when a message sent to be read now cut it.
func (t *turn) cutDetail() string {
	if t.cutNow.Load() {
		return session.DetailSendNow
	}
	return ""
}
