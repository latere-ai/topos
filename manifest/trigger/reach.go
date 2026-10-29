// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package trigger

import (
	"fmt"
	"regexp"

	v1 "latere.ai/x/topos/manifest/v1"
)

// MaxRunsPerFiring is the most sessions one firing of a split trigger
// starts (spec 095): an answer of trigger.runs naming more refuses the
// firing, and the authorizer orders a person's scopes so the rest wait
// for the next firing.
const MaxRunsPerFiring = 16

// ReachKindPattern is a reach's kind: a short lower-case name an
// authorizer maps (spec 095).
const ReachKindPattern = `^[a-z][a-z0-9_]{0,31}$`

// ScopePattern is a scope an answer of trigger.runs names: printable,
// with no space, and short enough to key a session.
const ScopePattern = `^[A-Za-z0-9][A-Za-z0-9_./:-]{0,127}$`

var (
	reachKind = regexp.MustCompile(ReachKindPattern)
	scopeName = regexp.MustCompile(ScopePattern)
)

// ValidReachKind reports whether kind may name a reach.
func ValidReachKind(kind string) bool { return reachKind.MatchString(kind) }

// ValidScope reports whether scope may name a run's scope.
func ValidScope(scope string) bool { return scopeName.MatchString(scope) }

// Split reports whether a trigger of spec runs once per due scope (spec
// 095).
func Split(spec v1.TriggerSpec) bool { return spec.Reach != nil && spec.Reach.Split == v1.SplitScope }

// CheckRuns holds the scopes an answer of trigger.runs names to the shape
// a firing acts on (spec 095): at most MaxRunsPerFiring, each a scope's
// name, none named twice.
func CheckRuns(scopes []string) error {
	if len(scopes) > MaxRunsPerFiring {
		return fmt.Errorf("runs names %d, more than %d", len(scopes), MaxRunsPerFiring)
	}
	seen := make(map[string]bool, len(scopes))
	for i, s := range scopes {
		switch {
		case !ValidScope(s):
			return fmt.Errorf("runs[%d] is %q, not a scope", i, s)
		case seen[s]:
			return fmt.Errorf("runs[%d] is %q, a scope named before", i, s)
		}
		seen[s] = true
	}
	return nil
}
