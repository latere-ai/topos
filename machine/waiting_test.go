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

// TestADeferredMachineHoldsWhatTheComputerAnswered: a refusal or a wait
// an open met answers every later open, a start included, without
// opening again, and AskAgain lets the next open ask anew; it also lets
// go of an open machine that is absent, so the next operation opens one
// again (spec 080).
func TestADeferredMachineHoldsWhatTheComputerAnswered(t *testing.T) {
	for _, held := range []error{&Refused{Environment: "dev-1", Detail: "no"}, &Waiting{Reason: WaitApproval, Environment: "dev-1"}} {
		opens := 0
		answer := held
		d := Defer(t.Context(), KindDevice, func(context.Context) (Machine, error) {
			opens++
			if answer != nil {
				return nil, answer
			}
			return &fake{}, nil
		})
		for range 3 {
			if err := d.Open(t.Context()); !errors.Is(err, held) {
				t.Fatalf("an open after %v: %v", held, err)
			}
		}
		d.Start()
		if err := d.Open(t.Context()); !errors.Is(err, held) || opens != 1 {
			t.Fatalf("after %v the computer was asked %d times, the last open %v", held, opens, err)
		}
		answer = nil
		d.AskAgain()
		if err := d.Open(t.Context()); err != nil || opens != 2 {
			t.Fatalf("an open after AskAgain: %v, %d opens", err, opens)
		}
	}
	opens := 0
	d := Defer(t.Context(), KindDevice, func(context.Context) (Machine, error) {
		opens++
		if opens == 1 {
			return &away{}, nil
		}
		return &fake{}, nil
	})
	if err := d.Open(t.Context()); err != nil || d.Absent() == nil {
		t.Fatalf("the first open: %v, absent %v", err, d.Absent())
	}
	d.AskAgain()
	if err := d.Open(t.Context()); err != nil || opens != 2 || d.Absent() != nil {
		t.Fatalf("an open after the computer came back: %v, %d opens, absent %v", err, opens, d.Absent())
	}
	d.AskAgain()
	if err := d.Open(t.Context()); err != nil || opens != 2 {
		t.Fatalf("an open machine that answers is opened again: %v, %d opens", err, opens)
	}
}
