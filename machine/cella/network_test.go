// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"latere.ai/x/cella/client"
	v1 "latere.ai/x/cella/manifest/v1"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/test/stubs/cellastub"
)

// TestManifestTakesTheSessionsNetwork: the sandbox is created with the
// session's network as its egress: open and none name no host, the hosts
// the machine was given among them, and an allowlist joins the hosts of
// the session's named secrets to the ones it was given, the session's
// network, the agent's and the repositories' (spec 052). The machine
// reports the mode the sandbox runs.
func TestManifestTakesTheSessionsNetwork(t *testing.T) {
	for _, c := range []struct {
		mode  string
		want  v1.Egress
		hosts []string
	}{
		{"", v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: []string{"api.example.com", "code.example.com", "docs.example.org", "lux.example.com"}}, nil},
		{"allowlist", v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: []string{"api.example.com", "code.example.com", "docs.example.org", "lux.example.com"}}, nil},
		{"open", v1.Egress{Mode: v1.EgressOpen}, nil},
		{"none", v1.Egress{Mode: v1.EgressNone}, nil},
	} {
		f := open(t, func(stub *cellastub.Server, o *Options) {
			stub.AddSecret("lux", "lux.example.com")
			o.Secrets = []v1.SecretMount{{Name: "lux", Env: "LUX_KEY"}}
			o.EgressMode = c.mode
			o.Egress = []string{"docs.example.org", "api.example.com", "code.example.com"}
		})
		sb, _ := f.stub.Sandbox(f.m.Name())
		got := sb.Spec.Network.Egress
		if got.Mode != c.want.Mode || !slices.Equal(got.AllowedHosts, c.want.AllowedHosts) {
			t.Errorf("mode %q: egress %+v, want %+v", c.mode, got, c.want)
		}
		if f.m.Info().Egress != string(c.want.Mode) {
			t.Errorf("mode %q: the machine reports %q", c.mode, f.m.Info().Egress)
		}
	}
}

// TestApplyNetworkWidensAndNarrows: the running sandbox is updated to the
// network it is given, with its whole spec so no immutable field moves,
// and a network it already runs sends nothing; a refused update leaves its
// egress as it was, and the next apply asks again (spec 052).
func TestApplyNetworkWidensAndNarrows(t *testing.T) {
	f := open(t, func(stub *cellastub.Server, o *Options) {
		stub.AddSecret("lux", "lux.example.com")
		o.Secrets = []v1.SecretMount{{Name: "lux", Env: "LUX_KEY"}}
		o.Egress = []string{"a.example.com"}
	})
	hosts := func() []string {
		sb, _ := f.stub.Sandbox(f.m.Name())
		return sb.Spec.Network.Egress.AllowedHosts
	}
	ctx := t.Context()
	if err := f.m.ApplyNetwork(ctx, machine.Network{Mode: "allowlist", Hosts: []string{"a.example.com", "b.example.com"}}); err != nil {
		t.Fatal(err)
	}
	if got := hosts(); !slices.Equal(got, []string{"a.example.com", "b.example.com", "lux.example.com"}) {
		t.Fatalf("widened to %q", got)
	}
	applies := f.stub.Count(cellastub.OpApply)
	if err := f.m.ApplyNetwork(ctx, machine.Network{Mode: "allowlist", Hosts: []string{"b.example.com", "a.example.com"}}); err != nil {
		t.Fatal(err)
	}
	if n := f.stub.Count(cellastub.OpApply); n != applies {
		t.Fatalf("the same network was sent again: %d applies, then %d", applies, n)
	}
	f.stub.Fail(cellastub.OpApply, cellastub.Failure{Status: http.StatusForbidden, Code: "forbidden", Detail: "sandbox.update: the boundary is the installation's"})
	if err := f.m.ApplyNetwork(ctx, machine.Network{Mode: "allowlist", Hosts: []string{"a.example.com", "b.example.com", "c.example.com"}}); err == nil {
		t.Fatal("a refused widening answered no error")
	}
	if got := hosts(); !slices.Equal(got, []string{"a.example.com", "b.example.com", "lux.example.com"}) {
		t.Fatalf("a refused widening changed the egress to %q", got)
	}
	if err := f.m.ApplyNetwork(ctx, machine.Network{Mode: "allowlist", Hosts: []string{"a.example.com"}}); err != nil {
		t.Fatal(err)
	}
	if got := hosts(); !slices.Equal(got, []string{"a.example.com", "lux.example.com"}) {
		t.Fatalf("narrowed to %q", got)
	}
	if err := f.m.ApplyNetwork(ctx, machine.Network{Mode: "none"}); err != nil {
		t.Fatal(err)
	}
	if sb, _ := f.stub.Sandbox(f.m.Name()); sb.Spec.Network.Egress.Mode != v1.EgressNone || len(sb.Spec.Network.Egress.AllowedHosts) != 0 || f.m.Info().Egress != "none" {
		t.Fatalf("narrowed to none: %+v, the machine reports %q", sb.Spec.Network.Egress, f.m.Info().Egress)
	}
	// A sandbox lost and created again takes the network applied last.
	if !f.stub.Remove(f.m.Name()) {
		t.Fatal("no sandbox to remove")
	}
	if err := f.m.Recreate(ctx); err != nil {
		t.Fatal(err)
	}
	if sb, _ := f.stub.Sandbox(f.m.Name()); sb.Spec.Network.Egress.Mode != v1.EgressNone {
		t.Fatalf("the sandbox created again runs %+v", sb.Spec.Network.Egress)
	}
}

