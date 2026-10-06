// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"net/http"
	"time"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/session"
)

// archiveSession is POST /sessions/{id}/archive.
func (c *call) archiveSession() error { return c.setArchived(true) }

// unarchiveSession is POST /sessions/{id}/unarchive.
func (c *call) unarchiveSession() error { return c.setArchived(false) }

// setArchived files an idle or ended session away from the lists, or
// back (specs 015 and 054). It asks session.read, so a caller who may not
// read the session hears not_found, then session.update with archived. A
// running session is conflict: archiving never stops work, so unarchiving
// restores exactly what was there. An archived idle session still takes
// a message, and its turn runs as any other. A repeat changes nothing.
func (c *call) setArchived(archived bool) error {
	var b struct{}
	if err := c.decodeOptional(&b); err != nil {
		return err
	}
	ar, ok := c.s.o.Sessions.(session.Archiver)
	if !ok {
		return errors.New("server: the session store does not archive")
	}
	s, err := c.session(authorizer.ActionSessionRead, nil)
	if err != nil {
		return err
	}
	// A running session is refused before the authorizer is asked, which
	// may act on an allowed question as it does on an end. Filing changes
	// nothing of the session, so a turn that starts while the authorizer
	// decides runs on, filed away.
	if archived && s.Status == session.StatusRunning {
		return refuse(CodeConflict, "the session is running; archiving never stops a turn, so interrupt it or wait for it to finish")
	}
	if _, err := c.ask(c.r.Context(), authorizer.ActionSessionUpdate, sessionResource(s, map[string]any{"session_id": s.ID, "archived": archived})); err != nil {
		return err
	}
	var at *time.Time
	if archived {
		at = new(c.s.o.Now())
	}
	if s, err = ar.SetArchived(c.r.Context(), s.ID, at); err != nil {
		return err
	}
	return c.replySession(http.StatusOK, s)
}
