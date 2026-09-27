// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package toposcli

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/bridge"
	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/test/stubs/luxstub"
)

// TestRunReachesTheFamilysDoorFromLuxsRoot: TOPOS_MODELS_URL naming a
// Lux root sends each request to its family's door, and a URL that does
// not answer is an error naming the variable.
func TestRunReachesTheFamilysDoorFromLuxsRoot(t *testing.T) {
	f := setup(t)
	f.vars["TOPOS_MODELS_URL"] = f.stub.URL()
	f.stub.Script(model, reply(text("Hello.")))
	if code, _, errOut := f.run("run", "--model", model, "Say hello."); code != ExitOK {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if reqs := f.stub.Requests(); len(reqs) != 1 || reqs[0].Dialect != ir.DialectAnthropicMessages {
		t.Fatalf("the requests %+v", reqs)
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	f.vars["TOPOS_MODELS_URL"] = gone.URL
	if code, _, errOut := f.run("run", "--model", model, "Say hello."); code != ExitError || !strings.Contains(errOut, "TOPOS_MODELS_URL") {
		t.Fatalf("an unanswering URL: exit %d, stderr %q", code, errOut)
	}
}

// TestRunSizesTheRequestByTheDoorsFigures: the output limit a Lux door
// serves for the model, not the embedded catalog's, is the max_tokens
// of each request, and a model only the door knows runs; a door that
// does not answer its model list fails the run.
func TestRunSizesTheRequestByTheDoorsFigures(t *testing.T) {
	f := setup(t)
	f.vars["TOPOS_MODELS_URL"] = f.stub.URL()
	f.stub.Models(bridge.Model{Name: model, ContextWindow: 100_000, MaxOutputTokens: 1_234}, bridge.Model{Name: "vendor/door-only", ContextWindow: 8_000, MaxOutputTokens: 777})
	limit := func(want int64) func(*ir.Request) error {
		return func(r *ir.Request) error {
			if r.MaxTokens == nil || *r.MaxTokens != want {
				return fmt.Errorf("max_tokens %v, want the door's %d", r.MaxTokens, want)
			}
			return nil
		}
	}
	f.stub.Script(model, luxstub.Reply{Response: reply(text("Hello.")).Response, Expect: limit(1_234)})
	f.stub.Script("vendor/door-only", luxstub.Reply{Response: reply(text("Hi.")).Response, Expect: limit(777)})
	for _, m := range []string{model, "vendor/door-only"} {
		if code, _, errOut := f.run("run", "--model", m, "Say hello."); code != ExitOK {
			t.Fatalf("%s: exit %d, stderr %q", m, code, errOut)
		}
	}
	if len(f.stub.Listed()) != 2 {
		t.Fatalf("the door's model list was read %d times", len(f.stub.Listed()))
	}
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1/models") {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error(errors.New("no hijacker"))
				return
			}
			conn, _, err := hj.Hijack()
			if err == nil {
				err = conn.Close()
			}
			if err != nil {
				t.Error(err)
			}
			return
		}
		http.NotFound(w, r)
	}))
	defer refusing.Close()
	f.vars["TOPOS_MODELS_URL"] = refusing.URL + "/anthropic"
	if code, _, errOut := f.run("run", "--model", model, "Say hello."); code != ExitError || !strings.Contains(errOut, "model list") {
		t.Fatalf("a door that drops its model list: exit %d, stderr %q", code, errOut)
	}
}
