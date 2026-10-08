// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/session"
)

func decision(t *testing.T, w any) authz.Decision {
	t.Helper()
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	return authz.Decision{Allow: true, Limits: raw}
}

func TestDecodeLimitsReadsEveryMember(t *testing.T) {
	budget := int64(2_000_000)
	w := WireLimits{
		AlwaysConfirm:  []string{"bash(git push*)"},
		AlwaysAllow:    []string{"read"},
		Thresholds:     &Thresholds{FlagAt: 0.2, AskAt: 0.4, BlockAt: 0.8},
		BudgetUSDMicro: &budget,
		TurnTimeout:    "30m",
		MaxAge:         "72h",
		Scope:          []json.RawMessage{json.RawMessage(`{"action":"repo.push","resource":"*"}`)},
		Retention:      "720h",
		Owner:          &Owner{Type: OwnerOrganization, ID: "org-1"},
		Model:          "vendor/model-a",
	}
	l, err := DecodeLimits(decision(t, w))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(l.AlwaysConfirm, w.AlwaysConfirm) || !slices.Equal(l.AlwaysAllow, w.AlwaysAllow) || *l.Thresholds != *w.Thresholds ||
		*l.BudgetUSDMicro != budget || l.TurnTimeout != 30*time.Minute || l.MaxAge != 72*time.Hour || l.Retention != 720*time.Hour || len(l.Scope) != 1 || *l.Owner != *w.Owner || l.Model != w.Model {
		t.Fatalf("DecodeLimits = %+v", l)
	}
	none, err := DecodeLimits(authz.Decision{Allow: true})
	if err != nil || none.BudgetUSDMicro != nil || none.Thresholds != nil || none.TurnTimeout != 0 || none.Model != "" {
		t.Fatalf("no limits decoded to %+v, %v", none, err)
	}
	zero := int64(0)
	if l, err := DecodeLimits(decision(t, WireLimits{BudgetUSDMicro: &zero})); err != nil || l.BudgetUSDMicro == nil || *l.BudgetUSDMicro != 0 {
		t.Fatalf("a zero ceiling is a ceiling: %+v, %v", l, err)
	}
	if _, err := DecodeLimits(decision(t, map[string]any{"a_member_from_later": 1})); err != nil {
		t.Fatalf("an unknown member: %v", err)
	}
}

