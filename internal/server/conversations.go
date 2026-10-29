// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"slices"
	"sync"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/memory"
	"latere.ai/x/topos/session"
)

// The folders of conversations (spec 094). A session's allow may name a
// memory folder that holds the initiator's other conversations in place
// of a folder of a files store; the server serves it from its own log, a
// Markdown file per conversation, and asks the authorizer every list and
// every read as the session's initiator in the session's context, with
// the session and the folder named as the question's origin.

// OriginMemory is the kind of origin a question of a folder of
// conversations names.
const OriginMemory = "memory"

// conversationsPage is how many sessions one read of the store's list asks
// for while a folder is listed.
const conversationsPage = 200

// conversationReaders is how many sessions of a page are asked
// session.read at once.
const conversationReaders = 8

// errListRefused is the sentence the model reads when the authorizer
// refuses a folder's list.
const errListRefused = "This session may not read these conversations now."

// SessionConversations is the session id's reach to its folders of
// conversations, which its memory machine asks: the runners this server
// drives read it directly, and a runner process's lease asks it over the
// internal listener.
func (s *Server) SessionConversations(id string) memory.Conversations {
	return sessionConversations{s: s, id: id}
}

type sessionConversations struct {
	s  *Server
	id string
}

var _ memory.Conversations = sessionConversations{}

// reach is the reading session, its folder of conversations named name,
// its initiator as an asker in its context, and the origin its questions
// name. A folder the session does not hold, or holds of a files store, is
// not there.
type reach struct {
	sess   session.Session
	folder session.MemoryFolder
	q      asker
	origin map[string]any
}

func (c sessionConversations) reach(ctx context.Context, name string) (reach, error) {
	sess, err := c.s.o.Sessions.Get(ctx, c.id)
	if err != nil {
		return reach{}, err
	}
	i := slices.IndexFunc(sess.Memory, func(f session.MemoryFolder) bool { return f.Name == name && f.Conversations() })
	if i < 0 {
		return reach{}, fmt.Errorf("%w: session %s holds no folder of conversations named %s", fs.ErrNotExist, sess.ID, name)
	}
	q, err := c.s.initiator(ctx, sess)
	if err != nil {
		return reach{}, err
	}
	return reach{sess: sess, folder: sess.Memory[i], q: q,
		origin: map[string]any{"kind": OriginMemory, "session_id": sess.ID, "folder": name}}, nil
}

// scope is where a folder's conversations are listed from: the sessions of
// the agents of the reading session's context, of its initiator alone,
// kept by the folder's metadata entry, of no rollout, one per fork tree. A
// list asks session.list first and holds the scope to the owners its
// decision names; a read is decided by its own session.read alone. none
// reports a scope that holds nothing.
func (r reach) scope(ctx context.Context, s *Server, list bool) (o session.ListOptions, none bool, err error) {
	here := contextOf(r.q.caller)
	initiator := r.sess.Initiator.Subject
	if list {
		d, err := r.q.ask(ctx, authorizer.ActionSessionList, authz.NewResource(authorizer.KindSession, "",
			map[string]any{"status": "", "runner": "", "agent_owner": here.field(), "origin": r.origin}))
		if err != nil {
			return o, false, listRefusal(err)
		}
		if d.Filter != nil && len(d.Filter.Owners) > 0 && !slices.Contains(d.Filter.Owners, initiator) {
			return o, true, nil
		}
	}
	agents, err := s.agentsOf(ctx, here)
	if err != nil {
		return o, false, err
	}
	o = session.ListOptions{Owners: []string{initiator}, Agents: agents, Metadata: r.folder.Sessions.Entry(),
		Rollout: session.RolloutNone, Group: session.GroupTree, Archived: session.ArchivedAny}
	return o, len(agents) == 0, nil
}

// keeps reports whether target is a conversation of the folder: of the
// scope o names, and of another fork tree than the reading session's.
func (r reach) keeps(o session.ListOptions, target session.Session) bool {
	return target.TreeRoot() != r.sess.TreeRoot() && target.ID != r.sess.ID &&
		slices.Contains(o.Owners, target.Initiator.Subject) && slices.Contains(o.Agents, target.Agent.ID) &&
		o.Metadata.Keeps(target) && target.Rollout == ""
}

