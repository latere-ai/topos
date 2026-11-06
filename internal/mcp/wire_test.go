// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"encoding/json/jsontext"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestARequestIsWrittenForAServerWeDoNotOwn: members are in a fixed order,
// a nil list or object is null, and the call's raw arguments pass through
// as the model wrote them.
func TestARequestIsWrittenForAServerWeDoNotOwn(t *testing.T) {
	c := &Client{url: "http://server.test", token: func(context.Context) (string, error) { return "tok", nil }}
	body := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "t",
			"arguments": jsontext.Value(`{"z":1,"a":"x","a":"y"}`),
			"list":      []string(nil),
			"object":    map[string]string(nil),
		},
	}
	req, err := c.request(t.Context(), body, http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":1,"jsonrpc":"2.0","method":"tools/call","params":{"arguments":{"z":1,"a":"x","a":"y"},"list":null,"name":"t","object":null}}`
	if string(got) != want {
		t.Fatalf("the request is %s, want %s", got, want)
	}
}

// TestAnAnswerIsReadAsTheServerWroteIt: a duplicate member, a member name
// in another letter case and bytes that are not UTF-8 are read, not
// refused.
func TestAnAnswerIsReadAsTheServerWroteIt(t *testing.T) {
	c := &Client{}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","ID":1,"id":1,"Result":{"text":"a` + "\xff" + `b"}}`)),
	}
	res, err := c.answer(resp, "1", false)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"text":"a` + "\xff" + `b"}`; string(res) != want {
		t.Fatalf("the result is %q, want %q", res, want)
	}
}

// TestAnArgumentKeepsItsDigitsInItsHeader: a number marked for a header
// goes out as it was written, past what a float holds.
func TestAnArgumentKeepsItsDigitsInItsHeader(t *testing.T) {
	params := []headerParam{
		{header: "Id", path: []string{"meta", "id"}},
		{header: "Ratio", path: []string{"ratio"}},
		{header: "On", path: []string{"on"}},
		{header: "Title", path: []string{"title"}},
		{header: "Gone", path: []string{"gone"}},
		{header: "Missing", path: []string{"meta", "nothing"}},
	}
	h := headersFor(params, jsontext.Value(`{"meta":{"id":9007199254740993},"ratio":1.50,"on":true,"title":"Build","gone":null}`))
	for name, want := range map[string]string{"Mcp-Param-Id": "9007199254740993", "Mcp-Param-Ratio": "1.50", "Mcp-Param-On": "true", "Mcp-Param-Title": "Build"} {
		if got := h.Get(name); got != want {
			t.Errorf("%s is %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"Mcp-Param-Gone", "Mcp-Param-Missing"} {
		if _, ok := h[name]; ok {
			t.Errorf("%s was set", name)
		}
	}
}
