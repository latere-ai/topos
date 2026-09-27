// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"fmt"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/models"
)

// AgentConfig is what a harness takes from a resolved Agent that needs
// no I/O. The caller adds what does: the model connection and its
// credential, the machine, the tool registry, the catalog entry.
type AgentConfig struct {
	// Name is the agent's metadata.name.
	Name         string
	Instructions string
	// Tools are the names the agent holds, built-in and client tools in
	// the manifest's order; empty is none.
	Tools []string
	// Policy is the mode, the lists, the thresholds and the egress of
	// spec 012.
	Policy harness.Policy
	// Model is spec.model as resolved, its credential a cred_ id.
	Model v1.AgentModel
	// Overlay is the figures spec.model gives, for Catalog.Resolve;
	// Overlay.Name is the model's name.
	Overlay models.Entry
	Effort  string
	// Subagents are the agents the threads may spawn, nested to
	// MaxDepth. Each Entry is its spec.model's overlay with its model's
	// name; Model and Connection are filled by the Connect function
	// AgentConfig was given, and left nil without one.
	Subagents     map[string]harness.Subagent
	MaxDepth      int
	MaxConcurrent int
	CompactAt     float64
	TurnTimeout   time.Duration
	MaxAge        time.Duration
	// MaxCostUSDMicro is spec.budget.maxCost in micro-USD; nil is no
	// budget.
	MaxCostUSDMicro *int64
	// Machine is spec.machine as resolved.
	Machine v1.Machine
}

// Connect gives a subagent its model from its spec.model and overlay:
// the Model, the Connection and the catalog entry it resolves to. A nil
// Model or Connection is the parent's.
type Connect func(m v1.AgentModel, overlay models.Entry) (models.Model, *models.Connection, *models.Entry, error)

// AgentConfig derives the harness pieces of a resolved Agent. connect
// may be nil.
func (r Resolved) AgentConfig(connect Connect) (AgentConfig, error) {
	if r.Agent == nil {
		return AgentConfig{}, fmt.Errorf("manifest: %s %s is not an Agent", r.Kind, r.Name)
	}
	s := r.Agent.Spec
	if !defaulted(s) {
		return AgentConfig{}, fmt.Errorf("manifest: %s is not a resolved spec: its fixed defaults are not written out", r.Name)
	}
	overlay, err := Overlay(s.Model)
	if err != nil {
		return AgentConfig{}, err
	}
	c := AgentConfig{
		Name: r.Agent.Metadata.Name, Instructions: s.Instructions, Tools: toolNames(s.Tools),
		Policy: policy(s), Model: s.Model, Overlay: overlay, Effort: s.Model.Effort,
		MaxDepth: *s.Threads.MaxDepth, MaxConcurrent: *s.Threads.MaxConcurrent, CompactAt: *s.Context.CompactAt,
		Machine: s.Machine,
	}
	if c.TurnTimeout, err = time.ParseDuration(s.Limits.TurnTimeout); err != nil {
		return AgentConfig{}, fmt.Errorf("manifest: spec.limits.turnTimeout: %w", err)
	}
	if c.MaxAge, err = time.ParseDuration(s.Limits.MaxAge); err != nil {
		return AgentConfig{}, fmt.Errorf("manifest: spec.limits.maxAge: %w", err)
	}
	if s.Budget.MaxCost != "" {
		p, err := models.ParsePrice(s.Budget.MaxCost)
		if err != nil {
			return AgentConfig{}, fmt.Errorf("manifest: spec.budget.maxCost: %w", err)
		}
		micro := int64(p)
		c.MaxCostUSDMicro = &micro
	}
	b := &builder{pinned: r.Pinned, connect: connect, limit: c.MaxDepth}
	if c.Subagents, err = b.subagents(s, 1); err != nil {
		return AgentConfig{}, err
	}
	return c, nil
}

