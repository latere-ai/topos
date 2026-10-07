// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/manifest"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
)

// updateBody is the body of PATCH /sessions/{id}: the fields of the
// session a request changes, its model, the approval mode of its policy
// and its title (spec 054), and its metadata, a merge in which a string
// sets its key and null deletes it (spec 057), any of them.
type updateBody struct {
	Model    *modelChange       `json:"model"`
	Policy   *policyChange      `json:"policy"`
	Title    *string            `json:"title,omitempty"`
	Metadata map[string]*string `json:"metadata,omitempty"`
}

// metadataOnly reports a body that changes the session's metadata and
// nothing else, which is taken in every status (spec 057).
func (b updateBody) metadataOnly() bool {
	return b.Metadata != nil && b.Model == nil && b.Policy == nil && b.Title == nil
}

// policyChange is what a PATCH changes of the session's policy: the
// approval mode its next steps decide calls under (spec 041). The lists
// and the thresholds are the authorizer's and the agent's, so the mode is
// its one member.
type policyChange struct {
	Mode *string `json:"mode"`
}

// modes are the approval modes a session may be set to, strictest first.
var modes = []string{v1.ModePlan, v1.ModeConfirm, v1.ModeProgressive}

// modelChange is what a PATCH changes of the session's model: its name,
// its reasoning level, or both. A member left out keeps what the session
// runs, so each is a pointer; an empty level returns to the agent's own.
// The level is named reasoning, or effort, its name before spec 049,
// which is read through every v0.x release; a body that names both names
// one level.
type modelChange struct {
	Name      *string `json:"name,omitempty"`
	Reasoning *string `json:"reasoning,omitempty"`
	Effort    *string `json:"effort,omitempty"`
}

// level is the reasoning level the change names under either name, nil
// when it names none.
func (m modelChange) level() *string {
	if m.Reasoning != nil {
		return m.Reasoning
	}
	return m.Effort
}

