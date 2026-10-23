// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/internal/triggers"
)

// The hold (spec 086) stops every trigger an owner owns at once: while it
// is set, the minute loop fires none of their triggers and records
// nothing, the fire route refuses each as it refuses a suspended one,
// and a one-shot whose time passes keeps it. On release a schedule starts
// from then, so it fires nothing it missed, and a one-shot fires within
// the minute, maxAge aside. The hold is the owner's across every context:
// it keys by the owner's subject, as the owner's triggers are.

// holdBody is the body of PUT /triggers/hold.
type holdBody struct {
	Held *bool `json:"held"`
}

// holdAnswer is the hold as the API answers it.
type holdAnswer struct {
	Held  bool       `json:"held"`
	Since *time.Time `json:"since,omitempty"`
}

// holdResource is the owner as trigger.hold names it: owner, and held
// when the question sets it.
func holdResource(owner string, held *bool) authz.Resource {
	f := map[string]any{"owner": owner}
	if held != nil {
		f["held"] = *held
	}
	return authz.NewResource(authorizer.KindTrigger, "", f)
}

// getHold is GET /triggers/hold: the caller's hold.
func (c *call) getHold() error {
	owner := c.caller.Subject
	if _, err := c.ask(c.r.Context(), authorizer.ActionTriggerHold, holdResource(owner, nil)); err != nil {
		return err
	}
	return c.replyHold(owner)
}

// setHold is PUT /triggers/hold: sets or releases the caller's hold.
func (c *call) setHold() error {
	var b holdBody
	if err := c.decode(&b); err != nil {
		return err
	}
	if b.Held == nil {
		return refuse(CodeInvalidRequest, "held is required: true to hold every trigger, false to release them")
	}
	ctx := c.r.Context()
	owner := c.caller.Subject
	if _, err := c.ask(ctx, authorizer.ActionTriggerHold, holdResource(owner, b.Held)); err != nil {
		return err
	}
	if *b.Held {
		if err := c.s.o.Objects.SetHold(ctx, owner, new(c.s.o.Now().UTC())); err != nil {
			return err
		}
		return c.replyHold(owner)
	}
	if err := c.s.release(ctx, owner); err != nil {
		return err
	}
	return c.replyHold(owner)
}

// replyHold answers owner's hold.
func (c *call) replyHold(owner string) error {
	since, err := c.s.o.Objects.Hold(c.r.Context(), owner)
	if err != nil {
		return err
	}
	return c.reply(http.StatusOK, holdAnswer{Held: since != nil, Since: since})
}

// release ends owner's hold: every schedule of theirs that runs starts
// from now, so it fires nothing it missed while held, and a one-shot
// keeps its time, which the next pass of the minute loop fires.
func (s *Server) release(ctx context.Context, owner string) error {
	if err := s.o.Objects.SetHold(ctx, owner, nil); err != nil {
		return err
	}
	now := s.o.Now().UTC()
	cursor := ""
	for {
		page, next, err := s.o.Objects.ListTriggers(ctx, store.TriggerList{Owners: []string{owner}, Limit: listPage, Cursor: cursor})
		if err != nil {
			return err
		}
		for _, t := range page {
			spec, err := triggers.Spec(t)
			if err != nil {
				return err
			}
			if t.Suspended || spec.At != nil || spec.On != nil {
				continue
			}
			at, err := triggers.NextFire(spec, now)
			if err != nil {
				return err
			}
			if err := s.o.Objects.SetNextFire(ctx, t.ID, at); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
}

// held reports whether owner holds every trigger they own, for the
// results of a session's trigger and wake calls, which go on as without
// the hold and tell the model so.
func (s *Server) held(ctx context.Context, owner string) (bool, error) {
	since, err := s.o.Objects.Hold(ctx, owner)
	return since != nil, err
}
