// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"fmt"
	"regexp"
)

// The values of a list's rollout filter that name no rollout (spec 077):
// RolloutNone keeps the sessions of no rollout, which the list route reads
// when it names none, and RolloutAny keeps every session.
const (
	RolloutNone = "none"
	RolloutAny  = "any"
)

// rolloutPattern is the form of a rollout id a create names: a caller's
// key for its run, such as an evaluation's cell and round.
var rolloutPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// RolloutRule states rolloutPattern for a refusal.
const RolloutRule = "a letter or digit, then at most 127 letters, digits, dots, underscores or hyphens, and neither none nor any"

// CheckRollout reports why id cannot be the rollout a session is created
// in: one outside the rule, or one of the two values a list's filter
// reads as no rollout or every one.
func CheckRollout(id string) error {
	if !rolloutPattern.MatchString(id) || id == RolloutNone || id == RolloutAny {
		return fmt.Errorf("rollout %q is not %s", id, RolloutRule)
	}
	return nil
}
