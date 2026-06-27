// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package topos

import (
	"context"
	"fmt"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/anthropic"
	"latere.ai/x/topos/models/fake"
)

// ModelKind selects how the SDK reaches a model. The model itself is always the
// internal models.Model seam; ModelKind only chooses which backing to build.
type ModelKind string

const (
	// ModelFake is the deterministic, network-free model for tests and the embed
	// check. It is the basis of the record/replay reproducibility story (M5).
	ModelFake ModelKind = "fake"
	// ModelLux reaches a provider through Lux (latere.ai/x/lux), the model gateway:
	// cloud (lux.latere.ai, metered, owner-billed) or a local stateless luxd
	// (LUX_STATELESS=1, BYO keys, no cloud dependency). Provider secrets stay in
	// Lux, never in the embedding consumer.
	ModelLux ModelKind = "lux"
	// ModelDirect talks to a provider endpoint directly (dev convenience / BYO key).
	ModelDirect ModelKind = "direct"
)

// ModelOptions is the public, embeddable model connection. It exposes no internal
// model types; the runner builds the right adapter from it. For ModelLux/ModelDirect
// supply exactly one credential: a static APIKey (e.g. a Lux "lux_*" virtual key)
// or a BearerSource (a per-call token, e.g. a rotating sandbox/JWT token).
type ModelOptions struct {
	Kind     ModelKind
	Provider string // "anthropic" (M1 supports anthropic-wire; others later)
	Model    string // model id, e.g. "claude-sonnet-4-6"
	BaseURL  string // e.g. "https://lux.latere.ai/anthropic" or "http://localhost:8080/anthropic"

	APIKey       string
	BearerSource func(ctx context.Context) (string, error)
}

// buildModel turns public ModelOptions into the internal models.Model seam.
func buildModel(opts ModelOptions) (models.Model, error) {
	switch opts.Kind {
	case ModelFake, "":
		return fake.New(), nil
	case ModelLux, ModelDirect:
		if opts.Provider != "" && opts.Provider != "anthropic" {
			return nil, fmt.Errorf("topos: model provider %q not yet supported (M1: anthropic-wire only)", opts.Provider)
		}
		var aopts []anthropic.Option
		if opts.Model != "" {
			aopts = append(aopts, anthropic.WithModel(opts.Model))
		}
		if opts.BearerSource != nil {
			aopts = append(aopts, anthropic.WithBearerSource(opts.BearerSource))
		}
		// Lux holds the provider secret; APIKey here is a Lux virtual key (or empty
		// when a BearerSource supplies a rotating token).
		return anthropic.New(opts.APIKey, opts.BaseURL, aopts...), nil
	default:
		return nil, fmt.Errorf("topos: unknown model kind %q", opts.Kind)
	}
}