// updateSession is PATCH /sessions/{id}: the model the session's next
// turn runs and the reasoning level it runs at (spec 015), and the
// approval mode its next steps decide calls under (spec 041). A caller
// who may not read the session hears not_found first. One question
// carries what the body changes: model when it names one, and the level,
// resolved to what the next turn runs, when it names one, under both
// effort and reasoning, so an authorizer that reads either name decides
// it (spec 049), beside the model the session stands on; approval_mode
// beside the mode the session runs and its agent's. The allow may answer
// a model the body names with the one to run in its place (spec 038), so
// the model is checked after the question, by the rule a session's create
// checks its own by: the one the allow names, or the one asked when it
// names none. The allow may also answer the level the change runs at
// (spec 049). A title, trimmed, is asked as title beside the rest (spec
// 054). An allowed change appends session.model_changed,
// session.policy_changed and session.title_changed in one batch, in that
// order, which the header takes; a change to what the session runs and
// the title it has appends nothing.
func (c *call) updateSession() error {
	var b updateBody
	if err := c.decode(&b); err != nil {
		return err
	}
	if err := b.check(); err != nil {
		return err
	}
	ctx := c.r.Context()
	s, err := c.session(authorizer.ActionSessionRead, nil)
	if err != nil {
		return err
	}
	var relabel *relabeling
	if b.Metadata != nil {
		if relabel, err = c.s.relabeling(s, b.Metadata); err != nil {
			return err
		}
	}
	if b.metadataOnly() {
		if _, err := c.ask(ctx, authorizer.ActionSessionUpdate, sessionResource(s, relabel.fields(map[string]any{"session_id": s.ID}))); err != nil {
			return err
		}
		if s, err = c.s.relabel(ctx, c.caller.Subject, s, relabel); err != nil {
			return err
		}
		return c.replySession(http.StatusOK, s)
	}
	if s.Status == session.StatusEnded {
		return refuse(CodeConflict, "the session ended %s", s.StopReason)
	}
	cfg, err := c.s.agentConfig(ctx, s)
	if err != nil {
		return err
	}
	old := standing(s, cfg)
	next := old
	fields := map[string]any{"session_id": s.ID}
	if b.Model != nil {
		fields["current_model"] = old.Name
		if old.Via != "" {
			fields["current_model_via"] = old.Via
		}
		if b.Model.Name != nil {
			fields["model"] = *b.Model.Name
		}
		if level := b.Model.level(); level != nil {
			next.Effort = cmp.Or(*level, cfg.Effort)
			fields["effort"], fields["reasoning"] = next.Effort, next.Effort
		}
	}
	agentMode := cmp.Or(cfg.Policy.Mode, harness.ModeConfirm)
	oldMode := standingMode(s, agentMode)
	nextMode := oldMode
	if b.Policy != nil {
		nextMode = harness.Mode(*b.Policy.Mode)
		fields["approval_mode"] = string(nextMode)
		fields["current_approval_mode"] = string(oldMode)
		fields["agent_approval_mode"] = string(agentMode)
	}
	if b.Title != nil {
		fields["title"] = *b.Title
	}
	if relabel != nil {
		fields = relabel.fields(fields)
	}
	limits, err := c.askLimits(ctx, authorizer.ActionSessionUpdate, sessionResource(s, fields))
	if err != nil {
		return err
	}
	// A change that names a model runs the one the allow names in its
	// place and keeps the name asked beside it. A level change alone
	// names no model, and an allow that names one for it moves nothing.
	if b.Model != nil && b.Model.Name != nil {
		asked := *b.Model.Name
		next.Name, next.Via = cmp.Or(limits.Model, asked), ""
		if next.Name != asked {
			next.Via = asked
		}
		m, overlay := cfg.SessionModel(next.Name)
		if err := c.s.runnable(ctx, m, overlay); err != nil {
			return err
		}
	}
	// A change of the model or the level runs at the level the allow
	// names when it names one, "" being the agent's own; a change of the
	// mode alone moves no model and reads none.
	if r := limits.Reasoning; b.Model != nil && r != nil {
		next.Effort = cmp.Or(*r, cfg.Effort)
	}
	sender := session.Sender{Subject: c.caller.Subject, Kind: session.SenderPerson}
	now := c.s.o.Now()
	var batch []session.Event
	if next != old {
		ev, err := session.NewEvent(session.TypeModelChanged, session.ModelChanged{By: sender, Old: old, New: next}, now)
		if err != nil {
			return err
		}
		batch = append(batch, ev)
	}
	if nextMode != oldMode {
		ev, err := session.NewEvent(session.TypePolicyChanged, session.PolicyChanged{By: sender, Old: session.PolicyRef{Mode: string(oldMode)}, New: session.PolicyRef{Mode: string(nextMode)}}, now)
		if err != nil {
			return err
		}
		batch = append(batch, ev)
	}
	if b.Title != nil && *b.Title != s.Title {
		ev, err := session.NewEvent(session.TypeTitleChanged, session.TitleChanged{By: sender, Old: s.Title, New: *b.Title}, now)
		if err != nil {
			return err
		}
		batch = append(batch, ev)
	}
	if len(batch) > 0 {
		if err := c.s.appendBatch(ctx, s.ID, batch); err != nil {
			return err
		}
		if s, err = c.s.o.Sessions.Get(ctx, s.ID); err != nil {
			return err
		}
	}
	// The metadata is written after the events, so a change refused
	// because the session ended meanwhile leaves the metadata as it was.
	if relabel != nil {
		if s, err = c.s.relabel(ctx, c.caller.Subject, s, relabel); err != nil {
			return err
		}
	}
	return c.replySession(http.StatusOK, s)
}

// relabeling is a checked change of a session's metadata: the change as
// the body sent it and the value each key it names has now.
type relabeling struct {
	change  map[string]*string
	current map[string]string
}

