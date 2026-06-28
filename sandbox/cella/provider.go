// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package cella

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"latere.ai/x/topos/sandbox"
)

// defaultImage is the platform base image used when [sandbox.CreateOptions.Image]
// is empty. The SandboxManifest schema requires spec.image, so — unlike the
// deprecated flat create body — the provider must supply one rather than relying
// on a server-side default. The server canonicalizes catalog refs.
const defaultImage = "ghcr.io/latere-ai/sandbox-base:latest"

// defaultAutoStop is the idle timeout stamped on every created sandbox when the
// caller does not override it. It is a cost backstop: a sandbox the host forgets
// to Destroy stops itself rather than billing indefinitely.
const defaultAutoStop = "15m"

// manifest is the Kubernetes-style SandboxManifest envelope POSTed to
// /v1/sandboxes. It replaces the deprecated flat CreateSandbox body.
type manifest struct {
	APIVersion string           `json:"apiVersion"`
	Kind       string           `json:"kind"`
	Metadata   manifestMetadata `json:"metadata"`
	Spec       manifestSpec     `json:"spec"`
}

type manifestMetadata struct {
	Name   string            `json:"name,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

type manifestSpec struct {
	Image     string             `json:"image"`
	Tier      string             `json:"tier,omitempty"`
	Policy    string             `json:"policy,omitempty"`
	Env       map[string]string  `json:"env,omitempty"`
	Lifecycle *manifestLifecycle `json:"lifecycle,omitempty"`
}

type manifestLifecycle struct {
	AutoStop string `json:"autoStop,omitempty"`
}

// sandboxResp is the subset of Cella's Sandbox response Topos consumes.
type sandboxResp struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	State     string `json:"state"`
	Tier      string `json:"tier"`
	CreatedAt string `json:"created_at"`
}

func (s sandboxResp) toSandbox() sandbox.Sandbox {
	return sandbox.Sandbox{
		ID:        s.ID,
		Name:      s.Name,
		State:     sandbox.State(s.State),
		Tier:      s.Tier,
		CreatedAt: s.CreatedAt,
	}
}

// Create provisions a new sandbox via POST /v1/sandboxes using a SandboxManifest
// body. The returned Sandbox may still be in the "creating" state; callers that
// need it running should poll HealthCheck.
func (p *Provider) Create(ctx context.Context, opts sandbox.CreateOptions) (sandbox.Sandbox, error) {
	image := opts.Image
	if image == "" {
		image = defaultImage
	}
	tier := opts.Tier
	if tier == "" {
		tier = "ephemeral"
	}

	m := manifest{
		APIVersion: "cella.latere.ai/v1",
		Kind:       "Sandbox",
		Metadata: manifestMetadata{
			Name:   opts.Name,
			Labels: opts.Labels,
		},
		Spec: manifestSpec{
			Image:     image,
			Tier:      tier,
			Policy:    opts.Policy,
			Env:       opts.Env,
			Lifecycle: &manifestLifecycle{AutoStop: defaultAutoStop},
		},
	}

	var resp sandboxResp
	if err := p.doJSON(ctx, "POST", "/v1/sandboxes", m, &resp); err != nil {
		return sandbox.Sandbox{}, err
	}
	return resp.toSandbox(), nil
}

// Destroy deletes the sandbox. It is idempotent: a 404 (the sandbox is already
// gone) is treated as success, matching the interface contract.
func (p *Provider) Destroy(ctx context.Context, id string) error {
	err := p.doJSON(ctx, "DELETE", "/v1/sandboxes/"+url.PathEscape(id), nil, nil)
	if errors.Is(err, sandbox.ErrNotFound) {
		return nil
	}
	return err
}

// HealthCheck returns nil iff the sandbox exists and is running. A missing
// sandbox yields [sandbox.ErrNotFound]; any other state yields a descriptive
// error.
func (p *Provider) HealthCheck(ctx context.Context, id string) error {
	var resp sandboxResp
	if err := p.doJSON(ctx, "GET", "/v1/sandboxes/"+url.PathEscape(id), nil, &resp); err != nil {
		return err
	}
	if sandbox.State(resp.State) != sandbox.StateRunning {
		return fmt.Errorf("cella: sandbox %q not running: state=%s", id, resp.State)
	}
	return nil
}
