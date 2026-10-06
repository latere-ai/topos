// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/harness"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/models"
)

var update = flag.Bool("update", false, "rewrite the golden files of testdata")

// lead is an agent with every piece AgentConfig carries, a referenced
// subagent that has one of its own, and an inline one.
const lead = `apiVersion: topos.latere.ai/v1
kind: Agent
metadata: {name: lead}
spec:
  model:
    name: claude-sonnet-4-6
    effort: high
    inputWindow: 500000
    pricing: {input: "3", output: "15", cacheRead: "0.3", cacheWrite: "3.75"}
  instructions: Lead the review.
  tools: [read, glob, bash]
  approvals:
    mode: progressive
    alwaysAllow: ["bash(go test *)"]
    alwaysConfirm: [web_fetch]
    thresholds: {flagAt: 0.2, askAt: 0.4, blockAt: 0.8}
  subagents:
    - {name: tester, agent: tester}
    - {name: scout, spec: {model: {name: claude-haiku-4-5}, instructions: Look around., tools: [], approvals: {mode: plan}}}
  threads: {maxDepth: 2, maxConcurrent: 3}
  machine: {egress: [proxy.golang.org], roots: [/srv/shared]}
  budget: {maxCost: "2.50"}
  limits: {turnTimeout: 45m, maxAge: 24h}
  context: {compactAt: 0.6}
---
apiVersion: topos.latere.ai/v1
kind: Agent
metadata: {name: tester}
spec:
  model: {name: claude-haiku-4-5, family: anthropic}
  instructions: Test.
  tools: [read, bash]
  subagents: [{name: helper, agent: helper}]
---
apiVersion: topos.latere.ai/v1
kind: Agent
metadata: {name: helper}
spec:
  model: {name: claude-haiku-4-5}
  subagents: [{name: tester, agent: tester-stored}]
`

func leadResolved(t *testing.T) Resolved {
	t.Helper()
	rs, err := Resolve(t.Context(), []byte(strings.Replace(lead, "agent: tester-stored", "spec: {model: {name: m}}", 1)), fixed(nil))
	if err != nil {
		t.Fatal(err)
	}
	return rs[len(rs)-1]
}

