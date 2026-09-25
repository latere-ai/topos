// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package cella

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	cellaclient "latere.ai/x/cella/client"
	v1 "latere.ai/x/cella/manifest/v1"

	"latere.ai/x/topos/sandbox"
)

// Provider satisfies the sandbox.Provider interface.
var _ sandbox.Provider = (*Provider)(nil)

// defaultImage is the catalog image used when [sandbox.CreateOptions.Image] is
// empty. The hosted control plane runs images from its catalog only, and base
// is the catalog's general-purpose entry; naming it rather than leaving the
// field empty keeps the choice the same on a control plane whose own default
// differs.
const defaultImage = "base"

// defaultAutoStop is the idle timeout stamped on every created sandbox. It is a
// cost backstop: a sandbox the host forgets to Destroy stops itself rather than
// running indefinitely. The workspace survives a stop.
const defaultAutoStop v1.Duration = "15m"

// ephemeralTTL is how long an ephemeral sandbox is kept from its creation. It
// is at or below every time to live the hosted plans admit, so the manifest is
// never refused for it.
const ephemeralTTL v1.Duration = "24h"

// The two tiers [sandbox.CreateOptions.Tier] names.
const (
	tierEphemeral  = "ephemeral"
	tierPersistent = "persistent"
)

// maxCreateHold is the longest a create is held for the sandbox to start when
// the caller's context carries no deadline: the control plane's own default.
const maxCreateHold = 10 * time.Minute

// holdMargin is left between the hold the server is asked for and the caller's
// deadline, so the server answers, naming the sandbox, before the caller gives
// up on the request.
const holdMargin = 5 * time.Second

// cleanupTimeout bounds the delete of a sandbox whose create failed. It runs on
// a context detached from the caller's, which may already be done.
const cleanupTimeout = 30 * time.Second

// The phases a Cella sandbox reports, as the control plane spells them.
const (
	phasePending    = "Pending"
	phaseQueued     = "Queued"
	phaseStarting   = "Starting"
	phaseRecovering = "Recovering"
	phaseRunning    = "Running"
	phaseStopped    = "Stopped"
	phaseDeleting   = "Deleting"
	phaseFailed     = "Failed"
	phaseLost       = "Lost"
)

// Create provisions a sandbox with a Sandbox manifest and holds the answer
// until the sandbox runs, fails, or the hold ends. A sandbox the control plane
// leaves Failed or Lost is deleted and reported as an error naming its id and
// reason. One still coming up when the hold ends is returned in the creating
// state, which callers that need it running wait out with HealthCheck.
func (p *Provider) Create(ctx context.Context, opts sandbox.CreateOptions) (sandbox.Sandbox, error) {
	obj, err := p.manifest(opts)
	if err != nil {
		return sandbox.Sandbox{}, err
	}
	m, err := cellaclient.Encode(obj)
	if err != nil {
		return sandbox.Sandbox{}, fmt.Errorf("cella: encode manifest: %w", err)
	}
	created, _, err := p.core.CreateSandbox(ctx, m, cellaclient.Wait(createHold(ctx)))
	if err != nil {
		return sandbox.Sandbox{}, mapError(err)
	}
	if phase := created.Status.Phase; phase == phaseFailed || phase == phaseLost {
		return sandbox.Sandbox{}, p.discardFailed(ctx, created)
	}
	return toSandbox(created), nil
}

// manifest is the Sandbox manifest for one create. The fields the control
// plane has no equivalent for are refused here, before any request, because
// each asks for a restriction or a credential the caller would otherwise run
// without.
func (p *Provider) manifest(opts sandbox.CreateOptions) (v1.Sandbox, error) {
	if opts.Policy != "" {
		return v1.Sandbox{}, fmt.Errorf("cella: CreateOptions.Policy %q has no equivalent: the Cella control plane has no named policies, and a sandbox's boundary is its own manifest", opts.Policy)
	}
	if len(opts.SecretMounts) > 0 {
		return v1.Sandbox{}, errors.New("cella: CreateOptions.SecretMounts has no equivalent: a Cella secret reaches a sandbox as a placeholder the egress gateway substitutes, never as a file holding the value")
	}
	lifecycle, err := lifecycleFor(opts.Tier)
	if err != nil {
		return v1.Sandbox{}, err
	}
	image := opts.Image
	if image == "" {
		image = defaultImage
	}
	return v1.Sandbox{
		APIVersion: v1.APIVersion,
		Kind:       v1.KindSandbox,
		Metadata: v1.Metadata{
			Name:   opts.Name,
			Labels: withAgentLabel(opts.Labels),
		},
		Spec: v1.SandboxSpec{
			Image: image,
			Env:   opts.Env,
			Network: v1.Network{Egress: v1.Egress{
				Mode:         v1.EgressAllowlist,
				AllowedHosts: p.allowedHosts,
			}},
			Lifecycle: lifecycle,
		},
	}, nil
}

