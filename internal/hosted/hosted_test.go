// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/client"
	"latere.ai/x/pkg/llmdialect/bridge"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/host"
	"latere.ai/x/topos/manifest"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
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

// newSession creates a hosted session of the manifest in st, on the
// machine kind its agent names.
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
	s := session.New(ref, session.Sender{Subject: "u", Kind: session.SenderPerson}, session.RunnerHosted, session.Machine{Kind: rs[0].Agent.Spec.Machine.Kind}, time.Now())
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
	door := luxstub.New(t).URL() + "/anthropic"
	h, err := Harness(Options{Store: st, ModelsURL: door, ModelsKey: "k", Machines: hostMachines(t, &asked)})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := h(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cfg.Machine.Release(context.Background(), true) })
	if asked.Image != "" {
		t.Fatal("a Cella machine was opened before a tool needed it")
	}
	if err := machine.Open(t.Context(), cfg.Machine); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range cfg.Tools.Definitions() {
		names = append(names, d.Name)
	}
	slices.Sort(names)
	if cfg.Connection.BaseURL != door || cfg.Connection.Credential != "k" || cfg.Connection.Model != "anthropic/claude-haiku-4.5" ||
		cfg.Connection.Family != models.FamilyAnthropic || cfg.Entry.InputWindow == 0 || cfg.Policy.Mode != harness.ModePlan ||
		cfg.Instructions != "Review." || cfg.TurnTimeout != 5*time.Minute || !slices.Equal(names, []string{"grep", "read"}) || asked.Image != "base" || cfg.Prompt.Host {
		t.Fatalf("config %+v tools %v machine %+v", cfg.Connection, names, asked)
	}
}

func TestTheSetupFailuresOfAHostedSession(t *testing.T) {
	st := session.NewMemoryStore()
	s := newSession(t, st, reviewer)
	var asked v1.Machine
	good := Options{Store: st, ModelsURL: luxstub.New(t).URL() + "/anthropic", ModelsKey: "k", Machines: hostMachines(t, &asked)}
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
		cfg, err := h(t.Context(), sc)
		if err == nil {
			// A Cella machine is opened on demand, and its failure is
			// the open's, with the code the setup error named.
			oe, ok := errors.AsType[*machine.OpenError](machine.Open(t.Context(), cfg.Machine))
			if !ok || oe.Code != want {
				t.Errorf("%s: the open answered %v", want, oe)
			}
			continue
		}
		if code(t, err) != want {
			t.Errorf("%s: %v", want, err)
		}
	}
	host := newSession(t, st, `apiVersion: topos.latere.ai/v1
kind: Agent
metadata:
  name: local
spec:
  model: {name: anthropic/claude-haiku-4.5}
  machine: {kind: host}
`)
	down := good
	down.Machines = func(context.Context, session.Session, v1.Machine) (machine.Machine, error) {
		return nil, errors.New("no host")
	}
	hd, err := Harness(down)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hd(t.Context(), host); code(t, err) != CodeMachineUnavailable {
		t.Fatalf("a host machine is opened with the harness: %v", err)
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
	if _, _, _, err := b.connect(t.Context(), v1.AgentModel{Name: "anthropic/claude-haiku-4.5", Credential: "cred_x"}, models.Entry{}); code(t, err) != CodeModelCredentialMissing {
		t.Fatalf("a credential the agent names: %v", err)
	}
	for _, o := range []Options{{Machines: good.Machines}, {Store: st}} {
		if _, err := Harness(o); err == nil {
			t.Fatalf("built with %+v", o)
		}
	}
}

