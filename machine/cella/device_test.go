// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/test/stubs/cellastub"
)

// laptop is the environment a person's computer serves in these tests.
const laptop = "dev-01jaq7v3k9m2abcd"

// device is a device machine on a stub host environment, the folder it
// works in and the options it opened with.
type device struct {
	stub   *cellastub.Server
	d      *Device
	folder string
	o      Options
}

// hostOf is a host that grants folder for writing and reports platform.
func hostOf(folder, platform string) v1.HostInfo {
	goos, arch, _ := strings.Cut(platform, "/")
	return v1.HostInfo{OS: goos, Arch: arch, Grants: []v1.HostGrant{{Path: folder, Write: true}},
		Network: v1.HostNetwork{Mode: "allowlist", Hosts: []string{"github.com"}}}
}

// deviceOptions are a device's options on stub: the session's machine is
// the folder on the environment laptop, which the stub serves as a
// computer reporting platform.
func deviceOptions(t *testing.T, stub *cellastub.Server, platform string) (Options, string) {
	t.Helper()
	folder, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	folder = filepath.ToSlash(folder)
	stub.AddHostEnvironment(laptop, hostOf(folder, platform), v1.IsolationProcess)
	o := options(t, stub)
	o.Environment, o.Workdir, o.Dir = laptop, folder, ""
	o.ApprovalWait = 2 * time.Second
	return o, folder
}

// openDevice opens a device on a new stub.
func openDevice(t *testing.T, edit ...func(*cellastub.Server, *Options)) device {
	t.Helper()
	stub := cellastub.New(t)
	o, folder := deviceOptions(t, stub, platformKey)
	for _, e := range edit {
		e(stub, &o)
	}
	d, err := OpenDevice(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.Release(context.Background(), true); err != nil {
			t.Error(err)
		}
	})
	return device{stub: stub, d: d, folder: folder, o: o}
}

// TestDeviceOpensAsAPersonsComputer: a device's create names its
// environment and its folder as the host workspace, and carries no
// image, no Secret, no resources and no variable of the machine's, HOME
// among them, and the labels and annotations it was given; the device
// says it is one, with the computer's platform and isolation and the
// folder as its working directory; and its files are the folder's,
// bound in place (spec 080).
func TestDeviceOpensAsAPersonsComputer(t *testing.T) {
	dv := openDevice(t, func(_ *cellastub.Server, o *Options) {
		o.Image, o.Resources = "golang", v1.Resources{CPU: "2"}
		o.Labels = map[string]string{LabelAgentVersion: "3", "tenant": "t1"}
		o.Annotations = map[string]string{AnnotationAgentName: "Site builder"}
	})
	created, ok := dv.stub.Created(SandboxName(dv.o.Session))
	if !ok {
		t.Fatal("no sandbox was created")
	}
	spec := created.Spec
	if spec.Environment != laptop || spec.Workspace.Host != dv.folder || spec.Workspace.Path != "" || spec.Image != "" || len(spec.Secrets) > 0 || len(spec.Env) > 0 ||
		spec.Resources != (v1.Resources{}) {
		t.Fatalf("the device's create: %+v", spec)
	}
	labels := created.Metadata.Labels
	if labels[LabelSession] != dv.o.Session || labels[LabelAgent] != "coder" || labels[LabelAgentVersion] != "3" || labels["tenant"] != "t1" || labels[LabelTrigger] != "" ||
		created.Metadata.Annotations[AnnotationAgentName] != "Site builder" {
		t.Fatalf("the device's labels %v and annotations %v", labels, created.Metadata.Annotations)
	}
	info := dv.d.Info()
	if info.Kind != machine.KindDevice || info.Workdir != dv.folder || info.Environment != laptop || info.Sandbox != v1.IsolationProcess || info.OS+"/"+info.Arch != platformKey {
		t.Fatalf("the device's info: %+v", info)
	}
	ctx := t.Context()
	if err := dv.d.WriteFile(ctx, "notes.md", strings.NewReader("hello\n"), 0); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dv.folder, "notes.md")); err != nil || string(b) != "hello\n" {
		t.Fatalf("the folder holds %q, %v", b, err)
	}
	res, err := dv.d.Exec(ctx, machine.ExecRequest{Command: "cat notes.md && pwd -P"})
	if err != nil || res.ExitCode != 0 || !strings.Contains(string(res.Output), "hello") || !strings.Contains(string(res.Output), dv.folder) {
		t.Fatalf("a command in the folder: %+v %q, %v", res, res.Output, err)
	}
	for _, iface := range []struct {
		name string
		ok   bool
	}{
		{"Recovering", is[machine.Recovering](dv.d)},
		{"Importer", is[machine.Importer](dv.d)},
		{"Networked", is[machine.Networked](dv.d)},
		{"Walker", is[machine.Walker](dv.d)},
	} {
		if iface.ok {
			t.Errorf("a device is a machine.%s, which would restore into or save the person's folder", iface.name)
		}
	}
	if err := dv.d.ApplyConnections(ctx, []machine.HTTPConnection{{Env: "X", Address: "https://c.example"}}); err != nil || dv.d.ReachesConnections() {
		t.Fatalf("a device takes connections: %v, reaches %t", err, dv.d.ReachesConnections())
	}
	if w := dv.d.Absent(); w != nil {
		t.Fatalf("a device that answers is absent: %+v", w)
	}
}

