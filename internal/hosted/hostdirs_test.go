// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"context"
	"testing"

	"latere.ai/x/topos/machine"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

func TestByKind(t *testing.T) {
	opened := ""
	open := func(kind string) Machines {
		return func(context.Context, session.Session, v1.Machine) (machine.Machine, error) {
			opened = kind
			return nil, nil
		}
	}
	for _, c := range []struct {
		session, agent, want string
	}{{"", v1.MachineCella, "cella"}, {session.MachineCella, v1.MachineHost, "cella"}, {session.MachineHost, v1.MachineCella, "host"}, {"", v1.MachineHost, "host"}} {
		opened = ""
		if _, err := ByKind(open("cella"), open("host"))(t.Context(), session.Session{Machine: session.Machine{Kind: c.session}}, v1.Machine{Kind: c.agent}); err != nil || opened != c.want {
			t.Fatalf("%+v: opened %q, %v", c, opened, err)
		}
	}
	if _, err := ByKind(open("cella"), nil)(t.Context(), session.Session{}, v1.Machine{Kind: v1.MachineHost}); code(t, err) != CodeMachineUnavailable {
		t.Fatalf("host sessions off: %v", err)
	}
}
