// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runnerapi

import (
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A runner's request is read strictly where the runner controls it and
// leniently where it carries model and process output: an unknown member
// or a second JSON value is refused, a repeated member and a byte that is
// not UTF-8 are read.
func TestDecodeReadsARunnerRequest(t *testing.T) {
	type body struct {
		Generation int64  `json:"generation"`
		Text       string `json:"text"`
	}
	read := func(raw string) (body, error) {
		var b body
		err := decode(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(raw)), &b)
		return b, err
	}
	if b, err := read(`{"generation":3,"text":"ok"}`); err != nil || b.Generation != 3 || b.Text != "ok" {
		t.Fatalf("a plain request: %+v, %v", b, err)
	}
	if b, err := read("{\"generation\":1,\"generation\":2,\"text\":\"a\xffb\"}"); err != nil || b.Generation != 2 || !strings.HasPrefix(b.Text, "a") {
		t.Fatalf("a repeated member and an invalid byte: %+v, %v", b, err)
	}
	for _, raw := range []string{`{"generation":1,"extra":true}`, `{"generation":1} {"generation":2}`, ``} {
		_, err := read(raw)
		var we *wireError
		if !errors.As(err, &we) || we.code != CodeInvalidRequest || we.status != http.StatusBadRequest {
			t.Errorf("%q: %v, want a 400 %s", raw, err, CodeInvalidRequest)
		}
	}
}

// A value that cannot be encoded is an error, and nothing reaches the
// client: no status, no header, no partial body.
func TestReplyWritesNothingForAValueItCannotEncode(t *testing.T) {
	w := httptest.NewRecorder()
	if err := reply(w, http.StatusOK, math.NaN()); err == nil {
		t.Fatal("NaN was encoded")
	}
	if w.Body.Len() != 0 || w.Header().Get("Content-Type") != "" {
		t.Fatalf("a failed reply wrote %q with %v", w.Body.String(), w.Header())
	}
}
