// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellastub

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/cella/client"
	v1 "latere.ai/x/cella/manifest/v1"
)

// hostFixture is a stub serving the environment laptop as a person's
// computer that grants folder for writing.
func hostFixture(t *testing.T) (*Server, *client.Client, string) {
	t.Helper()
	s := New(t)
	folder, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	folder = filepath.ToSlash(folder)
	s.AddHostEnvironment("laptop", v1.HostInfo{OS: "darwin", Arch: "arm64", Grants: []v1.HostGrant{{Path: folder, Write: true}}}, v1.IsolationProcess)
	return s, dial(t, s), folder
}

// onLaptop places a sandbox on laptop with folder as its host workspace.
func onLaptop(folder string) func(*v1.Sandbox) {
	return func(sb *v1.Sandbox) {
		sb.Spec.Environment = "laptop"
		sb.Spec.Workspace.Host = folder
	}
}

// TestAHostEnvironment: a person's computer's environment reads Ready
// with its host and isolation; a sandbox on it binds the granted folder
// in place, takes no image, names its scratch directories in
// status.paths, and its file routes serve those and the folder; a folder
// the host does not grant is workspace_not_granted, a host folder on an
// environment that binds none capability_unsupported, and an unknown
// environment's read not_found.
func TestAHostEnvironment(t *testing.T) {
	s, c, folder := hostFixture(t)
	ctx := t.Context()
	env, _, err := c.GetEnvironment(ctx, "laptop")
	if err != nil || env.Status.Phase != v1.EnvironmentReady || env.Status.Isolation != v1.IsolationProcess || len(env.Status.Hosts) != 1 || env.Status.Hosts[0].Host.OS != "darwin" {
		t.Fatalf("the environment: %+v, %v", env.Status, err)
	}
	if def, _, err := c.GetEnvironment(ctx, DefaultEnvironment); err != nil || def.Status.Phase != v1.EnvironmentReady || len(def.Status.Hosts) != 0 {
		t.Fatalf("the default environment: %+v, %v", def.Status, err)
	}
	_, _, err = c.GetEnvironment(ctx, "nowhere")
	code(t, err, "not_found")

	sb, _, err := c.CreateSandbox(ctx, manifest(t, "dev", onLaptop(folder)))
	if err != nil {
		t.Fatal(err)
	}
	if sb.Spec.Workspace.Path != folder || sb.Spec.Workdir != folder || sb.Spec.Image != "" || sb.Status.Isolation != v1.IsolationProcess || sb.Status.Phase != "Running" {
		t.Fatalf("the sandbox: %+v", sb)
	}
	if sb.Status.Paths == nil || sb.Status.Paths.Home == "" || strings.HasPrefix(sb.Status.Paths.Tmp, folder) {
		t.Fatalf("status.paths: %+v", sb.Status.Paths)
	}
	tmp := sb.Status.Paths.Tmp
	if created, ok := s.Created("dev"); !ok || created.Spec.Workspace.Path != "" || created.Spec.Workspace.Host != folder {
		t.Fatalf("the create as received: %+v", created.Spec)
	}
	if _, ok := s.Created("missing"); ok {
		t.Fatal("a sandbox that was never created")
	}
	for _, p := range []string{tmp + "/topos/bin/helper", folder + "/notes.md"} {
		if err := c.FilePut(ctx, sb.Status.ID, p, "0755", strings.NewReader("x")); err != nil {
			t.Fatalf("put %s: %v", p, err)
		}
		fi, err := os.Stat(filepath.FromSlash(p))
		if err != nil || fi.Mode().Perm() != 0o755 {
			t.Fatalf("%s: %v %v", p, fi, err)
		}
		rc, err := c.FileGet(ctx, sb.Status.ID, p)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		if err := errors.Join(err, rc.Close()); err != nil || string(b) != "x" {
			t.Fatalf("get %s: %q, %v", p, b, err)
		}
	}
	if err := c.FileMkdir(ctx, sb.Status.ID, tmp+"/topos/spill"); err != nil {
		t.Fatal(err)
	}
	if err := c.FileMove(ctx, sb.Status.ID, tmp+"/topos/bin/helper", tmp+"/topos/bin/moved"); err != nil {
		t.Fatal(err)
	}
	code(t, c.FileMove(ctx, sb.Status.ID, tmp+"/topos/bin/moved", folder+"/moved"), "invalid_field")
	code(t, c.FilePut(ctx, sb.Status.ID, "/etc/passwd", "", strings.NewReader("x")), "invalid_field")

	_, _, err = c.CreateSandbox(ctx, manifest(t, "elsewhere", onLaptop("/Users/someone/else")))
	code(t, err, "workspace_not_granted")
	_, _, err = c.CreateSandbox(ctx, manifest(t, "nohost", func(sb *v1.Sandbox) { sb.Spec.Workspace.Host = folder }))
	code(t, err, "capability_unsupported")
}