// is reports whether m implements T.
func is[T any](m machine.Machine) bool {
	_, ok := m.(T)
	return ok
}

// TestDeviceSetsNoHomeAndFindsItsHelperDir: the device's create carries
// no HOME, so its commands keep the driver's own scratch home, and the
// helper and the spill directory are under topos in the temporary
// directory the computer names in status.paths, where the helper probe
// runs (spec 080).
func TestDeviceSetsNoHomeAndFindsItsHelperDir(t *testing.T) {
	dv := openDevice(t)
	created, _ := dv.stub.Created(SandboxName(dv.o.Session))
	if _, set := created.Spec.Env["HOME"]; set {
		t.Fatalf("the device's create sets HOME: %v", created.Spec.Env)
	}
	ctx := t.Context()
	res, err := dv.d.Exec(ctx, machine.ExecRequest{Command: `printf '%s\n%s\n' "$HOME" "$TMPDIR"`})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("a command: %+v, %v", res, err)
	}
	lines := strings.Split(strings.TrimSpace(string(res.Output)), "\n")
	if len(lines) != 2 || path.Base(lines[0]) != "home" || path.Base(lines[1]) != "tmp" || path.Dir(lines[0]) != path.Dir(lines[1]) {
		t.Fatalf("a command's HOME and TMPDIR: %q", res.Output)
	}
	tmp := lines[1]
	if want := path.Join(tmp, DeviceDir, "spill"); dv.d.SpillDir() != want {
		t.Fatalf("the spill directory is %s, want %s", dv.d.SpillDir(), want)
	}
	helper := path.Join(tmp, DeviceDir, "bin", "topos-machine")
	if b, err := os.ReadFile(filepath.FromSlash(helper)); err != nil || !bytes.Equal(b, helperBin) {
		t.Fatalf("the helper at %s: %d bytes, %v", helper, len(b), err)
	}
	for _, r := range dv.d.Roots() {
		if strings.HasPrefix(r, dv.folder+"/") {
			t.Fatalf("a root %s of the session's own lies in the person's folder", r)
		}
	}
}

