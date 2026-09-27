// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/manifest"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// agentResource is an existing agent as the authorizer reads it.
func agentResource(a store.Agent) authz.Resource {
	return authz.NewResource(authorizer.KindAgent, a.ID, map[string]any{"name": a.Name, "owner": a.Owner})
}

// agent finds an agent by name or id and asks action about it. A denied
// read answers as a missing agent.
func (c *call) agent(ref, action string) (store.Agent, *v1.Agent, error) {
	a, err := c.s.o.Objects.Agent(c.r.Context(), ref)
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
// so a manifest's references cannot tell another subject's agent from
// none.
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
	rs, err := manifest.Resolve(ctx, body, manifest.Options{Lookup: scopedLookup{store.Lookup(c.s.o.Objects), c}, Now: c.s.o.Now})
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
	stored, err := c.s.o.Objects.Agent(ctx, name)
	exists := err == nil
	switch {
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return err
	case exists:
		if _, err := c.ask(c.r.Context(), authorizer.ActionAgentUpdate, agentResource(stored)); err != nil {
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
			return c.reply(http.StatusOK, r.Agent)
		}
	default:
		if _, err := c.ask(c.r.Context(), authorizer.ActionAgentCreate, authz.NewResource(authorizer.KindAgent, "", map[string]any{"name": name})); err != nil {
			return err
		}
	}
	doc, err := session.Marshal(r.Agent)
	if err != nil {
		return err
	}
	bundle, err := r.Bundle()
	if err != nil {
		return err
	}
	a := store.Agent{ID: st.ID, Name: name, Owner: c.caller.Subject, CreatedAt: st.CreatedAt}
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
	o := store.AgentList{Limit: limit, Cursor: cursor}
	if d.Filter != nil {
		o.Owners = d.Filter.Owners
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

// archiveAgent is POST /agents/{ref}/archive. Sessions keep the version
// they pinned; no new session starts on an archived agent.
func (c *call) archiveAgent() error {
	a, _, err := c.agent(c.r.PathValue("ref"), authorizer.ActionAgentArchive)
	if err != nil {
		return err
	}
	if err := c.s.o.Objects.Archive(c.r.Context(), a.ID, c.s.o.Now()); err != nil {
		return err
	}
	if a, err = c.s.o.Objects.Agent(c.r.Context(), a.ID); err != nil {
		return err
	}
	doc, err := c.latest(a)
	if err != nil {
		return err
	}
	return c.reply(http.StatusOK, doc)
}
