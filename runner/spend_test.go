// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
	"testing"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
)

// TestASetupRefusedForSpendStopsWithBudget: a session whose machine a
// core refused for spend at setup, such as Cella refusing the sandbox
// create, goes idle with budget and a session.error naming the refusal,
// so a resume continues it once the allowance is raised, rather than
// with error as other setup failures do.
func TestASetupRefusedForSpendStopsWithBudget(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	f.message(ctx, "Go.")
	refused := &models.SpendError{Core: machine.KindCella, Code: "budget_exhausted", Err: errors.New("Cella refused to create the sandbox")}
	f.r.o.Harness = func(context.Context, session.Session) (harness.Config, error) {
		return harness.Config{}, &SetupError{Code: machine.CodeUnavailable, Err: refused}
	}
	if _, err := f.r.Drive(ctx, f.s.ID); !errors.Is(err, refused) {
		t.Fatalf("drive: %v", err)
	}
	s, err := f.store.Get(ctx, f.s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != session.StatusIdle || s.StopReason != session.StopBudget {
		t.Fatalf("session %s %s", s.Status, s.StopReason)
	}
	evs, err := f.store.Events(ctx, f.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var se session.SessionError
	var st session.SessionStatus
	for _, e := range evs {
		switch e.Type {
		case session.TypeSessionError:
			if err := e.Decode(&se); err != nil {
				t.Fatal(err)
			}
		case session.TypeSessionStatus:
			if err := e.Decode(&st); err != nil {
				t.Fatal(err)
			}
		}
	}
	if se.Code != "budget_exhausted" || se.Retryable || st.StopReason != session.StopBudget || st.Detail != "budget_exhausted" {
		t.Fatalf("session.error %+v, last status %+v", se, st)
	}
}