// defaulted reports whether the fields AgentConfig reads through
// pointers carry the values Resolve writes, as they do in every resolved
// spec and bundle.
func defaulted(s v1.AgentSpec) bool {
	t := s.Approvals.Thresholds
	return s.Threads.MaxDepth != nil && s.Threads.MaxConcurrent != nil && s.Context.CompactAt != nil &&
		t.FlagAt != nil && t.AskAt != nil && t.BlockAt != nil
}

// Overlay is the catalog overlay of a spec.model: the figures it gives,
// each zero where it gives none, with Name the model's name.
func Overlay(m v1.AgentModel) (models.Entry, error) {
	e := models.Entry{
		Name: m.Name, Family: m.Family, Dialect: ir.Dialect(m.Dialect),
		InputWindow: m.InputWindow, MaxOutputTokens: m.MaxOutputTokens,
	}
	if p := m.Pricing; p != nil {
		e.Pricing = &models.Pricing{}
		for _, f := range []struct {
			name string
			v    string
			dst  **models.Price
		}{
			{"input", p.Input, &e.Pricing.Input}, {"output", p.Output, &e.Pricing.Output},
			{"cacheRead", p.CacheRead, &e.Pricing.CacheRead}, {"cacheWrite", p.CacheWrite, &e.Pricing.CacheWrite},
		} {
			if f.v == "" {
				continue
			}
			price, err := models.ParsePrice(f.v)
			if err != nil {
				return models.Entry{}, fmt.Errorf("manifest: model.pricing.%s: %w", f.name, err)
			}
			*f.dst = &price
		}
	}
	return e, nil
}

func policy(s v1.AgentSpec) harness.Policy {
	t := s.Approvals.Thresholds
	return harness.Policy{
		Mode: harness.Mode(s.Approvals.Mode), AlwaysAllow: s.Approvals.AlwaysAllow, AlwaysConfirm: s.Approvals.AlwaysConfirm,
		Thresholds: harness.Thresholds{FlagAt: *t.FlagAt, AskAt: *t.AskAt, BlockAt: *t.BlockAt},
		Egress:     s.Machine.Egress,
	}
}

// toolNames are the names of a spec's tools, never nil, so an empty
// list stays none rather than becoming the parent's.
func toolNames(list []v1.Tool) []string {
	names := make([]string, 0, len(list))
	for _, t := range list {
		names = append(names, t.Name)
	}
	return names
}

// builder builds the subagent tree from the pinned agents.
type builder struct {
	pinned  map[string]*v1.Agent
	connect Connect
	limit   int
}

// subagents are a spec's subagents at a level, 1 for the agent's own:
// a thread at depth d spawns the level d+1, and none past the limit.
func (b *builder) subagents(s v1.AgentSpec, level int) (map[string]harness.Subagent, error) {
	if level > b.limit || len(s.Subagents) == 0 {
		return nil, nil
	}
	out := make(map[string]harness.Subagent, len(s.Subagents))
	for _, sub := range s.Subagents {
		spec := sub.Spec
		if spec == nil {
			a, ok := b.pinned[sub.Agent]
			if !ok {
				return nil, fmt.Errorf("manifest: subagent %s pins %s, which the resolved agent does not carry", sub.Name, sub.Agent)
			}
			spec = &a.Spec
		}
		overlay, err := Overlay(spec.Model)
		if err != nil {
			return nil, fmt.Errorf("manifest: subagent %s: %w", sub.Name, err)
		}
		h := harness.Subagent{
			Name: sub.Name, Instructions: spec.Instructions, Entry: &overlay,
			Tools: toolNames(spec.Tools), Mode: harness.Mode(spec.Approvals.Mode),
		}
		if b.connect != nil {
			m, conn, entry, err := b.connect(spec.Model, overlay)
			if err != nil {
				return nil, fmt.Errorf("manifest: subagent %s: %w", sub.Name, err)
			}
			h.Model, h.Connection = m, conn
			if entry != nil {
				h.Entry = entry
			}
		}
		if h.Subagents, err = b.subagents(*spec, level+1); err != nil {
			return nil, err
		}
		out[sub.Name] = h
	}
	return out, nil
}
