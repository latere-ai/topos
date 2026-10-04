// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/topos/harness"

	"latere.ai/x/topos/manifest/trigger"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

func TestStrictDecodingCollectsEveryProblem(t *testing.T) {
	body := agent("reviewer",
		"model: {name: claude-haiku-4-5}",
		"token: sk-this-field-does-not-exist",
		"tools: read",
		"threads: {maxDepth: 5, maxConcurrent: many}",
		"approvals: {mode: yolo}",
	)
	e := refused(t, CodeInvalidManifest, body, Options{})
	for _, want := range []struct{ path, detail string }{
		{"spec.token", "unknown field"},
		{"spec.tools", "want a list, got a string"},
		{"spec.threads.maxConcurrent", "want an integer, got a string"},
		{"spec.threads.maxDepth", "5 is outside 1 to 4"},
		{"spec.approvals.mode", "not one of plan, confirm, progressive"},
	} {
		if !hasProblem(e, want.path, want.detail) {
			t.Errorf("no problem %s: %s in\n%s", want.path, want.detail, e.Detail())
		}
	}
	if len(e.Problems) != 5 {
		t.Fatalf("%d problems:\n%s", len(e.Problems), e.Detail())
	}
	if strings.Contains(e.Error(), "sk-this") || strings.Contains(e.Error(), "yolo") {
		t.Fatalf("the error quotes a value: %s", e.Error())
	}
	if !strings.HasPrefix(e.Error(), "invalid_manifest: The manifest is not valid.\nspec.token: unknown field") {
		t.Fatalf("rendered %q", e.Error())
	}

	// The same manifest in JSON is refused with the same paths.
	js := `{"apiVersion":"topos.latere.ai/v1","kind":"Agent","metadata":{"name":"reviewer"},"spec":{"model":{"name":"m"},"token":"x","tools":"read","threads":{"maxDepth":5,"maxConcurrent":"many"},"approvals":{"mode":"yolo"}}}`
	ej := refused(t, CodeInvalidManifest, js, Options{})
	if ej.Detail() != e.Detail() {
		t.Fatalf("JSON detail\n%s\nYAML detail\n%s", ej.Detail(), e.Detail())
	}

	// A field that did not decode is not reported again as missing.
	e = refused(t, CodeInvalidManifest, agent("reviewer", "model: claude"), Options{})
	if len(e.Problems) != 1 || !hasProblem(e, "spec.model", "want a mapping, got a string") {
		t.Fatalf("problems:\n%s", e.Detail())
	}
}

func TestManifestHoldingASecretIsRefused(t *testing.T) {
	const key = "sk-ant-api03-Zq8vXk2Lr9TnB4wYc7HdM1pF"
	body := agent("reviewer",
		"model: {name: claude-haiku-4-5}",
		"instructions: Use the key "+key+" for the API.",
		"mcpServers:",
		"  - name: docs",
		"    command: docs-server",
		"    env: {DOCS_TOKEN: ghp_R8dK2mQ9xT4vB7nL1cF6hJ3wZ5yP0sA}",
	)
	e := refused(t, CodeHoldsSecret, body, Options{})
	if !hasProblem(e, "spec.instructions", "a token of the form sk-ant-") || !hasProblem(e, "spec.mcpServers[0].env[DOCS_TOKEN]", "a token of the form ghp_") {
		t.Fatalf("problems:\n%s", e.Detail())
	}
	if strings.Contains(e.Error(), key) || strings.Contains(e.Error(), "ghp_R8") {
		t.Fatalf("the error quotes the value: %s", e.Error())
	}
	// Every free-text field is checked, and a credential reference that
	// is a pasted key is refused as one.
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	for path, lines := range map[string][]string{
		"spec.hooks[0].command":               {"model: {name: m}", "hooks: [{event: turn_end, command: 'curl -H \"Authorization: " + jwt + "\" x'}]"},
		"spec.mcpServers[0].args[1]":          {"model: {name: m}", "mcpServers: [{name: s, command: s, args: [--key, AKIAIOSFODNN7EXAMPLE]}]"},
		"spec.advisor.instructions":           {"model: {name: m}", "advisor: {model: {name: m}, instructions: 'token xoxb-1234567890-abcdefghij'}"},
		"spec.subagents[0].spec.instructions": {"model: {name: m}", "subagents: [{name: s, spec: {model: {name: m}, instructions: 'lux_abcdefghijklmnop1234'}}]"},
		"spec.instructions":                   {"model: {name: m}", "instructions: 'the hash 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08'"},
	} {
		e := refused(t, CodeHoldsSecret, agent("a", lines...), Options{})
		if len(e.Problems) != 1 || e.Problems[0].Path != path {
			t.Fatalf("%s: problems\n%s", path, e.Detail())
		}
	}
	trig := "apiVersion: topos.latere.ai/v1\nkind: Trigger\nmetadata: {name: t}\nspec:\n  agent: a\n  schedule: '@daily'\n  session: {message: 'use github_pat_11ABCDEFG0123456789abcdefghij'}\n"
	if e := refused(t, CodeHoldsSecret, trig, Options{}); !hasProblem(e, "spec.session.message", "github_pat_") {
		t.Fatalf("trigger: %s", e.Detail())
	}
	conn := "apiVersion: topos.latere.ai/v1\nkind: Connection\nmetadata: {name: c}\nspec: {service: http, mode: agent, hosts: [example.com], credential: sk-proj-abcdefghijklmnopqrstu}\n"
	if e := refused(t, CodeHoldsSecret, conn, Options{}); !hasProblem(e, "spec.credential", "sk-") {
		t.Fatalf("connection: %s", e.Detail())
	}
	// Prose, paths, CamelCase test names and words that contain a
	// prefix are not secrets.
	clean := "instructions: 'Run TestStrictDecodingCollectsEveryProblem in internal/toposcli/toposcli_test.go, assess the risk-assessment-procedures and see https://example.com/docs/getting-started-with-agents.'"
	one(t, agent("clean", "model: {name: m}", clean), Options{})
}

