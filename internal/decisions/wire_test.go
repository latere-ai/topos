// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package decisions

import (
	"encoding/json/jsontext"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAnActionCarriesTheCallsInputAsItCame: the model's input is passed on
// whole, a duplicate member included, bytes that are not UTF-8 are written
// as the replacement character instead of failing the request, and a call
// with no input leaves the member out.
func TestAnActionCarriesTheCallsInputAsItCame(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	d, err := New(Options{URL: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		input jsontext.Value
		want  string
	}{
		{"duplicate members", jsontext.Value(`{"a":1,"a":2}`), `"input":{"a":1,"a":2},`},
		{"bytes that are not UTF-8", jsontext.Value("{\"a\":\"\xff\"}"), "\"input\":{\"a\":\"\uFFFD\"},"},
		{"an empty object", jsontext.Value(`{}`), `"input":{},`},
	} {
		a := action{Name: "bash", Input: c.input}
		if err := d.post(t.Context(), "/v1/predictions", a, nil); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !strings.Contains(string(body), c.want) {
			t.Errorf("%s: the body is %q, want it to hold %q", c.name, body, c.want)
		}
	}
	if err := d.post(t.Context(), "/v1/predictions", action{Name: "bash"}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"input"`) {
		t.Errorf("a call with no input sent %s", body)
	}
}
