// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"

	"latere.ai/x/topos/internal/identity"
	"latere.ai/x/topos/internal/store"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// The error codes spec 018 owns that the API answers.
const (
	CodeIdentityRefused      = "identity_refused"
	CodeIdentityUnavailable  = "identity_unavailable"
	CodeAgentIdentityMissing = "agent_identity_missing"
)

// Identities is the identity provider that hosts the installation's
// agents (spec 018); *identity.Client is the one toposd runs. Nil runs
// every agent with the installation's own credentials.
type Identities interface {
	Create(ctx context.Context, ref, name string, owner identity.Owner, appliedBy string) (string, error)
	Archive(ctx context.Context, ref string) error
	Disable(ctx context.Context, ref, subject string) error
	Archived(ctx context.Context) ([]identity.Hosted, error)
}

// identityRefusal is err of an identity provider call as the API answers
// it: the provider's refusal, or its silence.
func identityRefusal(err error) error {
	if e, ok := errors.AsType[*identity.Error](err); ok {
		return &apiError{code: CodeIdentityRefused, detail: e.Code + ": " + e.Message, err: err}
	}
	if errors.Is(err, identity.ErrUnavailable) {
		return &apiError{code: CodeIdentityUnavailable, detail: err.Error(), err: err}
	}
	return err
}

// ensureIdentity gives an agent being applied its identity at the
// identity provider when it has none, after the authorizer allowed the
// apply: the owner is the applier's organization or the applier, and the
// subject becomes the agent's status.identity, which every later version
// carries.
func (c *call) ensureIdentity(ctx context.Context, a *v1.Agent) error {
	if c.s.o.Identities == nil || a.Status.Identity != "" {
		return nil
	}
	subject, err := c.s.o.Identities.Create(ctx, a.Status.ID, a.Metadata.Name, identity.OwnerOf(c.caller.Sub, c.caller.Claims), c.caller.Sub)
	if err != nil {
		return identityRefusal(err)
	}
	a.Status.Identity = subject
	return nil
}

// agentIdentity is the identity of the agent: its latest version's
// status.identity.
func (s *Server) agentIdentity(ctx context.Context, a store.Agent) (string, error) {
	v, err := s.o.Objects.Version(ctx, a.ID, a.Latest)
	if err != nil {
		return "", err
	}
	doc, err := store.DecodeAgent(v.Doc)
	if err != nil {
		return "", err
	}
	return doc.Status.Identity, nil
}

// live reports whether the agent has a session that has not ended.
func (s *Server) live(ctx context.Context, agentID string) (bool, error) {
	for _, st := range []session.Status{session.StatusRunning, session.StatusIdle} {
		page, _, err := s.o.Sessions.List(ctx, session.ListOptions{AgentID: agentID, Status: st, Limit: 1})
		if err != nil {
			return false, err
		}
		if len(page) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// retire disables the identity subject of the archived agent agentID
// once no session of the agent is left unended. Disabling is permanent.
func (s *Server) retire(ctx context.Context, agentID, subject string) error {
	busy, err := s.live(ctx, agentID)
	if err != nil || busy {
		return err
	}
	return s.o.Identities.Disable(ctx, agentID, subject)
}

// Reconcile disables the identity of every archived agent the identity
// provider lists that has no session left: an agent archived at the
// provider and in the store, or one the store does not hold. It runs at
// start and on every reaper pass, so a disable a crash lost or a session
// that ended outside the API is caught up.
func (s *Server) Reconcile(ctx context.Context) error {
	if s.o.Identities == nil {
		return nil
	}
	archived, err := s.o.Identities.Archived(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, h := range archived {
		a, err := s.o.Objects.Agent(ctx, h.Ref)
		switch {
		case errors.Is(err, store.ErrNotFound):
			errs = append(errs, s.o.Identities.Disable(ctx, h.Ref, h.Subject))
			continue
		case err != nil:
			errs = append(errs, err)
			continue
		case a.ArchivedAt == nil:
			// The provider archived it and the store did not: the archive
			// failed half way, and a person retries it.
			continue
		}
		errs = append(errs, s.retire(ctx, a.ID, h.Subject))
	}
	return errors.Join(errs...)
}