// relabeling checks a change of s's metadata on the merged result, which
// holds a session created before the key and value rules to them too
// (spec 057), and answers it with the values the keys it names hold now.
func (s *Server) relabeling(sess session.Session, change map[string]*string) (*relabeling, error) {
	if _, ok := s.o.Sessions.(session.Labeler); !ok {
		return nil, errors.New("server: the session store does not change metadata")
	}
	merged, _ := session.MergeMetadata(sess.Metadata, change)
	if err := session.CheckMetadata(merged); err != nil {
		return nil, refuse(CodeInvalidRequest, "the metadata after the change: %v", err)
	}
	current := map[string]string{}
	for k := range change {
		if v, ok := sess.Metadata[k]; ok {
			current[k] = v
		}
	}
	return &relabeling{change: change, current: current}, nil
}

// fields adds the change to a session.update question's fields: metadata,
// the change as sent with null for a deletion, and current_metadata, the
// value each key it names holds now, a key the session does not hold left
// out.
func (r *relabeling) fields(fields map[string]any) map[string]any {
	change := make(map[string]any, len(r.change))
	for k, v := range r.change {
		if v == nil {
			change[k] = nil
		} else {
			change[k] = *v
		}
	}
	fields["metadata"], fields["current_metadata"] = change, metadataField(r.current)
	return fields
}

// relabel writes an allowed change of sess's metadata through the store,
// with no event, answers the session after, and reports the keys it
// changed to the installation's sink (spec 057). A change to what the
// session holds writes and reports nothing.
func (s *Server) relabel(ctx context.Context, subject string, sess session.Session, r *relabeling) (session.Session, error) {
	l, ok := s.o.Sessions.(session.Labeler)
	if !ok {
		return session.Session{}, errors.New("server: the session store does not change metadata")
	}
	after, err := l.SetMetadata(ctx, sess.ID, r.change)
	if err != nil {
		return session.Session{}, err
	}
	var keys []string
	for k := range r.change {
		if old, had := sess.Metadata[k]; had != hasKey(after.Metadata, k) || old != after.Metadata[k] {
			keys = append(keys, k)
		}
	}
	if len(keys) > 0 {
		slices.Sort(keys)
		s.report(ctx, SinkEvent{
			Type: authorizer.ActionSessionUpdate, OccurredAt: s.o.Now().UTC(), Subject: subject,
			Object: SinkObject{Kind: authorizer.KindSession, ID: sess.ID}, SessionID: sess.ID, Agent: &SinkAgent{ID: sess.Agent.ID, Version: sess.Agent.Version},
			Outcome: "ok", Attributes: map[string]any{"metadata_keys": keys},
		})
	}
	return after, nil
}

// hasKey reports whether m holds k.
func hasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}

