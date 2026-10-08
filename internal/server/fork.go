// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/manifest"
	"latere.ai/x/topos/session"
)

// CodeInvalidForkPoint is spec 017's code for a fork at a sequence that
// is not a turn boundary, or of a session that has none, and spec 056's
// for a fork before an event that is no person's message that opened a
// turn.
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
// at the last turn boundary. Attended is the fork's own declaration that
// a person answers its questions: a fork is a session of its own, and
// its client declares it again (spec 039). BeforeSeq forks before a
// person's message in place of a turn boundary, Message is the message
// the fork is sent in the same call, and Title the fork's title in place
// of its continuation title, "" for none, and Tree, when TreeNew, starts
// a fork tree of the fork's own in place of joining its parent's (spec
// 056).
type forkBody struct {
	AtSeq     *uint64      `json:"at_seq,omitempty"`
	BeforeSeq *uint64      `json:"before_seq,omitempty"`
	Title     *string      `json:"title,omitempty"`
	Message   *messageBody `json:"message,omitempty"`
	Tree      *string      `json:"tree,omitempty"`
	Attended  bool         `json:"attended,omitempty"`
}

// TreeNew is the fork body's tree that starts a fork tree of the fork's
// own: a new conversation from the fork point, whose root is its own id
// and whose parent stays its lineage (spec 056). Absent, a fork joins
// its parent's tree.
const TreeNew = "new"

// forkMessage is the message a fork is sent in the same call: its
// payload without its files, and its files, checked, each new one's
// bytes or a blob of the session forked with that session's record of
// it.
type forkMessage struct {
	payload session.UserMessage
	files   []file
}

// shape is the message's shape as its send question tells it (spec 061).
func (m *forkMessage) shape() shape { return shapeOf(m.payload.Content, len(m.files)) }

// forkSession is POST /sessions/{id}/fork (spec 017): a new session of
// the same agent version whose log starts as a copy of this one's up to
// a turn boundary, or up to a person's message the fork replaces (spec
// 056), with the caller as its initiator and its own lifetime, budget
// and credentials. Any session the caller may read is forked, an ended
// or expired one included; a caller who may not read it hears not_found
// before anything else. A fork sent a message in the same call is asked
// session.send of after session.fork, before anything is written.
func (c *call) forkSession() error {
	raw, err := c.body()
	if err != nil {
		return err
	}
	var b forkBody
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := decodeBody(raw, &b); err != nil {
			return err
		}
	}
	// The route takes a message's bytes; a body without one is held to
	// the bound of every other body.
	if b.Message == nil && len(raw) > MaxBody {
		return refuse(CodePayloadTooLarge, "a fork without a message is at most %d bytes", MaxBody)
	}
	if b.AtSeq != nil && b.BeforeSeq != nil {
		return refuse(CodeInvalidRequest, "at_seq and before_seq name two fork points; a fork names one")
	}
	if b.Tree != nil && *b.Tree != TreeNew {
		return refuse(CodeInvalidRequest, "tree is %q; a fork joins its parent's tree when tree is absent, and starts its own with %q", *b.Tree, TreeNew)
	}
	sender := session.Sender{Subject: c.caller.Subject, Kind: session.SenderPerson}
	var message *forkMessage
	if b.Message != nil {
		payload, files, err := b.Message.check(sender, true)
		if err != nil {
			return err
		}
		message = &forkMessage{payload: payload, files: files}
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
	var seq uint64
	if b.BeforeSeq != nil {
		seq, err = session.ForkBefore(evs, *b.BeforeSeq)
	} else {
		seq, err = session.ForkPoint(evs, b.AtSeq)
	}
	if err != nil {
		return err
	}
	if message != nil {
		if err := message.keep(attached(evs)); err != nil {
			return err
		}
	}
	title := continuedTitle(parent.Title)
	if b.Title != nil {
		title = *b.Title
	}
	s, err := c.s.create(ctx, c.asker(), creation{
		fork: &forkOrigin{parent: parent, seq: seq, events: evs[:seq], model: modelAt(parent, evs, seq), title: title, message: message,
			newTree: b.Tree != nil},
		attended: b.Attended,
		sender:   sender,
	})
	if err != nil {
		return err
	}
	return c.replySession(http.StatusCreated, s)
}

