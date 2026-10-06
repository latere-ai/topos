// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	// message is a user.message's payload, which takes its files' blobs
	// once the session is known to take it.
	var message *session.UserMessage
	var files []file
	action := authorizer.ActionSessionSend
	switch b.Type {
	case session.TypeUserMessage:
		var m messageBody
		if err := strict(b.Payload, &m); err != nil {
			return err
		}
		if len(m.Content) == 0 && len(m.Attachments) == 0 {
			return refuse(CodeInvalidRequest, "a user.message holds content or attachments")
		}
		if err := checkContent(m.Content); err != nil {
			return err
		}
		var err error
		if files, err = checkAttachments(m.Attachments); err != nil {
			return err
		}
		if m.Content == nil {
			m.Content = []lux.Block{}
		}
		message = &session.UserMessage{Sender: sender, Content: m.Content}
		payload = *message
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
		if (p.ToolUseID == "") == (p.ApprovalID == "") || (p.Decision != session.DecisionAllow && p.Decision != session.DecisionDeny) {
			return refuse(CodeInvalidRequest, "a user.tool_confirmation names exactly one of tool_use_id and approval_id, and a decision of allow or deny")
		}
		// An approval is of a connection, which no call pattern names
		// (spec 052).
		if p.ApprovalID != "" && p.Remember != "" {
			return refuse(CodeInvalidRequest, "remember names a call's argument pattern; a user.tool_confirmation of an approval_id carries none")
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
	case session.TypeUserAnswer:
		// The answer's shape is checked before the authorizer is asked, and
		// its fit to the open question after the allow, so a caller who
		// may not send learns nothing of the session's calls (spec 039).
		var p session.UserAnswer
		if err := strict(b.Payload, &p); err != nil {
			return err
		}
		if err := p.CheckShape(); err != nil {
			return refuse(CodeInvalidRequest, "%v", err)
		}
		p.Sender = sender
		payload = p
	default:
		return refuse(CodeInvalidRequest, "type %q is not one a person sends: %s", b.Type, strings.Join(sentTypes, ", "))
	}
	// A send's allow may move the session to another model before the
	// turn the event starts (spec 038); an interrupt starts none.
	fields := map[string]any{"sender": sender.Subject, "event_type": string(b.Type)}
	var s session.Session
	var change sendChanges
	var err error
	if action == authorizer.ActionSessionSend {
		s, change, err = c.s.sendAs(c.r.Context(), c.asker(), c.r.PathValue("id"), fields)
	} else {
		s, err = c.session(action, fields)
	}
	if err != nil {
		return err
	}
	// A message's files are stored before the event that names them, and
	// only for a session that takes the message, each in a directory named
	// after the event's id, minted here for it.
	id := session.NewID(session.PrefixEvent)
	if message != nil && len(files) > 0 {
		if s.Status == session.StatusEnded {
			return refuse(CodeConflict, "the session ended %s", s.StopReason)
		}
		if message.Attachments, err = c.storeAttachments(c.r.Context(), s, id, files); err != nil {
			return err
		}
		payload = *message
	}
	ev, err := session.NewEvent(b.Type, payload, c.s.o.Now())
	if err != nil {
		return err
	}
	ev.ID = id
	appended, err := c.s.appendSent(c.r.Context(), s.ID, change, ev, answering(b.Type, payload))
	if err != nil {
		return err
	}
	c.s.o.Notify()
	return c.reply(http.StatusOK, appended)
}

// sentTypes are the event types a person sends to a session, in the
// order the send route's description names them. Each but user.interrupt
// is asked of the authorizer as session.send with the type as its
// event_type, which an authorizer that lists the types it allows reads.
var sentTypes = []string{
	string(session.TypeUserMessage), string(session.TypeUserInterrupt), string(session.TypeUserToolConfirmation),
	string(session.TypeUserToolResult), string(session.TypeUserAnswer),
}