// check refuses a body that names nothing to change or a value no
// session takes, before the session is read, and trims the title it
// names.
func (b *updateBody) check() error {
	if (b.Model == nil || (b.Model.Name == nil && b.Model.level() == nil)) && b.Policy == nil && b.Title == nil && b.Metadata == nil {
		return refuse(CodeInvalidRequest, `the body names what changes: {"model": {"name": "...", "reasoning": "..."}}, either member or both, {"policy": {"mode": "..."}}, {"title": "..."}, {"metadata": {"<key>": "<value>" or null}}, or more than one`)
	}
	if b.Metadata != nil {
		if len(b.Metadata) == 0 {
			return refuse(CodeInvalidRequest, "metadata names no key to set or delete")
		}
		for _, k := range slices.Sorted(maps.Keys(b.Metadata)) {
			if !session.ValidMetadataKey(k) {
				return refuse(CodeInvalidRequest, "metadata key %q is not %s", k, session.MetadataKeyRule)
			}
			if v := b.Metadata[k]; v != nil {
				if why := session.CheckMetadataValue(*v); why != "" {
					return refuse(CodeInvalidRequest, "metadata %q: %s", k, why)
				}
			}
		}
	}
	if b.Title != nil {
		title := strings.TrimSpace(*b.Title)
		if why := session.CheckTitle(title); why != "" {
			return refuse(CodeInvalidRequest, "title: %s; a title is 1 to %d characters on one line", why, session.MaxTitleLength)
		}
		b.Title = &title
	}
	if b.Model != nil {
		if b.Model.Name == nil && b.Model.level() == nil {
			return refuse(CodeInvalidRequest, "model names neither name nor reasoning")
		}
		if n := b.Model.Name; n != nil && (*n == "" || strings.TrimSpace(*n) != *n) {
			return refuse(CodeInvalidRequest, "model.name is %q, not a model's name", *n)
		}
		for _, l := range []struct {
			name  string
			level *string
		}{{"reasoning", b.Model.Reasoning}, {"effort", b.Model.Effort}} {
			if l.level != nil && *l.level != "" && !slices.Contains(v1.Efforts, *l.level) {
				return refuse(CodeInvalidRequest, "model.%s is %q, not one of %s, or empty for the agent's own", l.name, *l.level, strings.Join(v1.Efforts, ", "))
			}
		}
		if r, e := b.Model.Reasoning, b.Model.Effort; r != nil && e != nil && *r != *e {
			return refuse(CodeInvalidRequest, "model.reasoning is %q and model.effort %q: name the level once, as reasoning", *r, *e)
		}
	}
	if b.Policy != nil && (b.Policy.Mode == nil || !slices.Contains(modes, *b.Policy.Mode)) {
		got := "absent"
		if b.Policy.Mode != nil {
			got = strconv.Quote(*b.Policy.Mode)
		}
		return refuse(CodeInvalidRequest, "policy.mode is %s, not one of %s", got, strings.Join(modes, ", "))
	}
	return nil
}

// standingMode is the approval mode s runs: its recorded policy's, and
// its agent's, agentMode, for a session that records none (spec 041).
func standingMode(s session.Session, agentMode harness.Mode) harness.Mode {
	if s.Policy != nil && s.Policy.Mode != "" {
		return harness.Mode(s.Policy.Mode)
	}
	return agentMode
}

// agentConfig is the configuration of the agent version sess runs.
func (s *Server) agentConfig(ctx context.Context, sess session.Session) (manifest.AgentConfig, error) {
	v, err := s.o.Objects.Version(ctx, sess.Agent.ID, sess.Agent.Version)
	if err != nil {
		return manifest.AgentConfig{}, err
	}
	r, err := manifest.ReadBundle(v.Bundle)
	if err != nil {
		return manifest.AgentConfig{}, err
	}
	return r.AgentConfig(nil)
}

// standing is the model s runs as it stands, with the name it was asked
// by and the reasoning level its next turn runs at: its header's, or its
// agent's until a first change. A header written before changes carried
// a level names none, and its turns run at the agent's.
func standing(s session.Session, cfg manifest.AgentConfig) session.ModelRef {
	if s.Model == nil {
		return session.ModelRef{Name: cfg.Model.Name, Effort: cfg.Effort}
	}
	return session.ModelRef{Name: s.Model.Name, Via: s.Model.Via, Effort: cmp.Or(s.Model.Effort, cfg.Effort)}
}

// runnable refuses a session's model the installation does not run
// (spec 007): model_unknown for one no source gives figures, and
// model_unavailable for one whose figures could not be read.
func (s *Server) runnable(ctx context.Context, m v1.AgentModel, overlay models.Entry) error {
	err := s.o.Runnable(ctx, m, overlay)
	if err == nil {
		return nil
	}
	if mc, ok := errors.AsType[*models.Coded](err); ok && mc.Code == models.CodeUnknown {
		return &apiError{code: models.CodeUnknown, detail: mc.Message, err: err}
	}
	return &apiError{code: models.CodeUnavailable, detail: "the figures of " + m.Name + " could not be read", err: err}
}
