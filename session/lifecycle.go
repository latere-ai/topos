// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"fmt"

	"latere.ai/x/pkg/statemachine"
)

// StatusTable is the lifecycle of a session's status. The header's status is
// folded from the session.status events of the log, and every store folds
// an appended batch through ApplyBatch after CheckStatuses has held the
// batch to this table. A session starts idle; a runner claims it (running),
// a turn stops it (idle with a reason) or finishes it (ended), and an end
// request or the reaper ends an idle one. Nothing follows ended. A status
// event that repeats the session's status, as a new runner's claim after a
// lost lease does, moves nothing and is no step.
var StatusTable = statemachine.Must(statemachine.New("Session", []Status{StatusIdle}, map[Status][]Status{
	StatusIdle:    {StatusRunning, StatusEnded},
	StatusRunning: {StatusIdle, StatusEnded},
	StatusEnded:   nil,
}))

// CheckStatuses holds the session.status events of a batch to StatusTable,
// from the status the session has before the batch, and reports the first
// step the table lacks as an error that wraps statemachine.ErrIllegalTransition.
// A store calls it in the transaction or under the lock that appends the
// batch, so the status it checks is the one the append lands on.
func CheckStatuses(cur Status, events []Event) error {
	for _, e := range events {
		if e.Type != TypeSessionStatus || e.Redacted() {
			continue
		}
		var p SessionStatus
		if e.Decode(&p) != nil || p.Status == StatusNone || p.Status == cur {
			continue
		}
		if err := StatusTable.Check(cur, p.Status); err != nil {
			return fmt.Errorf("session %s event %d: %w", e.SessionID, e.Seq, err)
		}
		cur = p.Status
	}
	return nil
}

// ArchiveState is whether a session is filed away from the lists.
type ArchiveState string

// The states of a session's archive, derived from its archived_at.
const (
	ArchiveActive   ArchiveState = "active"
	ArchiveArchived ArchiveState = "archived"
)

// ArchiveTable is the lifecycle of a session's archived_at. Archive is the
// one writer, run by every store's SetArchived under the session's lock: it
// files an active session away and returns an archived one, and leaves the
// time of the first archive alone when a session is archived again.
var ArchiveTable = statemachine.Must(statemachine.New("Archive", []ArchiveState{ArchiveActive}, map[ArchiveState][]ArchiveState{
	ArchiveActive:   {ArchiveArchived},
	ArchiveArchived: {ArchiveActive},
}))

// MessageTable is the lifecycle of a waiting message, which WaitingMessages
// folds from the log and no store keeps. A message waits from the user.message
// appended while a turn runs; a model request or a messages.read reads it, and
// a user.withdraw withdraws it, whichever comes first. Read and withdrawn are
// final: a later withdraw of a read message, or a later read of a withdrawn
// one, changes nothing.
var MessageTable = statemachine.Must(statemachine.New("Message", []MessageState{MessageWaiting}, map[MessageState][]MessageState{
	MessageWaiting:   {MessageRead, MessageWithdrawn},
	MessageRead:      nil,
	MessageWithdrawn: nil,
}))
