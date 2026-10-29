// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"

	"latere.ai/x/topos/memory"
	"latere.ai/x/topos/session"
)

// conversations is the reach to the folders of conversations of a drive
// holding lease on the session id (spec 094): the lease's own,
// Options.Conversations's, or nil.
func (r *Runner) conversations(id string, lease session.Lease) memory.Conversations {
	if c, ok := lease.(memory.Conversations); ok {
		return c
	}
	if r.o.Conversations == nil {
		return nil
	}
	return r.o.Conversations(id)
}

type conversationsKey struct{}

// WithConversations is ctx carrying a drive's reach to its folders of
// conversations, which the harness builder reads with ConversationsFrom.
func WithConversations(ctx context.Context, c memory.Conversations) context.Context {
	return context.WithValue(ctx, conversationsKey{}, c)
}

// ConversationsFrom is the reach to its folders of conversations of the
// drive ctx belongs to, nil when the runner reaches no server that keeps
// its sessions' logs.
func ConversationsFrom(ctx context.Context) memory.Conversations {
	c, _ := ctx.Value(conversationsKey{}).(memory.Conversations)
	return c
}