// keep reads the record of each file the message names by blob from
// kept, the files the session forked attached, and refuses a blob it
// does not hold.
func (m *forkMessage) keep(kept map[session.Digest]session.Attachment) error {
	for i, f := range m.files {
		if f.blob == "" {
			continue
		}
		a, ok := kept[f.blob]
		if !ok {
			return refuse(CodeInvalidRequest, "attachments[%d] names the blob %s, which no message of the session forked attaches", i, f.blob)
		}
		m.files[i].media, m.files[i].size = a.MediaType, a.Size
	}
	return nil
}

// events are the message as the events the fork appends after its copy:
// the changes the allow of its send made, then the message, its files
// recorded under its event's id, each new file's bytes added to blobs.
func (m *forkMessage) events(changes sendChanges, now time.Time, blobs map[session.Digest][]byte) ([]session.Event, error) {
	out, err := changes.events(now)
	if err != nil {
		return nil, err
	}
	id := session.NewID(session.PrefixEvent)
	payload := m.payload
	for _, f := range m.files {
		a := session.Attachment{Name: f.name, MediaType: f.media, Size: f.size, Blob: f.blob, Path: session.AttachmentPath(id, f.name)}
		if f.blob == "" {
			a.Size, a.Blob = int64(len(f.data)), session.DigestOf(f.data)
			blobs[a.Blob] = f.data
		}
		payload.Attachments = append(payload.Attachments, a)
	}
	ev, err := session.NewEvent(session.TypeUserMessage, payload, now)
	if err != nil {
		return nil, err
	}
	ev.ID = id
	return append(out, ev), nil
}

// writeFork writes the fork sess of f, with blobs, the agent's. A fork
// sent a message asks session.send of its header first, with the model
// it starts on and the whole seconds since the last request it copied,
// and writes the copy, the changes the allow made and the message as one
// batch, after which a runner claims it (spec 056). A send refused after
// the authorizer allowed session.fork writes nothing and is reported to
// the sink, so an authorizer that recorded the fork at that allow closes
// the record.
func (s *Server) writeFork(ctx context.Context, q asker, sess session.Session, cfg manifest.AgentConfig, blobs map[session.Digest][]byte, f *forkOrigin) (session.Session, error) {
	if f.message == nil {
		return session.Fork(ctx, s.o.Sessions, sess, blobs, f.parent, f.events)
	}
	t := tailOfCopy(f.events)
	fields := map[string]any{"sender": q.caller.Subject, "event_type": string(session.TypeUserMessage)}
	maps.Copy(fields, f.message.shape().fields(t.tools))
	changes, err := s.askSend(ctx, q, sess, cfg, fields, t.at, t.made)
	if err != nil {
		s.refusedFork(ctx, q, sess, f, err)
		return session.Session{}, err
	}
	all := maps.Clone(blobs)
	if all == nil {
		all = map[session.Digest][]byte{}
	}
	then, err := f.message.events(changes, s.o.Now(), all)
	if err != nil {
		return session.Session{}, err
	}
	forked, err := session.Fork(ctx, s.o.Sessions, sess, all, f.parent, f.events, then...)
	if err != nil {
		return session.Session{}, err
	}
	s.o.Notify()
	return forked, nil
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

// treeFilters reads the list's fork tree filters (spec 056): root and
// parent, each a session's id, which an id of no session matches nothing
// by, and group, which takes tree alone.
func treeFilters(q url.Values) (session.ListOptions, error) {
	o := session.ListOptions{Root: q.Get("root"), Parent: q.Get("parent"), Group: session.Group(q.Get("group"))}
	if o.Group != "" && o.Group != session.GroupTree {
		return o, refuse(CodeInvalidRequest, "group is %q; a list groups by tree alone", o.Group)
	}
	return o, nil
}
