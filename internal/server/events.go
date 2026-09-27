// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/session"
)

// listEvents is GET /sessions/{id}/events: the log from from_seq, paged.
func (c *call) listEvents() error {
	limit, cursor, err := c.pageParams()
	if err != nil {
		return err
	}
	from, err := c.seqParam("from_seq", 1)
	if err != nil {
		return err
	}
	if cursor != "" {
		after, err := uncursorSeq(cursor)
		if err != nil {
			return err
		}
		from = after + 1
	}
	s, err := c.session(authorizer.ActionSessionRead, nil)
	if err != nil {
		return err
	}
	evs, err := c.s.o.Sessions.Events(c.r.Context(), s.ID, from, limit+1)
	if err != nil {
		return err
	}
	next := ""
	if len(evs) > limit {
		evs = evs[:limit]
		next = cursorSeq(evs[limit-1].Seq)
	}
	if evs == nil {
		evs = []session.Event{}
	}
	return c.replyPage(evs, next)
}

// seqParam reads a sequence number query parameter.
func (c *call) seqParam(name string, def uint64) (uint64, error) {
	v := c.r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil || n < 1 {
		return 0, refuse(CodeInvalidRequest, "%s is %q, not a sequence number", name, v)
	}
	return n, nil
}

func cursorSeq(seq uint64) string { return store.Cursor(strconv.FormatUint(seq, 10)) }

func uncursorSeq(c string) (uint64, error) {
	s, err := store.Uncursor(c)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, refuse(CodeInvalidRequest, "the cursor was not issued by this server")
	}
	return n, nil
}

