// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/topos/manifest"
	v1 "latere.ai/x/topos/manifest/v1"
)

func TestApplyingAnAgentVersionsItsSpec(t *testing.T) {
	f := newFixture(t)
	first := f.do(http.MethodPut, "/v1/agents/reviewer", "alice", agentYAML("reviewer", "Review."))
	if first.status != http.StatusCreated {
		t.Fatalf("create: %d %s", first.status, first.body)
	}
	var a v1.Agent
	first.decode(t, &a)
	if a.Status.Version != 1 || !strings.HasPrefix(a.Status.ID, "agent_") || a.Spec.Instructions != "Review." {
		t.Fatalf("created %+v", a.Status)
	}
	same := f.do(http.MethodPut, "/v1/agents/reviewer", "alice", agentYAML("reviewer", "Review."))
	var again v1.Agent
	same.decode(t, &again)
	if same.status != http.StatusOK || again.Status.Version != 1 {
		t.Fatalf("an unchanged spec: %d, version %d", same.status, again.Status.Version)
	}
	changed := f.do(http.MethodPut, "/v1/agents/reviewer", "alice", agentYAML("reviewer", "Review closely."))
	var second v1.Agent
	changed.decode(t, &second)
	if changed.status != http.StatusOK || second.Status.Version != 2 || second.Status.ID != a.Status.ID {
		t.Fatalf("a changed spec: %d %+v", changed.status, second.Status)
	}
	for _, ref := range []string{"reviewer", a.Status.ID} {
		var got v1.Agent
		g := f.do(http.MethodGet, "/v1/agents/"+ref, "alice", "")
		g.decode(t, &got)
		if g.status != http.StatusOK || got.Status.Version != 2 || got.Spec.Instructions != "Review closely." {
			t.Fatalf("GET %s: %d %+v", ref, g.status, got.Status)
		}
	}
	var one v1.Agent
	if g := f.do(http.MethodGet, "/v1/agents/reviewer/versions/1", "alice", ""); g.status != http.StatusOK {
		t.Fatalf("version 1: %d", g.status)
	} else {
		g.decode(t, &one)
	}
	if one.Spec.Instructions != "Review." {
		t.Fatalf("version 1 is %+v", one.Spec)
	}
	var versions struct {
		Items []agentVersion `json:"items"`
	}
	f.do(http.MethodGet, "/v1/agents/reviewer/versions", "alice", "").decode(t, &versions)
	if len(versions.Items) != 2 || versions.Items[1].CreatedBy != alice || versions.Items[0].Digest == versions.Items[1].Digest {
		t.Fatalf("versions %+v", versions.Items)
	}
	for path, code := range map[string]string{
		"/v1/agents/reviewer/versions/9":          CodeNotFound,
		"/v1/agents/reviewer/versions/x":          CodeInvalidRequest,
		"/v1/agents/nobody/versions":              CodeNotFound,
		"/v1/agents/nobody/versions/1":            CodeNotFound,
		"/v1/agents/reviewer/versions?limit=9999": CodeInvalidRequest,
	} {
		if g := f.do(http.MethodGet, path, "alice", ""); g.code() != code {
			t.Errorf("GET %s: %d %s, want %s", path, g.status, g.body, code)
		}
	}
}

func TestApplyRefusesAWrongManifest(t *testing.T) {
	f := newFixture(t)
	for name, c := range map[string]struct{ path, body, code string }{
		"name differs":  {"/v1/agents/other", agentYAML("reviewer", "x"), CodeInvalidRequest},
		"two documents": {"/v1/agents/reviewer", agentYAML("reviewer", "x") + "---\n" + agentYAML("second", "y"), CodeInvalidRequest},
		"a trigger":     {"/v1/agents/nightly", "apiVersion: topos.latere.ai/v1\nkind: Trigger\nmetadata:\n  name: nightly\nspec:\n  agent: reviewer\n  schedule: {cron: \"0 3 * * *\"}\n  session: {message: go}\n", ""},
		"unknown field": {"/v1/agents/reviewer", agentYAML("reviewer", "x") + "  colour: blue\n", manifest.CodeInvalidManifest},
		"a secret":      {"/v1/agents/reviewer", agentYAML("reviewer", "use sk-ant-api03-"+strings.Repeat("a1B2", 20)), manifest.CodeHoldsSecret},
		"old version":   {"/v1/agents/reviewer", strings.Replace(agentYAML("reviewer", "x"), "topos.latere.ai/v1", "topos.latere.ai/v0", 1), manifest.CodeUnsupportedVersion},
		"unknown ref":   {"/v1/agents/reviewer", agentYAML("reviewer", "x") + "  subagents: [{name: helper, agent: nobody}]\n", manifest.CodeUnknownReference},
	} {
		a := f.do(http.MethodPut, c.path, "alice", c.body)
		if a.status != http.StatusBadRequest || (c.code != "" && a.code() != c.code) {
			t.Errorf("%s: %d %s", name, a.status, a.body)
		}
	}
	a := f.do(http.MethodPut, "/v1/agents/reviewer", "alice", agentYAML("reviewer", "x")+"  colour: blue\n")
	if !strings.Contains(string(a.body), `"problems"`) || !strings.Contains(string(a.body), "colour") {
		t.Fatalf("an invalid manifest names its problems: %s", a.body)
	}
}

