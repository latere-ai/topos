// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/client"
	v1 "latere.ai/x/cella/manifest/v1"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/cellastub"
)

// fixture is a Cella machine on the stub.
type fixture struct {
	stub *cellastub.Server
	m    *Machine
	o    Options
}

// platformKey is the platform the stub runs commands on.
var platformKey = runtime.GOOS + "/" + runtime.GOARCH

// options are a machine's options on a stub, with a fresh session and the
// helper built for the machine the tests run on.
func options(t *testing.T, stub *cellastub.Server) Options {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return Options{
		URL:          stub.URL(),
		Token:        client.StaticToken("session-token"),
		Session:      session.NewID(session.PrefixSession),
		Agent:        "coder",
		Helpers:      map[string][]byte{platformKey: helperBin},
		Dir:          filepath.ToSlash(dir),
		PollInterval: 5 * time.Millisecond,
		StartTimeout: 10 * time.Second,
	}
}

// open opens a machine on a new stub.
func open(t *testing.T, edit ...func(*cellastub.Server, *Options)) fixture {
	t.Helper()
	stub := cellastub.New(t)
	o := options(t, stub)
	for _, e := range edit {
		e(stub, &o)
	}
	m, err := Open(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Release(context.Background(), true); err != nil {
			t.Error(err)
		}
	})
	return fixture{stub: stub, m: m, o: o}
}

// ws is the sandbox's workspace.
func (f fixture) ws() string { return f.m.Info().Workdir }

func TestOpenCreatesTheSandbox(t *testing.T) {
	f := open(t, func(stub *cellastub.Server, o *Options) {
		stub.RequireToken("session-token")
		stub.AddEnvironment("my-workers")
		stub.AddSecret("gh", "api.github.com", "GitHub.com.")
		o.Environment = "my-workers"
		o.Image = "golang"
		o.Resources = v1.Resources{CPU: "2", Memory: "4Gi"}
		o.Egress = []string{"proxy.golang.org", "github.com", " "}
		o.Secrets = []v1.SecretMount{{Name: "gh", Env: "GITHUB_TOKEN"}}
		o.Env = map[string]string{"LUX_URL": "https://lux.example/v1/models"}
		o.Labels = map[string]string{"tenant.example/id": "t-1", LabelSession: "not-the-session"}
		o.TTL = 2 * time.Hour
	})
	if !f.m.Created() {
		t.Error("a new sandbox is not reported created")
	}
	name := "ses-" + strings.ToLower(strings.TrimPrefix(f.o.Session, "ses_"))
	if f.m.Name() != name || SandboxName(f.o.Session) != name {
		t.Errorf("name %s, want %s", f.m.Name(), name)
	}
	sb, ok := f.stub.Sandbox(name)
	if !ok {
		t.Fatalf("no sandbox named %s", name)
	}
	// The installation's labels join the session's, which win a key they share.
	if sb.Metadata.Labels[LabelSession] != f.o.Session || sb.Metadata.Labels[LabelAgent] != "coder" || sb.Metadata.Labels["tenant.example/id"] != "t-1" || len(sb.Metadata.Labels) != 3 {
		t.Errorf("labels = %v", sb.Metadata.Labels)
	}
	if f.o.Labels[LabelSession] != "not-the-session" {
		t.Error("the installation's labels were changed in place")
	}
	spec := sb.Spec
	if spec.Environment != "my-workers" || spec.Image != "golang" || spec.Resources.CPU != "2" || spec.Resources.Memory != "4Gi" {
		t.Errorf("spec = %+v", spec)
	}
	egress := spec.Network.Egress
	if egress.Mode != v1.EgressAllowlist || strings.Join(egress.AllowedHosts, ",") != "api.github.com,github.com,proxy.golang.org" {
		t.Errorf("egress = %+v: the agent's hosts and the secret's, sorted and once each", egress)
	}
	if len(spec.Secrets) != 1 || spec.Secrets[0] != (v1.SecretMount{Name: "gh", Env: "GITHUB_TOKEN"}) {
		t.Errorf("secrets = %+v", spec.Secrets)
	}
	// HOME is the machine's own writable directory: the image's home sits
	// on Cella's read-only root filesystem, where git config --global fails.
	if len(spec.Env) != 2 || spec.Env["LUX_URL"] != "https://lux.example/v1/models" || spec.Env["HOME"] != f.o.Dir+"/home" {
		t.Errorf("env = %+v", spec.Env)
	}
	if st, err := os.Stat(filepath.Join(f.o.Dir, "home")); err != nil || !st.IsDir() {
		t.Errorf("the home directory was not made: %v", err)
	}
	if _, set := f.o.Env["HOME"]; set {
		t.Error("the options' environment was changed in place")
	}
	if spec.Lifecycle != (v1.Lifecycle{AutoStop: "900s", TTL: "7200s", AutoDelete: v1.DurationNever}) {
		t.Errorf("lifecycle = %+v", spec.Lifecycle)
	}
	info := f.m.Info()
	if info.Kind != machine.KindCella || info.ID != sb.Status.ID || info.Workdir != sb.Spec.Workspace.Path ||
		info.OS != runtime.GOOS || info.Arch != runtime.GOARCH || info.Environment != "my-workers" {
		t.Errorf("info = %+v", info)
	}
	if got := strings.Join(f.m.Roots(), ","); got != info.Workdir+","+f.o.Dir+"/spill" {
		t.Errorf("roots = %s", got)
	}
	if f.m.SpillDir() != f.o.Dir+"/spill" {
		t.Errorf("spill = %s", f.m.SpillDir())
	}
	b, err := os.ReadFile(filepath.Join(f.o.Dir, "bin", "topos-machine"))
	if err != nil || string(b) != string(helperBin) {
		t.Errorf("the helper was not uploaded: %v", err)
	}
	for _, r := range f.stub.Requests() {
		if r.Header.Get("Authorization") != "Bearer session-token" {
			t.Errorf("%s %s carried %q", r.Method, r.Path, r.Header.Get("Authorization"))
		}
	}
}