// TestDeviceHelperByOSAndArch: on a computer reporting darwin/arm64, the
// helper written is the darwin arm64 build, whatever the platform the
// sandbox's uname says, through Cella's file route with mode 0755 into
// the scratch directory and never through a command's input; a computer
// of a platform the runner carries no build for is refused naming it
// (spec 080).
func TestDeviceHelperByOSAndArch(t *testing.T) {
	stub := cellastub.New(t)
	o, _ := deviceOptions(t, stub, "darwin/arm64")
	// The stub runs commands where the tests run, so the darwin arm64
	// build here is the one that runs; a build keyed to the test machine's
	// own platform is another, which the device must not write.
	o.Helpers = map[string][]byte{"darwin/arm64": helperBin}
	if platformKey != "darwin/arm64" {
		o.Helpers[platformKey] = []byte("not this build")
	}
	d, err := OpenDevice(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.Release(context.Background(), true); err != nil {
			t.Error(err)
		}
	})
	if info := d.Info(); info.OS != "darwin" || info.Arch != "arm64" {
		t.Fatalf("the device's platform: %+v", info)
	}
	helper := path.Join(path.Dir(d.SpillDir()), "bin", "topos-machine")
	put := false
	for _, r := range stub.Requests() {
		if r.Op == cellastub.OpFiles && r.Method == "PUT" && r.Query.Get("path") == helper && r.Query.Get("mode") == "0755" {
			put = true
		}
	}
	if n := stub.Count(cellastub.OpSession); !put || n != 0 {
		t.Fatalf("the helper was written through the file route: %t; exec sessions opened: %d", put, n)
	}
	fi, err := os.Stat(filepath.FromSlash(helper))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("the helper's mode: %v, %v", fi, err)
	}

	other := cellastub.New(t)
	o2, _ := deviceOptions(t, other, "linux/riscv64")
	if _, err := OpenDevice(t.Context(), o2); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "linux/riscv64") {
		t.Fatalf("a computer of a platform with no build: %v", err)
	}
}

