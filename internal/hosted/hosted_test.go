// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/host"
	"latere.ai/x/topos/manifest"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

const reviewer = `apiVersion: topos.latere.ai/v1
kind: Agent
metadata:
  name: reviewer
spec:
  model: {name: anthropic/claude-haiku-4.5}
  instructions: Review.
  tools: [read, grep]
  approvals: {mode: plan}
  machine: {kind: cella, image: base}
`

// session creates a hosted session of the manifest in st.
func newSession(t *testing.T, st session.Store, doc string) session.Session {
	t.Helper()
	rs, err := manifest.Resolve(t.Context(), []byte(doc), manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ref, blobs, err := runner.AgentRef(rs[0])
	if err != nil {
		t.Fatal(err)
	}
	s := session.New(ref, session.Sender{Subject: "u", Kind: session.SenderPerson}, session.RunnerHosted, session.Machine{Kind: session.MachineCella}, time.Now())
	s.Limits.TurnTimeout = "5m0s"
	if err := st.Create(t.Context(), s, blobs); err != nil {
		t.Fatal(err)
	}
	return s
}

// hostMachines opens a host machine in a temporary directory and records
// what it was asked for.
func hostMachines(t *testing.T, asked *v1.Machine) Machines {
	return func(ctx context.Context, s session.Session, m v1.Machine) (machine.Machine, error) {
		*asked = m
		dir := t.TempDir()
		return host.Open(host.Options{Workdir: dir, SpillDir: filepath.Join(dir, ".spill")})
	}
}

func code(t *testing.T, err error) string {
	t.Helper()
	se, ok := errors.AsType[*runner.SetupError](err)
	if !ok {
		t.Fatalf("%v is not a setup error", err)
	}
	return se.Code
}

func TestTheHarnessOfAHostedSession(t *testing.T) {
	st := session.NewMemoryStore()
	s := newSession(t, st, reviewer)
	var asked v1.Machine
	h, err := Harness(Options{Store: st, ModelsURL: "https://lux.example/anthropic", ModelsKey: "k", Machines: hostMachines(t, &asked)})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := h(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cfg.Machine.Release(context.Background(), true) })
	var names []string
	for _, d := range cfg.Tools.Definitions() {
		names = append(names, d.Name)
	}
	slices.Sort(names)
	if cfg.Connection.BaseURL != "https://lux.example/anthropic" || cfg.Connection.Credential != "k" || cfg.Connection.Model != "anthropic/claude-haiku-4.5" ||
		cfg.Connection.Family != models.FamilyAnthropic || cfg.Entry.InputWindow == 0 || cfg.Policy.Mode != harness.ModePlan ||
		cfg.Instructions != "Review." || cfg.TurnTimeout != 5*time.Minute || !slices.Equal(names, []string{"grep", "read"}) || asked.Image != "base" || cfg.Prompt.Host {
		t.Fatalf("config %+v tools %v machine %+v", cfg.Connection, names, asked)
	}
}

func TestTheSetupFailuresOfAHostedSession(t *testing.T) {
	st := session.NewMemoryStore()
	s := newSession(t, st, reviewer)
	var asked v1.Machine
	good := Options{Store: st, ModelsURL: "https://lux.example/anthropic", ModelsKey: "k", Machines: hostMachines(t, &asked)}
	for want, mut := range map[string]func(*Options, *session.Session){
		CodeAgentMissing:           func(_ *Options, s *session.Session) { s.Agent.Bundle = "" },
		CodeModelUnavailable:       func(o *Options, _ *session.Session) { o.ModelsURL = "" },
		CodeModelCredentialMissing: func(o *Options, _ *session.Session) { o.ModelsKey = "" },
		CodeMachineUnavailable: func(o *Options, _ *session.Session) {
			o.Machines = func(context.Context, session.Session, v1.Machine) (machine.Machine, error) {
				return nil, errors.New("no sandbox")
			}
		},
		"quota": func(o *Options, _ *session.Session) {
			o.Machines = func(context.Context, session.Session, v1.Machine) (machine.Machine, error) {
				return nil, &runner.SetupError{Code: "quota", Err: errors.New("no room")}
			}
		},
	} {
		o, sc := good, s
		mut(&o, &sc)
		h, err := Harness(o)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h(t.Context(), sc); code(t, err) != want {
			t.Errorf("%s: %v", want, err)
		}
	}
	missing := s
	missing.Agent.Bundle = session.DigestOf([]byte("gone"))
	h, err := Harness(good)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h(t.Context(), missing); code(t, err) != CodeAgentMissing {
		t.Fatalf("a bundle the store lacks: %v", err)
	}
	unknown := newSession(t, st, `apiVersion: topos.latere.ai/v1
kind: Agent
metadata:
  name: odd
spec:
  model: {name: vendor/no-such-model}
  machine: {kind: cella}
`)
	if _, err := h(t.Context(), unknown); err == nil {
		t.Fatal("an unknown model configured")
	} else if c, ok := errors.AsType[*models.Coded](err); !ok || c.Code != models.CodeUnknown {
		t.Fatalf("an unknown model: %v", err)
	}
	b := builder{o: good}
	if _, _, err := b.connect(v1.AgentModel{Name: "anthropic/claude-haiku-4.5", Credential: "cred_x"}, models.Entry{}); code(t, err) != CodeModelCredentialMissing {
		t.Fatalf("a credential the agent names: %v", err)
	}
	for _, o := range []Options{{Machines: good.Machines}, {Store: st}} {
		if _, err := Harness(o); err == nil {
			t.Fatalf("built with %+v", o)
		}
	}
}

func TestCellaMachinesNeedAURL(t *testing.T) {
	if _, err := Cella(CellaOptions{})(t.Context(), session.Session{}, v1.Machine{}); code(t, err) != CodeMachineUnavailable {
		t.Fatalf("no URL: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	s := session.Session{ID: session.NewID(session.PrefixSession), Agent: session.AgentRef{Name: "reviewer"}, ExpiresAt: time.Now().Add(time.Hour)}
	if _, err := Cella(CellaOptions{URL: "http://127.0.0.1:1"})(ctx, s, v1.Machine{Image: "base"}); err == nil {
		t.Fatal("opened a machine on a Cella that does not answer")
	}
}

func TestReadHelpers(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadHelpers(dir); err == nil {
		t.Fatal("read helpers from an empty directory")
	}
	for name, body := range map[string]string{"topos-machine-linux-arm64": "arm", "topos-machine-darwin-arm64": "mac", "topos-machine-odd": "x", "README": "r"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadHelpers(dir)
	if err != nil || string(got["linux/arm64"]) != "arm" || string(got["darwin/arm64"]) != "mac" || len(got) != 2 {
		t.Fatalf("helpers %v, %v", got, err)
	}
	if err := os.Mkdir(filepath.Join(dir, "topos-machine-linux-amd64"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHelpers(dir); err == nil {
		t.Fatal("read a directory as a helper")
	}
	if _, err := ReadHelpers("["); err == nil {
		t.Fatal("read helpers from a malformed pattern")
	}
}