func TestAHomeTheOptionsNameIsKept(t *testing.T) {
	f := open(t, func(_ *cellastub.Server, o *Options) {
		o.Env = map[string]string{"HOME": "/workspace/.home"}
	})
	sb, ok := f.stub.Sandbox(f.m.Name())
	if !ok {
		t.Fatal("no sandbox")
	}
	if len(sb.Spec.Env) != 1 || sb.Spec.Env["HOME"] != "/workspace/.home" {
		t.Errorf("env = %+v, want the options' HOME alone", sb.Spec.Env)
	}
}

func TestTheLifecycleStaysInsideTheTTL(t *testing.T) {
	f := open(t, func(_ *cellastub.Server, o *Options) { o.TTL = 90 * time.Second; o.Agent = ""; o.Workdir = "" })
	sb, _ := f.stub.Sandbox(f.m.Name())
	if sb.Spec.Lifecycle.AutoStop != "90s" || sb.Spec.Lifecycle.TTL != "90s" {
		t.Errorf("lifecycle = %+v: the idle stop is held inside the ttl", sb.Spec.Lifecycle)
	}
	if _, ok := sb.Metadata.Labels[LabelAgent]; ok {
		t.Errorf("labels = %v: no agent names no agent label", sb.Metadata.Labels)
	}
	if sb.Spec.Network.Egress.Mode != v1.EgressAllowlist || len(sb.Spec.Network.Egress.AllowedHosts) != 0 {
		t.Errorf("egress = %+v: an allowlist of nothing", sb.Spec.Network.Egress)
	}
}