func TestDecodeLimitsRefusesWhatItCannotApply(t *testing.T) {
	neg := int64(-1)
	for name, w := range map[string]any{
		"negative budget":   WireLimits{BudgetUSDMicro: &neg},
		"bad duration":      WireLimits{TurnTimeout: "soon"},
		"zero duration":     WireLimits{MaxAge: "0s"},
		"negative duration": WireLimits{Retention: "-1h"},
		"unordered":         WireLimits{Thresholds: &Thresholds{FlagAt: 0.6, AskAt: 0.4, BlockAt: 0.9}},
		"above one":         WireLimits{Thresholds: &Thresholds{FlagAt: 0.1, AskAt: 0.4, BlockAt: 1.5}},
		"scope entry":       WireLimits{Scope: []json.RawMessage{json.RawMessage(`"repo.push"`)}},
		"wrong type":        map[string]any{"turn_timeout": 30},
		"owner of no type":  WireLimits{Owner: &Owner{Type: "team", ID: "t"}},
		"owner with no id":  WireLimits{Owner: &Owner{Type: OwnerUser}},
		"model with space":  WireLimits{Model: " vendor/model-a"},
		"model of no name":  WireLimits{Model: " "},
		"model not a name":  map[string]any{"model": map[string]any{"name": "vendor/model-a"}},
		"reasoning unknown": map[string]any{"reasoning": "extreme"},
		"reasoning cased":   map[string]any{"reasoning": "High"},
		"reasoning number":  map[string]any{"reasoning": 3},
	} {
		if _, err := DecodeLimits(decision(t, w)); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
	if _, err := DecodeLimits(authz.Decision{Allow: true, Limits: json.RawMessage(`[`)}); err == nil || !strings.Contains(err.Error(), "limits") {
		t.Errorf("a malformed object: %v", err)
	}
}

// TestTheModelIsOneMemberOnTheWire: an allow that routes carries the
// model's name as limits.model, a string, and an allow that routes
// nothing carries no such member.
func TestTheModelIsOneMemberOnTheWire(t *testing.T) {
	raw, err := json.Marshal(WireLimits{Model: "vendor/model-a"})
	if err != nil || string(raw) != `{"model":"vendor/model-a"}` {
		t.Fatalf("the limits of a routed allow are %s, %v", raw, err)
	}
	if raw, err := json.Marshal(WireLimits{}); err != nil || string(raw) != `{}` {
		t.Fatalf("the limits of an allow that routes nothing are %s, %v", raw, err)
	}
	if l, err := DecodeLimits(authz.Decision{Allow: true, Limits: json.RawMessage(`{"model":""}`)}); err != nil || l.Model != "" {
		t.Fatalf("an empty model routes nothing: %+v, %v", l, err)
	}
}

// TestDecodeLimitsReadsARoute: limits.route is read beside limits.model as
// written, ignored without a model, and refused with space around it or
// as anything but a string (spec 061).
func TestDecodeLimitsReadsARoute(t *testing.T) {
	raw, err := json.Marshal(WireLimits{Model: "vendor/model-b", Route: "tier/thorough"})
	if err != nil || string(raw) != `{"model":"vendor/model-b","route":"tier/thorough"}` {
		t.Fatalf("the limits of an allow that names a route are %s, %v", raw, err)
	}
	if l, err := DecodeLimits(authz.Decision{Allow: true, Limits: raw}); err != nil || l.Model != "vendor/model-b" || l.Route != "tier/thorough" {
		t.Fatalf("a route beside a model decoded to %+v, %v", l, err)
	}
	if l, err := DecodeLimits(decision(t, WireLimits{Route: "tier/thorough"})); err != nil || l.Route != "" {
		t.Fatalf("a route with no model decoded to %+v, %v", l, err)
	}
	for name, w := range map[string]any{
		"route with space": WireLimits{Model: "vendor/model-b", Route: "tier/thorough "},
		"route not a name": map[string]any{"model": "vendor/model-b", "route": 3},
	} {
		if _, err := DecodeLimits(decision(t, w)); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

// TestTheReasoningLevelHasThreeStatesOnTheWire: limits.reasoning absent
// keeps the session's level, a level sets it, and "" returns the session
// to its agent's own, so the member is a pointer and "" is written out
// (spec 049).
func TestTheReasoningLevelHasThreeStatesOnTheWire(t *testing.T) {
	high, own := "high", ""
	for _, c := range []struct {
		name string
		in   *string
		wire string
	}{
		{"absent", nil, `{"model":"vendor/model-a"}`},
		{"a level", &high, `{"model":"vendor/model-a","reasoning":"high"}`},
		{"the agent's own", &own, `{"model":"vendor/model-a","reasoning":""}`},
	} {
		raw, err := json.Marshal(WireLimits{Model: "vendor/model-a", Reasoning: c.in})
		if err != nil || string(raw) != c.wire {
			t.Fatalf("%s: the limits are %s, %v", c.name, raw, err)
		}
		l, err := DecodeLimits(authz.Decision{Allow: true, Limits: raw})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if (l.Reasoning == nil) != (c.in == nil) || (l.Reasoning != nil && *l.Reasoning != *c.in) {
			t.Fatalf("%s: decoded to %v", c.name, l.Reasoning)
		}
	}
}

// TestDecodeNetwork: an allow's network is read with its hosts lowercased
// and sorted, and refused as one toposd cannot apply when its mode is
// none of the three, it names hosts or ask with a mode other than
// allowlist, a host is not a name or a "*." pattern, or it names more
// than MaxNetworkHosts hosts (spec 052).
func TestDecodeNetwork(t *testing.T) {
	l, err := DecodeLimits(decision(t, WireLimits{Network: &Network{Mode: "allowlist", Hosts: []string{"Docs.Example.com", "*.example.org", "docs.example.com"}, Ask: true}}))
	if err != nil || l.Network == nil || l.Network.Mode != "allowlist" || !l.Network.Ask || !slices.Equal(l.Network.Hosts, []string{"*.example.org", "docs.example.com"}) {
		t.Fatalf("network %+v, %v", l.Network, err)
	}
	for _, mode := range []string{"open", "none"} {
		if l, err := DecodeLimits(decision(t, map[string]any{"network": map[string]any{"mode": mode}})); err != nil || l.Network.Mode != mode || len(l.Network.Hosts) != 0 {
			t.Fatalf("%s: %+v, %v", mode, l.Network, err)
		}
	}
	if l, err := DecodeLimits(decision(t, WireLimits{})); err != nil || l.Network != nil {
		t.Fatalf("an allow without a network: %+v, %v", l.Network, err)
	}
	many := make([]string, MaxNetworkHosts+1)
	for i := range many {
		many[i] = fmt.Sprintf("h%d.example.com", i)
	}
	for name, n := range map[string]map[string]any{
		"no mode":         {"hosts": []string{"example.com"}},
		"a bad mode":      {"mode": "closed"},
		"hosts with open": {"mode": "open", "hosts": []string{"example.com"}},
		"hosts with none": {"mode": "none", "hosts": []string{"example.com"}},
		"ask with open":   {"mode": "open", "ask": true},
		"ask with none":   {"mode": "none", "ask": true},
		"a single label":  {"mode": "allowlist", "hosts": []string{"localhost"}},
		"an address":      {"mode": "allowlist", "hosts": []string{"192.0.2.1"}},
		"a port":          {"mode": "allowlist", "hosts": []string{"example.com:443"}},
		"a URL":           {"mode": "allowlist", "hosts": []string{"https://example.com"}},
		"a bare wildcard": {"mode": "allowlist", "hosts": []string{"*"}},
		"513 hosts":       {"mode": "allowlist", "hosts": many},
	} {
		if _, err := DecodeLimits(decision(t, map[string]any{"network": n})); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
	if l, err := DecodeLimits(decision(t, WireLimits{Network: &Network{Mode: "allowlist", Hosts: many[:MaxNetworkHosts]}})); err != nil || len(l.Network.Hosts) != MaxNetworkHosts {
		t.Fatalf("%d hosts: %v", MaxNetworkHosts, err)
	}
}

// TestDecodeInstructions: an allow's instructions are read as they are,
// up to MaxInitiatorInstructions bytes of valid UTF-8, and refused past it
// or when they are not UTF-8 (spec 053).
func TestDecodeInstructions(t *testing.T) {
	text := "Call me Ada. I work on the parser. " + strings.Repeat("ü", 10)
	if l, err := DecodeLimits(decision(t, WireLimits{Instructions: text})); err != nil || l.Instructions != text {
		t.Fatalf("instructions %q, %v", l.Instructions, err)
	}
	full := strings.Repeat("x", MaxInitiatorInstructions)
	if l, err := DecodeLimits(decision(t, WireLimits{Instructions: full})); err != nil || len(l.Instructions) != MaxInitiatorInstructions {
		t.Fatalf("%d bytes: %v", MaxInitiatorInstructions, err)
	}
	if _, err := DecodeLimits(decision(t, WireLimits{Instructions: full + "x"})); err == nil {
		t.Fatal("8 KiB plus one byte decoded")
	}
	// encoding/json replaces invalid UTF-8 when it encodes, so the raw
	// answer carries the bytes as a server that writes them would.
	raw := []byte(`{"instructions":"bad ` + "\xff\xfe" + ` text"}`)
	if _, err := DecodeLimits(authz.Decision{Allow: true, Limits: raw}); err == nil {
		t.Fatal("invalid UTF-8 decoded")
	}
	if l, err := DecodeLimits(decision(t, WireLimits{})); err != nil || l.Instructions != "" {
		t.Fatalf("no instructions: %q, %v", l.Instructions, err)
	}
}

// TestInstructionsEscapesAreHeldToUTF8: a lone surrogate escape, which
// encoding/json would read as a replacement character, is refused, and a
// pair, an escaped backslash and the other escapes are read.
func TestInstructionsEscapesAreHeldToUTF8(t *testing.T) {
	for raw, ok := range map[string]bool{
		`{"instructions":"a 😀 face"}`:             true,
		`{"instructions":"a \\ud800 text"}`:       true,
		`{"instructions":"tab\t \"q\" ü"}`:        true,
		`{"instructions":"lone \ud800 high"}`:     false,
		`{"instructions":"lone \udc00 low"}`:      false,
		`{"instructions":"swapped \udc00\ud800"}`: false,
		`{"instructions":null}`:                   true,
	} {
		if _, err := DecodeLimits(authz.Decision{Allow: true, Limits: []byte(raw)}); (err == nil) != ok {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

// TestTheAttachMembers: an allow's repositories are read as repository
// resources marked attached, each with its app, and its context as it is;
// a member past its bound or breaking its rule does not decode, and the
// error names it. files is not read by this release, so it is ignored as
// any unknown member is.
func TestTheAttachMembers(t *testing.T) {
	app := &session.ResourceApp{Slug: "tide-tables", Name: "Tide tables", URL: "https://tide-tables.apps.example.com"}
	w := WireLimits{
		Repositories: []WireRepository{{URL: "https://git.example.com/r/7f3c.git", App: app}, {URL: "https://git.example.com/acme/lib.git", Ref: "main"}},
		Context:      []session.ContextPart{{Title: "Project", Text: "Tide tables for the harbor club."}, {Title: "Memory", Text: "- ent_01: The club's color is navy.\n"}},
	}
	l, err := DecodeLimits(decision(t, w))
	if err != nil {
		t.Fatal(err)
	}
	want := []session.Resource{
		{Type: session.ResourceRepository, URL: w.Repositories[0].URL, App: app, Attached: true},
		{Type: session.ResourceRepository, URL: w.Repositories[1].URL, Ref: "main", Attached: true},
	}
	if len(l.Repositories) != 2 || l.Repositories[0].URL != want[0].URL || *l.Repositories[0].App != *app || !l.Repositories[0].Attached ||
		l.Repositories[1].Ref != "main" || l.Repositories[1].App != nil || !l.Repositories[1].Attached || l.Repositories[1].Type != session.ResourceRepository {
		t.Fatalf("repositories %+v", l.Repositories)
	}
	if !slices.Equal(l.Context, w.Context) {
		t.Fatalf("context %+v", l.Context)
	}
	if l, err := DecodeLimits(decision(t, map[string]any{"files": []any{map[string]any{"url": "https://storage.example.com/f", "path": "notes.md", "size": 2}}})); err != nil || l.Repositories != nil || l.Context != nil {
		t.Fatalf("an allow with files alone: %+v, %v", l, err)
	}
	many := make([]WireRepository, session.MaxRepositories+1)
	for i := range many {
		many[i] = WireRepository{URL: fmt.Sprintf("https://git.example.com/r/%d.git", i)}
	}
	for name, c := range map[string]struct {
		w     any
		names string
	}{
		"too many repositories": {WireLimits{Repositories: many}, "limits.repositories"},
		"an http repository":    {WireLimits{Repositories: []WireRepository{{URL: "http://git.example.com/r.git"}}}, "limits.repositories[0]"},
		"a credential":          {WireLimits{Repositories: []WireRepository{{URL: "https://u:p@git.example.com/r.git"}}}, "limits.repositories[0]"},
		"a ref of git's syntax": {WireLimits{Repositories: []WireRepository{{URL: "https://git.example.com/r.git", Ref: "--upload-pack=x"}}}, "limits.repositories[0]"},
		"a repeated url":        {WireLimits{Repositories: []WireRepository{{URL: "https://git.example.com/r.git"}, {URL: "https://git.example.com/r.git"}}}, "limits.repositories[1]"},
		"a bad slug":            {WireLimits{Repositories: []WireRepository{{URL: "https://git.example.com/r.git", App: &session.ResourceApp{Slug: "Tide", Name: "T", URL: app.URL}}}}, "limits.repositories[0].app"},
		"an app with no name":   {WireLimits{Repositories: []WireRepository{{URL: "https://git.example.com/r.git", App: &session.ResourceApp{Slug: "tide", URL: app.URL}}}}, "limits.repositories[0].app"},
		"an http app":           {WireLimits{Repositories: []WireRepository{{URL: "https://git.example.com/r.git", App: &session.ResourceApp{Slug: "tide", Name: "T", URL: "http://tide.example"}}}}, "limits.repositories[0].app"},
		"a repeated app": {WireLimits{Repositories: []WireRepository{
			{URL: "https://git.example.com/r/1.git", App: app}, {URL: "https://git.example.com/r/2.git", App: app},
		}}, "limits.repositories[1]"},
		"a repositories object":   {map[string]any{"repositories": map[string]any{"url": "https://git.example.com/r.git"}}, "limits"},
		"a context with no title": {WireLimits{Context: []session.ContextPart{{Text: "x"}}}, "limits.context[0].title"},
		"a context with no text":  {WireLimits{Context: []session.ContextPart{{Title: "P"}}}, "limits.context[0].text"},
		"a context too long":      {WireLimits{Context: []session.ContextPart{{Title: "P", Text: strings.Repeat("x", session.MaxContext)}}}, "limits.context is"},
		"a lone surrogate":        {json.RawMessage(`{"context":[{"title":"P","text":"\ud800"}]}`), "limits.context[0]"},
	} {
		if _, err := DecodeLimits(decision(t, c.w)); err == nil || !strings.Contains(err.Error(), c.names) {
			t.Errorf("%s: %v, want an error naming %s", name, err, c.names)
		}
	}
}