// TestLuxServedFiguresOverlayTheCatalog: the figures a Lux door serves
// for the agent's model replace the embedded catalog's, and the agent's
// own replace both; a model only the door knows runs on the door's
// figures, a door that does not answer is model_unavailable, and a
// provider's own API is not asked.
func TestLuxServedFiguresOverlayTheCatalog(t *testing.T) {
	st := session.NewMemoryStore()
	stub := luxstub.New(t)
	stub.Models(
		bridge.Model{Name: "anthropic/claude-haiku-4.5", ContextWindow: 150_000, MaxOutputTokens: 9_000, Pricing: &bridge.ModelPricing{Currency: "USD", Input: "2", Output: "8"}},
		bridge.Model{Name: "vendor/door-only", ContextWindow: 32_000, MaxOutputTokens: 4_000},
	)
	var asked v1.Machine
	h, err := Harness(Options{Store: st, ModelsURL: stub.URL() + "/anthropic", ModelsKey: "k", Machines: hostMachines(t, &asked)})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := models.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	embedded, ok := cat.Lookup("anthropic/claude-haiku-4.5")
	if !ok || embedded.InputWindow == 150_000 {
		t.Fatalf("the embedded catalog's entry %+v cannot tell the door's figures apart", embedded)
	}
	agent := func(model string) string {
		return strings.Replace(reviewer, "model: {name: anthropic/claude-haiku-4.5}", "model: "+model, 1)
	}
	for name, c := range map[string]struct {
		model          string
		window, output int64
		input          models.Price
	}{
		"the door's":    {"{name: anthropic/claude-haiku-4.5}", 150_000, 9_000, 2_000_000},
		"the agent's":   {"{name: anthropic/claude-haiku-4.5, inputWindow: 100000, pricing: {input: \"3\", output: \"9\"}}", 100_000, 9_000, 3_000_000},
		"the door only": {"{name: vendor/door-only, family: anthropic}", 32_000, 4_000, 0},
	} {
		cfg, err := h(t.Context(), newSession(t, st, agent(c.model)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Cleanup(func() { _ = cfg.Machine.Release(context.Background(), true) })
		e := cfg.Entry
		if e.InputWindow != c.window || e.MaxOutputTokens != c.output || (c.input != 0) != (e.Pricing != nil) || e.Pricing != nil && *e.Pricing.Input != c.input {
			t.Fatalf("%s: %+v %+v", name, e, e.Pricing)
		}
	}
	if len(stub.Listed()) != 3 || stub.Listed()[0].Get("X-Api-Key") != "k" {
		t.Fatalf("the door's list was asked %d times: %v", len(stub.Listed()), stub.Listed())
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	down, err := Harness(Options{Store: st, ModelsURL: gone.URL + "/anthropic", ModelsKey: "k", Machines: hostMachines(t, &asked)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := down(t.Context(), newSession(t, st, reviewer)); code(t, err) != CodeModelUnavailable {
		t.Fatalf("a door that does not answer: %v", err)
	}
	provider, err := Harness(Options{Store: st, ModelsURL: gone.URL, ModelsKey: "k", Machines: hostMachines(t, &asked)})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := provider(t.Context(), newSession(t, st, reviewer))
	if err != nil {
		t.Fatalf("a provider's API was asked for a model list: %v", err)
	}
	t.Cleanup(func() { _ = cfg.Machine.Release(context.Background(), true) })
	if cfg.Entry.InputWindow != embedded.InputWindow {
		t.Fatalf("a provider's API: %+v", cfg.Entry)
	}
}

func TestCellaMachinesNeedAURL(t *testing.T) {
	if _, err := Cella(CellaOptions{})(t.Context(), session.Session{}, v1.Machine{}); code(t, err) != CodeMachineUnavailable {
		t.Fatalf("no URL: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	s := session.Session{ID: session.NewID(session.PrefixSession), Agent: session.AgentRef{Name: "reviewer"}, ExpiresAt: time.Now().Add(time.Hour)}
	if _, err := Cella(CellaOptions{URL: "http://127.0.0.1:1"})(ctx, s, v1.Machine{Image: "base"}); code(t, err) != CodeMachineUnavailable {
		t.Fatalf("no bearer and nothing minted: %v", err)
	}
	if _, err := Cella(CellaOptions{URL: "http://127.0.0.1:1", Token: client.StaticToken("b")})(ctx, s, v1.Machine{Image: "base"}); err == nil {
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