// TestAFoundSandboxTakesTheSessionsNetwork: a runner that finds the
// session's sandbox by name, which runs another network than the session
// has now, updates it before any call runs in it.
func TestAFoundSandboxTakesTheSessionsNetwork(t *testing.T) {
	f := open(t, func(_ *cellastub.Server, o *Options) { o.Egress = []string{"a.example.com", "b.example.com"} })
	o := f.o
	o.Egress = []string{"a.example.com"}
	m, err := Open(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if m.Created() {
		t.Fatal("the sandbox was created again")
	}
	if sb, _ := f.stub.Sandbox(f.m.Name()); !slices.Equal(sb.Spec.Network.Egress.AllowedHosts, []string{"a.example.com"}) {
		t.Fatalf("the found sandbox runs %+v", sb.Spec.Network.Egress)
	}
	applies := f.stub.Count(cellastub.OpApply)
	if _, err := Open(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	if n := f.stub.Count(cellastub.OpApply); n != applies {
		t.Fatalf("a sandbox that runs the session's network was updated: %d applies, then %d", applies, n)
	}
}

// TestRefusedReadsTheGatewaysDenials: the hosts the gateway refused since
// a time are read once each, oldest first, with the port of the first
// refusal; an allowed connection and a refusal before the time are not.
func TestRefusedReadsTheGatewaysDenials(t *testing.T) {
	f := open(t)
	since := time.Now().UTC()
	for _, r := range []client.EgressRecord{
		{Host: "old.example.com", Port: 443, Decision: "denied", At: since.Add(-time.Second)},
		{Host: "Pkg.Example.com", Port: 443, Decision: "denied", At: since.Add(time.Millisecond)},
		{Host: "ok.example.com", Port: 443, Decision: "allowed", At: since.Add(2 * time.Millisecond)},
		{Host: "pkg.example.com", Port: 80, Decision: "denied", At: since.Add(3 * time.Millisecond)},
		{Host: "cdn.example.org", Port: 8443, Decision: "denied", At: since.Add(4 * time.Millisecond)},
	} {
		if !f.stub.AddEgressRecord(f.m.Name(), r) {
			t.Fatal("no sandbox")
		}
	}
	got, err := f.m.Refused(t.Context(), since)
	if err != nil {
		t.Fatal(err)
	}
	want := []machine.Connection{{Host: "pkg.example.com", Port: 443, At: since.Add(time.Millisecond)}, {Host: "cdn.example.org", Port: 8443, At: since.Add(4 * time.Millisecond)}}
	if len(got) != len(want) {
		t.Fatalf("refused %+v", got)
	}
	for i := range want {
		if got[i].Host != want[i].Host || got[i].Port != want[i].Port || !got[i].At.Equal(want[i].At) {
			t.Fatalf("refused %+v, want %+v", got, want)
		}
	}
	f.stub.Fail(cellastub.OpEgress, cellastub.Failure{Status: http.StatusServiceUnavailable, Code: "driver_unavailable"})
	if _, err := f.m.Refused(t.Context(), since); err == nil {
		t.Fatal("a failed read answered no error")
	}
}
