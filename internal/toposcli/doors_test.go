// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package toposcli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
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
