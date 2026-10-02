// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/session"
)

// documented is one operation of the committed document, as far as its
// examples go.
type documented struct {
	OperationID string `yaml:"operationId"`
	RequestBody struct {
		Content map[string]shown `yaml:"content"`
	} `yaml:"requestBody"`
	Responses map[string]struct {
		Content map[string]shown `yaml:"content"`
	} `yaml:"responses"`
}

// shown is one media type of a body: its example, and its schema's
// format where it has one.
type shown struct {
	Example any `yaml:"example"`
	Schema  struct {
		Format string `yaml:"format"`
	} `yaml:"schema"`
}

// answers is the operation's first success answer: its status and its
// one media type, none when the answer has no body.
func (d documented) answers(t *testing.T) (status int, media string, body shown) {
	t.Helper()
	for _, code := range slices.Sorted(maps.Keys(d.Responses)) {
		n, err := strconv.Atoi(code)
		if err != nil || n < 200 || n > 299 {
			continue
		}
		content := d.Responses[code].Content
		if len(content) > 1 {
			t.Fatalf("%s answers %d media types", d.OperationID, len(content))
		}
		for m, b := range content {
			media, body = m, b
		}
		return n, media, body
	}
	t.Fatalf("%s documents no success answer", d.OperationID)
	return 0, "", shown{}
}