func TestDefaultsAreWrittenOut(t *testing.T) {
	docs := agent("reviewer", "model: {name: claude-haiku-4-5}", "machine: {kind: cella}") + "---\n" +
		"apiVersion: topos.latere.ai/v1\nkind: Trigger\nmetadata: {name: nightly}\nspec:\n  agent: reviewer\n  schedule: '@daily'\n  session: {message: Review.}\n---\n" +
		"apiVersion: topos.latere.ai/v1\nkind: Connection\nmetadata: {name: web}\nspec: {service: http, mode: person, hosts: [example.com]}\n---\n" +
		"apiVersion: topos.latere.ai/v1\nkind: MemoryStore\nmetadata: {name: notes}\nspec: {description: Notes.}\n"
	rs, err := Resolve(t.Context(), []byte(docs), fixed(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		v1.KindAgent: `{"model":{"name":"claude-haiku-4-5"},"instructions":"","tools":["read","write","edit","bash","grep","glob","web_fetch","todo"],` +
			`"approvals":{"mode":"confirm","thresholds":{"flagAt":0.3,"askAt":0.5,"blockAt":0.9}},"threads":{"maxDepth":2,"maxConcurrent":8},` +
			`"machine":{"kind":"cella","image":"base"},"limits":{"turnTimeout":"2h","maxAge":"168h"},"context":{"compactAt":0.8}}`,
		v1.KindTrigger:     `{"agent":"agent_00000000000000000000000001","schedule":"@daily","timeZone":"UTC","session":{"message":"Review.","endOnIdle":true},"skipIfActive":true,"maxAge":"1h","suspend":false}`,
		v1.KindConnection:  `{"service":"http","mode":"person","hosts":["example.com"],"inject":{"header":"Authorization","format":"Bearer {value}"}}`,
		v1.KindMemoryStore: `{"description":"Notes."}`,
	}
	for _, r := range rs {
		if got := string(r.Spec); got != want[r.Kind] {
			t.Errorf("%s:\n got %s\nwant %s", r.Kind, got, want[r.Kind])
		}
	}
	// The host machine takes no image.
	r := one(t, agent("h"), fixed(nil))
	if !reflect.DeepEqual(r.Agent.Spec.Machine, v1.Machine{Kind: v1.MachineHost}) {
		t.Fatalf("machine %+v", r.Agent.Spec.Machine)
	}
	// An empty tools list stays empty.
	r = one(t, agent("none", "model: {name: m}", "tools: []"), fixed(nil))
	if r.Agent.Spec.Tools == nil || len(r.Agent.Spec.Tools) != 0 || !strings.Contains(string(r.Spec), `"tools":[]`) {
		t.Fatalf("tools %s", r.Spec)
	}
}

func TestAgentVersioning(t *testing.T) {
	s := newStore()
	body := agent("reviewer", "model: {name: claude-haiku-4-5}", "instructions: Review the diff.")
	first := one(t, body, fixed(s))
	st := first.Agent.Status
	if st.Version != 1 || st.ID != "agent_00000000000000000000000001" || st.Digest != first.Digest || st.CreatedAt.IsZero() {
		t.Fatalf("first status %+v", st)
	}
	s.apply([]Resolved{first})

	// The same agent again resolves to the stored version: nothing new.
	o := fixed(s)
	o.NewID = func(string) string { t.Fatal("a stored agent took a new id"); return "" }
	again := one(t, body, o)
	if again.Agent.Status != st {
		t.Fatalf("same agent: %+v, want %+v", again.Agent.Status, st)
	}
	// A comment, key order and flow style do not change the digest.
	restyled := "# the same agent\nkind: Agent\napiVersion: topos.latere.ai/v1\nspec:\n  instructions: Review the diff.\n  model:\n    name: claude-haiku-4-5\nmetadata: {name: reviewer}\n"
	if r := one(t, restyled, o); r.Digest != st.Digest {
		t.Fatalf("restyled digest %s, want %s", r.Digest, st.Digest)
	}
	// Labels are metadata, not the spec.
	labeled := strings.Replace(body, "  name: reviewer\n", "  name: reviewer\n  labels: {team: platform}\n", 1)
	if r := one(t, labeled, o); r.Agent.Status.Version != 1 {
		t.Fatalf("labels made version %d", r.Agent.Status.Version)
	}

	// Changed instructions are the next version under the same id.
	next := one(t, strings.Replace(body, "Review the diff.", "Review the diff and fix it.", 1), o)
	ns := next.Agent.Status
	if ns.ID != st.ID || ns.Version != 2 || ns.Digest == st.Digest {
		t.Fatalf("next status %+v", ns)
	}
	s.apply([]Resolved{next})
	if a, err := s.Agent(t.Context(), "reviewer"); err != nil || a.Status.Version != 2 {
		t.Fatalf("latest %+v %v", a, err)
	}
}

func TestReferencesPinVersions(t *testing.T) {
	s := newStore()
	// A stored tester at version 2.
	for _, instr := range []string{"Test.", "Test it all."} {
		r := one(t, agent("tester", "model: {name: claude-haiku-4-5}", "instructions: "+instr), fixed(s))
		s.apply([]Resolved{r})
	}
	stored, err := s.Agent(t.Context(), "tester")
	if err != nil || stored.Status.Version != 2 {
		t.Fatalf("stored tester %+v %v", stored, err)
	}
	body := agent("lead",
		"model: {name: claude-haiku-4-5}",
		"subagents:",
		"  - {name: tester, agent: tester}",
		"  - {name: old-tester, agent: "+stored.Status.ID+"@1}",
		"  - {name: helper, agent: helper}",
		"  - {name: inline, spec: {model: {name: m}, subagents: [{name: deep, agent: helper}]}}",
	) + "---\n" + agent("helper", "model: {name: claude-haiku-4-5}", "tools: [read]")
	rs, err := Resolve(t.Context(), []byte(body), fixed(s))
	if err != nil {
		t.Fatal(err)
	}
	// helper is referenced by lead, so it comes first.
	if len(rs) != 2 || rs[0].Name != "helper" || rs[1].Name != "lead" || rs[0].Doc != 1 {
		t.Fatalf("order %s %s", rs[0].Name, rs[1].Name)
	}
	helper, lead := rs[0], rs[1]
	subs := lead.Agent.Spec.Subagents
	want := []string{stored.Status.ID + "@2", stored.Status.ID + "@1", pin(helper.Agent.Status)}
	for i, w := range want {
		if subs[i].Agent != w {
			t.Fatalf("subagent %d pins %s, want %s", i, subs[i].Agent, w)
		}
	}
	if subs[3].Spec.Subagents[0].Agent != pin(helper.Agent.Status) {
		t.Fatalf("the inline subagent pins %s", subs[3].Spec.Subagents[0].Agent)
	}
	for _, ref := range want {
		if lead.Pinned[ref] == nil {
			t.Fatalf("lead does not carry %s: %v", ref, lead.Pinned)
		}
	}
	if lead.Pinned[stored.Status.ID+"@1"].Spec.Instructions != "Test." {
		t.Fatal("the pinned version is not the one named")
	}
	if !strings.Contains(string(lead.Spec), `"agent":"`+stored.Status.ID+`@2"`) {
		t.Fatalf("spec %s", lead.Spec)
	}

	// A trigger pins the agent's id and follows its latest version,
	// unless it names a version.
	trig := "apiVersion: topos.latere.ai/v1\nkind: Trigger\nmetadata: {name: t}\nspec:\n  agent: tester\n  schedule: '*/15 * * * *'\n  session: {message: Go.}\n"
	if r := one(t, trig, fixed(s)); r.Trigger.Spec.Agent != stored.Status.ID {
		t.Fatalf("trigger pins %s", r.Trigger.Spec.Agent)
	}
	pinned := strings.Replace(trig, "agent: tester", "agent: "+stored.Status.ID+"@1", 1)
	if r := one(t, pinned, fixed(s)); r.Trigger.Spec.Agent != stored.Status.ID+"@1" {
		t.Fatalf("trigger pins %s", r.Trigger.Spec.Agent)
	}

	// Every unknown reference is reported in one error.
	unknown := agent("lone",
		"model: {name: m, credential: nowhere}",
		"subagents: [{name: ghost, agent: ghost}]",
		"memoryStores: [{name: void, access: readOnly}]",
		"connections: [none]",
		"mcpServers: [{name: s, url: 'https://example.com/mcp', connection: none}]",
		"advisor: {model: {name: m, credential: nowhere}}",
	)
	for _, l := range []Lookup{nil, s} {
		e := refused(t, CodeUnknownReference, unknown, fixed(l))
		for _, p := range []string{"spec.model.credential", "spec.subagents[0].agent", "spec.memoryStores[0].name", "spec.connections[0]", "spec.mcpServers[0].connection", "spec.advisor.model.credential"} {
			if !hasProblem(e, p, "names no object") {
				t.Fatalf("lookup %v: no problem at %s:\n%s", l, p, e.Detail())
			}
		}
	}
	e := refused(t, CodeUnknownReference, strings.Replace(trig, "agent: tester", "agent: nobody", 1)+"  \n", fixed(nil))
	if !hasProblem(e, "spec.agent", "names no object") {
		t.Fatalf("trigger: %s", e.Detail())
	}
}

// TestAnAgentNamesItsRepositories: an agent's repositories resolve into
// its spec as written, and one whose URL holds a credential is refused
// with a detail that does not carry it.
func TestAnAgentNamesItsRepositories(t *testing.T) {
	rs, err := Resolve(t.Context(), []byte(agent("builder", "model: {name: m}",
		"repositories: [{url: 'https://code.example/acme/app.git', ref: release/1.2}, {url: 'https://code.example/acme/lib'}]")), Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []v1.Repository{{URL: "https://code.example/acme/app.git", Ref: "release/1.2"}, {URL: "https://code.example/acme/lib"}}
	if got := rs[0].Agent.Spec.Repositories; !reflect.DeepEqual(got, want) {
		t.Fatalf("repositories %+v", got)
	}
	e := refused(t, CodeInvalidManifest, agent("builder", "model: {name: m}", "repositories: [{url: 'https://bot:hunter2@code.example/app'}]"), Options{})
	if !hasProblem(e, "spec.repositories[0]", "holds a credential") || strings.Contains(e.Detail(), "hunter2") {
		t.Fatalf("a credential in the url: %s", e.Detail())
	}
}

func TestValidationRules(t *testing.T) {
	e := refused(t, CodeInvalidManifest, agent("deep", "model: {name: m}", "threads: {maxDepth: 5}"), Options{})
	if !hasProblem(e, "spec.threads.maxDepth", "outside 1 to 4") {
		t.Fatalf("maxDepth: %s", e.Detail())
	}
	conn := "apiVersion: topos.latere.ai/v1\nkind: Connection\nmetadata: {name: c}\nspec: {service: github, mode: person, hosts: [api.github.com], credential: bot}\n"
	e = refused(t, CodeInvalidManifest, conn, Options{})
	if !hasProblem(e, "spec.credential", "refused when mode is person") {
		t.Fatalf("person credential: %s", e.Detail())
	}

	const m = "model: {name: m}"
	trigger := func(spec string) string {
		return "apiVersion: topos.latere.ai/v1\nkind: Trigger\nmetadata: {name: t}\nspec: " + spec + "\n"
	}
	connection := func(spec string) string {
		return "apiVersion: topos.latere.ai/v1\nkind: Connection\nmetadata: {name: c}\nspec: " + spec + "\n"
	}
	for _, c := range []struct {
		body, path, detail string
	}{
		{agent("a", "description: x"), "spec.model.name", "required"},
		{agent("Bad_Name", m), "metadata.name", "not a DNS label"},
		{agent("", m), "metadata.name", "required"},
		{agent("a", "model: {name: m, family: acme, dialect: grpc, baseURL: 'ftp://x', effort: max, inputWindow: -1, maxOutputTokens: -2}"), "spec.model.family", "not one of"},
		{agent("a", "model: {name: m, dialect: grpc}"), "spec.model.dialect", "not one of"},
		{agent("a", "model: {name: m, baseURL: 'ftp://x'}"), "spec.model.baseURL", "not an http or https URL"},
		{agent("a", "model: {name: m, effort: max}"), "spec.model.effort", "not one of minimal"},
		{agent("a", "model: {name: m, inputWindow: -1}"), "spec.model.inputWindow", "negative"},
		{agent("a", "model: {name: m, maxOutputTokens: -1}"), "spec.model.maxOutputTokens", "negative"},
		{agent("a", "model: {name: m, pricing: {input: '1.1234567'}}"), "spec.model.pricing.input", "at most 6 fraction digits"},
		{agent("a", "model: {name: m, credential: cred_nope}"), "spec.model.credential", "not a cred_<ulid> id"},
		{agent("a", "model: {name: m, credential: 'Bad Name'}"), "spec.model.credential", "not a name or a cred_<ulid> id"},
		{agent("a", m, "instructions: x", "instructionsFile: x.md"), "spec.instructionsFile", "not both"},
		{agent("a", m, "instructionsFile: x.md"), "spec.instructionsFile", "reads no files"},
		{agent("a", m, "tools: [read, read]"), "spec.tools[1]", "repeats an earlier entry"},
		{agent("a", m, "tools: [deploy]"), "spec.tools[0].name", "not a built-in tool or question"},
		{agent("a", m, "tools: ['bad name']"), "spec.tools[0].name", "not a tool name"},
		{agent("a", m, "tools: [{outputLimit: 3}]"), "spec.tools[0].name", "required"},
		{agent("a", m, "tools: [{name: read, outputLimit: -1}]"), "spec.tools[0].outputLimit", "negative"},
		{agent("a", m, "tools: [{name: read, description: x}]"), "spec.tools[0]", "only name and outputLimit"},
		{agent("a", m, "tools: [{name: read, client: true}]"), "spec.tools[0].name", "may not take a built-in's name"},
		{agent("a", m, "tools: [{name: ask, client: true}]"), "spec.tools[0].description", "needs a description"},
		{agent("a", m, "tools: [{name: ask, client: true, description: x}]"), "spec.tools[0].inputSchema", "needs an input schema"},
		{agent("a", m, "tools: [{name: ask, client: true, description: x, inputSchema: [1]}]"), "spec.tools[0].inputSchema", "want a mapping"},
		{agent("a", m, "permissions: [{resource: x}]"), "spec.permissions[0].action", "required"},
		{agent("a", m, "permissions: [{action: x}]"), "spec.permissions[0].resource", "required"},
		{agent("a", m, "identity: agent"), "spec.identity", "removed: the agent's owner, a person or an organization, decides"},
		{agent("a", m, "identity: person"), "spec.identity", "removed: the agent's owner"},
		{agent("a", m, "approvals: {alwaysAllow: ['bash(go test *']}"), "spec.approvals.alwaysAllow[0]", "not a pattern"},
		{agent("a", m, "approvals: {alwaysConfirm: ['a b']}"), "spec.approvals.alwaysConfirm[0]", "not a pattern"},
		{agent("a", m, "approvals: {thresholds: {flagAt: -0.1}}"), "spec.approvals.thresholds.flagAt", "outside 0 to 1"},
		{agent("a", m, "approvals: {thresholds: {askAt: 0.95}}"), "spec.approvals.thresholds", "must increase"},
		{agent("a", m, "hooks: [{event: always, command: x}]"), "spec.hooks[0].event", "not one of pre_tool_use"},
		{agent("a", m, "hooks: [{event: turn_end}]"), "spec.hooks[0].command", "required"},
		{agent("a", m, "hooks: [{event: pre_tool_use, command: x, matcher: 'bash(', timeout: 10}]"), "spec.hooks[0].timeout", "want a string, got a number"},
		{agent("a", m, "hooks: [{event: pre_tool_use, command: x, matcher: 'bash('}]"), "spec.hooks[0].matcher", "not a pattern"},
		{agent("a", m, "hooks: [{event: pre_tool_use, command: x, timeout: soon}]"), "spec.hooks[0].timeout", "not a positive Go duration"},
		{agent("a", m, "subagents: [{name: s, agent: b, spec: {model: {name: m}}}]"), "spec.subagents[0]", "not both"},
		{agent("a", m, "subagents: [{name: s}]"), "spec.subagents[0]", "set agent"},
		{agent("a", m, "subagents: [{name: s, agent: b}, {name: s, agent: c}]"), "spec.subagents[1]", "repeats"},
		{agent("a", m, "subagents: [{name: s, agent: agent_x}]"), "spec.subagents[0].agent", "not a agent_<ulid> id"},
		{agent("a", m, "subagents: [{name: s, agent: agent_00000000000000000000000001@0}]"), "spec.subagents[0].agent", "@<n> from 1"},
		{agent("a", m, "subagents: [{name: s, spec: {model: {name: m}, identity: agent}}]"), "spec.subagents[0].spec.identity", "removed: the agent's owner"},
		{agent("a", m, "threads: {maxConcurrent: 33}"), "spec.threads.maxConcurrent", "outside 1 to 32"},
		{agent("a", m, "threads: {maxDepth: 0}"), "spec.threads.maxDepth", "0 is outside 1 to 4"},
		{agent("a", m, "advisor: {model: {effort: max}}"), "spec.advisor.model.name", "required"},
		{agent("a", m, "skills: [{}]"), "spec.skills[0]", "set path, or git"},
		{agent("a", m, "skills: [{path: x, ref: main}]"), "spec.skills[0].ref", "a ref needs git"},
		{agent("a", m, "mcpServers: [{name: s}]"), "spec.mcpServers[0]", "exactly one of command"},
		{agent("a", m, "mcpServers: [{command: x}]"), "spec.mcpServers[0].name", "required"},
		{agent("a", m, "mcpServers: [{name: s, url: 'x:y'}]"), "spec.mcpServers[0].url", "not an http or https URL"},
		{agent("a", m, "mcpServers: [{name: s, url: 'https://x', args: [a]}]"), "spec.mcpServers[0]", "args and env are for a stdio server"},
		{agent("a", m, "mcpServers: [{name: s, url: 'https://x', connection: Bad}]"), "spec.mcpServers[0].connection", "not a DNS label"},
		{agent("a", m, "mcpServers: [{name: s, command: x, connection: c}]"), "spec.mcpServers[0].connection", "for an HTTP server"},
		{agent("a", m, "mcpServers: [{name: s, command: x, env: {A: 1}}]"), "spec.mcpServers[0].env[A]", "want a string, got a number"},
		{agent("a", m, "memoryStores: [{name: n, access: all}]"), "spec.memoryStores[0].access", "not one of readWrite, readOnly"},
		{agent("a", m, "memoryStores: [{name: n, access: readOnly}, {name: n, access: readOnly}]"), "spec.memoryStores[1]", "repeats"},
		{agent("a", m, "connections: [Bad]"), "spec.connections[0]", "not a DNS label"},
		{agent("a", m, "repositories: [{ref: main}]"), "spec.repositories[0].url", "required"},
		{agent("a", m, "repositories: [{url: 'http://code.example/app'}]"), "spec.repositories[0]", "is not https"},
		{agent("a", m, "repositories: [{url: 'file:///srv/app.git'}]"), "spec.repositories[0]", "is not https"},
		{agent("a", m, "repositories: [{url: 'https:///app'}]"), "spec.repositories[0]", "names no host"},
		{agent("a", m, "repositories: [{url: 'https://code.example/app', ref: '--upload-pack=x'}]"), "spec.repositories[0]", "not a branch, a tag or a commit"},
		{agent("a", m, "repositories: [{url: 'https://code.example/app', access: readOnly}]"), "spec.repositories[0].access", "unknown field"},
		{agent("a", m, "repositories: ["+strings.Repeat("{url: 'https://code.example/app'}, ", session.MaxRepositories)+"{url: 'https://code.example/app'}]"), "spec.repositories", fmt.Sprintf("at most %d", session.MaxRepositories)},
		{agent("a", m, "machine: {kind: vm}"), "spec.machine.kind", "not one of host, cella"},
		{agent("a", m, "machine: {image: base}"), "spec.machine", "are for a cella machine"},
		{agent("a", m, "machine: {kind: cella, roots: [/x]}"), "spec.machine", "are for the host"},
		{agent("a", m, "machine: {kind: cella, resources: {cpu: two}}"), "spec.machine.resources.cpu", "not a Kubernetes quantity"},
		{agent("a", m, "machine: {egress: ['https://proxy.golang.org']}"), "spec.machine.egress[0]", "not a host name"},
		{agent("a", m, "machine: {egress: [a.com, a.com]}"), "spec.machine.egress[1]", "repeats"},
		{agent("a", m, "machine: {roots: [relative]}"), "spec.machine.roots[0]", "not a clean absolute path"},
		{agent("a", m, "machine: {readPaths: [/a/../b]}"), "spec.machine.readPaths[0]", "not a clean absolute path"},
		{agent("a", m, "budget: {maxCost: -5}"), "spec.budget.maxCost", "want a string, got a number"},
		{agent("a", m, "budget: {maxCost: '-5'}"), "spec.budget.maxCost", "non-negative decimal"},
		{agent("a", m, "limits: {turnTimeout: 0s}"), "spec.limits.turnTimeout", "not a positive Go duration"},
		{agent("a", m, "limits: {maxAge: forever}"), "spec.limits.maxAge", "not a positive Go duration"},
		{agent("a", m, "context: {compactAt: 0.99}"), "spec.context.compactAt", "outside 0.5 to 0.95"},
		{trigger("{schedule: '@daily', session: {message: x}}"), "spec.agent", "required"},
		{trigger("{agent: a, session: {message: x}}"), "spec.schedule", "required"},
		{trigger("{agent: a, schedule: '0 25 * * *', session: {message: x}}"), "spec.schedule", "not a five-field cron"},
		{trigger("{agent: a, schedule: '* * *', session: {message: x}}"), "spec.schedule", "not a five-field cron"},
		{trigger("{agent: a, schedule: '*/0 * * * *', session: {message: x}}"), "spec.schedule", "not a five-field cron"},
		{trigger("{agent: a, schedule: '5-2 * * * *', session: {message: x}}"), "spec.schedule", "not a five-field cron"},
		{trigger("{agent: a, schedule: '@daily', timeZone: 'Not a zone', session: {message: x}}"), "spec.timeZone", "not an IANA time zone"},
		{trigger("{agent: a, schedule: '@daily', session: {}}"), "spec.session.message", "required"},
		{trigger("{agent: a, schedule: '@daily', session: {message: x, machine: {kind: vm}}}"), "spec.session.machine.kind", "not one of"},
		{trigger("{agent: a, schedule: '@daily', session: {message: x, resources: [{type: disk}]}}"), "spec.session.resources[0].type", "not one of memoryStore, repository"},
		{trigger("{agent: a, schedule: '@daily', session: {message: x, resources: [{type: memoryStore, memoryStore: n, access: all}]}}"), "spec.session.resources[0].access", "not one of"},
		{trigger("{agent: a, schedule: '@daily', session: {message: x, resources: [{type: memoryStore, memoryStore: n, access: readOnly, url: u}]}}"), "spec.session.resources[0]", "are for a repository"},
		{trigger("{agent: a, schedule: '@daily', session: {message: x, resources: [{type: repository}]}}"), "spec.session.resources[0].url", "required"},
		{trigger("{agent: a, schedule: '@daily', session: {message: x, resources: [{type: repository, url: u, access: readOnly}]}}"), "spec.session.resources[0]", "are for a memory store"},
		{trigger("{agent: a, schedule: '@daily', session: {message: x, budget: {maxCost: 'x'}}}"), "spec.session.budget.maxCost", "non-negative decimal"},
		{trigger("{agent: a, schedule: '@daily', session: {message: x, limits: {turnTimeout: x}}}"), "spec.session.limits.turnTimeout", "not a positive Go duration"},
		{trigger("{agent: a, schedule: '@daily', maxAge: x, session: {message: x}}"), "spec.maxAge", "not a positive Go duration"},
		{trigger("{agent: a, schedule: '@daily', suspend: 'yes', session: {message: x}}"), "spec.suspend", "want a boolean, got a string"},
		{"apiVersion: topos.latere.ai/v1\nkind: MemoryStore\nmetadata: {name: n}\nspec: {}\n", "spec.description", "required"},
		{"apiVersion: topos.latere.ai/v1\nkind: MemoryStore\nmetadata: {name: n}\nspec: {description: " + strings.Repeat("x", 1025) + "}\n", "spec.description", "1025 characters, at most 1024"},
		{connection("{mode: agent, hosts: [a.com], credential: bot}"), "spec.service", "required"},
		{connection("{service: s, mode: shared, hosts: [a.com]}"), "spec.mode", "not one of person, agent"},
		{connection("{service: s, mode: person}"), "spec.hosts", "required"},
		{connection("{service: s, mode: agent, hosts: [a.com]}"), "spec.credential", "required when mode is agent"},
		{connection("{service: s, mode: agent, hosts: [a.com], credential: cred_1}"), "spec.credential", "not a cred_<ulid> id"},
		{connection("{service: s, mode: person, hosts: [a.com], inject: {header: 'X Y', format: 'token'}}"), "spec.inject.header", "not an HTTP header name"},
		{connection("{service: s, mode: person, hosts: [a.com], inject: {format: 'token'}}"), "spec.inject.format", "must contain {value}"},
		{connection("{service: s, mode: person, hosts: [a.com], inject: {header: 7}}"), "spec.inject.header", "want a string, got a number"},
	} {
		_, err := Resolve(t.Context(), []byte(c.body), Options{})
		e, ok := errors.AsType[*Error](err)
		if !ok || e.Code != CodeInvalidManifest || !hasProblem(e, c.path, c.detail) {
			t.Errorf("%s: want %s: %s, got %v", strings.ReplaceAll(c.body, "\n", " "), c.path, c.detail, err)
		}
	}
}

// TestADisplayNameIsOneLineOfText: metadata.displayName takes any text
// on one line up to MaxDisplayName characters, scripts, emoji and their
// joiners included, and is outside the spec, so it moves no digest. A
// blank one, a longer one, and one holding a control character, a line
// break or a bidirectional control are refused at its path; a secret in
// it is refused as one.
func TestADisplayNameIsOneLineOfText(t *testing.T) {
	named := func(display string) string {
		return fmt.Sprintf("apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: reviewer\n  displayName: %q\nspec: {model: {name: m}}\n", display)
	}
	bare := one(t, agent("reviewer", "model: {name: m}"), Options{})
	for _, display := range []string{
		"Code Reviewer",
		"代码审查 · Revue de code 👩‍💻",
		strings.Repeat("é", MaxDisplayName),
	} {
		r := one(t, named(display), Options{})
		if r.Agent.Metadata.DisplayName != display || r.Agent.Metadata.Name != "reviewer" {
			t.Fatalf("%q resolved to %+v", display, r.Agent.Metadata)
		}
		if r.Digest != bare.Digest || string(r.Spec) != string(bare.Spec) {
			t.Fatalf("%q moved the digest: %s, want %s", display, r.Digest, bare.Digest)
		}
		doc, err := session.Marshal(r.Agent)
		if err != nil || !strings.Contains(string(doc), `"displayName":`) {
			t.Fatalf("the resolved agent does not carry it: %s %v", doc, err)
		}
	}
	if doc, err := session.Marshal(bare.Agent); err != nil || strings.Contains(string(doc), "displayName") {
		t.Fatalf("an agent without one writes the field: %s %v", doc, err)
	}
	for display, want := range map[string]string{
		"   ":                                 "blank",
		strings.Repeat("x", MaxDisplayName+1): fmt.Sprintf("%d characters, at most %d", MaxDisplayName+1, MaxDisplayName),
		"Code\tReviewer":                      "a control character",
		"Code\nReviewer":                      "a line break",
		"Code\u2028Reviewer":                  "a line break",
		"Reviewer \u202egnp.exe":              "a bidirectional control",
		strings.Repeat("😀", MaxDisplayName+1): "characters, at most",
	} {
		e := refused(t, CodeInvalidManifest, named(display), Options{})
		if len(e.Problems) != 1 || !hasProblem(e, "metadata.displayName", want) {
			t.Errorf("%q: want metadata.displayName: %s, got\n%s", display, want, e.Detail())
		}
	}
	e := refused(t, CodeHoldsSecret, named("Reviewer sk-ant-api03-"+strings.Repeat("a1B2", 20)), Options{})
	if !hasProblem(e, "metadata.displayName", "holds") {
		t.Fatalf("a secret in the display name: %s", e.Detail())
	}
}

// eventTrigger is a Trigger document on delivered events, its spec's
// fields after the agent.
func eventTrigger(spec string) string {
	return "apiVersion: topos.latere.ai/v1\nkind: Trigger\nmetadata: {name: t}\nspec: {agent: a, " + spec + "}\n"
}

// storedA are options whose lookup holds the agent a, the one every
// trigger of these tests runs, stored as agent_<1>.
func storedA(t *testing.T) Options {
	t.Helper()
	s := newStore()
	rs, err := Resolve(t.Context(), []byte(agent("a")), fixed(s))
	if err != nil {
		t.Fatal(err)
	}
	s.apply(rs)
	return fixed(s)
}

// TestApplyRefusesABadTemplate: a template of an unknown root, a {{ that
// opens no placeholder, and an event path in a schedule trigger are each
// refused as invalid_manifest at the field's path, in the message, the
// title, the key and a repository's url and ref alike; an event trigger
// takes event paths.
func TestApplyRefusesABadTemplate(t *testing.T) {
	schedule := func(session string) string {
		return "apiVersion: topos.latere.ai/v1\nkind: Trigger\nmetadata: {name: t}\nspec: {agent: a, schedule: '@daily', session: " + session + "}\n"
	}
	for _, c := range []struct{ body, path, detail string }{
		{schedule("{message: 'Look at {{env.HOME}}.'}"), "spec.session.message", `unknown root "env"`},
		{schedule("{message: 'Fix {{event.resource'}"), "spec.session.message", "opens no placeholder"},
		{schedule("{message: 'Fix {{ not a path }}'}"), "spec.session.message", "opens no placeholder"},
		{schedule("{message: 'Fix {{event.resource}}.'}"), "spec.session.message", "fires with no event"},
		{schedule("{message: x, title: '{{event.verb}}'}"), "spec.session.title", "fires with no event"},
		{schedule("{message: x, key: '{{secrets.key}}'}"), "spec.session.key", "unknown root"},
		{schedule("{message: x, resources: [{type: repository, url: 'https://git.example/{{event.resource}}.git'}]}"), "spec.session.resources[0].url", "fires with no event"},
		{schedule("{message: x, resources: [{type: repository, url: 'https://git.example/r.git', ref: '{{firing.id'}]}"), "spec.session.resources[0].ref", "opens no placeholder"},
		{eventTrigger("on: {product: github, verbs: [push]}, session: {message: '{{payload.ref}}'}"), "spec.session.message", `unknown root "payload"`},
	} {
		e := refused(t, CodeInvalidManifest, c.body, Options{})
		if !hasProblem(e, c.path, c.detail) {
			t.Errorf("%s: want %s: %s, got\n%s", strings.ReplaceAll(c.body, "\n", " "), c.path, c.detail, e.Detail())
		}
	}
	// A schedule takes the trigger and firing roots and a literal {{; an
	// event trigger takes event paths everywhere a template is.
	for _, body := range []string{
		schedule(`{message: 'Nightly {{trigger.name}} at {{ firing.time }}; \{{literal}}', title: '{{firing.id}}', key: nightly}`),
		eventTrigger("on: {product: github, verbs: ['issue.*']}, session: {message: 'Triage {{event.resource}}: {{event.payload.issue.title}}', title: '{{event.verb}}', key: '{{event.subject}}', " +
			"resources: [{type: repository, url: 'https://git.example/{{event.payload.repo}}.git', ref: '{{event.payload.ref}}'}]}"),
	} {
		one(t, body, storedA(t))
	}
}

// TestTheTriggerFieldsOfSpec022: an event trigger resolves with its
// filter; the fields spec 022 added are left out of a trigger that does
// not set them, so its resolved spec is as it was; continue turns
// endOnIdle's default off; and the new fields' rules are checked.
func TestTheTriggerFieldsOfSpec022(t *testing.T) {
	r := one(t, eventTrigger("on: {product: github, verbs: ['pull_request.*'], resources: ['o/r#*'], match: [{path: payload.action, in: [opened]}]}, "+
		"session: {message: 'Review {{event.resource}}', policy: continue, key: '{{event.resource}}'}, maxActive: 3"), storedA(t))
	want := `{"agent":"agent_00000000000000000000000001","timeZone":"UTC","on":{"product":"github","verbs":["pull_request.*"],"resources":["o/r#*"],"match":[{"path":"payload.action","in":["opened"]}]},` +
		`"session":{"message":"Review {{event.resource}}","policy":"continue","key":"{{event.resource}}","endOnIdle":false},"skipIfActive":true,"maxActive":3,"maxAge":"1h","suspend":false}`
	if string(r.Spec) != want {
		t.Fatalf("the resolved event trigger:\n%s\nwant\n%s", r.Spec, want)
	}
	plain := one(t, "apiVersion: topos.latere.ai/v1\nkind: Trigger\nmetadata: {name: t}\nspec: {agent: a, schedule: '@daily', session: {message: x}}\n", storedA(t))
	if s := string(plain.Spec); strings.Contains(s, "policy") || strings.Contains(s, "maxActive") || strings.Contains(s, `"on"`) || strings.Contains(s, `"key"`) || !strings.Contains(s, `"endOnIdle":true`) {
		t.Fatalf("a schedule trigger writes a field it does not set: %s", s)
	}
	for _, c := range []struct{ body, path, detail string }{
		{eventTrigger("schedule: '@daily', on: {product: github, verbs: [push]}, session: {message: x}"), "spec", "set schedule or on, not both"},
		{eventTrigger("on: {verbs: [push]}, session: {message: x}"), "spec.on.product", "required"},
		{eventTrigger("on: {product: GitHub, verbs: [push]}, session: {message: x}"), "spec.on.product", "lowercase"},
		{eventTrigger("on: {product: github}, session: {message: x}"), "spec.on.verbs", "required"},
		{eventTrigger("on: {product: github, verbs: ['']}, session: {message: x}"), "spec.on.verbs[0]", "empty"},
		{eventTrigger("on: {product: github, verbs: ['pull*request']}, session: {message: x}"), "spec.on.verbs[0]", "may only end an entry"},
		{eventTrigger("on: {product: github, verbs: [push], resources: ['*a*']}, session: {message: x}"), "spec.on.resources[0]", "may only end an entry"},
		{eventTrigger("on: {product: github, verbs: [push], match: [{path: action, in: [x]}]}, session: {message: x}"), "spec.on.match[0].path", "a path into the payload"},
		{eventTrigger("on: {product: github, verbs: [push], match: [{path: payload.action}]}, session: {message: x}"), "spec.on.match[0].in", "required"},
		{eventTrigger("on: {product: github, verbs: [push], extra: 1}, session: {message: x}"), "spec.on.extra", "unknown field"},
		{eventTrigger("on: {product: github, verbs: [push]}, session: {message: x, policy: always}"), "spec.session.policy", "not one of new, continue"},
		{eventTrigger("on: {product: github, verbs: [push]}, maxActive: 0, session: {message: x}"), "spec.maxActive", fmt.Sprintf("0 is outside 1 to %d", trigger.MaxActiveCeiling)},
		{eventTrigger(fmt.Sprintf("on: {product: github, verbs: [push]}, maxActive: %d, session: {message: x}", trigger.MaxActiveCeiling+1)), "spec.maxActive", "is outside 1 to"},
		{eventTrigger("schedule: '0 0 30 2 *', session: {message: x}"), "spec.schedule", "never fires"},
		{eventTrigger("on: {product: github, verbs: [push]}, session: {message: x, resources: [{type: repository, url: 'http://git.example/r.git'}]}"), "spec.session.resources[0]", "https"},
	} {
		e := refused(t, CodeInvalidManifest, c.body, Options{})
		if !hasProblem(e, c.path, c.detail) {
			t.Errorf("%s: want %s: %s, got\n%s", strings.ReplaceAll(c.body, "\n", " "), c.path, c.detail, e.Detail())
		}
	}
}

// TestQuestionIsAKnownToolName: an agent names the harness's question
// tool among its tools, and its config holds the name; an agent that
// names no tools holds the eight built-ins and not question, so its
// resolved spec and digest are what they were; a client tool may not
// take the name, and the question tool takes nothing but its name.
func TestQuestionIsAKnownToolName(t *testing.T) {
	const m = "model: {name: m}"
	r := one(t, agent("asker", m, "tools: [read, question]"), fixed(newStore()))
	c, err := r.AgentConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.Tools, []string{"read", harness.ToolQuestion}) {
		t.Fatalf("the config holds %v", c.Tools)
	}
	sub := one(t, agent("lead", m, "subagents: [{name: s, spec: {model: {name: m}, tools: [question]}}]"), fixed(newStore()))
	if _, err := sub.AgentConfig(nil); err != nil {
		t.Fatalf("a subagent that names question: %v", err)
	}

	plain := one(t, agent("plain", m), fixed(newStore()))
	if got := toolNames(plain.Agent.Spec.Tools); !slices.Equal(got, Builtins()) || slices.Contains(got, harness.ToolQuestion) || len(got) != 8 {
		t.Fatalf("an agent that names no tools holds %v", got)
	}
	again := one(t, agent("plain", m), fixed(newStore()))
	if plain.Digest != again.Digest || plain.Digest == "" {
		t.Fatalf("the digest moved: %s, %s", plain.Digest, again.Digest)
	}

	for _, c := range []struct{ body, path, detail string }{
		{agent("a", m, "tools: [{name: question, client: true, description: x, inputSchema: {type: object}}]"), "spec.tools[0].name", "reserved for the harness's question tool"},
		{agent("a", m, "tools: [{name: question, outputLimit: 10}]"), "spec.tools[0]", "takes only its name"},
		{agent("a", m, "tools: [{name: question, description: x}]"), "spec.tools[0]", "takes only its name"},
		{agent("a", m, "tools: [question, question]"), "spec.tools[1]", "repeats an earlier entry"},
	} {
		e := refused(t, CodeInvalidManifest, c.body, Options{})
		if !hasProblem(e, c.path, c.detail) {
			t.Errorf("%s: %s", c.body, e.Detail())
		}
	}
}
