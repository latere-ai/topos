// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/manifest"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// agentResource is an existing agent as the authorizer reads it, its
// owner a person's subject or an organization's {type, id} (spec 036).
func agentResource(a store.Agent) authz.Resource {
	return authz.NewResource(authorizer.KindAgent, a.ID, map[string]any{"name": a.Name, "owner": ownerOf(a).field()})
}

// agent finds an agent by id, or by a name among the agents of the
// caller's context, and asks action about it. A name is unique within
// its owner, so another owner's agent of the name is not found by it. A
// denied read answers as a missing agent.
func (c *call) agent(ref, action string) (store.Agent, *v1.Agent, error) {
	a, err := store.FindAgent(c.r.Context(), c.s.o.Objects, c.here().subject, ref)
	if err != nil {
		return store.Agent{}, nil, err
	}
	doc, err := c.latest(a)
	if err != nil {
		return store.Agent{}, nil, err
	}
	if _, err := c.ask(c.r.Context(), action, agentResource(a)); err != nil {
		return store.Agent{}, nil, err
	}
	return a, doc, nil
}

// latest is an agent's latest version, as the answer renders it.
func (c *call) latest(a store.Agent) (*v1.Agent, error) {
	v, err := c.s.o.Objects.Version(c.r.Context(), a.ID, a.Latest)
	if err != nil {
		return nil, err
	}
	return render(a, v)
}

// render is a stored version with the agent's archive state.
func render(a store.Agent, v store.AgentVersion) (*v1.Agent, error) {
	doc, err := store.DecodeAgent(v.Doc)
	if err != nil {
		return nil, err
	}
	doc.Status.ArchivedAt = a.ArchivedAt
	return doc, nil
}

// scopedLookup answers the resolver with the agents the caller may read,
// a name read among the agents of the caller's context, so a manifest's
// references cannot tell another owner's agent from none.
type scopedLookup struct {
	manifest.Lookup
	c *call
}