// answering is the check of a sent event against the log it is appended
// after, nil for an event that answers no call: a confirmation, a client
// tool's result and an answer to a question each name a call that waits
// for exactly that answer. The check runs on the log the append follows,
// so of two answers to one call one is appended and the other refused.
func answering(typ session.Type, payload any) func([]session.Event) error {
	switch p := payload.(type) {
	case session.UserToolConfirmation:
		return awaits(typ, cmp.Or(p.ToolUseID, p.ApprovalID), session.AnswerConfirmation)
	case session.UserToolResult:
		return awaits(typ, p.ToolUseID, session.AnswerResult)
	case session.UserAnswer:
		return func(evs []session.Event) error { return fits(evs, p) }
	}
	return nil
}

// awaits refuses a confirmation or a client tool's result that answers
// no call waiting for it: a call never asked, already answered, denied by
// a person's message, or of the other kind. A confirmation of an
// approval_id answers an approval.requested that asks and that nothing
// answered (spec 052).
func awaits(typ session.Type, toolUseID string, want session.Answer) func([]session.Event) error {
	return func(evs []session.Event) error {
		if session.Awaiting(evs)[toolUseID] != want {
			return refuse(CodeConflict, "%s names %s, which waits for no such answer", typ, toolUseID)
		}
		return nil
	}
}

// fits refuses an answer that names no open question, as conflict with
// what closed the question in the detail, and one that does not fit the
// question it names, as invalid_request (spec 039).
func fits(evs []session.Event, a session.UserAnswer) error {
	c, asked := session.Closed(evs, a.ToolUseID)
	switch {
	case !asked:
		if q, open := session.OpenQuestion(evs); open {
			return refuse(CodeConflict, "user.answer names %s, which is no question of this session; the open question is %s", a.ToolUseID, q.ToolUseID)
		}
		return refuse(CodeConflict, "user.answer names %s, and no question is open", a.ToolUseID)
	case !c.Open():
		return refuse(CodeConflict, "user.answer names %s, which %s", a.ToolUseID, closedBy(c))
	}
	q, _ := session.OpenQuestion(evs)
	if err := a.Fits(q.Input); err != nil {
		return refuse(CodeInvalidRequest, "%v", err)
	}
	return nil
}

// closedBy says what closed a question, for a refusal's detail.
func closedBy(c session.Closing) string {
	switch c.By {
	case session.ClosedByAnswer:
		return "the user.answer " + c.EventID + " already answered"
	case session.ClosedByMessage:
		return "the person's user.message " + c.EventID + " closed in place of an answer"
	case session.ClosedByInterrupt:
		return "the user.interrupt " + c.EventID + " dismissed"
	case session.ClosedByUnattended:
		return "was answered at once, since nobody attends the session"
	}
	return "already has its result " + c.ResultID
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
// Server-Sent Events, until the event that ends the session. With
// deltas=1 it also carries the session's live deltas as they are
// published, on a store that carries them, each a frame with no id.
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
	withDeltas := c.r.URL.Query().Get("deltas")
	if withDeltas != "" && withDeltas != "0" && withDeltas != "1" {
		return refuse(CodeInvalidRequest, "deltas is %q, not 0 or 1", withDeltas)
	}
	s, err := c.session(authorizer.ActionSessionRead, nil)
	if err != nil {
		return err
	}
	release, ok := c.s.streams.take(c.caller.Subject)
	if !ok {
		return refuse(CodeRateLimited, "at most %d streams are open at once per subject; close one before opening another", c.s.o.MaxStreams)
	}
	defer release()
	flusher, ok := c.w.(http.Flusher)
	if !ok {
		return errors.New("server: the response writer cannot flush")
	}
	ctx := c.r.Context()
	// The deltas are followed before the log is, so none published while
	// the watch starts is missed; a delta is never replayed, and one that
	// arrives while the replay runs goes out between replayed events.
	var deltas <-chan session.Delta
	if sub, ok := c.s.o.Sessions.(session.DeltaSubscriber); ok && withDeltas == "1" {
		deltas = sub.SubscribeDeltas(ctx, s.ID)
	}
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
			shown, err := eventAnswer(ev)
			if err != nil {
				c.s.o.Log.ErrorContext(ctx, "stream: an event does not read; the stream ends", "session", s.ID, "seq", ev.Seq, "err", err)
				return nil
			}
			if err := frame(c.w, shown); err != nil {
				return nil
			}
			flusher.Flush()
			if ends(ev) {
				return nil
			}
		case d, open := <-deltas:
			if !open {
				deltas = nil
				continue
			}
			if err := deltaFrame(c.w, d); err != nil {
				return nil
			}
			flusher.Flush()
		}
	}
}

