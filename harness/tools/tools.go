// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

// Package tools defines the Tool interface and registry for the Topos agentic
// loop's governed tool surface.
//
// Every tool invocation in the agentic loop goes through the hook bus
// three-phase path (validate → permission → execute + post). The registry is
// the canonical source of available tools; the loop calls it after the bus
// has granted permission.
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/sandbox"
)

// Tool is the interface every agentic tool must implement.
type Tool interface {
	// Name returns the stable tool identifier, matching ToolDef.Name.
	Name() string
	// Def returns the tool's canonical definition for injection into model
	// requests.
	Def() models.ToolDef
	// Invoke executes the tool with the given input and returns a ToolResult.
	// sb and sandboxID identify the execution sandbox. input is the normalised
	// (post-hook) JSON object from the model.
	Invoke(ctx context.Context, input json.RawMessage, sb sandbox.Provider, sandboxID string) (models.ToolResult, error)
}

// Registry is an ordered, name-indexed collection of Tools.
// Safe for concurrent use after construction (no mutation post-build).
type Registry struct {
	order  []Tool
	byName map[string]Tool
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Tool)}
}

// Register adds a Tool to the registry. Panics on duplicate name (programming error).
func (r *Registry) Register(t Tool) {
	if _, exists := r.byName[t.Name()]; exists {
		panic(fmt.Sprintf("tools: duplicate tool name %q", t.Name()))
	}
	r.order = append(r.order, t)
	r.byName[t.Name()] = t
}

// Get returns the named Tool or nil if not found.
func (r *Registry) Get(name string) Tool {
	return r.byName[name]
}

// Defs returns the ToolDef list in registration order — ready to pass to a
// model Request.
func (r *Registry) Defs() []models.ToolDef {
	defs := make([]models.ToolDef, len(r.order))
	for i, t := range r.order {
		defs[i] = t.Def()
	}
	return defs
}