func TestOpenFindsTheSandboxByName(t *testing.T) {
	f := open(t)
	id := f.m.Info().ID
	if err := os.WriteFile(filepath.Join(f.ws(), "kept.txt"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	uploads := f.stub.Count(cellastub.OpSession)
	creates := f.stub.Count(cellastub.OpCreate)

	// A runner that claims the session later finds the sandbox by name.
	again, err := Open(t.Context(), f.o)
	if err != nil {
		t.Fatal(err)
	}
	if again.Created() || again.Info().ID != id {
		t.Errorf("found %s created %v, want %s", again.Info().ID, again.Created(), id)
	}
	if f.stub.Count(cellastub.OpCreate) != creates || f.stub.Count(cellastub.OpSession) != uploads {
		t.Error("finding the sandbox created one or uploaded the helper again")
	}

	// One Cella stopped for idleness is started, and one starting is
	// waited for.
	f.stub.Stop(id)
	f.stub.StartAfter(3)
	again, err = Open(t.Context(), f.o)
	if err != nil {
		t.Fatal(err)
	}
	if sb, _ := f.stub.Sandbox(id); sb.Status.Phase != "Running" || again.Created() {
		t.Errorf("a stopped sandbox is %s, created %v", sb.Status.Phase, again.Created())
	}
	rc, err := again.ReadFile(t.Context(), "kept.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, rerr := io.ReadAll(rc)
	if err := errors.Join(rerr, rc.Close()); err != nil || string(b) != "kept" {
		t.Errorf("the workspace after the stop = %q %v", b, err)
	}
	f.stub.StartAfter(0)

	// A failed sandbox cannot be started; it is replaced, and reported
	// created, so the runner restores its files.
	f.stub.SetFailed(id, "OOMKilled")
	again, err = Open(t.Context(), f.o)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Created() || again.Info().ID == id {
		t.Errorf("a failed sandbox: %s created %v", again.Info().ID, again.Created())
	}
	if err := again.Release(t.Context(), true); err != nil {
		t.Error(err)
	}
}

func TestOpenRefusals(t *testing.T) {
	stub := cellastub.New(t)
	base := options(t, stub)
	for name, c := range map[string]struct {
		edit        func(*cellastub.Server, *Options)
		unavailable bool
	}{
		"no URL":                 {func(_ *cellastub.Server, o *Options) { o.URL = "" }, true},
		"a bad URL":              {func(_ *cellastub.Server, o *Options) { o.URL = "ftp://cella" }, true},
		"no helper":              {func(_ *cellastub.Server, o *Options) { o.Helpers = nil }, true},
		"no build":               {func(_ *cellastub.Server, o *Options) { o.Helpers = map[string][]byte{"plan9/mips": {1}} }, true},
		"a bad session":          {func(_ *cellastub.Server, o *Options) { o.Session = "ses_nope" }, false},
		"a relative dir":         {func(_ *cellastub.Server, o *Options) { o.Dir = "tmp/topos" }, false},
		"an unknown environment": {func(_ *cellastub.Server, o *Options) { o.Environment = "elsewhere" }, true},
		"a refused image": {func(s *cellastub.Server, o *Options) {
			s.Fail(cellastub.OpCreate, cellastub.Failure{Status: 422, Code: "admission_refused", Detail: "the image is not allowed"})
		}, true},
		"an unknown secret": {func(_ *cellastub.Server, o *Options) { o.Secrets = []v1.SecretMount{{Name: "gone", Env: "X"}} }, true},
		"a wrong token": {func(s *cellastub.Server, o *Options) {
			s.RequireToken("other")
		}, true},
		"a sandbox that never starts": {func(s *cellastub.Server, o *Options) {
			s.StartAfter(1 << 20)
			o.StartTimeout = 50 * time.Millisecond
		}, true},
		"a refused probe": {func(s *cellastub.Server, o *Options) {
			s.Fail(cellastub.OpExec, cellastub.Failure{Status: 422, Code: "capability_unsupported"})
		}, true},
		"a refused upload": {func(s *cellastub.Server, o *Options) {
			s.Fail(cellastub.OpSession, cellastub.Failure{Status: 422, Code: "capability_unsupported"})
		}, true},
		"an unreadable name": {func(s *cellastub.Server, o *Options) {
			s.Fail(cellastub.OpGet, cellastub.Failure{Status: 403, Code: "forbidden"})
		}, true},
		"a driver that is down": {func(s *cellastub.Server, o *Options) {
			s.Fail(cellastub.OpCreate, cellastub.Failure{Status: 503, Code: "driver_unavailable"})
		}, false},
		"a rate limit": {func(s *cellastub.Server, o *Options) {
			s.Fail(cellastub.OpCreate, cellastub.Failure{Status: 429, Code: "rate_limited"})
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			o := base
			o.Session = session.NewID(session.PrefixSession)
			c.edit(stub, &o)
			defer func() {
				stub.RequireToken("")
				stub.StartAfter(0)
			}()
			m, err := Open(t.Context(), o)
			if err == nil {
				t.Fatalf("opened %s", m.Name())
			}
			if got := Code(err) == machine.CodeUnavailable; got != c.unavailable {
				t.Errorf("%v: machine_unavailable %v, want %v", err, got, c.unavailable)
			}
		})
	}
}

func TestCode(t *testing.T) {
	if Code(ErrLost) != machine.CodeLost || Code(ErrUnavailable) != machine.CodeUnavailable || Code(errors.New("x")) != "" {
		t.Error("Code does not name the codes")
	}
	ce := &client.Error{Status: 422, Code: "admission_refused", Message: "The request was refused.", Detail: "the image"}
	err := refused("create", ce)
	if Code(err) != machine.CodeUnavailable || !errors.Is(err, ce) || !strings.Contains(err.Error(), "admission_refused: the image") {
		t.Errorf("refused = %v", err)
	}
	if err := refused("create", &client.Error{Status: 400, Message: "Bad."}); !strings.HasSuffix(err.Error(), ": Bad.") {
		t.Errorf("a refusal without a code = %v", err)
	}
	for _, status := range []int{429, 500, 503} {
		if Code(refused("create", &client.Error{Status: status})) != "" {
			t.Errorf("a %d is read as a refusal", status)
		}
	}
	if Code(refused("create", &client.Unreachable{Op: "GET /", Err: errors.New("down")})) != "" {
		t.Error("an unreachable Cella is read as a refusal")
	}
}

func TestRelease(t *testing.T) {
	f := open(t)
	if err := f.m.Release(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if sb, ok := f.stub.Sandbox(f.m.Name()); !ok || sb.Status.Phase != "Running" {
		t.Errorf("an idle release left %+v %v: the sandbox is Cella's to stop", sb.Status, ok)
	}
	if _, err := f.m.Exec(t.Context(), machine.ExecRequest{Command: "true"}); err != nil {
		t.Errorf("the machine after an idle release: %v", err)
	}
	f.stub.Fail(cellastub.OpDelete, cellastub.Failure{Status: http.StatusServiceUnavailable, Code: "driver_unavailable"})
	if err := f.m.Release(t.Context(), true); err == nil {
		t.Error("a delete Cella could not do was reported done")
	}
	if err := f.m.Release(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.stub.Sandbox(f.m.Name()); ok {
		t.Error("the session's end left its sandbox")
	}
	if err := f.m.Release(t.Context(), true); err != nil {
		t.Errorf("a second release: %v", err)
	}
	for name, err := range map[string]error{
		"exec":     execErr(t, f.m),
		"stat":     statErr(t, f.m, "x"),
		"search":   searchErr(t, f.m),
		"recreate": f.m.Recreate(t.Context()),
	} {
		if !errors.Is(err, machine.ErrReleased) {
			t.Errorf("%s after the release: %v", name, err)
		}
	}
}

func execErr(t *testing.T, m *Machine) error {
	t.Helper()
	_, err := m.Exec(t.Context(), machine.ExecRequest{Command: "true"})
	return err
}

func statErr(t *testing.T, m *Machine, p string) error {
	t.Helper()
	_, err := m.Stat(t.Context(), p)
	return err
}

func searchErr(t *testing.T, m *Machine) error {
	t.Helper()
	_, err := m.Search(t.Context(), machine.SearchRequest{Kind: machine.SearchGlob, Pattern: "*"})
	return err
}

func TestALostSandboxIsRecreated(t *testing.T) {
	f := open(t)
	id := f.m.Info().ID
	if err := f.m.WriteFile(t.Context(), "work.txt", strings.NewReader("work"), 0); err != nil {
		t.Fatal(err)
	}
	f.stub.Remove(id)
	_, err := f.m.Exec(t.Context(), machine.ExecRequest{Command: "true"})
	if !errors.Is(err, ErrLost) || Code(err) != machine.CodeLost {
		t.Fatalf("exec on a lost sandbox: %v", err)
	}
	for name, err := range map[string]error{
		"stat":   statErr(t, f.m, "work.txt"),
		"search": searchErr(t, f.m),
	} {
		if Code(err) != machine.CodeLost {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := f.m.Recreate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !f.m.Created() || f.m.Info().ID == id {
		t.Errorf("recreated %s, created %v", f.m.Info().ID, f.m.Created())
	}
	if _, err := f.m.Stat(t.Context(), "work.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a recreated sandbox has the old files: %v", err)
	}
	if _, err := f.m.Exec(t.Context(), machine.ExecRequest{Command: "true"}); err != nil {
		t.Errorf("exec after the recreate: %v", err)
	}
	// A loss found by a file route is a loss too.
	f.stub.Remove(f.m.Info().ID)
	if _, err := f.m.Stat(t.Context(), "anything"); Code(err) != machine.CodeLost {
		t.Errorf("stat on a lost sandbox: %v", err)
	}
	f.stub.Fail(cellastub.OpCreate, cellastub.Failure{Status: 422, Code: "quota_exceeded"})
	if err := f.m.Recreate(t.Context()); Code(err) != machine.CodeUnavailable {
		t.Errorf("a recreate Cella refused: %v", err)
	}
	if err := f.m.Recreate(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestAStoppedSandboxIsStartedByTheNextCall(t *testing.T) {
	f := open(t)
	id := f.m.Info().ID
	for name, call := range map[string]func() error{
		"exec":   func() error { return execErr(t, f.m) },
		"stat":   func() error { return statErr(t, f.m, ".") },
		"search": func() error { return searchErr(t, f.m) },
		"write":  func() error { return f.m.WriteFile(t.Context(), "w.txt", strings.NewReader("w"), 0) },
		"job": func() error {
			_, err := f.m.Exec(t.Context(), machine.ExecRequest{Command: "true", Background: true})
			return err
		},
	} {
		f.stub.Stop(id)
		f.stub.StartAfter(2)
		if err := call(); err != nil {
			t.Errorf("%s on a stopped sandbox: %v", name, err)
		}
		if sb, _ := f.stub.Sandbox(id); sb.Status.Phase != "Running" {
			t.Errorf("%s left the sandbox %s", name, sb.Status.Phase)
		}
	}
	f.stub.StartAfter(0)
	// A sandbox deleted while it is being woken is lost.
	f.stub.Stop(id)
	f.stub.Fail(cellastub.OpStart, cellastub.Failure{Status: 404, Code: "not_found"})
	if err := execErr(t, f.m); Code(err) != machine.CodeLost {
		t.Errorf("a sandbox gone while it was started: %v", err)
	}
}

func TestTheHelperIsPutBackWhenMissing(t *testing.T) {
	f := open(t)
	helper := filepath.Join(f.o.Dir, "bin", "topos-machine")
	for name, call := range map[string]func() error{
		"exec":   func() error { return execErr(t, f.m) },
		"search": func() error { return searchErr(t, f.m) },
		"spill":  func() error { return statErr(t, f.m, f.m.SpillDir()) },
		"read": func() error {
			rc, err := f.m.ReadFile(t.Context(), f.m.SpillDir()+"/none")
			if err == nil {
				return rc.Close()
			}
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		},
		"write": func() error { return f.m.WriteFile(t.Context(), f.m.SpillDir()+"/w", strings.NewReader("w"), 0) },
	} {
		if err := os.Remove(helper); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(f.m.SpillDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := call(); err != nil {
			t.Errorf("%s with the helper gone: %v", name, err)
		}
		if _, err := os.Stat(helper); err != nil {
			t.Errorf("%s did not put the helper back: %v", name, err)
		}
	}
}
