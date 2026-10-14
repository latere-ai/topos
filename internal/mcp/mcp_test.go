// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/mcpstub"
)

func text(s string) func(json.RawMessage) any {
	return func(json.RawMessage) any {
		return map[string]any{"resultType": "complete", "content": []any{map[string]any{"type": "text", "text": s}}}
	}
}

// trackerTools are a read tool, a write tool that answers on a stream and
// mirrors its title into a header, and two tools no session is offered:
// one whose name is no tool name, one whose header mark is on an array's
// item.
func trackerTools() []mcpstub.Tool {
	return []mcpstub.Tool{
		{Name: "list_issues", Description: "List the open issues.", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`), Answer: text("#1 The build is broken")},
		{Name: "create_issue", Description: "Open an issue.", Stream: true,
			InputSchema: json.RawMessage(`{"type":"object","minProperties":1,"properties":{"title":{"type":"string","x-mcp-header":"Title"},"meta":{"type":"object","properties":{"priority":{"type":"integer","x-mcp-header":"Priority"}}}},"required":["title"]}`),
			Answer: func(args json.RawMessage) any {
				var in struct {
					Title string `json:"title"`
				}
				if err := json.Unmarshal(args, &in); err != nil {
					return map[string]any{"content": []any{}, "isError": true}
				}
				return map[string]any{"content": []any{map[string]any{"type": "text", "text": "Opened #2: " + in.Title}}}
			}},
		{Name: "admin.reset", InputSchema: json.RawMessage(`{"type":"object"}`), Answer: text("reset")},
		{Name: "bulk", InputSchema: json.RawMessage(`{"type":"object","properties":{"ids":{"type":"array","items":{"type":"string","x-mcp-header":"Id"}}}}`), Answer: text("done")},
	}
}

func tracker(url string, token string) Server {
	c := session.Connection{Name: "tracker", Kind: session.ConnectionMCP, Title: "Issue tracker", Audience: "connections", Address: url, ReadOnly: []string{"list_*"}}
	return Server{Connection: c, Client: NewClient(url, func(context.Context) (string, error) { return token, nil }, nil, "test")}
}

func byName(ts []tools.Tool, name string) tools.Tool {
	for _, t := range ts {
		if t.Definition().Name == name {
			return t
		}
	}
	return nil
}

// TestStreamableHTTP: a server of revision 2026-07-28 is listed page by
// page and its tools are offered as mcp__<name>__<tool>, read for a
// read_only match and external otherwise, remote and parallel, a schema
// keyword the validator ignores dropped and named, and a tool with no
// tool name or a broken header mark left out; every request carries the
// session's bearer, the protocol's headers and metadata, and a call its
// marked arguments as headers, base64 where they are not plain; an answer
// on a stream is read past its notification to its response (spec 069).
func TestStreamableHTTP(t *testing.T) {
	ctx := t.Context()
	stub := mcpstub.New(t, mcpstub.Options{Admit: func(b string) bool { return b == "session-token" }, PageSize: 1, Tools: trackerTools()})
	ts, reduced, err := tracker(stub.URL(), "session-token").Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 || !slices.Equal(reduced, []string{"create_issue"}) {
		t.Fatalf("offered %d tools, reduced %v", len(ts), reduced)
	}
	list, create := byName(ts, "mcp__tracker__list_issues"), byName(ts, "mcp__tracker__create_issue")
	if list == nil || create == nil {
		t.Fatalf("the tools are %v", ts)
	}
	if p := list.Properties(); p.Effect != tools.EffectRead || !p.Remote || !p.Parallel || p.Repeatable {
		t.Fatalf("list_issues %+v", p)
	}
	if p := create.Properties(); p.Effect != tools.EffectExternal || !p.Remote {
		t.Fatalf("create_issue %+v", p)
	}
	if d := create.Definition(); strings.Contains(string(d.InputSchema), "minProperties") || d.Description != "Open an issue." {
		t.Fatalf("create_issue's definition %s", d.InputSchema)
	}
	reg := tools.NewRegistry()
	for _, tool := range ts {
		if err := reg.Add(tool); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(stub.Requests()); n != 4 {
		t.Fatalf("the list took %d requests, want one per page", n)
	}

	res, err := list.Run(ctx, tools.Call{ID: "toolu_1", Input: json.RawMessage(`{}`)})
	if err != nil || res.Outcome != tools.OutcomeOK || res.Content[0].Text != "#1 The build is broken" {
		t.Fatalf("list_issues: %+v %v", res, err)
	}
	res, err = create.Run(ctx, tools.Call{ID: "toolu_2", Input: json.RawMessage(`{"title":"Fix the build","meta":{"priority":2}}`)})
	if err != nil || res.Outcome != tools.OutcomeOK || res.Content[0].Text != "Opened #2: Fix the build" {
		t.Fatalf("create_issue: %+v %v", res, err)
	}
	reqs := stub.Requests()
	call := reqs[len(reqs)-1]
	for h, want := range map[string]string{
		"Authorization": "Bearer session-token", "MCP-Protocol-Version": Revision, "Mcp-Method": "tools/call", "Mcp-Name": "create_issue",
		"Mcp-Param-Title": "Fix the build", "Mcp-Param-Priority": "2", "Content-Type": "application/json",
	} {
		if got := call.Header.Get(h); got != want {
			t.Errorf("%s is %q, want %q", h, got, want)
		}
	}
	var body struct {
		Params struct {
			Meta map[string]any `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(call.Body, &body); err != nil || body.Params.Meta["io.modelcontextprotocol/protocolVersion"] != Revision ||
		body.Params.Meta["io.modelcontextprotocol/clientInfo"].(map[string]any)["name"] != "topos" {
		t.Fatalf("the call's metadata %s", call.Body)
	}
	if _, err := create.Run(ctx, tools.Call{ID: "toolu_3", Input: json.RawMessage(`{"title":" Überprüfen "}`)}); err != nil {
		t.Fatal(err)
	}
	reqs = stub.Requests()
	if got := reqs[len(reqs)-1].Header.Get("Mcp-Param-Title"); got != "=?base64?"+base64Std(" Überprüfen ")+"?=" {
		t.Fatalf("a title that is not plain went as %q", got)
	}
	if _, err := create.Run(ctx, tools.Call{ID: "toolu_4", Input: json.RawMessage(`{"title":"No priority","meta":{"priority":null}}`)}); err != nil {
		t.Fatal(err)
	}
	reqs = stub.Requests()
	if _, sent := reqs[len(reqs)-1].Header["Mcp-Param-Priority"]; sent {
		t.Fatal("a null argument was mirrored")
	}
}

// TestAHiddenToolIsNotOffered: a tool a glob of the connection's hidden
// matches is left out of the session's tools, a read_only match too, and a
// hidden tool whose schema the validator reduces is not named as reduced
// (spec 069).
func TestAHiddenToolIsNotOffered(t *testing.T) {
	stub := mcpstub.New(t, mcpstub.Options{Admit: func(b string) bool { return b == "session-token" }, Tools: append(trackerTools(),
		mcpstub.Tool{Name: "list_secrets", InputSchema: json.RawMessage(`{"type":"object"}`), Answer: text("hunter2")},
		mcpstub.Tool{Name: "delete_issue", InputSchema: json.RawMessage(`{"type":"object"}`), Answer: text("deleted")},
	)})
	s := tracker(stub.URL(), "session-token")
	s.Connection.Hidden = []string{"delete_*", "list_secrets", "create_issue"}
	ts, reduced, err := s.Tools(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range ts {
		names = append(names, tool.Definition().Name)
	}
	if !slices.Equal(names, []string{"mcp__tracker__list_issues"}) || len(reduced) != 0 {
		t.Fatalf("offered %v, reduced %v", names, reduced)
	}
}

// TestANotConnectedServer: a connection service's 409
// connection_not_connected fails a call with a result naming the
// connection and the reason, which asks for the connection again, and
// fails a listing with NotConnected (spec 069).
func TestANotConnectedServer(t *testing.T) {
	ctx := t.Context()
	stub := mcpstub.New(t, mcpstub.Options{Tools: trackerTools()})
	s := tracker(stub.URL(), "t")
	ts, _, err := s.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stub.Disconnect("revoked", "issues:write")
	res, err := byName(ts, "mcp__tracker__create_issue").Run(ctx, tools.Call{ID: "toolu_1", Input: json.RawMessage(`{"title":"x"}`)})
	if err != nil || res.Outcome != tools.OutcomeError {
		t.Fatalf("a call to a server not connected: %+v %v", res, err)
	}
	if want := prompts.Render(prompts.CallNotConnected, prompts.Data{"Title": "Issue tracker", "Reason": "revoked"}); res.Content[0].Text != want {
		t.Fatalf("the result says %q", res.Content[0].Text)
	}
	if r := res.Requested; r == nil || r.Name != "tracker" || r.Title != "Issue tracker" || r.Kind != session.ConnectionMCP || r.Reason != "revoked" || !slices.Equal(r.Scopes, []string{"issues:write"}) {
		t.Fatalf("the request to connect %+v", res.Requested)
	}
	var nc *NotConnected
	if _, _, err := s.Tools(ctx); !errors.As(err, &nc) || nc.Reason != "revoked" || nc.State != "needs_reconnect" || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("a listing of a server not connected: %v", err)
	}
	stub.Disconnect("")
	stub.Disconnect("", "x")
	if (&NotConnected{}).Error() != "the connection is not connected: no reason given" {
		t.Fatal("a reasonless refusal reads wrong")
	}
}

// TestAnEarlierRevision: a server that answers 400 with no error of the
// current revision is reached through revision 2025-11-25's handshake,
// in the session it opens, with the version it negotiated; a session the
// server ended is opened again once (spec 069).
func TestAnEarlierRevision(t *testing.T) {
	ctx := t.Context()
	stub := mcpstub.New(t, mcpstub.Options{Legacy: true, Tools: trackerTools()})
	s := tracker(stub.URL(), "t")
	ts, _, err := s.Tools(ctx)
	if err != nil || len(ts) != 2 {
		t.Fatalf("an earlier revision's tools: %d %v", len(ts), err)
	}
	var methods []string
	for _, r := range stub.Requests() {
		methods = append(methods, r.Method)
	}
	if want := []string{"tools/list", "initialize", "notifications/initialized", "tools/list"}; !slices.Equal(methods, want) {
		t.Fatalf("the requests were %v, want %v", methods, want)
	}
	last := stub.Requests()[3]
	if last.Header.Get("Mcp-Session-Id") != "sess-1" || last.Header.Get("MCP-Protocol-Version") != Legacy {
		t.Fatalf("a request in the session carried %v", last.Header)
	}
	stub.EndSessions()
	res, err := byName(ts, "mcp__tracker__list_issues").Run(ctx, tools.Call{ID: "toolu_1", Input: json.RawMessage(`{}`)})
	if err != nil || res.Outcome != tools.OutcomeOK {
		t.Fatalf("a call after the session ended: %+v %v", res, err)
	}
	reqs := stub.Requests()
	if got := reqs[len(reqs)-1].Header.Get("Mcp-Session-Id"); got != "sess-2" {
		t.Fatalf("the call went in session %q", got)
	}
}

// TestWhatAResultHolds: text, a link, an embedded resource, an image and
// an unknown kind of content reach the model; isError fails the call; a
// result of structured content alone is its JSON; an empty one says so;
// and one that asks for input fails the call.
func TestWhatAResultHolds(t *testing.T) {
	ctx := t.Context()
	answers := map[string]any{
		"mixed": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "Summary"},
			map[string]any{"type": "resource_link", "uri": "https://tracker.example.com/2", "name": "Issue 2"},
			map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///a", "text": "embedded"}},
			map[string]any{"type": "resource", "resource": map[string]any{"uri": "file:///b", "blob": "aGk="}},
			map[string]any{"type": "audio", "data": "aGk=", "mimeType": "audio/wav"},
			map[string]any{"type": "image", "data": "aGk=", "mimeType": "image/png"},
		}},
		"failed":     map[string]any{"content": []any{map[string]any{"type": "text", "text": "no such project"}}, "isError": true},
		"structured": map[string]any{"content": []any{}, "structuredContent": map[string]any{"count": 2}},
		"empty":      map[string]any{"content": []any{}},
		"input":      map[string]any{"resultType": "input_required", "inputRequests": map[string]any{}},
	}
	var defs []mcpstub.Tool
	for name, a := range answers {
		defs = append(defs, mcpstub.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`), Answer: func(json.RawMessage) any { return a }})
	}
	stub := mcpstub.New(t, mcpstub.Options{Tools: defs})
	ts, _, err := tracker(stub.URL(), "t").Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run := func(name string) tools.Result {
		t.Helper()
		res, err := byName(ts, "mcp__tracker__"+name).Run(ctx, tools.Call{ID: "toolu_" + name})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if r := run("mixed"); r.Outcome != tools.OutcomeOK || len(r.Content) != 2 || r.Content[0].Text != "Summary\nIssue 2 https://tracker.example.com/2\nembedded\nfile:///b\n[audio]" ||
		r.Content[1].Type != ir.BlockImage || r.Content[1].Image.MediaType != "image/png" {
		t.Fatalf("mixed: %+v", r)
	}
	if r := run("failed"); r.Outcome != tools.OutcomeError || r.Content[0].Text != "no such project" {
		t.Fatalf("failed: %+v", r)
	}
	if r := run("structured"); r.Content[0].Text != `{"count":2}` {
		t.Fatalf("structured: %+v", r)
	}
	if r := run("empty"); r.Content[0].Text != prompts.Text(prompts.CallNoContent) {
		t.Fatalf("empty: %+v", r)
	}
	if r := run("input"); r.Outcome != tools.OutcomeError || r.Content[0].Text != prompts.Render(prompts.CallInputRequired, prompts.Data{"Title": "Issue tracker"}) {
		t.Fatalf("input: %+v", r)
	}
}

// TestTheClientsErrors: an error of the current revision on a 400 is the
// server's error and no fallback, a JSON-RPC error is the call's, and any
// other answer is an error naming its status; a token that cannot be had
// sends nothing.
func TestTheClientsErrors(t *testing.T) {
	ctx := t.Context()
	answer := func(status int, contentType, body string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if contentType != "" {
				w.Header().Set("Content-Type", contentType)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	for name, c := range map[string]struct {
		url  string
		want func(error) bool
	}{
		"a modern 400": {answer(http.StatusBadRequest, "application/json", `{"jsonrpc":"2.0","id":1,"error":{"code":-32022,"message":"Unsupported protocol version"}}`),
			func(err error) bool { var e *RPCError; return errors.As(err, &e) && e.Code == -32022 }},
		"a JSON-RPC error": {answer(http.StatusOK, "application/json", `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"boom"}}`),
			func(err error) bool {
				var e *RPCError
				return errors.As(err, &e) && strings.Contains(e.Error(), "boom")
			}},
		"a server error": {answer(http.StatusInternalServerError, "", "down for maintenance"),
			func(err error) bool {
				var e *StatusError
				return errors.As(err, &e) && e.Status == 500 && strings.Contains(e.Error(), "down for maintenance")
			}},
		"another 409": {answer(http.StatusConflict, "application/json", `{"error":{"code":"wrong_kind"}}`),
			func(err error) bool { var e *StatusError; return errors.As(err, &e) && e.Status == 409 }},
		"a body that is not JSON": {answer(http.StatusOK, "application/json", `<html>`),
			func(err error) bool { return err != nil && strings.Contains(err.Error(), "not a JSON-RPC response") }},
		"a stream with no response": {answer(http.StatusOK, "text/event-stream", "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n"),
			func(err error) bool { return err != nil && strings.Contains(err.Error(), "ended before the response") }},
		"a result that is no list": {answer(http.StatusOK, "application/json", `{"jsonrpc":"2.0","id":1,"result":[]}`),
			func(err error) bool { return err != nil && strings.Contains(err.Error(), "tools/list answered") }},
		"an earlier revision that fails its handshake": {answer(http.StatusBadRequest, "text/plain", "no"),
			func(err error) bool { var e *StatusError; return errors.As(err, &e) && e.Status == 400 }},
	} {
		_, _, err := tracker(c.url, "t").Tools(ctx)
		if !c.want(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	s := tracker("http://127.0.0.1:1/mcp", "")
	s.Client = NewClient("http://127.0.0.1:1/mcp", func(context.Context) (string, error) { return "", errors.New("no token") }, nil, "test")
	if _, _, err := s.Tools(ctx); err == nil || !strings.Contains(err.Error(), "no token") {
		t.Fatalf("no token: %v", err)
	}
	if _, _, err := tracker("http://127.0.0.1:1/mcp", "t").Tools(ctx); err == nil {
		t.Fatal("a server that does not answer answered")
	}
}

// TestHeaderValues: a value is sent as it is when it is plain ASCII with
// no space around it, and in the base64 form otherwise, a value that looks
// like that form included; a header mark is a token.
func TestHeaderValues(t *testing.T) {
	for in, want := range map[string]string{
		"us-west1": "us-west1", "": "", "a\tb": "a\tb",
		"Hello, 世界": "=?base64?SGVsbG8sIOS4lueVjA==?=", " padded ": "=?base64?IHBhZGRlZCA=?=",
		"line1\nline2": "=?base64?bGluZTEKbGluZTI=?=", "=?base64?literal?=": "=?base64?PT9iYXNlNjQ/bGl0ZXJhbD89?=",
	} {
		if got := headerValue(in); got != want {
			t.Errorf("%q went as %q, want %q", in, got, want)
		}
	}
	for name, ok := range map[string]bool{"Region": true, "X-Id_2": true, "": false, "a b": false, "a:b": false} {
		if token(name) != ok {
			t.Errorf("%q a token: %v", name, !ok)
		}
	}
	if ps, ok := headerParams(json.RawMessage(`{"properties":{"a":{"type":"string","x-mcp-header":"A"},"b":{"type":"string","x-mcp-header":"a"}}}`)); ok {
		t.Fatalf("a name marked twice: %v", ps)
	}
	if _, ok := headerParams(json.RawMessage(`{"properties":{"a":{"type":"number","x-mcp-header":"A"}}}`)); ok {
		t.Fatal("a number was marked")
	}
	if _, ok := headerParams(json.RawMessage(`{"oneOf":[{"properties":{"a":{"type":"string","x-mcp-header":"A"}}}]}`)); ok {
		t.Fatal("a mark under oneOf was taken")
	}
	if _, ok := headerParams(json.RawMessage(`not json`)); ok {
		t.Fatal("a schema that is not JSON was read")
	}
	if ps, ok := headerParams(nil); !ok || ps != nil {
		t.Fatal("no schema")
	}
	if h := headersFor([]headerParam{{header: "A", path: []string{"a"}}}, json.RawMessage(`not json`)); h != nil {
		t.Fatalf("arguments that are not JSON mirrored %v", h)
	}
	if h := headersFor([]headerParam{{header: "A", path: []string{"a", "b"}}, {header: "B", path: []string{"c"}}}, json.RawMessage(`{"a":"x","c":true}`)); h.Get("Mcp-Param-A") != "" || h.Get("Mcp-Param-B") != "true" {
		t.Fatalf("mirrored %v", h)
	}
}
