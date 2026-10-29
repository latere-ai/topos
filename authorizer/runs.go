// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer

import (
	"fmt"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/manifest/trigger"
)

// WireRuns is the limits object of an allow of trigger.runs (spec 095):
// the scopes due at a firing of a trigger whose reach splits by scope, in
// the order their sessions start.
type WireRuns struct {
	Runs []WireRun `json:"runs"`
}

// WireRun is one scope due: its name, which keys its session, and the
// time since which its run reads, which the session's create carries;
// nil when the endpoint names none.
type WireRun struct {
	Scope string     `json:"scope"`
	Since *time.Time `json:"since,omitempty"`
}

// DecodeRuns reads the runs an allow of trigger.runs names; an allow with
// no limits names none. Limits that do not read, or runs past
// trigger.MaxRunsPerFiring, outside a scope's pattern or naming one scope
// twice, are an error, and toposd refuses the firing as runs_invalid.
func DecodeRuns(d authz.Decision) ([]WireRun, error) {
	var w WireRuns
	if err := d.DecodeLimits(&w); err != nil {
		return nil, fmt.Errorf("limits: %w", err)
	}
	scopes := make([]string, len(w.Runs))
	for i, r := range w.Runs {
		scopes[i] = r.Scope
	}
	if err := trigger.CheckRuns(scopes); err != nil {
		return nil, fmt.Errorf("limits.%w", err)
	}
	for i := range w.Runs {
		if s := w.Runs[i].Since; s != nil {
			w.Runs[i].Since = new(s.UTC())
		}
	}
	return w.Runs, nil
}