func (l scopedLookup) Agent(ctx context.Context, ref string) (*v1.Agent, error) {
	doc, err := l.Lookup.Agent(ctx, ref)
	if err != nil {
		return nil, err
	}
	a, err := l.c.s.o.Objects.Agent(ctx, doc.Status.ID)
	if err != nil {
		return nil, err
	}
	if _, err := l.c.ask(ctx, authorizer.ActionAgentRead, agentResource(a)); err != nil {
		if auth.Code(err) == auth.CodeNotFound {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return doc, nil
}

// applyAgent is PUT /agents/{name}: resolve one Agent manifest and store
// the version it resolves to when its spec changed.
func (c *call) applyAgent() error {
	ctx := c.r.Context()
	name := c.r.PathValue("name")
	body, err := c.body()
	if err != nil {
		return err
	}
	here := c.here()
	rs, err := manifest.Resolve(ctx, body, manifest.Options{Lookup: scopedLookup{store.LookupIn(c.s.o.Objects, here.subject, c.caller.Subject), c}, Now: c.s.o.Now})
	if err != nil {
		return err
	}
	if len(rs) != 1 || rs[0].Agent == nil {
		return refuse(CodeInvalidRequest, "the body holds %d documents; PUT /agents takes one Agent", len(rs))
	}
	r := rs[0]
	if r.Name != name {
		return refuse(CodeInvalidRequest, "the manifest names the agent %q and the path %q", r.Name, name)
	}
	st := r.Agent.Status
	// The name is the context's: an agent of it another owner holds is
	// not this one, and the apply creates the context's.
	stored, err := c.s.o.Objects.AgentByName(ctx, here.subject, name)
	exists := err == nil
	var allowed authorizer.Limits
	switch {
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return err
	case exists:
		if allowed, err = c.askLimits(ctx, authorizer.ActionAgentUpdate, agentResource(stored)); err != nil {
			return err
		}
		if stored.ArchivedAt != nil {
			return refuse(CodeConflict, "the agent %s is archived", name)
		}
		if st.ID != stored.ID {
			// The caller may update the agent but not read it: the
			// resolver saw no agent of the name.
			return refuse(CodeConflict, "the agent %s changed while it was applied; apply again", name)
		}
		if st.Version == stored.Latest {
			return c.applyMetadata(ctx, r)
		}
	default:
		// A create in an organization's context names the organization
		// the agent will belong to; a person's names none, the creator
		// becoming the owner.
		fields := map[string]any{"name": name}
		if _, ok := here.organization(); ok {
			fields["owner"] = here.field()
		}
		if allowed, err = c.askLimits(ctx, authorizer.ActionAgentCreate, authz.NewResource(authorizer.KindAgent, "", fields)); err != nil {
			return err
		}
	}
	held := here
	if exists {
		held = ownerOf(stored)
	}
	if err := c.ensureIdentity(ctx, r.Agent, allowed.Owner, held); err != nil {
		return err
	}
	doc, err := session.Marshal(r.Agent)
	if err != nil {
		return err
	}
	bundle, err := r.Bundle()
	if err != nil {
		return err
	}
	a := store.Agent{ID: st.ID, Name: name, Owner: here.subject, OwnerType: here.kind, CreatedAt: st.CreatedAt}
	v := store.AgentVersion{AgentID: st.ID, Version: st.Version, Digest: st.Digest, Doc: doc, Bundle: bundle, CreatedBy: c.caller.Subject, CreatedAt: st.CreatedAt}
	if err := c.s.o.Objects.PutVersion(ctx, a, v); err != nil {
		return err
	}
	status := http.StatusOK
	if !exists {
		status = http.StatusCreated
	}
	return c.reply(status, r.Agent)
}

// applyMetadata answers an apply whose spec resolves to the stored
// latest version's digest. The metadata is not part of the spec, so a
// changed display name, label or annotation makes no version; it
// rewrites the latest version's document and bundle, and every later
// read and every session created from it carries what was last applied.
// An apply that changes nothing writes nothing.
func (c *call) applyMetadata(ctx context.Context, r manifest.Resolved) error {
	st := r.Agent.Status
	v, err := c.s.o.Objects.Version(ctx, st.ID, st.Version)
	if err != nil {
		return err
	}
	stored, err := store.DecodeAgent(v.Doc)
	if err != nil {
		return err
	}
	was, err := session.Marshal(stored.Metadata)
	if err != nil {
		return err
	}
	now, err := session.Marshal(r.Agent.Metadata)
	if err != nil {
		return err
	}
	if bytes.Equal(was, now) {
		return c.reply(http.StatusOK, r.Agent)
	}
	doc, err := session.Marshal(r.Agent)
	if err != nil {
		return err
	}
	bundle, err := r.Bundle()
	if err != nil {
		return err
	}
	if err := c.s.o.Objects.RewriteLatest(ctx, store.AgentVersion{AgentID: st.ID, Version: st.Version, Digest: st.Digest, Doc: doc, Bundle: bundle}); err != nil {
		return err
	}
	return c.reply(http.StatusOK, r.Agent)
}

// listAgents is GET /agents.
func (c *call) listAgents() error {
	limit, cursor, err := c.pageParams()
	if err != nil {
		return err
	}
	d, err := c.ask(c.r.Context(), authorizer.ActionAgentList, authz.NewResource(authorizer.KindAgent, "", nil))
	if err != nil {
		return err
	}
	// The list holds the context's agents, and the authorizer's owners
	// narrow it further: one that names others than the context lists
	// none of them.
	here := c.here()
	o := store.AgentList{Limit: limit, Cursor: cursor, Owners: []string{here.subject}}
	if d.Filter != nil && !slices.Contains(d.Filter.Owners, here.subject) {
		return c.replyPage([]*v1.Agent{}, "")
	}
	agents, next, err := c.s.o.Objects.ListAgents(c.r.Context(), o)
	if err != nil {
		return err
	}
	items := make([]*v1.Agent, 0, len(agents))
	for _, a := range agents {
		doc, err := c.latest(a)
		if err != nil {
			return err
		}
		items = append(items, doc)
	}
	return c.replyPage(items, next)
}

// getAgent is GET /agents/{ref}.
func (c *call) getAgent() error {
	_, doc, err := c.agent(c.r.PathValue("ref"), authorizer.ActionAgentRead)
	if err != nil {
		return err
	}
	return c.reply(http.StatusOK, doc)
}

// agentVersion is one row of GET /agents/{ref}/versions.
type agentVersion struct {
	Version   int       `json:"version"`
	Digest    string    `json:"digest"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// listAgentVersions is GET /agents/{ref}/versions.
func (c *call) listAgentVersions() error {
	limit, cursor, err := c.pageParams()
	if err != nil {
		return err
	}
	a, _, err := c.agent(c.r.PathValue("ref"), authorizer.ActionAgentRead)
	if err != nil {
		return err
	}
	vs, next, err := c.s.o.Objects.Versions(c.r.Context(), a.ID, limit, cursor)
	if err != nil {
		return err
	}
	items := make([]agentVersion, len(vs))
	for i, v := range vs {
		items[i] = agentVersion{Version: v.Version, Digest: v.Digest, CreatedBy: v.CreatedBy, CreatedAt: v.CreatedAt.UTC()}
	}
	return c.replyPage(items, next)
}

// getAgentVersion is GET /agents/{ref}/versions/{n}.
func (c *call) getAgentVersion() error {
	n, err := strconv.Atoi(c.r.PathValue("n"))
	if err != nil || n < 1 {
		return refuse(CodeInvalidRequest, "version %q is not a positive number", c.r.PathValue("n"))
	}
	a, _, err := c.agent(c.r.PathValue("ref"), authorizer.ActionAgentRead)
	if err != nil {
		return err
	}
	v, err := c.s.o.Objects.Version(c.r.Context(), a.ID, n)
	if err != nil {
		return err
	}
	doc, err := render(a, v)
	if err != nil {
		return err
	}
	return c.reply(http.StatusOK, doc)
}

// archiveBody is the body of POST /agents/{ref}/archive.
type archiveBody struct {
	Permanent bool `json:"permanent"`
}

// confirmPermanent refuses an archive whose body does not confirm it is
// permanent, an empty body included.
func (c *call) confirmPermanent() error {
	b, err := c.body()
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return refuse(CodeConfirmationRequired, "the body is empty")
	}
	var body archiveBody
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return refuse(CodeInvalidRequest, "the body does not decode: %v", err)
	}
	if !body.Permanent {
		return refuse(CodeConfirmationRequired, "permanent is not true")
	}
	return nil
}

// archiveAgent is POST /agents/{ref}/archive. Sessions keep the version
// they pinned; no new session starts on an archived agent. The agent's
// identity is archived first, so a failed call leaves the agent as it
// was and a retry is idempotent, and it is disabled for good once no
// session of the agent is left: here when none runs, and otherwise by
// the reconcile pass (spec 018). Archiving cannot be undone, so the
// caller confirms it with {"permanent": true}, and a client shows the
// person that consequence before it sends the request.
func (c *call) archiveAgent() error {
	ctx := c.r.Context()
	if err := c.confirmPermanent(); err != nil {
		return err
	}
	a, doc, err := c.agent(c.r.PathValue("ref"), authorizer.ActionAgentArchive)
	if err != nil {
		return err
	}
	subject := doc.Status.Identity
	if c.s.o.Identities != nil && subject != "" {
		if err := c.s.o.Identities.Archive(ctx, a.ID); err != nil {
			return identityRefusal(err)
		}
	}
	if err := c.s.o.Objects.Archive(ctx, a.ID, c.s.o.Now()); err != nil {
		return err
	}
	if c.s.o.Identities != nil && subject != "" {
		if err := c.s.retire(ctx, a.ID, subject); err != nil {
			c.s.o.Log.ErrorContext(ctx, "disable an archived agent's identity; the reconcile pass retries", "agent", a.ID, "err", err)
		}
	}
	if a, err = c.s.o.Objects.Agent(ctx, a.ID); err != nil {
		return err
	}
	if doc, err = c.latest(a); err != nil {
		return err
	}
	return c.reply(http.StatusOK, doc)
}