// lifecycleFor maps a tier onto the lifecycle rules that give it its meaning:
// both stop on idle, and an ephemeral sandbox is also deleted a fixed time
// after its creation.
func lifecycleFor(tier string) (v1.Lifecycle, error) {
	switch tier {
	case "", tierEphemeral:
		return v1.Lifecycle{AutoStop: defaultAutoStop, TTL: ephemeralTTL}, nil
	case tierPersistent:
		return v1.Lifecycle{AutoStop: defaultAutoStop}, nil
	default:
		return v1.Lifecycle{}, fmt.Errorf("cella: CreateOptions.Tier %q is neither %s nor %s", tier, tierEphemeral, tierPersistent)
	}
}

// tierOf reads the tier back from the lifecycle the control plane answered: a
// sandbox that is deleted a fixed time after its creation is ephemeral. An
// installation's admission may give a sandbox a time to live its caller did not
// ask for, and the sandbox is then reported as what it is.
func tierOf(l v1.Lifecycle) string {
	if l.TTL != "" && l.TTL != v1.DurationNever {
		return tierEphemeral
	}
	return tierPersistent
}

// createHold is the hold a create asks the server for: the caller's remaining
// time less holdMargin, so the answer arrives before the caller stops waiting,
// and maxCreateHold when the context has no deadline. A hold that would fall
// below a second asks for one, and the answer is whatever phase the sandbox has
// reached by then.
func createHold(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return maxCreateHold
	}
	return min(max(time.Until(deadline)-holdMargin, time.Second), maxCreateHold)
}

// discardFailed deletes a sandbox that failed to start and returns the error
// the create reports. The caller receives no handle to a failed sandbox, so it
// is removed here rather than left for the lifecycle rules. The delete runs on
// a context detached from the caller's, which may be what ended the create.
func (p *Provider) discardFailed(ctx context.Context, obj v1.Sandbox) error {
	failed := fmt.Errorf("cella: sandbox %s did not start: phase %s, reason %q", obj.Status.ID, obj.Status.Phase, obj.Status.Reason)
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := p.Destroy(cleanup, obj.Status.ID); err != nil {
		return errors.Join(failed, fmt.Errorf("cella: delete the failed sandbox %s: %w", obj.Status.ID, err))
	}
	return failed
}

// withAgentLabel returns a copy of labels with "kind=agent" stamped in, the
// label the Cella backend tags every Topos-created sandbox with (per the
// sandbox.CreateOptions.Labels contract). It never mutates the caller's map and
// does not override a caller-supplied "kind".
func withAgentLabel(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels)+1)
	maps.Copy(out, labels)
	if _, ok := out["kind"]; !ok {
		out["kind"] = "agent"
	}
	return out
}

// toSandbox converts the control plane's object into the interface's handle.
func toSandbox(obj v1.Sandbox) sandbox.Sandbox {
	out := sandbox.Sandbox{
		ID:    obj.Status.ID,
		Name:  obj.Metadata.Name,
		State: stateOf(obj.Status.Phase),
		Tier:  tierOf(obj.Spec.Lifecycle),
	}
	if !obj.Status.CreatedAt.IsZero() {
		out.CreatedAt = obj.Status.CreatedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// stateOf maps a Cella phase onto the interface's states. The phases on the
// way to running (waiting in a queue, coming up, coming back after a lost
// node) are creating; a sandbox that failed or was lost is in error.
func stateOf(phase string) sandbox.State {
	switch phase {
	case phasePending, phaseQueued, phaseStarting, phaseRecovering:
		return sandbox.StateCreating
	case phaseRunning:
		return sandbox.StateRunning
	case phaseStopped:
		return sandbox.StateStopped
	case phaseDeleting:
		return sandbox.StateDeleting
	default:
		return sandbox.StateError
	}
}

// Destroy deletes the sandbox. It is idempotent: a sandbox that is already
// gone is success, matching the interface contract.
func (p *Provider) Destroy(ctx context.Context, id string) error {
	if _, err := p.core.Delete(ctx, cellaclient.KindSandbox, id); err != nil {
		if err = mapError(err); errors.Is(err, sandbox.ErrNotFound) {
			return nil
		}
		return err
	}
	return nil
}

// HealthCheck returns nil iff the sandbox exists and is running. A missing
// sandbox yields [sandbox.ErrNotFound]; any other phase yields an error naming
// the phase and the control plane's reason for it.
func (p *Provider) HealthCheck(ctx context.Context, id string) error {
	obj, _, err := p.core.GetSandbox(ctx, id)
	if err != nil {
		return mapError(err)
	}
	if obj.Status.Phase != phaseRunning {
		if obj.Status.Reason != "" {
			return fmt.Errorf("cella: sandbox %q not running: phase=%s reason=%s", id, obj.Status.Phase, obj.Status.Reason)
		}
		return fmt.Errorf("cella: sandbox %q not running: phase=%s", id, obj.Status.Phase)
	}
	return nil
}