// readable asks session.read of target as the initiator, naming the
// origin and target's metadata, and reports whether it is allowed. A
// denied read is false and no error.
func (r reach) readable(ctx context.Context, target session.Session) (bool, error) {
	fields := map[string]any{"origin": r.origin}
	if len(target.Metadata) > 0 {
		fields["metadata"] = metadataField(target.Metadata)
	}
	_, err := r.q.ask(ctx, authorizer.ActionSessionRead, sessionResource(target, fields))
	switch {
	case err == nil:
		return true, nil
	case auth.Code(err) == auth.CodeNotFound || auth.Code(err) == auth.CodeForbidden:
		return false, nil
	}
	return false, err
}

// ListConversations answers the folder's files, newest first: each
// session of the scope the initiator may read, at most memory.MaxListed,
// and whether the scope held more.
func (c sessionConversations) ListConversations(ctx context.Context, folder string) ([]memory.ConversationFile, bool, error) {
	r, err := c.reach(ctx, folder)
	if err != nil {
		return nil, false, err
	}
	o, none, err := r.scope(ctx, c.s, true)
	if err != nil || none {
		return []memory.ConversationFile{}, false, err
	}
	out := []memory.ConversationFile{}
	o.Limit = conversationsPage
	for {
		page, next, err := c.s.o.Sessions.List(ctx, o)
		if err != nil {
			return nil, false, err
		}
		kept := make([]session.Session, 0, len(page))
		for _, s := range page {
			if r.keeps(o, s) {
				kept = append(kept, s)
			}
		}
		allowed, err := r.admit(ctx, kept)
		if err != nil {
			return nil, false, err
		}
		for i, s := range kept {
			if !allowed[i] {
				continue
			}
			if len(out) == memory.MaxListed {
				return out, true, nil
			}
			out = append(out, memory.ConversationFileOf(s))
		}
		if next == "" {
			return out, false, nil
		}
		o.Cursor = next
	}
}

// admit asks session.read of each of kept, conversationReaders at a time,
// and answers which are allowed, in kept's order.
func (r reach) admit(ctx context.Context, kept []session.Session) ([]bool, error) {
	allowed := make([]bool, len(kept))
	errs := make([]error, len(kept))
	var wg sync.WaitGroup
	next := make(chan int)
	for range min(conversationReaders, len(kept)) {
		wg.Go(func() {
			for i := range next {
				allowed[i], errs[i] = r.readable(ctx, kept[i])
			}
		})
	}
	for i := range kept {
		next <- i
	}
	close(next)
	wg.Wait()
	return allowed, errors.Join(errs...)
}

// ReadConversation answers the file name of the folder: the conversation
// it names when that is a session of the folder's scope the initiator may
// read, rendered from its log.
func (c sessionConversations) ReadConversation(ctx context.Context, folder, name string) (memory.ConversationFile, []byte, error) {
	id, ok := memory.ConversationID(name)
	if !ok {
		return memory.ConversationFile{}, nil, fmt.Errorf("%w: %s names no conversation", fs.ErrNotExist, name)
	}
	r, err := c.reach(ctx, folder)
	if err != nil {
		return memory.ConversationFile{}, nil, err
	}
	o, none, err := r.scope(ctx, c.s, false)
	if err != nil {
		return memory.ConversationFile{}, nil, err
	}
	gone := fmt.Errorf("%w: %s is no conversation of the folder %s", fs.ErrNotExist, name, folder)
	if none {
		return memory.ConversationFile{}, nil, gone
	}
	target, err := c.s.o.Sessions.Get(ctx, id)
	switch {
	case errors.Is(err, session.ErrNotFound):
		return memory.ConversationFile{}, nil, gone
	case err != nil:
		return memory.ConversationFile{}, nil, err
	case memory.ConversationName(target) != name || !r.keeps(o, target):
		return memory.ConversationFile{}, nil, gone
	}
	allowed, err := r.readable(ctx, target)
	if err != nil {
		return memory.ConversationFile{}, nil, err
	}
	if !allowed {
		return memory.ConversationFile{}, nil, gone
	}
	evs, err := c.s.o.Sessions.Events(ctx, target.ID, 1, 0)
	if err != nil {
		return memory.ConversationFile{}, nil, err
	}
	text := memory.RenderConversation(target, evs)
	file := memory.ConversationFileOf(target)
	file.Size = int64(len(text))
	return file, text, nil
}

// listRefusal is a refused list of a folder as the file tools say it: a
// deny is a refusal naming the authorizer's reason, and every other error
// is as it is.
func listRefusal(err error) error {
	e, ok := errors.AsType[*auth.Error](err)
	if !ok || e.Code != auth.CodeForbidden {
		return err
	}
	return &memory.Refusal{Op: "listing", Status: http.StatusForbidden, Code: e.Code, Message: errListRefused, Reason: e.Reason}
}