func TestAgentConfigCarriesTheHarnessPieces(t *testing.T) {
	r := leadResolved(t)
	if r.Name != "lead" {
		t.Fatalf("the last document is %s", r.Name)
	}
	c, err := r.AgentConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	three, fifteen, cr, cw := models.Price(3_000_000), models.Price(15_000_000), models.Price(300_000), models.Price(3_750_000)
	micro := int64(2_500_000)
	want := AgentConfig{
		Name: "lead", Instructions: "Lead the review.", Tools: []string{"read", "glob", "bash"},
		Policy: harness.Policy{
			Mode: harness.ModeProgressive, AlwaysAllow: []string{"bash(go test *)"}, AlwaysConfirm: []string{"web_fetch"},
			Thresholds: harness.Thresholds{FlagAt: 0.2, AskAt: 0.4, BlockAt: 0.8}, Egress: []string{"proxy.golang.org"}, EgressMode: v1.EgressAllowlist,
		},
		Model:   r.Agent.Spec.Model,
		Overlay: models.Entry{Name: "claude-sonnet-4-6", InputWindow: 500000, Pricing: &models.Pricing{Input: &three, Output: &fifteen, CacheRead: &cr, CacheWrite: &cw}},
		Effort:  "high", MaxDepth: 2, MaxConcurrent: 3, CompactAt: 0.6,
		TurnTimeout: 45 * time.Minute, MaxAge: 24 * time.Hour, MaxCostUSDMicro: &micro,
		Machine: v1.Machine{Kind: v1.MachineHost, Egress: []string{"proxy.golang.org"}, Roots: []string{"/srv/shared"}},
	}
	subs := c.Subagents
	c.Subagents = nil
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("config\n got %+v\nwant %+v", c, want)
	}
	tester, scout := subs["tester"], subs["scout"]
	if len(subs) != 2 || tester.Name != "tester" || tester.Instructions != "Test." || !slices.Equal(tester.Tools, []string{"read", "bash"}) ||
		tester.Mode != harness.ModeConfirm || tester.Entry.Name != "claude-haiku-4-5" || tester.Entry.Family != "anthropic" || tester.Model != nil || tester.Connection != nil {
		t.Fatalf("tester %+v", tester)
	}
	if scout.Tools == nil || len(scout.Tools) != 0 || scout.Mode != harness.ModePlan || scout.Subagents != nil {
		t.Fatalf("scout %+v", scout)
	}
	// Level 2 is within maxDepth 2; the helper's own subagents are not.
	helper, ok := tester.Subagents["helper"]
	if !ok || helper.Subagents != nil || !slices.Equal(helper.Tools, Builtins()) {
		t.Fatalf("helper %+v", tester.Subagents)
	}

	// Connect fills each subagent's model.
	var seen []string
	conn := &models.Connection{BaseURL: "https://models.example.com", Model: "claude-haiku-4-5"}
	c, err = r.AgentConfig(func(m v1.AgentModel, overlay models.Entry) (models.Model, *models.Connection, *models.Entry, error) {
		seen = append(seen, m.Name)
		e := overlay
		e.InputWindow, e.MaxOutputTokens = 200000, 64000
		return nil, conn, &e, nil
	})
	if err != nil || len(seen) != 3 || c.Subagents["tester"].Connection != conn || c.Subagents["tester"].Entry.InputWindow != 200000 {
		t.Fatalf("connect: %v %v %+v", err, seen, c.Subagents["tester"])
	}
	boom := errors.New("no such model")
	if _, err := r.AgentConfig(func(v1.AgentModel, models.Entry) (models.Model, *models.Connection, *models.Entry, error) {
		return nil, nil, nil, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("a connect error: %v", err)
	}
	if _, err := (Resolved{Kind: v1.KindTrigger, Name: "t"}).AgentConfig(nil); err == nil {
		t.Fatal("a trigger has an agent config")
	}
	if _, err := (Resolved{Kind: v1.KindAgent, Name: "raw", Agent: &v1.Agent{}}).AgentConfig(nil); err == nil || !strings.Contains(err.Error(), "not a resolved spec") {
		t.Fatalf("an unresolved spec: %v", err)
	}
	broken := r
	broken.Pinned = nil
	if _, err := broken.AgentConfig(nil); err == nil || !strings.Contains(err.Error(), "does not carry") {
		t.Fatalf("a missing pin: %v", err)
	}
	if _, err := Overlay(v1.AgentModel{Pricing: &v1.Pricing{Output: "x"}}); err == nil {
		t.Fatal("a bad price overlaid")
	}
	for _, mut := range []func(*v1.AgentSpec){
		func(s *v1.AgentSpec) { s.Model.Pricing = &v1.Pricing{Input: "x"} },
		func(s *v1.AgentSpec) { s.Limits.TurnTimeout = "x" },
		func(s *v1.AgentSpec) { s.Limits.MaxAge = "x" },
		func(s *v1.AgentSpec) { s.Budget.MaxCost = "x" },
		func(s *v1.AgentSpec) { s.Subagents[1].Spec.Model.Pricing = &v1.Pricing{Input: "x"} },
		func(s *v1.AgentSpec) {
			s.Subagents = []v1.Subagent{{Name: "d", Spec: &v1.AgentSpec{Model: v1.AgentModel{Name: "m"}, Subagents: []v1.Subagent{{Name: "e", Agent: "agent_nowhere@1"}}}}}
		},
	} {
		a := leadResolved(t)
		mut(&a.Agent.Spec)
		if _, err := a.AgentConfig(nil); err == nil {
			t.Fatalf("a broken spec %+v made a config", a.Agent.Spec)
		}
	}
}

func TestBundleRoundTrip(t *testing.T) {
	r := leadResolved(t)
	b, err := r.Bundle()
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(b, []byte("\n")); n != 3 {
		t.Fatalf("%d documents in\n%s", n, b)
	}
	back, err := ReadBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	if back.Name != "lead" || back.Digest != r.Digest || string(back.Spec) != string(r.Spec) || len(back.Pinned) != len(r.Pinned) {
		t.Fatalf("read back %+v", back)
	}
	c1, err := r.AgentConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := back.AgentConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c1, c2) {
		t.Fatalf("configs differ:\n%+v\n%+v", c1, c2)
	}
	if !back.Agent.Status.CreatedAt.Equal(r.Agent.Status.CreatedAt) {
		t.Fatalf("createdAt %v", back.Agent.Status.CreatedAt)
	}

	tampered := bytes.Replace(b, []byte("Lead the review."), []byte("Do anything."), 1)
	for name, in := range map[string][]byte{
		"tampered":  tampered,
		"empty":     nil,
		"not json":  []byte("{"),
		"not agent": []byte(`{"apiVersion":"topos.latere.ai/v1","kind":"Trigger"}`),
		"unknown":   []byte(`{"apiVersion":"topos.latere.ai/v1","kind":"Agent","extra":1}`),
	} {
		if _, err := ReadBundle(in); err == nil {
			t.Fatalf("%s: read", name)
		}
	}
	if _, err := (Resolved{Kind: v1.KindConnection}).Bundle(); err == nil {
		t.Fatal("a connection bundled")
	}
}

