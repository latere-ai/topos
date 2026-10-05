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

// successes are the statuses of the operation's success answers, in
// order.
func (d documented) successes(t *testing.T) []int {
	t.Helper()
	var out []int
	for _, code := range slices.Sorted(maps.Keys(d.Responses)) {
		if n, err := strconv.Atoi(code); err == nil && n >= 200 && n <= 299 {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s documents no success answer", d.OperationID)
	}
	return out
}

// answer is the operation's answer of status: its one media type and
// that type's body, none when the answer has no body.
func (d documented) answer(t *testing.T, status int) (media string, body shown) {
	t.Helper()
	content := d.Responses[strconv.Itoa(status)].Content
	if len(content) > 1 {
		t.Fatalf("%s answers %d media types", d.OperationID, len(content))
	}
	for m, b := range content {
		media, body = m, b
	}
	return media, body
}

// answers is the operation's first success answer.
func (d documented) answers(t *testing.T) (status int, media string, body shown) {
	t.Helper()
	status = d.successes(t)[0]
	media, body = d.answer(t, status)
	return status, media, body
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
// mints compared by where they appear. An apply is driven as the one
// that creates its object and as the one that changes it, each held to
// the answer the document shows for its status, and a route the document
// shows no request body for is sent none. An example a handler no longer
// answers, a success status the document does not list, and a route the
// table gains without a step here, fail it.
func TestTheExamplesAreWhatTheRoutesAnswer(t *testing.T) {
	doc := readDocument(t)
	now := exampleTime(12, 0, 0)
	workdir := &memFiles{files: map[string][]byte{"/workspace/notes.bin": {0, 1, 2, 3}}}
	f := newFixture(t, func(o *Options) {
		o.Now = func() time.Time { return now }
		o.Objects = store.NewMemory(o.Now)
		o.Workspaces = workdir.workspaces
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

	// requests are the bodies the document shows for op, by media type in
	// the order JSON then YAML, or one empty body when it shows none. A
	// route that reads no body, or an empty one, has no request body in
	// the document, and every other has an example.
	type request struct{ media, body string }
	requests := func(op string) []request {
		t.Helper()
		content := doc[op].RequestBody.Content
		if rt := routes[op]; (rt.body > 0 && !emptyBodies[op]) != (len(content) > 0) {
			t.Fatalf("%s reads a body of at most %d bytes and the document shows %d request media types", op, rt.body, len(content))
		}
		var out []request
		for _, media := range []string{mediaJSON, mediaYAML} {
			ex := content[media].Example
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
			out = append(out, request{media, body})
		}
		if len(out) != len(content) {
			t.Fatalf("%s: %d of the document's %d request media types show an example", op, len(out), len(content))
		}
		if len(out) == 0 {
			return []request{{}}
		}
		return out
	}
	// address is the route of op with its path values and its query.
	address := func(op string, path map[string]string, query string) string {
		return "/v1" + pathParam.ReplaceAllStringFunc(routes[op].path, func(p string) string { return path[strings.Trim(p, "{}")] }) + query
	}
	// send asks op's route, wants status of it, and holds the body to the
	// example the document shows for its answer of the status as.
	send := func(op string, path map[string]string, query string, r request, status, as int) answer {
		t.Helper()
		a := f.do(routes[op].method, address(op, path, query), "alice", r.body, "Content-Type", r.media)
		if a.status != status {
			t.Fatalf("%s (%s): %d %s, want %d", op, r.media, a.status, a.body, status)
		}
		media, want := doc[op].answer(t, as)
		if got := a.header.Get("Content-Type"); got != media {
			t.Errorf("%s answers %d with the media type %q, the document shows %q", op, status, got, media)
		}
		if media == mediaJSON {
			var got any
			a.decode(t, &got)
			if g, w := live.of(canonical(t, got)), written.of(canonical(t, want.Example)); g != w {
				t.Errorf("%s (%s): the route answers %d\n%s\nthe document shows for %d\n%s", op, r.media, status, g, as, w)
			}
		}
		return a
	}
	// listed holds the success statuses the document lists for op to the
	// ones its route answers.
	listed := func(op string) {
		t.Helper()
		if _, ok := doc[op]; !ok {
			t.Fatalf("the document has no operation %s", op)
		}
		driven[op] = true
		want := []int{routes[op].status}
		if routes[op].creates {
			want = append(want, http.StatusCreated)
		}
		if got := doc[op].successes(t); !slices.Equal(got, want) {
			t.Fatalf("%s: the document lists the success answers %v, the route answers %v", op, got, want)
		}
	}
	// step drives op as the document shows it, each request example in
	// turn, and holds every answer to the document's example, returning
	// the last.
	step := func(op string, path map[string]string, query string) answer {
		t.Helper()
		listed(op)
		var a answer
		for _, r := range requests(op) {
			a = send(op, path, query, r, routes[op].status, routes[op].status)
		}
		return a
	}
	// apply drives an apply route through both of its answers. The first
	// request example creates the object and answers 201; the next finds
	// it there and leaves it, which answers 200 with what the create
	// answered. change, another manifest of the name, is then applied at
	// changed, so the request examples sent again at again change the
	// object back and then leave it, which answers what the document
	// shows for 200.
	apply := func(op string, path map[string]string, change string, changed, again time.Time) {
		t.Helper()
		listed(op)
		if !routes[op].creates {
			t.Fatalf("%s does not create its object", op)
		}
		for i, r := range requests(op) {
			status := http.StatusOK
			if i == 0 {
				status = http.StatusCreated
			}
			send(op, path, "", r, status, http.StatusCreated)
		}
		now = changed
		if a := f.do(routes[op].method, address(op, path, ""), "alice", change); a.status != http.StatusOK {
			t.Fatalf("%s, changed: %d %s", op, a.status, a.body)
		}
		now = again
		for _, r := range requests(op) {
			send(op, path, "", r, http.StatusOK, http.StatusOK)
		}
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

	// The agent: created, changed and changed back, which is its third
	// version, and a second agent so the list has a next page.
	agent := map[string]string{"name": "release-notes", "ref": "release-notes", "n": "3"}
	apply("applyAgent", agent, agentYAML("release-notes", "Write the release notes for a tag."), exampleTime(12, 1, 0), exampleTime(12, 2, 0))
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
	if file := step("getFile", ses, "?path=notes.bin"); string(file.body) != "\x00\x01\x02\x03" {
		t.Errorf("the file is %q", file.body)
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

	// The trigger: created, changed and changed back, with a second so
	// the list has a next page, and fired by an earlier delivery so its
	// firings have one too.
	now = exampleTime(12, 30, 0)
	trg := map[string]string{"name": "on-release", "ref": "on-release"}
	apply("applyTrigger", trg, triggerOf("release-notes", "on-release", "on: {product: github, verbs: [release.published]}, session: {message: Write the release notes.}"),
		exampleTime(12, 31, 0), exampleTime(12, 32, 0))
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