// TestAnotherSubjectsAgentIsNotThere: bob cannot read, list, change or
// reference alice's agent, by its name or its id, and every refusal
// answers as if it did not exist; an admin acts on it by its id.
func TestAnotherSubjectsAgentIsNotThere(t *testing.T) {
	f := newFixture(t)
	id := f.apply("alice", "reviewer", "Review.").Status.ID
	missing := f.do(http.MethodGet, "/v1/agents/nobody", "bob", "")
	for _, ref := range []string{"reviewer", id, "reviewer/versions", id + "/versions", id + "/versions/1"} {
		denied := f.do(http.MethodGet, "/v1/agents/"+ref, "bob", "")
		if denied.status != http.StatusNotFound || string(denied.body) != string(missing.body) {
			t.Fatalf("bob's GET %s: %d %s differs from a missing agent %s", ref, denied.status, denied.body, missing.body)
		}
	}
	for _, ref := range []string{"reviewer", id, id + "@1"} {
		if a := f.do(http.MethodPut, "/v1/agents/lead", "bob", agentYAML("lead", "Lead.")+"  subagents: [{name: helper, agent: "+ref+"}]\n"); a.code() != manifest.CodeUnknownReference {
			t.Fatalf("bob referencing alice's agent as %s: %d %s", ref, a.status, a.body)
		}
	}
	var page struct {
		Items []v1.Agent `json:"items"`
	}
	f.do(http.MethodGet, "/v1/agents", "bob", "").decode(t, &page)
	if len(page.Items) != 0 {
		t.Fatalf("bob lists %d agents", len(page.Items))
	}
	f.do(http.MethodGet, "/v1/agents", "root", "").decode(t, &page)
	if len(page.Items) != 1 {
		t.Fatalf("the admin lists %d agents", len(page.Items))
	}
	for _, ref := range []string{"reviewer", id} {
		if a := f.do(http.MethodPost, "/v1/agents/"+ref+"/archive", "bob", `{"permanent":true}`); a.status != http.StatusNotFound || string(a.body) != string(missing.body) {
			t.Fatalf("bob's archive of %s: %d %s", ref, a.status, a.body)
		}
	}
	if a := f.do(http.MethodGet, "/v1/agents/"+id, "root", ""); a.status != http.StatusOK {
		t.Fatalf("the admin's read by id: %d %s", a.status, a.body)
	}
	if a := f.do(http.MethodGet, "/v1/agents/reviewer", "root", ""); a.status != http.StatusNotFound {
		t.Fatalf("the admin's read of alice's name, which is not the admin's own: %d %s", a.status, a.body)
	}
}

// TestNamesArePerOwner: a name is unique within its owner, not across
// the installation. alice and bob each apply coding-agent and get an
// agent of their own, each reads and versions their own by the name,
// and a subject holding no agent of the name reads it as missing.
func TestNamesArePerOwner(t *testing.T) {
	f := newFixture(t)
	mine := f.do(http.MethodPut, "/v1/agents/coding-agent", "alice", agentYAML("coding-agent", "Alice's."))
	theirs := f.do(http.MethodPut, "/v1/agents/coding-agent", "bob", agentYAML("coding-agent", "Bob's."))
	if mine.status != http.StatusCreated || theirs.status != http.StatusCreated {
		t.Fatalf("two subjects apply one name: %d %s, %d %s", mine.status, mine.body, theirs.status, theirs.body)
	}
	var a, b v1.Agent
	mine.decode(t, &a)
	theirs.decode(t, &b)
	if a.Status.ID == b.Status.ID || a.Status.Version != 1 || b.Status.Version != 1 {
		t.Fatalf("two subjects' coding-agent: %+v and %+v", a.Status, b.Status)
	}
	if again := f.do(http.MethodPut, "/v1/agents/coding-agent", "bob", agentYAML("coding-agent", "Bob's, closer.")); again.status != http.StatusOK {
		t.Fatalf("bob's second version: %d %s", again.status, again.body)
	}
	for _, c := range []struct {
		token, id, instructions string
		version                 int
	}{
		{"alice", a.Status.ID, "Alice's.", 1},
		{"bob", b.Status.ID, "Bob's, closer.", 2},
	} {
		var got v1.Agent
		g := f.do(http.MethodGet, "/v1/agents/coding-agent", c.token, "")
		g.decode(t, &got)
		if g.status != http.StatusOK || got.Status.ID != c.id || got.Spec.Instructions != c.instructions || got.Status.Version != c.version {
			t.Fatalf("%s's coding-agent: %d %+v", c.token, g.status, got.Status)
		}
		var versions struct {
			Items []agentVersion `json:"items"`
		}
		f.do(http.MethodGet, "/v1/agents/coding-agent/versions", c.token, "").decode(t, &versions)
		if len(versions.Items) != c.version {
			t.Fatalf("%s's versions of coding-agent: %+v", c.token, versions.Items)
		}
	}
	missing := f.do(http.MethodGet, "/v1/agents/nobody", "carol", "")
	if g := f.do(http.MethodGet, "/v1/agents/coding-agent", "carol", ""); g.status != http.StatusNotFound || string(g.body) != string(missing.body) {
		t.Fatalf("carol's read of a name two others hold: %d %s, a missing agent %s", g.status, g.body, missing.body)
	}
	if ar := f.do(http.MethodPost, "/v1/agents/coding-agent/archive", "bob", `{"permanent":true}`); ar.status != http.StatusOK {
		t.Fatalf("bob archives his own: %d %s", ar.status, ar.body)
	}
	var got v1.Agent
	f.do(http.MethodGet, "/v1/agents/coding-agent", "alice", "").decode(t, &got)
	if got.Status.ID != a.Status.ID || got.Status.ArchivedAt != nil {
		t.Fatalf("bob's archive reached alice's agent: %+v", got.Status)
	}
	// A subagent named in alice's manifest is alice's agent of the name.
	var lead v1.Agent
	f.do(http.MethodPut, "/v1/agents/lead", "alice", agentYAML("lead", "Lead.")+"  subagents: [{name: helper, agent: coding-agent}]\n").decode(t, &lead)
	if len(lead.Spec.Subagents) != 1 || lead.Spec.Subagents[0].Agent != a.Status.ID+"@1" {
		t.Fatalf("alice's subagent coding-agent pins %+v, want %s@1", lead.Spec.Subagents, a.Status.ID)
	}
}

