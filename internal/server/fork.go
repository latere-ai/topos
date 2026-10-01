// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/session"
)

// CodeInvalidForkPoint is spec 017's code for a fork at a sequence that
// is not a turn boundary, or of a session that has none.
const CodeInvalidForkPoint = "invalid_fork_point"

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
		fork:   &forkOrigin{parent: parent, seq: seq, events: evs[:seq]},
		sender: session.Sender{Subject: c.caller.Subject, Kind: session.SenderPerson},
	})
	if err != nil {
		return err
	}
	return c.replySession(http.StatusCreated, s)
}