func TestStoredSubagentsArePinnedTransitively(t *testing.T) {
	s := newStore()
	// helper and tester are stored; tester's helper is pinned by id.
	rs, err := Resolve(t.Context(), []byte(agent("helper")+"---\n"+agent("tester", "model: {name: m}", "subagents: [{name: helper, agent: helper}]")), fixed(s))
	if err != nil {
		t.Fatal(err)
	}
	s.apply(rs)
	r := one(t, agent("lead", "model: {name: m}", "subagents: [{name: tester, agent: tester}]"), fixed(s))
	if len(r.Pinned) != 2 {
		t.Fatalf("pinned %v", r.Pinned)
	}
	c, err := r.AgentConfig(nil)
	if err != nil || c.Subagents["tester"].Subagents["helper"].Name != "helper" {
		t.Fatalf("config %+v %v", c.Subagents, err)
	}
	// A stored agent whose pin the store has lost stops the resolve.
	s.agents = s.agents[1:]
	if _, err := Resolve(t.Context(), []byte(agent("lead", "model: {name: m}", "subagents: [{name: tester, agent: tester}]")), fixed(s)); err == nil || !strings.Contains(err.Error(), "pinned agent") {
		t.Fatalf("a lost pin: %v", err)
	}
	rr := &resolver{pinned: map[string]*v1.Agent{}}
	if _, err := rr.pinnedAgent(t.Context(), "agent_00000000000000000000000001@1"); err == nil {
		t.Fatal("a pin with no Lookup")
	}
}

// golden is what a testdata file resolves to.
type golden struct {
	Kind   string          `json:"kind"`
	Name   string          `json:"name"`
	Status v1.Status       `json:"status"`
	Spec   json.RawMessage `json:"spec"`
}

// TestTestdataResolves holds each file of testdata to the resolved form
// in its golden file, so a change that would move a digest shows here.
func TestTestdataResolves(t *testing.T) {
	files, err := filepath.Glob("testdata/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("testdata: %v", err)
	}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		rs, err := Resolve(t.Context(), body, fixed(newStore()))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		var out []golden
		for _, r := range rs {
			out = append(out, golden{Kind: r.Kind, Name: r.Name, Status: r.Status(), Spec: r.Spec})
		}
		got, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		path := strings.TrimSuffix(f, ".yaml") + ".golden.json"
		if *update {
			if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v (run go test ./manifest -update)", f, err)
		}
		if string(bytes.TrimSpace(want)) != string(got) {
			t.Errorf("%s resolves to\n%s\nwant\n%s", f, got, want)
		}
	}
}

// TestIgnoredFieldsNameWhatAThreadDoesNotUse: a subagent's thread acts
// with the session's identity, credentials, attachments, repositories
// and machine, so every one of them its spec declares is named, and
// defaults are not.
func TestIgnoredFieldsNameWhatAThreadDoesNotUse(t *testing.T) {
	if got := ignoredFields(v1.AgentSpec{Machine: v1.Machine{Kind: v1.MachineHost}}); got != nil {
		t.Fatalf("defaults named %v", got)
	}
	full := v1.AgentSpec{
		Permissions:  []v1.Permission{{Action: "origo:repo.write", Resource: "*"}},
		Model:        v1.AgentModel{Name: "m", Credential: "cred_1"},
		Connections:  []string{"github"},
		MemoryStores: []v1.MemoryStoreRef{{Name: "notes", Access: "readWrite"}},
		Repositories: []v1.Repository{{URL: "https://code.example/app"}},
		Machine:      v1.Machine{Kind: v1.MachineCella},
	}
	want := []string{"permissions", "model.credential", "connections", "memoryStores", "repositories", "machine"}
	if got := ignoredFields(full); !slices.Equal(got, want) {
		t.Fatalf("ignored %v, want %v", got, want)
	}
	for name, m := range map[string]v1.Machine{
		"egress":    {Kind: v1.MachineHost, Egress: []string{"example.com"}},
		"roots":     {Roots: []string{"/srv"}},
		"resources": {Resources: v1.Resources{CPU: "2"}},
	} {
		if got := ignoredFields(v1.AgentSpec{Machine: m}); !slices.Equal(got, []string{"machine"}) {
			t.Errorf("%s: ignored %v", name, got)
		}
	}
}