// slots counts the streams each subject holds open. A stream holds a
// watch on the store for as long as it is open, so without a bound one
// subject within the request rate could hold any number of them.
type slots struct {
	limit int
	mu    sync.Mutex
	held  map[string]int
}

// newSlots bounds each subject to limit streams, none when limit is
// negative.
func newSlots(limit int) *slots { return &slots{limit: limit, held: map[string]int{}} }

// take reserves one of subject's slots and returns the function that
// frees it, or false when the subject holds every one.
func (s *slots) take(subject string) (func(), bool) {
	if s.limit < 0 {
		return func() {}, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held[subject] >= s.limit {
		return nil, false
	}
	s.held[subject]++
	return sync.OnceFunc(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.held[subject]--; s.held[subject] == 0 {
			delete(s.held, subject)
		}
	}), true
}

// frame writes one event as a Server-Sent Events frame, as eventAnswer
// reads it.
func frame(w io.Writer, ev session.Event) error {
	b, err := session.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, b)
	return err
}

// deltaFrame writes one live delta as a Server-Sent Events frame with no
// id, so a browser's last event id stays the last event's and a
// reconnect resumes the log where it was.
func deltaFrame(w io.Writer, d session.Delta) error {
	b, err := session.Marshal(d)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: delta\ndata: %s\n\n", b)
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
// content becomes a tombstone. A user.answer's words are also in the
// tool.result the runner rendered from it, so that result is redacted in
// the same call (spec 039); the answer is found again by its id after a
// redaction, so a call that failed between the two is safe to send
// again. The agent.tool_use of a question whose result is not in the log
// yet is refused, since its tombstone would leave a wait nothing can
// find.
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
	evs, err := c.s.o.Sessions.Events(c.r.Context(), s.ID, 1, 0)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(evs, func(e session.Event) bool { return e.ID == id })
	if i < 0 {
		return refuse(CodeNotFound, "no event %s", id)
	}
	if !session.Redactable(evs[i].Type) {
		return refuse(CodeInvalidRequest, "a %s event is part of the session's record and holds no content a redaction removes", evs[i].Type)
	}
	var also string
	switch evs[i].Type {
	case session.TypeAgentToolUse:
		var use session.AgentToolUse
		if evs[i].Decode(&use) == nil && !evs[i].Redacted() {
			if cl, asked := session.Closed(evs, use.ToolUseID); asked && !cl.Settled {
				return refuse(CodeConflict, "%s is the question %s, which has no result yet; dismiss it with user.interrupt first", id, use.ToolUseID)
			}
		}
	case session.TypeUserAnswer:
		for _, q := range session.Questions(evs) {
			if q.Closing.EventID == id && q.Closing.Settled {
				also = q.Closing.ResultID
			}
		}
	}
	by := session.Sender{Subject: c.caller.Subject, Kind: session.SenderPerson}
	if err := c.s.o.Sessions.Redact(c.r.Context(), s.ID, id, by, b.Reason); err != nil {
		return err
	}
	if also != "" {
		if err := c.s.o.Sessions.Redact(c.r.Context(), s.ID, also, by, b.Reason); err != nil {
			return err
		}
	}
	c.w.WriteHeader(http.StatusNoContent)
	return nil
}