// TestAHostThatHoldsRefusesAndGoesAway: a held create reads Pending with
// HostApproval Unknown until Admit runs it, RefuseCreate fails it with
// the host's words and TimeoutHost with HostTimeout; a computer that is
// not Ready refuses a create with driver_unavailable; and one that went
// away answers every operation on its sandboxes so, and ends each exec
// session open on one with no exit, until it is back.
func TestAHostThatHoldsRefusesAndGoesAway(t *testing.T) {
	s, c, folder := hostFixture(t)
	ctx := t.Context()
	s.HoldCreates("laptop", true)
	for _, name := range []string{"allowed", "refused", "late"} {
		sb, _, err := c.CreateSandbox(ctx, manifest(t, name, onLaptop(folder)))
		if err != nil {
			t.Fatal(err)
		}
		cond := sb.Status.Conditions
		if sb.Status.Phase != "Pending" || len(cond) != 1 || cond[0].Type != v1.ConditionHostApproval || cond[0].Status != v1.ConditionUnknown {
			t.Fatalf("a held create: %+v", sb.Status)
		}
	}
	if !s.Admit("allowed") || !s.RefuseCreate("refused", "not now") || !s.TimeoutHost("late") || s.Admit("missing") {
		t.Fatal("an answer found no sandbox")
	}
	for name, want := range map[string]struct{ phase, reason, status, message string }{
		"allowed": {"Running", "", v1.ConditionTrue, ""},
		"refused": {"Failed", v1.ReasonHostRefused, v1.ConditionFalse, "not now"},
		"late":    {"Failed", v1.ReasonHostTimeout, v1.ConditionFalse, "The host did not answer in time."},
	} {
		sb, _, err := c.GetSandbox(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		cond := sb.Status.Conditions
		if sb.Status.Phase != want.phase || sb.Status.Reason != want.reason || len(cond) != 1 || cond[0].Status != want.status || cond[0].Message != want.message {
			t.Fatalf("%s: %+v", name, sb.Status)
		}
	}
	s.HoldCreates("laptop", false)

	s.SetEnvironmentPhase("laptop", v1.EnvironmentOffline)
	if env, _, err := c.GetEnvironment(ctx, "laptop"); err != nil || env.Status.Phase != v1.EnvironmentOffline {
		t.Fatalf("an offline environment reads %+v, %v", env.Status, err)
	}
	_, _, err := c.CreateSandbox(ctx, manifest(t, "offline", onLaptop(folder)))
	code(t, err, "driver_unavailable")
	s.SetEnvironmentPhase("laptop", v1.EnvironmentReady)

	sess, err := c.ExecSession(ctx, "allowed", client.ExecRequest{Command: []string{"sh", "-c", "echo up; sleep 30"}})
	if err != nil {
		t.Fatal(err)
	}
	up := make([]byte, 3)
	if _, err := io.ReadFull(sess, up); err != nil || string(up) != "up\n" {
		t.Fatalf("the command did not start: %q, %v", up, err)
	}
	s.Disconnect("laptop")
	if _, err := io.ReadAll(sess); err == nil {
		t.Fatal("a session on a computer that went away read to its end")
	}
	if _, err := sess.Wait(); err == nil {
		t.Fatal("a session on a computer that went away ended with an exit")
	}
	_, _, err = c.GetSandbox(ctx, "allowed")
	code(t, err, "driver_unavailable")
	if env, _, err := c.GetEnvironment(ctx, "laptop"); err != nil || len(env.Status.Hosts) != 0 || env.Status.Workers != 0 {
		t.Fatalf("a computer that went away still lists its host: %+v, %v", env.Status, err)
	}
	_, _, err = c.CreateSandbox(ctx, manifest(t, "away", onLaptop(folder)))
	code(t, err, "driver_unavailable")
	s.Reconnect("laptop")
	if sb, _, err := c.GetSandbox(ctx, "allowed"); err != nil || sb.Status.Phase != "Running" {
		t.Fatalf("the sandbox once its computer is back: %+v, %v", sb.Status, err)
	}
	// The answers of an environment the stub does not serve as a computer
	// change nothing.
	s.SetEnvironmentPhase(DefaultEnvironment, v1.EnvironmentOffline)
	s.HoldCreates(DefaultEnvironment, true)
	s.Disconnect(DefaultEnvironment)
	s.Reconnect(DefaultEnvironment)
	create(t, s, "plain")
}

// TestASessionThatOpensOnAComputerThatWentAway: a session that registers
// after its computer went away, between its route finding the sandbox
// running and its command starting, is refused registration so it ends
// as one Disconnect drops; once the computer is back a session registers.
func TestASessionThatOpensOnAComputerThatWentAway(t *testing.T) {
	s, c, folder := hostFixture(t)
	sb, _, err := c.CreateSandbox(t.Context(), manifest(t, "late", onLaptop(folder)))
	if err != nil {
		t.Fatal(err)
	}
	id := sb.Status.ID
	s.Disconnect("laptop")
	if s.open(id, &session{}) {
		t.Fatal("a session registered on a computer that went away")
	}
	s.Reconnect("laptop")
	if !s.open(id, &session{}) {
		t.Fatal("a session did not register once its computer was back")
	}
	s.mu.Lock()
	n := len(s.sessions[id])
	s.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d sessions registered", n)
	}
	if s.open("sbx_none", &session{}) != true {
		t.Fatal("a session of a sandbox the stub does not hold was refused")
	}
}