func TestAnArchivedAgentTakesNoVersionAndNoSession(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	// Archiving cannot be undone, so it waits for an explicit confirmation
	// and archives nothing without one.
	for _, body := range []string{"", `{}`, `{"permanent":false}`} {
		if a := f.do(http.MethodPost, "/v1/agents/reviewer/archive", "alice", body); a.code() != CodeConfirmationRequired {
			t.Fatalf("an archive with body %q: %d %s", body, a.status, a.body)
		}
	}
	var live v1.Agent
	f.do(http.MethodGet, "/v1/agents/reviewer", "alice", "").decode(t, &live)
	if live.Status.ArchivedAt != nil {
		t.Fatal("an unconfirmed archive archived the agent")
	}
	a := f.do(http.MethodPost, "/v1/agents/reviewer/archive", "alice", `{"permanent":true}`)
	var archived v1.Agent
	a.decode(t, &archived)
	if a.status != http.StatusOK || archived.Status.ArchivedAt == nil {
		t.Fatalf("archive: %d %s", a.status, a.body)
	}
	if a := f.do(http.MethodPost, "/v1/agents/reviewer/archive", "alice", `{"permanent":true}`); a.code() != CodeConflict {
		t.Fatalf("a second archive: %d %s", a.status, a.body)
	}
	if a := f.do(http.MethodPut, "/v1/agents/reviewer", "alice", agentYAML("reviewer", "Again.")); a.code() != CodeConflict {
		t.Fatalf("a version of an archived agent: %d %s", a.status, a.body)
	}
	if a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer"}`); a.code() != CodeConflict {
		t.Fatalf("a session of an archived agent: %d %s", a.status, a.body)
	}
}

// TestPaging: every agent appears once across pages, the next page's URL
// is in a Link header, and the last page has no next_cursor.
func TestPaging(t *testing.T) {
	f := newFixture(t)
	for _, n := range []string{"a1", "a2", "a3", "a4", "a5"} {
		f.apply("alice", n, "x")
	}
	var names []string
	path := "/v1/agents?limit=2"
	for range 5 {
		a := f.do(http.MethodGet, path, "alice", "")
		var page struct {
			Items      []v1.Agent `json:"items"`
			NextCursor string     `json:"next_cursor"`
		}
		a.decode(t, &page)
		for _, it := range page.Items {
			names = append(names, it.Metadata.Name)
		}
		if page.NextCursor == "" {
			if strings.Contains(string(a.body), "next_cursor") || a.header.Get("Link") != "" {
				t.Fatalf("the last page names a next one: %s %v", a.body, a.header)
			}
			break
		}
		link := a.header.Get("Link")
		if !strings.HasPrefix(link, "<https://topos.example/v1/agents?") || !strings.HasSuffix(link, `>; rel="next"`) {
			t.Fatalf("Link %q", link)
		}
		path = strings.TrimPrefix(strings.TrimSuffix(link, `>; rel="next"`), "<https://topos.example")
	}
	if strings.Join(names, ",") != "a1,a2,a3,a4,a5" {
		t.Fatalf("the pages held %v", names)
	}
}
