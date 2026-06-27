// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package hooks_test

import (
	"encoding/json"
	"testing"

	"github.com/latere-ai/topos/harness/hooks"
	"github.com/latere-ai/topos/models"
)

// TestBusDispatchOrdering asserts consumers are called in registration order.
func TestBusDispatchOrdering(t *testing.T) {
	bus := hooks.New()
	var order []int

	bus.Register("first", nil, func(_ hooks.EventName, _ any) hooks.Decision {
		order = append(order, 1)
		return hooks.Allow()
	})
	bus.Register("second", nil, func(_ hooks.EventName, _ any) hooks.Decision {
		order = append(order, 2)
		return hooks.Allow()
	})
	bus.Register("third", nil, func(_ hooks.EventName, _ any) hooks.Decision {
		order = append(order, 3)
		return hooks.Allow()
	})

	bus.Dispatch(hooks.EventPreToolUse, nil)

	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("order = %v, want [1 2 3]", order)
	}
}

// TestBusEventFilteredByName asserts that a consumer registered for specific
// events only fires on those events.
func TestBusEventFilteredByName(t *testing.T) {
	bus := hooks.New()
	fired := 0

	bus.Register("specific", []hooks.EventName{hooks.EventPreToolUse}, func(_ hooks.EventName, _ any) hooks.Decision {
		fired++
		return hooks.Allow()
	})

	bus.Dispatch(hooks.EventSessionStart, nil)
	if fired != 0 {
		t.Fatalf("fired on wrong event: fired = %d", fired)
	}
	bus.Dispatch(hooks.EventPreToolUse, nil)
	if fired != 1 {
		t.Fatalf("fired = %d, want 1", fired)
	}
}

// TestBusConsumerCanDenyPreToolUse asserts a consumer on PreToolUse can deny.
func TestBusConsumerCanDenyPreToolUse(t *testing.T) {
	bus := hooks.New()

	bus.Register("denier", []hooks.EventName{hooks.EventPreToolUse}, func(_ hooks.EventName, _ any) hooks.Decision {
		return hooks.Deny("test denial")
	})

	d := bus.Dispatch(hooks.EventPreToolUse, &hooks.PreToolUsePayload{
		Version:   "1",
		SessionID: "sess-1",
	})

	if d.Verdict != hooks.VerdictDeny {
		t.Fatalf("verdict = %q, want deny", d.Verdict)
	}
	if d.Reason != "test denial" {
		t.Fatalf("reason = %q, want 'test denial'", d.Reason)
	}
}

// TestBusDenyShortCircuits asserts deny stops subsequent consumers.
func TestBusDenyShortCircuits(t *testing.T) {
	bus := hooks.New()
	secondCalled := false

	bus.Register("denier", nil, func(_ hooks.EventName, _ any) hooks.Decision {
		return hooks.Deny("stopped")
	})
	bus.Register("after", nil, func(_ hooks.EventName, _ any) hooks.Decision {
		secondCalled = true
		return hooks.Allow()
	})

	bus.Dispatch(hooks.EventPreToolUse, nil)
	if secondCalled {
		t.Fatal("second consumer was called after deny")
	}
}

// TestBusEveryDispatchRecorded asserts every dispatch appears in the event log.
func TestBusEveryDispatchRecorded(t *testing.T) {
	bus := hooks.New()

	bus.Dispatch(hooks.EventSessionStart, nil)
	bus.Dispatch(hooks.EventPreToolUse, nil)
	bus.Dispatch(hooks.EventSessionEnd, nil)

	log := bus.EventLog()
	if len(log) != 3 {
		t.Fatalf("log len = %d, want 3", len(log))
	}
	if log[0].EventName != hooks.EventSessionStart {
		t.Fatalf("log[0].EventName = %q", log[0].EventName)
	}
	if log[1].EventName != hooks.EventPreToolUse {
		t.Fatalf("log[1].EventName = %q", log[1].EventName)
	}
	if log[2].EventName != hooks.EventSessionEnd {
		t.Fatalf("log[2].EventName = %q", log[2].EventName)
	}
}

// TestToolPathHookAllowAndDenyRuleNetDeny asserts the invariant:
// hook-allow AND NOT deny-rule → net deny when a deny-rule matches.
func TestToolPathHookAllowAndDenyRuleNetDeny(t *testing.T) {
	bus := hooks.New()

	// Hook consumer allows.
	bus.Register("allower", []hooks.EventName{hooks.EventPreToolUse}, func(_ hooks.EventName, _ any) hooks.Decision {
		return hooks.Allow()
	})

	// Policy deny-rule matches "rm".
	denyRules := []hooks.DenyRule{
		{
			Name: "no-rm",
			Predicate: func(toolName string, _ json.RawMessage) bool {
				return toolName == "rm"
			},
		},
	}

	tp := hooks.NewToolPath(bus, denyRules)
	result := tp.Resolve("sess-1", models.ToolCall{ID: "c1", Name: "rm", Input: json.RawMessage(`{}`)})

	if result.Allowed {
		t.Fatal("expected net deny, got allowed")
	}
	if result.DeniedBy != "rule:no-rm" {
		t.Fatalf("denied_by = %q, want rule:no-rm", result.DeniedBy)
	}
}

// TestToolPathHookDenyOverridesAllow asserts hook deny wins.
func TestToolPathHookDenyOverridesAllow(t *testing.T) {
	bus := hooks.New()

	bus.Register("denier", []hooks.EventName{hooks.EventPreToolUse}, func(_ hooks.EventName, _ any) hooks.Decision {
		return hooks.Deny("hook said no")
	})

	tp := hooks.NewToolPath(bus, nil)
	result := tp.Resolve("sess-1", models.ToolCall{ID: "c1", Name: "bash", Input: json.RawMessage(`{}`)})

	if result.Allowed {
		t.Fatal("expected deny, got allowed")
	}
	if result.DeniedBy != "hook:PreToolUse" {
		t.Fatalf("denied_by = %q, want hook:PreToolUse", result.DeniedBy)
	}
}

// TestToolPathAllowedWithNoDenyRules asserts the happy path when no consumers
// or deny-rules are registered (MVP: trusted sandbox, tools open).
func TestToolPathAllowedWithNoDenyRules(t *testing.T) {
	bus := hooks.New()
	tp := hooks.NewToolPath(bus, nil)

	result := tp.Resolve("sess-1", models.ToolCall{ID: "c1", Name: "bash", Input: json.RawMessage(`{"command":"echo hi"}`)})
	if !result.Allowed {
		t.Fatalf("expected allow, got deny (%s: %s)", result.DeniedBy, result.Reason)
	}
	if string(result.ModifiedInput) != `{"command":"echo hi"}` {
		t.Fatalf("modified_input = %q", result.ModifiedInput)
	}
}

// TestSessionEndFiresOnce is a behavioural test: the loop must defer bus.Dispatch
// (EventSessionEnd, ...) exactly once even on error paths. This test simulates
// that contract at the bus level.
func TestSessionEndFiresOnce(t *testing.T) {
	bus := hooks.New()
	count := 0
	bus.Register("session-end-counter", []hooks.EventName{hooks.EventSessionEnd}, func(_ hooks.EventName, _ any) hooks.Decision {
		count++
		return hooks.Allow()
	})

	// Only one dispatch.
	bus.Dispatch(hooks.EventSessionEnd, &hooks.SessionEndPayload{Version: "1"})

	if count != 1 {
		t.Fatalf("session end fired %d times, want 1", count)
	}
	if len(bus.EventLog()) != 1 {
		t.Fatalf("event log len = %d, want 1", len(bus.EventLog()))
	}
}