// TestDeviceWaitsForItsComputer: a computer that is offline, or whose
// worker does not answer, is machine.Waiting offline; one that holds the
// create tells the session once that it waits for its owner and answers
// machine.Waiting approval after ApprovalWait, leaving the create held,
// which the next open finds and takes once the owner allowed it; one
// whose owner refused, or did not answer in time, is machine.Refused with
// its own words and its sandbox deleted, so the next open asks again; and
// one that stops answering under a command leaves the command lost and
// the device absent, offline (spec 080).
func TestDeviceWaitsForItsComputer(t *testing.T) {
	ctx := t.Context()
	t.Run("offline", func(t *testing.T) {
		stub := cellastub.New(t)
		o, _ := deviceOptions(t, stub, platformKey)
		stub.SetEnvironmentPhase(laptop, v1.EnvironmentOffline)
		_, err := OpenDevice(ctx, o)
		var w *machine.Waiting
		if !errors.As(err, &w) || w.Reason != machine.WaitOffline || w.Environment != laptop || stub.Count(cellastub.OpCreate) != 0 {
			t.Fatalf("an offline computer: %v", err)
		}
		stub.SetEnvironmentPhase(laptop, v1.EnvironmentReady)
		stub.Disconnect(laptop)
		if _, err := OpenDevice(ctx, o); !errors.As(err, &w) || w.Reason != machine.WaitOffline {
			t.Fatalf("a computer whose worker does not answer: %v", err)
		}
	})
	t.Run("approval", func(t *testing.T) {
		stub := cellastub.New(t)
		o, _ := deviceOptions(t, stub, platformKey)
		stub.HoldCreates(laptop, true)
		o.ApprovalWait = 200 * time.Millisecond
		var mu sync.Mutex
		var told []machine.Waiting
		wctx := machine.WithWaiting(ctx, func(_ context.Context, w machine.Waiting) error {
			mu.Lock()
			defer mu.Unlock()
			told = append(told, w)
			return nil
		})
		_, err := OpenDevice(wctx, o)
		var w *machine.Waiting
		if !errors.As(err, &w) || w.Reason != machine.WaitApproval {
			t.Fatalf("a held create: %v", err)
		}
		mu.Lock()
		if len(told) != 1 || told[0].Reason != machine.WaitApproval || told[0].Environment != laptop {
			t.Fatalf("the session was told %+v", told)
		}
		mu.Unlock()
		name := SandboxName(o.Session)
		if sb, ok := stub.Sandbox(name); !ok || sb.Status.Phase != "Pending" {
			t.Fatalf("the held create was not left waiting: %+v", sb.Status)
		}
		stub.Admit(name)
		d, err := OpenDevice(ctx, o)
		if err != nil {
			t.Fatalf("the open after the owner allowed it: %v", err)
		}
		if n := stub.Count(cellastub.OpCreate); n != 1 {
			t.Fatalf("%d creates, want the held one taken", n)
		}
		if err := d.Release(ctx, true); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("refused", func(t *testing.T) {
		for _, timeout := range []bool{false, true} {
			stub := cellastub.New(t)
			o, _ := deviceOptions(t, stub, platformKey)
			stub.HoldCreates(laptop, true)
			name := SandboxName(o.Session)
			go func() {
				for range 400 {
					if sb, ok := stub.Sandbox(name); ok && sb.Status.Phase == "Pending" {
						if timeout {
							stub.TimeoutHost(name)
						} else {
							stub.RefuseCreate(name, "not while I am presenting")
						}
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}()
			_, err := OpenDevice(ctx, o)
			var r *machine.Refused
			if !errors.As(err, &r) || r.Timeout != timeout || r.Environment != laptop || (!timeout && r.Detail != "not while I am presenting") {
				t.Fatalf("a refused create (timeout %t): %v", timeout, err)
			}
			if _, ok := stub.Sandbox(name); ok {
				t.Fatal("the refused sandbox was kept, so the next open would not ask again")
			}
		}
	})
	t.Run("lost under a command", func(t *testing.T) {
		dv := openDevice(t)
		started := make(chan struct{})
		go func() {
			<-started
			time.Sleep(300 * time.Millisecond)
			dv.stub.Disconnect(laptop)
		}()
		close(started)
		res, err := dv.d.Exec(ctx, machine.ExecRequest{Command: "sleep 5"})
		if err != nil || res.Stopped == nil || !res.Stopped.Lost {
			t.Fatalf("a command whose computer went away: %+v, %v", res, err)
		}
		if w := dv.d.Absent(); w == nil || w.Reason != machine.WaitOffline || w.Environment != laptop {
			t.Fatalf("the device after it went away: %+v", w)
		}
		if _, err := dv.d.ReadFile(ctx, "anything"); !errors.Is(err, machine.ErrLost) {
			t.Fatalf("a read after the computer went away: %v", err)
		}
		dv.stub.Reconnect(laptop)
	})
	t.Run("a call that finds it gone", func(t *testing.T) {
		dv := openDevice(t)
		dv.stub.Disconnect(laptop)
		if _, err := dv.d.Stat(ctx, "anything"); !errors.Is(err, machine.ErrLost) {
			t.Fatalf("a stat on a computer that does not answer: %v", err)
		}
		if w := dv.d.Absent(); w == nil || w.Reason != machine.WaitOffline {
			t.Fatalf("the device: %+v", w)
		}
		dv.stub.Reconnect(laptop)
	})
}

// TestADevicesAttachmentsStayOutOfTheFolder: a message's file, which the
// session records at attachments/<event id>/<name>, is written to and
// read from the scratch directory, never the person's folder; a path of
// the folder's own under attachments/ stays the folder's; and the
// session's end deletes the sandbox and leaves the folder as the session
// last wrote it (spec 080).
func TestADevicesAttachmentsStayOutOfTheFolder(t *testing.T) {
	stub := cellastub.New(t)
	o, folder := deviceOptions(t, stub, platformKey)
	d, err := OpenDevice(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	const att = "attachments/evt_01JAQ7V3K9M2/report.csv"
	if err := d.WriteFile(ctx, att, strings.NewReader("a,b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(folder, "attachments")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a message's file reached the person's folder: %v", err)
	}
	want := path.Join(path.Dir(d.SpillDir()), att)
	if b, err := os.ReadFile(filepath.FromSlash(want)); err != nil || string(b) != "a,b\n" {
		t.Fatalf("the file at %s: %q, %v", want, b, err)
	}
	rc, err := d.ReadFile(ctx, att)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	if err := errors.Join(err, rc.Close()); err != nil || string(b) != "a,b\n" {
		t.Fatalf("read back: %q, %v", b, err)
	}
	if err := d.WriteFile(ctx, "attachments/design.md", strings.NewReader("mine\n"), 0); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(folder, "attachments", "design.md")); err != nil || string(b) != "mine\n" {
		t.Fatalf("the folder's own attachments directory: %q, %v", b, err)
	}
	if err := d.Release(ctx, true); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(folder, "attachments", "design.md")); err != nil || string(b) != "mine\n" {
		t.Fatalf("the session's end touched the folder: %q, %v", b, err)
	}
	if _, ok := stub.Sandbox(SandboxName(o.Session)); ok {
		t.Fatal("the session's end kept the sandbox")
	}
}