// readDocument reads the committed document's operations by id.
func readDocument(t *testing.T) map[string]documented {
	t.Helper()
	raw, err := os.ReadFile(committed)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]documented `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	ops := map[string]documented{}
	for _, methods := range doc.Paths {
		for _, op := range methods {
			ops[op.OperationID] = op
		}
	}
	return ops
}

// minted matches what a server mints anew on every run: an object's id
// and a digest that covers one.
var minted = regexp.MustCompile(`\b(?:agent|ses|evt|trg|frg)_[0-9A-HJKMNP-TV-Z]{26}\b|sha256:[0-9a-f]{64}`)

// numbering replaces each minted value by the order it first appeared
// in, so a body of one run compares to a body of another: the same
// objects in the same places, whatever their ids are.
type numbering map[string]string

func (n numbering) of(text string) string {
	return minted.ReplaceAllStringFunc(text, func(m string) string {
		if _, ok := n[m]; !ok {
			n[m] = "#" + strconv.Itoa(len(n)+1)
		}
		return n[m]
	})
}

// canonical renders a JSON value with its members sorted and its cursor,
// which is opaque and names a minted id, written as present or absent.
func canonical(t *testing.T, v any) string {
	t.Helper()
	if m, ok := v.(map[string]any); ok {
		if c, ok := m["next_cursor"]; ok && c != "" {
			m["next_cursor"] = "present"
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestTheExamplesAreWhatTheRoutesAnswer drives every route of the table
// through the one story the document's examples tell, on a clock set to
// the story's times, sending each request example the document shows and
// holding the route's answer to the response example beside it: the same
// status, media type, members and values, with the ids and digests a run
// mints compared by where they appear. An example a handler no longer
// answers, and a route the table gains without a step here, fail it.
func TestTheExamplesAreWhatTheRoutesAnswer(t *testing.T) {
	doc := readDocument(t)
	now := exampleTime(12, 0, 0)
	f := newFixture(t, func(o *Options) {
		o.Now = func() time.Time { return now }
		o.Objects = store.NewMemory(o.Now)
	})
	routes := map[string]route{}
	for _, rt := range table() {
		routes[rt.op] = rt
	}
	// An example's id is one its route reads: a prefixed ULID of its kind.
	for id, prefix := range map[string]string{
		exampleAgentID: session.PrefixAgent, exampleSessionID: session.PrefixSession, exampleStoppedID: session.PrefixSession,
		exampleForkID: session.PrefixSession, exampleFiredID: session.PrefixSession, exampleFirstEventID: session.PrefixEvent,
		exampleSecondEventID: session.PrefixEvent, exampleThirdEventID: session.PrefixEvent, exampleSentEventID: session.PrefixEvent,
		exampleTriggerID: session.PrefixTrigger, exampleFiringID: session.PrefixFiring,
	} {
		if err := session.CheckID(prefix, id); err != nil {
			t.Errorf("the example id %s: %v", id, err)
		}
	}
	live, written := numbering{}, numbering{}
	driven := map[string]bool{}

	// send asks op's route with the path values and the query given, and
	// body in the place of the document's request example when it is set.
	send := func(op string, path map[string]string, query, media, body string) answer {
		t.Helper()
		rt := routes[op]
		url := "/v1" + pathParam.ReplaceAllStringFunc(rt.path, func(p string) string { return path[strings.Trim(p, "{}")] }) + query
		a := f.do(rt.method, url, "alice", body, "Content-Type", media)
		if a.status != rt.status && (a.status != http.StatusCreated || rt.method != http.MethodPut) {
			t.Fatalf("%s: %d %s, want %d", op, a.status, a.body, rt.status)
		}
		return a
	}
	// step drives op as the document shows it and holds the answer to the
	// document's example, returning the answer.
	step := func(op string, path map[string]string, query string) answer {
		t.Helper()
		d, ok := doc[op]
		if !ok {
			t.Fatalf("the document has no operation %s", op)
		}
		driven[op] = true
		status, media, want := d.answers(t)
		if status != routes[op].status {
			t.Fatalf("%s: the document's first success answer is %d, the route's %d", op, status, routes[op].status)
		}
		var a answer
		sent := 0
		for _, kind := range []string{mediaJSON, mediaYAML} {
			ex := d.RequestBody.Content[kind].Example
			if ex == nil {
				continue
			}
			body, isText := ex.(string)
			if !isText {
				b, err := json.Marshal(ex)
				if err != nil {
					t.Fatal(err)
				}
				body = string(b)
			}
			a = send(op, path, query, kind, body)
			sent++
			if status != http.StatusNoContent && media == mediaJSON {
				var got any
				a.decode(t, &got)
				// Each request example is answered the same, so both
				// compare to the one response example under one numbering.
				if g, w := live.of(canonical(t, got)), written.of(canonical(t, want.Example)); g != w {
					t.Errorf("%s (%s): the route answers\n%s\nthe document shows\n%s", op, kind, g, w)
				}
			}
		}
		if sent == 0 {
			a = send(op, path, query, "", "")
			if media == mediaJSON {
				var got any
				a.decode(t, &got)
				if g, w := live.of(canonical(t, got)), written.of(canonical(t, want.Example)); g != w {
					t.Errorf("%s: the route answers\n%s\nthe document shows\n%s", op, g, w)
				}
			}
		}
		if got := a.header.Get("Content-Type"); got != media {
			t.Errorf("%s answers the media type %q, the document shows %q", op, got, media)
		}
		return a
	}
	// turn appends a turn of a session as a runner writes one, over 41
	// seconds from now: running, the agent's message when it wrote one,
	// the model request it cost, and idle with stop.
	turn := func(id, said string, stop session.StopReason) {
		t.Helper()
		s, err := f.sessions.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		cost := int64(1200)
		var batch []session.Event
		add := func(typ session.Type, payload any, after time.Duration) {
			e, err := session.NewEvent(typ, payload, now.Add(after))
			if err != nil {
				t.Fatal(err)
			}
			e.Turn = s.Turn + 1
			batch = append(batch, e)
		}
		add(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusRunning}, time.Second)
		if said != "" {
			add(session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockText, Text: said}}}, StopReason: ir.StopEndTurn}, 40*time.Second)
		}
		add(session.TypeModelRequest, session.ModelRequest{Model: "claude-haiku-4-5", CostUSDMicro: &cost, Outcome: "ok"}, 40*time.Second)
		add(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: stop}, 41*time.Second)
		session.Stamp(id, s.LastSeq, batch)
		if _, err := f.sessions.Append(t.Context(), id, s.LastSeq, batch); err != nil {
			t.Fatal(err)
		}
	}
	setup := func(method, path, body string, status int) answer {
		t.Helper()
		a := f.do(method, path, "alice", body)
		if a.status != status {
			t.Fatalf("%s %s: %d %s", method, path, a.status, a.body)
		}
		return a
	}
	id := func(a answer) string {
		t.Helper()
		var out struct {
			ID string `json:"id"`
		}
		a.decode(t, &out)
		return out.ID
	}

	// The agent: a first version, then the one the document applies, and
	// a second agent so the list has a next page.
	agent := map[string]string{"name": "release-notes", "ref": "release-notes", "n": "2"}
	setup(http.MethodPut, "/v1/agents/release-notes", agentYAML("release-notes", "Write the release notes for a tag."), http.StatusCreated)
	now = exampleTime(12, 2, 0)
	step("applyAgent", agent, "")
	f.apply("alice", "triage", "Triage the new issues.")
	step("listAgents", nil, "?limit=1")
	step("getAgent", agent, "")
	step("listAgentVersions", agent, "?limit=1")
	step("getAgentVersion", agent, "")

	// The session: created after another of the agent, so the list has a
	// next page, and taken through a turn, a change of the model, a
	// second message, a fork, its end and its archive.
	now = exampleTime(12, 3, 0)
	stopped := map[string]string{"id": f.create("alice", "release-notes").ID}
	now = exampleTime(12, 5, 0)
	created := step("createSession", nil, "")
	var s session.Session
	created.decode(t, &s)
	if link := created.header.Get("Link"); !strings.Contains(link, "/sessions/"+s.ID+"/stream") {
		t.Errorf("the created session's Link is %q", link)
	}
	ses := map[string]string{"id": s.ID, "digest": string(s.Agent.Bundle)}
	turn(s.ID, "The release notes for v1.4.0 are in NOTES.md.", session.StopEndTurn)
	now = exampleTime(12, 6, 0)
	step("listSessions", nil, "?limit=1")
	step("getSessionSummary", nil, "")
	step("getSession", ses, "")
	now = exampleTime(12, 10, 0)
	step("updateSession", ses, "")
	step("listEvents", ses, "?limit=1")
	now = exampleTime(12, 12, 0)
	ses["event_id"] = id(step("sendEvent", ses, ""))
	if blob := step("getBlob", ses, ""); len(blob.body) == 0 {
		t.Error("the blob is empty")
	}
	if _, media, body := doc["getBlob"].answers(t); media != mediaBytes || body.Schema.Format != "binary" || body.Example != nil {
		t.Errorf("the document shows a blob as %s %+v", media, body)
	}

	// The stream's example is the first frames of the log, a comment
	// line between them left out.
	driven["streamEvents"] = true
	_, media, frames := doc["streamEvents"].answers(t)
	want, _ := frames.Example.(string)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.srv.URL+"/v1/sessions/"+s.ID+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer alice")
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := resp.Header.Get("Content-Type"); resp.StatusCode != http.StatusOK || got != media || media != mediaStream {
		t.Fatalf("the stream: %d %s, the document shows %s", resp.StatusCode, got, media)
	}
	var got strings.Builder
	lines := bufio.NewScanner(resp.Body)
	lines.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for n, comment := 0, false; n < strings.Count(want, "\n\n") && lines.Scan(); {
		line := lines.Text()
		switch {
		case strings.HasPrefix(line, ":"):
			comment = true
		case line == "" && comment:
			comment = false
		default:
			fmt.Fprintln(&got, line)
			if line == "" {
				n++
			}
		}
	}
	cancel()
	if g, w := live.of(got.String()), written.of(want); g != w || w == "" {
		t.Errorf("the stream answers\n%s\nthe document shows\n%s", g, w)
	}

	now = exampleTime(12, 15, 0)
	step("forkSession", ses, "")
	now = exampleTime(12, 20, 0)
	step("endSession", ses, "")
	now = exampleTime(12, 25, 0)
	step("archiveSession", ses, "")
	now = exampleTime(12, 26, 0)
	step("unarchiveSession", ses, "")
	step("redactEvent", ses, "")

	// The other session stops on its budget, is resumed and deleted.
	now = exampleTime(12, 27, 0)
	turn(stopped["id"], "", session.StopBudget)
	now = exampleTime(12, 28, 0)
	step("resumeSession", stopped, "")
	step("deleteSession", stopped, "")

	// The trigger: applied, with a second so the list has a next page,
	// and fired by an earlier delivery so its firings have one too.
	now = exampleTime(12, 30, 0)
	trg := map[string]string{"name": "on-release", "ref": "on-release"}
	step("applyTrigger", trg, "")
	setup(http.MethodPut, "/v1/triggers/nightly", triggerOf("release-notes", "nightly", "schedule: '@daily', session: {message: Nightly.}"), http.StatusCreated)
	step("listTriggers", nil, "?limit=1")
	now = exampleTime(12, 35, 0)
	setup(http.MethodPost, "/v1/triggers/on-release/fire",
		`{"id":"0b8f2a44-7d1c-4c5e-9f3a-2e6d8c1b4a70","product":"github","verb":"release.published","resource":"example/widgets@v1.3.9","time":"2026-09-22T12:35:00Z"}`, http.StatusOK)
	now = exampleTime(12, 40, 0)
	step("fireTrigger", trg, "")
	step("getTrigger", trg, "")
	step("listFirings", trg, "?limit=1")
	step("deleteTrigger", map[string]string{"ref": "nightly"}, "")

	now = exampleTime(13, 0, 0)
	step("archiveAgent", agent, "")

	// The document's own example is the first lines of what is served.
	driven["getOpenAPI"] = true
	served := f.do(http.MethodGet, "/v1/openapi.yaml", "", "")
	var self struct {
		Paths map[string]map[string]documented `yaml:"paths"`
	}
	if err := yaml.Unmarshal(served.body, &self); err != nil {
		t.Fatal(err)
	}
	_, media, head := self.Paths["/openapi.yaml"]["get"].answers(t)
	if first, _ := head.Example.(string); first == "" || !strings.HasPrefix(string(served.body), first) || media != served.header.Get("Content-Type") {
		t.Errorf("the served document does not begin with its example %q", head.Example)
	}

	for _, rt := range table() {
		if !driven[rt.op] {
			t.Errorf("%s %s is not driven against its example", rt.method, rt.path)
		}
	}
}
