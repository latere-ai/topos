// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"regexp"
	"strconv"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/session"
)

// CodeInvalidForkPoint is spec 017's code for a fork at a sequence that
// is not a turn boundary, or of a session that has none.
const CodeInvalidForkPoint = "invalid_fork_point"

// ForkKeptFiles is the fork route's sentence on a hosted session's files
// (spec 035): a session that works in a private repository on the git
// host keeps its checkpoints there, so its fork restores them after the
// sandbox is gone, and no other session keeps them past its sandbox. A
// client reads it to promise a continued session its files only where
// the core restores them.
const ForkKeptFiles = "A hosted session that works in a private repository on the git host keeps each turn's checkpoint at that repository, under refs/topos/checkpoints/<session>/latest, " +
	"so its fork restores the fork point's files after the session's sandbox is gone. A repository that a reader with no credential can read, and one whose answer to such a read is not a refusal for want of a credential, is not known private and gets no checkpoint, " +
	"since whoever reads a repository reads its checkpoints, uncommitted files included. A checkpoint the runner cannot have leaves the fork with its conversation and its repositories, " +
	"recorded beside its first session.machine as session.error checkpoint_missing."

// continuedMark matches the mark continuedTitle puts at the end of a
// fork's title: " (continued)" or " (continued N)".
var continuedMark = regexp.MustCompile(`^(.*) \(continued(?: ([0-9]+))?\)$`)

// continuedTitle is a fork's title: the forked session's, marked as its
// continuation so the two read apart in a list, "Notes (continued)",
// then "Notes (continued 2)" for a fork of that fork. A session without
// a title gives a fork without one.
func continuedTitle(title string) string {
	if title == "" {
		return ""
	}
	m := continuedMark.FindStringSubmatch(title)
	if m == nil {
		return title + " (continued)"
	}
	n := 1
	if m[2] != "" {
		n, _ = strconv.Atoi(m[2])
	}
	return m[1] + " (continued " + strconv.Itoa(n+1) + ")"
}

// forkBody is the body of POST /sessions/{id}/fork; an empty body forks
// at the last turn boundary.
type forkBody struct {
	AtSeq *uint64 `json:"at_seq,omitempty"`
}

// forkSession is POST /sessions/{id}/fork (spec 017): a new session of
// the same agent version whose log starts as a copy of this one's up to
// a turn boundary, with the caller as its initiator and its own lifetime,
// budget and credentials. Any session the caller may read is forked, an
// ended or expired one included; a caller who may not read it hears
// not_found before anything else.
func (c *call) forkSession() error {
	var b forkBody
	if err := c.decodeOptional(&b); err != nil {
		return err
	}
	ctx := c.r.Context()
	parent, err := c.session(authorizer.ActionSessionRead, nil)
	if err != nil {
		return err
	}
	evs, err := c.s.o.Sessions.Events(ctx, parent.ID, 1, 0)
	if err != nil {
		return err
	}
	seq, err := session.ForkPoint(evs, b.AtSeq)
	if err != nil {
		return err
	}
	s, err := c.s.create(ctx, c.asker(), creation{
		fork:   &forkOrigin{parent: parent, seq: seq, events: evs[:seq], model: modelAt(parent, evs, seq)},
		sender: session.Sender{Subject: c.caller.Subject, Kind: session.SenderPerson},
	})
	if err != nil {
		return err
	}
	return c.replySession(http.StatusCreated, s)
}

// modelAt is the model a session stood on after the first seq events of
// its log evs, nil when it ran its agent's with nothing recorded: the new
// model of the last change among those events, or, with none among them,
// the model the session started on, which is the old model of its first
// change after them and, in a log that holds no change, its header's.
func modelAt(s session.Session, evs []session.Event, seq uint64) *session.ModelRef {
	at := s.Model
	changed := false
	for _, e := range evs {
		var m session.ModelChanged
		if e.Type != session.TypeModelChanged || e.Redacted() || e.Decode(&m) != nil {
			continue
		}
		if e.Seq > seq {
			if !changed {
				at = &m.Old
			}
			break
		}
		at, changed = &m.New, true
	}
	return at
}