// sendBody is the body of POST /sessions/{id}/events.
type sendBody struct {
	Type    session.Type    `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// sendEvent is POST /sessions/{id}/events: one user event, its sender
// the verified subject whatever the body says.
func (c *call) sendEvent() error {
	var b sendBody
	if err := c.decode(&b); err != nil {
		return err
	}
	sender := session.Sender{Subject: c.caller.Subject, Kind: session.SenderPerson}
	var payload any
	action := authorizer.ActionSessionSend
	switch b.Type {
	case session.TypeUserMessage:
		var p session.UserMessage
		if err := strict(b.Payload, &p); err != nil {
			return err
		}
		if len(p.Content) == 0 {
			return refuse(CodeInvalidRequest, "a user.message holds content")
		}
		p.Sender = sender
		payload = p
	case session.TypeUserInterrupt:
		var p session.UserInterrupt
		if err := strict(b.Payload, &p); err != nil {
			return err
		}
		p.Sender = sender
		payload, action = p, authorizer.ActionSessionInterrupt
	case session.TypeUserToolConfirmation:
		var p session.UserToolConfirmation
		if err := strict(b.Payload, &p); err != nil {
			return err
		}
		if p.ToolUseID == "" || (p.Decision != session.DecisionAllow && p.Decision != session.DecisionDeny) {
			return refuse(CodeInvalidRequest, "a user.tool_confirmation names its tool_use_id and a decision of allow or deny")
		}
		p.Sender = sender
		payload = p
	case session.TypeUserToolResult:
		var p session.UserToolResult
		if err := strict(b.Payload, &p); err != nil {
			return err
		}
		if p.ToolUseID == "" {
			return refuse(CodeInvalidRequest, "a user.tool_result names its tool_use_id")
		}
		if p.Content == nil {
			p.Content = []lux.Block{}
		}
		p.Sender = sender
		payload = p
	default:
		return refuse(CodeInvalidRequest, "type %q is not one a person sends: user.message, user.interrupt, user.tool_confirmation or user.tool_result", b.Type)
	}
	s, err := c.session(action, map[string]any{"sender": sender.Subject, "event_type": string(b.Type)})
	if err != nil {
		return err
	}
	ev, err := session.NewEvent(b.Type, payload, c.s.o.Now())
	if err != nil {
		return err
	}
	appended, err := c.append(s.ID, ev)
	if err != nil {
		return err
	}
	c.s.o.Notify()
	return c.reply(http.StatusOK, appended)
}

// strict decodes a payload refusing fields the type does not name.
func strict(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return refuse(CodeInvalidRequest, "the payload does not decode: %v", err)
	}
	return nil
}

// stream is GET /sessions/{id}/stream: every event from from_seq, or
// after Last-Event-ID, then each new one as it is appended, as
// Server-Sent Events, until the event that ends the session.
func (c *call) stream() error {
	from, err := c.seqParam("from_seq", 1)
	if err != nil {
		return err
	}
	if v := c.r.Header.Get("Last-Event-ID"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return refuse(CodeInvalidRequest, "Last-Event-ID is %q, not a sequence number", v)
		}
		from = n + 1
	}
	if v := c.r.URL.Query().Get("deltas"); v != "" && v != "0" && v != "1" {
		return refuse(CodeInvalidRequest, "deltas is %q, not 0 or 1", v)
	}
	s, err := c.session(authorizer.ActionSessionRead, nil)
	if err != nil {
		return err
	}
	flusher, ok := c.w.(http.Flusher)
	if !ok {
		return errors.New("server: the response writer cannot flush")
	}
	ctx := c.r.Context()
	events, err := c.s.o.Sessions.Watch(ctx, s.ID, from)
	if err != nil {
		return err
	}
	h := c.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	c.w.WriteHeader(http.StatusOK)
	flusher.Flush()
	beat := time.NewTicker(c.s.o.Heartbeat)
	defer beat.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-beat.C:
			if _, err := io.WriteString(c.w, ": keepalive\n\n"); err != nil {
				return nil
			}
			flusher.Flush()
		case ev, open := <-events:
			if !open {
				return nil
			}
			if err := frame(c.w, ev); err != nil {
				return nil
			}
			flusher.Flush()
			if ends(ev) {
				return nil
			}
		}
	}
}

// frame writes one event as a Server-Sent Events frame.
func frame(w io.Writer, ev session.Event) error {
	b, err := session.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, b)
	return err
}

// ends reports whether ev ends its session.
func ends(ev session.Event) bool {
	if ev.Type != session.TypeSessionStatus {
		return false
	}
	var p session.SessionStatus
	return ev.Decode(&p) == nil && p.Status == session.StatusEnded
}

// blob is GET /sessions/{id}/blobs/{digest}.
func (c *call) blob() error {
	d := session.Digest(c.r.PathValue("digest"))
	if !d.Valid() {
		return refuse(CodeInvalidRequest, "%q is not a sha256: digest", d)
	}
	s, err := c.session(authorizer.ActionSessionRead, nil)
	if err != nil {
		return err
	}
	rc, err := c.s.o.Sessions.Blob(c.r.Context(), s.ID, d)
	if err != nil {
		return err
	}
	defer func() {
		if err := rc.Close(); err != nil {
			c.s.o.Log.WarnContext(c.r.Context(), "close a blob", "session", s.ID, "err", err)
		}
	}()
	c.w.Header().Set("Content-Type", "application/octet-stream")
	c.w.WriteHeader(http.StatusOK)
	_, err = io.Copy(c.w, rc)
	return err
}

// redactBody is the body of POST /sessions/{id}/events/{event_id}/redact.
type redactBody struct {
	Reason string `json:"reason"`
}

// redact is POST /sessions/{id}/events/{event_id}/redact: the event's
// content becomes a tombstone.
func (c *call) redact() error {
	var b redactBody
	if err := c.decode(&b); err != nil {
		return err
	}
	id := c.r.PathValue("event_id")
	s, err := c.session(authorizer.ActionSessionRedact, map[string]any{"event_id": id})
	if err != nil {
		return err
	}
	by := session.Sender{Subject: c.caller.Subject, Kind: session.SenderPerson}
	if err := c.s.o.Sessions.Redact(c.r.Context(), s.ID, id, by, b.Reason); err != nil {
		return err
	}
	c.w.WriteHeader(http.StatusNoContent)
	return nil
}
