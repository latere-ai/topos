// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// away is a fake machine whose computer stopped answering.
type away struct{ fake }

func (a *away) Absent() *Waiting { return &Waiting{Reason: WaitOffline, Environment: "dev-1"} }

// TestAMachineThatWaits: a wait and a refusal say what the person can do,
// with what said so; a machine that is never away is not absent, and a
// deferred machine is absent as the machine it opened is; and the notice
// of a wait reaches the function the context carries, and nothing when
// it carries none (spec 080).
func TestAMachineThatWaits(t *testing.T) {
	cause := errors.New("driver_unavailable")
	for _, c := range []struct {
		err  error
		want string
	}{
		{&Waiting{Reason: WaitOffline, Environment: "dev-1"}, "the computer is offline (dev-1)"},
		{&Waiting{Reason: WaitOffline, Environment: "dev-1", Err: cause}, "the computer is offline (dev-1): driver_unavailable"},
		{&Waiting{Reason: WaitApproval, Environment: "dev-1"}, "has not allowed the session yet (dev-1)"},
		{&Refused{Environment: "dev-1"}, "refused the session (dev-1)"},
		{&Refused{Environment: "dev-1", Detail: "not now"}, "refused the session (dev-1): not now"},
		{&Refused{Environment: "dev-1", Timeout: true}, "did not allow the session in time (dev-1)"},
	} {
		if !strings.Contains(c.err.Error(), c.want) {
			t.Errorf("%q, want it to say %q", c.err.Error(), c.want)
		}
	}
	if !errors.Is(&Waiting{Err: cause}, cause) {
		t.Fatal("a wait hides what said so")
	}
	if AbsentOf(&fake{}) != nil {
		t.Fatal("a machine that is never away is absent")
	}
	if w := AbsentOf(&away{}); w == nil || w.Reason != WaitOffline {
		t.Fatalf("an away machine: %+v", w)
	}
	d := Defer(t.Context(), KindDevice, func(context.Context) (Machine, error) { return &away{}, nil })
	if d.Absent() != nil {
		t.Fatal("a machine not opened yet is absent")
	}
	if err := d.Open(t.Context()); err != nil {
		t.Fatal(err)
	}
	if w := d.Absent(); w == nil || w.Environment != "dev-1" {
		t.Fatalf("the deferred machine once open: %+v", w)
	}
	if err := TellWaiting(t.Context(), Waiting{Reason: WaitApproval}); err != nil {
		t.Fatalf("a notice with no one to tell: %v", err)
	}
	var told []Waiting
	ctx := WithWaiting(t.Context(), func(_ context.Context, w Waiting) error {
		told = append(told, w)
		return cause
	})
	if err := TellWaiting(ctx, Waiting{Reason: WaitApproval, Environment: "dev-1"}); !errors.Is(err, cause) || len(told) != 1 || told[0].Environment != "dev-1" {
		t.Fatalf("the notice: %v, told %+v", err, told)
	}
}
