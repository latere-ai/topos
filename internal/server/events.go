// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/runner"
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
	sender := personSender(c.caller)
	var payload any
	// message is a user.message's payload, which takes its files' blobs
	// once the session is known to take it.
	var message *session.UserMessage
	var files []file
	// word is what a message says to the authorizer beside its content,
	// forwarded on its question and kept in no event (spec 063).
	var word map[string]any
	action := authorizer.ActionSessionSend
	switch b.Type {
	case session.TypeUserMessage:
		var m messageBody
		if err := strict(b.Payload, &m); err != nil {
			return err
		}
		checked, checkedFiles, err := m.check(sender, false)
		if err != nil {
			return err
		}
		message, files, word = &checked, checkedFiles, m.word(sourcesOf(sender, checkedFiles))
		payload = *message
	case session.TypeUserInterrupt:
		var p session.UserInterrupt
		if err := strict(b.Payload, &p); err != nil {
			return err
		}
		p.Sender = sender
		payload, action = p, authorizer.ActionSessionInterrupt
	case session.TypeUserWithdraw:
		// A withdrawal starts nothing and spends nothing, as an interrupt
		// does (spec 098).
		var p session.UserWithdraw
		if err := strict(b.Payload, &p); err != nil {
			return err
		}
		if p.EventID == "" {
			return refuse(CodeInvalidRequest, "a user.withdraw names the event_id of the message it withdraws")
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
	maps.Copy(fields, word)
	var s session.Session
	var change sendChanges
	var err error
	if action == authorizer.ActionSessionSend {
		// A message tells its shape, never its words (spec 061).
		var sh *shape
		if message != nil {
			sh = new(shapeOf(message.Content, len(files)))
		}
		s, change, err = c.s.sendAs(c.r.Context(), c.asker(), c.r.PathValue("id"), fields, sh)
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
		// A source's spool is removed once the message is stored or
		// refused (spec 081).
		defer c.s.closeSpools(c.r.Context(), files)
		if err := c.s.readSources(c.r.Context(), s, sender, files); err != nil {
			return err
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
	appended, err := c.s.appendSent(c.r.Context(), s.ID, change, ev, answering(b.Type, payload, s.Initiator.Subject))
	if err != nil {
		return err
	}
	c.s.o.Notify()
	return c.reply(http.StatusOK, appended)
}

// sentTypes are the event types a person sends to a session, in the
// order the send route's description names them. Each but user.interrupt
// and user.withdraw is asked of the authorizer as session.send with the
// type as its event_type, which an authorizer that lists the types it
// allows reads.
var sentTypes = []string{
	string(session.TypeUserMessage), string(session.TypeUserInterrupt), string(session.TypeUserWithdraw),
	string(session.TypeUserToolConfirmation), string(session.TypeUserToolResult), string(session.TypeUserAnswer),
}

// answering is the check of a sent event against the log it is appended
// after, nil for an event that answers no call: a confirmation, a client
// tool's result and an answer to a question each name a call that waits
// for exactly that answer. The check runs on the log the append follows,
// so of two answers to one call one is appended and the other refused.
func answering(typ session.Type, payload any, initiator string) func([]session.Event) error {
	switch p := payload.(type) {
	case session.UserToolConfirmation:
		wait := awaits(typ, cmp.Or(p.ToolUseID, p.ApprovalID), session.AnswerConfirmation)
		return func(evs []session.Event) error {
			if err := wait(evs); err != nil {
				return err
			}
			// A trigger call changes standing work the person who started
			// the session owns, and a held wake runs as them, so only that
			// person answers either (spec 086), whoever else may send to
			// the session.
			if p.ToolUseID == "" || p.Sender.Subject == initiator {
				return nil
			}
			switch name := callName(evs, p.ToolUseID); name {
			case session.ToolTrigger:
				return refuse(auth.CodeForbidden, "only the person who started the session answers a %s call: the trigger it changes would be theirs", name)
			case session.ToolWake:
				return refuse(auth.CodeForbidden, "only the person who started the session answers a %s call: its wake runs as them", name)
			}
			return nil
		}
	case session.UserToolResult:
		return awaits(typ, p.ToolUseID, session.AnswerResult)
	case session.UserAnswer:
		return func(evs []session.Event) error { return fits(evs, p) }
	case session.UserWithdraw:
		return func(evs []session.Event) error { return withdrawable(evs, p) }
	}
	return nil
}

// withdrawable refuses a withdrawal of anything but a waiting message its
// sender sent that no request read and nobody withdrew (spec 098). The
// check runs on the log the append follows, so a withdrawal and the
// runner's messages.read of the same message never both stand.
func withdrawable(evs []session.Event, p session.UserWithdraw) error {
	sent := slices.IndexFunc(evs, func(e session.Event) bool {
		return e.ID == p.EventID && e.Type == session.TypeUserMessage && e.Subagent == ""
	})
	if sent < 0 {
		return refuse(CodeConflict, "user.withdraw names %s, which is no message of this session", p.EventID)
	}
	var m session.UserMessage
	if err := evs[sent].Decode(&m); err != nil || m.Sender.Subject != p.Sender.Subject {
		return refuse(auth.CodeForbidden, "only the person who sent %s withdraws it", p.EventID)
	}
	for _, w := range session.WaitingMessages(evs) {
		if w.Event.ID != p.EventID {
			continue
		}
		switch w.State {
		case session.MessageRead:
			return refuse(CodeConflict, "user.withdraw names %s, which a request already read", p.EventID)
		case session.MessageWithdrawn:
			return refuse(CodeConflict, "user.withdraw names %s, which was already withdrawn", p.EventID)
		}
		return nil
	}
	return refuse(CodeConflict, "user.withdraw names %s, which was sent while the session was idle and started its turn", p.EventID)
}

// callName is the name of the tool the call toolUseID of evs calls, ""
// for a call the log does not hold.
func callName(evs []session.Event, toolUseID string) string {
	for _, e := range evs {
		if e.Type != session.TypeAgentToolUse {
			continue
		}
		var u session.AgentToolUse
		if e.Decode(&u) == nil && u.ToolUseID == toolUseID {
			return u.Name
		}
	}
	return ""
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

// KeptRule is the sentence the event routes state files.kept by (specs
// 055 and 064): what a client draws an answer's images and offers its
// linked files from.
var KeptRule = fmt.Sprintf("files.kept {message, files, skipped} follows an agent.message of the session's own conversation that shows a local image or links a local file, once the message's step is committed and before the turn's session.status: "+
	"message is the agent.message's id, files each {path, resolved, blob, media_type, size, width, height} for a file kept as a blob of the session, path the destination as the message wrote it, "+
	"and skipped each {path, reason}, reason one of not_found, too_large, not_an_image, not_a_file, outside_workdir, no_machine, limit and unavailable. "+
	"A destination names a file of the machine when it has no scheme, the file: scheme, or the prefix sandbox: before a path with no scheme, and lies inside the working directory; a reference in a code span or a fenced block is not read. "+
	"An image is shown by a Markdown image or an HTML img element's src: the first %d of a message are read, each PNG, JPEG, GIF or WebP by its bytes, never an SVG, at most %d bytes, %d pixels a side and %d pixels, "+
	"and its entry carries the media_type of its bytes and the width and height of its header. "+
	"A file is linked by a Markdown link or an HTML a element's href: the first %d of a message are read, each a regular file of any type of at most %d bytes, %d bytes together, "+
	"and its entry carries the media_type its name's extension names, or its leading bytes where the extension names none, its size in bytes, and width and height 0. "+
	"A session with a folder of the files store keeps each file there in place of a blob, at results/<turn>/<path>, path its place in the working directory, written once, and its entry carries ref, that path, beside blob, its digest; "+
	"a linked file is then bounded by what the files service takes and not by the two byte figures above, the counts are as above, a file the files service refuses for its size is too_large and one it refuses otherwise unavailable, "+
	"and a path an earlier answer of the turn took is kept by its ref when the file is the same and unavailable when it changed. "+
	"A destination both shown and linked is kept once, as the image. A link to a directory is not_a_file, an image reference to one not_an_image, and limit is a reference past its count or a linked file past the bytes of its message. "+
	"Files are read from a machine the turn opened, never one opened to keep them. "+
	"A client draws an image and offers a linked file from the blob its path names, and reads the files route for one no files.kept recorded; a client that does not know the type skips it.",
	tools.MaxImages, tools.ReadMaxImage, harness.MaxKeptImageSide, harness.MaxKeptImagePixels, harness.MaxKeptFiles, harness.MaxKeptFileBytes, harness.MaxKeptFilesBytes)

// MachineRule is the sentence the event routes state session.machine by
// (specs 004 and 009): what a client tells a machine that restarted with
// its files from one that was replaced without them by.
var MachineRule = fmt.Sprintf("session.machine {machine, reason, cause, context, instructions, skills, repositories, checkpoint, restore} records the machine a session works on. "+
	"reason is %s for the session's first machine, %s for one the session moved to, %s for a new one whose files were put back, "+
	"%s for the machine the session had, which stopped on a failure and runs again with the files of its working directory kept and every process that ran on it gone, "+
	"and %s for a new, empty one in the place of one that was lost, with nothing put back, whose files are gone. "+
	"cause is %s on a %s record whose machine is known to have stopped because it ran out of memory, and absent otherwise; a %s record restates the context, instructions and skills of the machine's last record. "+
	"A %s record is followed in the same append by session.error {code: %s, message: %q}. "+
	"A %s record names what gave the files back: checkpoint, or restore {source: %s, files, bytes, unverified, omitted, commands, paths, unverified_paths, omitted_files} for a working directory rebuilt from the session's own log, "+
	"its writes and edits, the files its answers showed or linked, and the files read in full, into a machine made new for a session that lost its own or for a fork with no checkpoint. "+
	"files and bytes are what was written back, unverified how many of those a command changed after the log last held them, omitted how many files the log shows and could not rebuild, "+
	"and commands how many commands ran after the first of the files was made, whose own files are not back; paths, unverified_paths and omitted_files name the first %d of each, omitted_files each {path, reason}, "+
	"reason one of %s, %s, %s and %s. A rebuild holds at most %d files of %d bytes together. "+
	"A session that keeps a copy of its working directory in its folder of the files store (work.saved) is restored into such a machine from that copy first, "+
	"recorded as restore {source: %s, copy {files, bytes, turn, inherited, skipped, skipped_files}, files, bytes, unverified, omitted, commands, paths, unverified_paths, omitted_files}: "+
	"copy files and bytes are what the copy gave back, turn the turn whose end saved it last, inherited true for a fork's copy of its parent's latest, which may hold changes made after the point the fork continues from, "+
	"skipped and skipped_files what the copy leaves out; the other members count from the copy, files, bytes and paths the files the log wrote over the copy or beside it, commands the commands that ran after it, "+
	"and omitted reason %s a file the copy holds that the folder no longer had and the log could not give back. "+
	"A restore from the log names copy {failed}, a developer's detail, when the copy could not be written.",
	session.MachineAttached, session.MachineHandoff, session.MachineRestored, session.MachineRestarted, session.MachineReplaced,
	session.MachineOutOfMemory, session.MachineRestarted, session.MachineRestarted, session.MachineReplaced, machine.CodeLost, runner.MessageMachineLost,
	session.MachineRestored, session.RestoreFromLog, session.MaxRestoreNames,
	session.OmittedUnknown, session.OmittedEdit, session.OmittedLimit, session.OmittedBlob, tools.MaxRebuiltFiles, tools.MaxRebuiltBytes,
	session.RestoreFromFolder, session.OmittedCopy)

// WorkRule is the sentence the event routes state work.saved by (spec
// 073): what a client reads of the copy a session keeps of its working
// directory in its folder of the files store.
var WorkRule = fmt.Sprintf("work.saved {covers, written, removed, unsaved, deferred, skipped, skipped_files, held, failed, whole, from} records, for a session that keeps its files in a folder of the files store, "+
	"the copy of its working directory kept at %s/<path> in that folder, path the file's place in the working directory, the latest state alone and no turn's before it. "+
	"A turn of the session's own conversation that ran a call ends with one, before its session.status, when the turn end wrote, removed or left out a file: "+
	"covers is the last sequence the turn end had read when it walked the directory; written each {path, size, mode, sha256} for a file written, new or changed, mode its permission bits and sha256 the hex digest of its bytes; "+
	"removed the paths whose files were deleted; unsaved the paths whose newer bytes were not written, which the copy no longer vouches for; deferred how many changed files were left for a later turn end; "+
	"skipped how many files the copy leaves out and skipped_files the first %d of them, each {path, reason}, reason %s for a file past %d bytes and %s for one past the copy's %d files or %d bytes in path order; "+
	"held {files, bytes} what the copy holds after it; and failed {code, detail} why the save stopped short, code %s or %s, with the files written before it recorded. "+
	"One turn end writes at most %d files of %d bytes together. The copy leaves out the directories %s at any depth, what the credential deny-list names, and anything that is no regular file. "+
	"A fork's first work.saved, whole with from its parent's id, names every file of the copy it starts from, its parent's latest. The blob route does not answer a file of the copy.",
	session.WorkDir, session.MaxRestoreNames, session.WorkTooLarge, session.MaxWorkFileBytes, session.WorkLimit, session.MaxWorkFiles, session.MaxWorkBytes,
	session.WorkMachineUnavailable, session.WorkFilesUnavailable, session.MaxWorkTurnFiles, session.MaxWorkTurnBytes, strings.Join(session.WorkLeftOut, ", "))

// blobCache is a blob's Cache-Control: its bytes never change under its
// digest, so a client keeps it as long as it likes, and private keeps it
// out of any shared cache, since the answer depends on who may read the
// session (spec 055).
const blobCache = "private, max-age=31536000, immutable"

// blob is GET /sessions/{id}/blobs/{digest}. A blob whose leading bytes
// are PNG, JPEG, GIF or WebP is answered as that image type, inline. Any
// other blob is an attachment, labeled with the media type the first
// live event that records it gave the file when it was kept, a message's
// attachment or a file an answer linked, and otherwise with the type its
// leading bytes give. Every answer carries nosniff and the sandboxing
// policy of a file's answer, so no blob is ever a document of the API's
// origin, whatever its label. A file the session keeps in its folder of
// the files store is read from there with the server's own token, as the
// files store streams it, and a blob of the session's store as the store
// reads it; either answers its length. HEAD answers the same headers
// without the bytes.
func (c *call) blob() error {
	d := session.Digest(c.r.PathValue("digest"))
	if !d.Valid() {
		return refuse(CodeInvalidRequest, "%q is not a sha256: digest", d)
	}
	s, err := c.session(authorizer.ActionSessionRead, nil)
	if err != nil {
		return err
	}
	ctx := c.r.Context()
	rec, err := session.FileRecordOf(ctx, c.s.o.Sessions, s.ID, d)
	recorded := err == nil
	if err != nil && !errors.Is(err, session.ErrNotFound) {
		return err
	}
	rc, size, kept, err := c.s.openFile(ctx, s, d)
	if err != nil {
		return err
	}
	if !kept {
		if rc, err = c.s.o.Sessions.Blob(ctx, s.ID, d); err != nil {
			return err
		}
		size = blobSize(rc, rec, recorded)
	}
	defer func() {
		if err := rc.Close(); err != nil {
			c.s.o.Log.WarnContext(ctx, "close a blob", "session", s.ID, "err", err)
		}
	}()
	br := bufio.NewReaderSize(rc, tools.SniffBytes)
	head, err := br.Peek(tools.SniffBytes)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	h := c.w.Header()
	if media := tools.ImageType(head); media != "" {
		h.Set("Content-Type", media)
		h.Set("Content-Disposition", "inline")
	} else {
		h.Set("Content-Type", blobType(rec.MediaType, head))
		h.Set("Content-Disposition", "attachment")
	}
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", filePolicy)
	h.Set("Cache-Control", blobCache)
	h.Set("ETag", strconv.Quote(string(d)))
	if size >= 0 {
		h.Set("Content-Length", strconv.FormatInt(size, 10))
	}
	c.w.WriteHeader(http.StatusOK)
	if c.r.Method == http.MethodHead {
		return nil
	}
	_, err = io.Copy(c.w, br)
	return err
}

// blobSize is the length of a blob of the session's store, read from rc:
// the length its reader states, as every store of this module's does,
// else the size its record names, which was counted from the same bytes
// when the file was kept, and -1 when neither says.
func blobSize(rc io.Reader, rec session.FileRecord, recorded bool) int64 {
	if sized, ok := rc.(interface{ Size() int64 }); ok {
		return sized.Size()
	}
	if recorded {
		return rec.Size
	}
	return -1
}

// blobType is the media type of a blob that is no image: the one its
// record names, written again in canonical form, when it is a type and a
// subtype, and otherwise the one head, its leading bytes, gives.
func blobType(recorded string, head []byte) string {
	if base, params, err := mime.ParseMediaType(recorded); err == nil && strings.Contains(base, "/") {
		if t := mime.FormatMediaType(base, params); t != "" {
			return t
		}
	}
	return http.DetectContentType(head)
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
// find. A file of the session's folder that no live event names once the
// redaction is written is deleted from the folder (spec 073).
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
	by := personSender(c.caller)
	if err := c.s.o.Sessions.Redact(c.r.Context(), s.ID, id, by, b.Reason); err != nil {
		return err
	}
	if also != "" {
		if err := c.s.o.Sessions.Redact(c.r.Context(), s.ID, also, by, b.Reason); err != nil {
			return err
		}
	}
	c.s.dropRedactedFiles(c.r.Context(), s, evs)
	c.w.WriteHeader(http.StatusNoContent)
	return nil
}
