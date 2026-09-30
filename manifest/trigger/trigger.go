// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package trigger holds the parts of the Trigger kind that need no I/O
// (spec 022): the template a firing's message, title, key and repository
// are rendered from, the schedule a trigger fires on, the envelope a
// delivered event arrives in and the filter that selects it, the
// defaults spec 022 added, and its limits. The resolver checks a trigger
// with it at apply, and a server fires the trigger with it, so the two
// read one grammar.
//
// The package imports manifest/v1 and the standard library alone.
package trigger

import v1 "latere.ai/x/topos/manifest/v1"

// The limits of spec 022.
const (
	// DefaultMaxActive is maxActive when a trigger sets none.
	DefaultMaxActive = 5
	// MaxActiveCeiling is the most a trigger's maxActive may be.
	MaxActiveCeiling = 100
	// MaxValueBytes bounds one placeholder's rendered value, which is
	// cut at a character boundary and marked with CutMark past it.
	MaxValueBytes = 16 << 10
	// MaxMessageBytes bounds a rendered message; a firing whose message
	// is longer is refused.
	MaxMessageBytes = 64 << 10
)

// DefaultEventKey is an event trigger's session.key when it sets none:
// the resource the event happened to, so each resource has its own
// session.
const DefaultEventKey = "{{event.resource}}"

// Policy is s's session policy, v1.PolicyNew when it names none.
func Policy(s v1.TriggerSpec) string {
	if s.Session.Policy == "" {
		return v1.PolicyNew
	}
	return s.Session.Policy
}

// Key is s's session.key template: the one it names, or "" on a schedule
// and DefaultEventKey on an event.
func Key(s v1.TriggerSpec) string {
	switch {
	case s.Session.Key != "":
		return s.Session.Key
	case s.On != nil:
		return DefaultEventKey
	}
	return ""
}

// MaxActive is s's maxActive, DefaultMaxActive when it sets none.
func MaxActive(s v1.TriggerSpec) int {
	if s.MaxActive == nil {
		return DefaultMaxActive
	}
	return *s.MaxActive
}
