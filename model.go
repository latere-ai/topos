// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package topos

import (
	"context"
	"fmt"
	"strings"

	"latere.ai/x/pkg/luxsdk"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/fake"
	"latere.ai/x/topos/models/lux"
)

// tokenFunc adapts a BearerSource to luxsdk.TokenSource.
type tokenFunc func(ctx context.Context) (string, error)

func (f tokenFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

// ModelKind selects how the SDK reaches a model. The model itself is always the
// internal models.Model seam; ModelKind only chooses which backing to build.
type ModelKind string

const (
	// ModelFake is the deterministic, network-free model for tests and the embed
	// check. It is the basis of the record/replay reproducibility story.
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
	Provider string // ModelDirect only: a luxsdk.Provider name (anthropic, openai, gemini, openrouter, ollama); defaults to anthropic. Ignored for ModelLux — the gateway routes any provider.
	Model    string // model id, e.g. "claude-sonnet-4-6"
	BaseURL  string // ModelLux: the gateway root, e.g. "https://lux.latere.ai". ModelDirect: the provider endpoint.

	APIKey       string
	BearerSource func(ctx context.Context) (string, error)
}

// buildModel turns public ModelOptions into the internal models.Model seam.
func buildModel(opts ModelOptions) (models.Model, error) {
	switch opts.Kind {
	case ModelFake, "":
		return fake.New(), nil
	case ModelLux:
		// The lux-native dialect (lux spec 33): one adapter over luxsdk,
		// every provider the gateway routes. Provider secrets stay in
		// Lux; APIKey here is a Lux virtual key (or empty when a
		// BearerSource supplies a rotating token).
		var lopts []lux.Option
		if opts.Model != "" {
			lopts = append(lopts, lux.WithModel(opts.Model))
		}
		if opts.BearerSource != nil {
			lopts = append(lopts, lux.WithBearerSource(opts.BearerSource))
		}
		// Pre-migration configs pointed BaseURL at the /anthropic
		// passthrough prefix; the native surface lives at the root.
		base := strings.TrimSuffix(strings.TrimRight(opts.BaseURL, "/"), "/anthropic")
		return lux.New(opts.APIKey, base, lopts...), nil
	case ModelDirect:
		// Direct access speaks the lux format too: the request is
		// down-converted client-side through the same llmdialect
		// backends the gateway uses, so any supported provider works
		// with a BYO endpoint and secret. Topos owns no wire mapping.
		provider := luxsdk.Provider(opts.Provider)
		if provider == "" {
			provider = luxsdk.ProviderAnthropic
		}
		var sdkOpts []luxsdk.Option
		if opts.BearerSource != nil {
			sdkOpts = append(sdkOpts, luxsdk.WithTokenSource(tokenFunc(opts.BearerSource)))
		}
		d, err := luxsdk.NewDirect(provider, opts.APIKey, opts.BaseURL, sdkOpts...)
		if err != nil {
			return nil, fmt.Errorf("topos: %w", err)
		}
		var lopts []lux.Option
		if opts.Model != "" {
			lopts = append(lopts, lux.WithModel(opts.Model))
		}
		return lux.NewFromCaller(d, lopts...), nil
	default:
		return nil, fmt.Errorf("topos: unknown model kind %q", opts.Kind)
	}
}
