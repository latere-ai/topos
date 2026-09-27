// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"testing"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/cellastub"
)

// TestASpendRefusalIsNamedAsOne: Cella refusing a sandbox create, a
// command or a file operation because the session's allowance is spent
// answers the refusal models.SpendRefused reads, whatever its status,
// never machine_unavailable and never retried.
func TestASpendRefusalIsNamedAsOne(t *testing.T) {
	stub := cellastub.New(t)
	o := options(t, stub)
	o.Session = session.NewID(session.PrefixSession)
	stub.Fail(cellastub.OpCreate, cellastub.Failure{Status: 402, Code: "budget_exhausted", Detail: "the wallet is empty"})
	_, err := Open(t.Context(), o)
	if code, spent := models.SpendRefused(err); !spent || code != "budget_exhausted" || Code(err) != "" {
		t.Fatalf("a refused create: %v (code %q)", err, Code(err))
	}
	if n := stub.Count(cellastub.OpCreate); n != 1 {
		t.Fatalf("the create was asked %d times", n)
	}

	f := open(t)
	f.stub.Fail(cellastub.OpSession, cellastub.Failure{Status: 429, Code: "spend_exceeded"})
	_, err = f.m.Exec(t.Context(), machine.ExecRequest{Command: "true"})
	if code, spent := models.SpendRefused(err); !spent || code != "spend_exceeded" || Code(err) != "" {
		t.Fatalf("a refused command: %v", err)
	}
	f.stub.Fail(cellastub.OpFiles, cellastub.Failure{Status: 403, Code: "budget_exhausted"})
	if _, err := f.m.Stat(t.Context(), "."); !isSpent(err) {
		t.Fatalf("a refused file operation: %v", err)
	}
	if res, err := f.m.Exec(t.Context(), machine.ExecRequest{Command: "echo ok"}); err != nil || string(res.Output) != "ok\n" {
		t.Fatalf("the next command: %q, %v", res.Output, err)
	}
}

func isSpent(err error) bool {
	_, spent := models.SpendRefused(err)
	return spent
}
