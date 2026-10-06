// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// TestAModelErrorsDetailCarriesTheGatewaysDetail: the session.error a
// failed model request ends a turn with carries the gateway's developer
// detail in its own detail, after the answer's status and type, as the
// failed model.request carries it: a provider's rejection, a spent budget,
// and a gateway's own refusal alike. A detail past models.MaxDetail is cut
// there on a character's boundary, and the message stays the error's.
func TestAModelErrorsDetailCarriesTheGatewaysDetail(t *testing.T) {
	const rejected = `upstream status 400: {"error":{"message":"Provider returned error","code":400}}`
	long := "x" + strings.Repeat("é", models.MaxDetail)
	cut := "x" + strings.Repeat("é", models.MaxDetail/2-1)
	for _, c := range []struct {
		name   string
		status int
		typ    string
		detail string
		stop   session.StopReason
		code   string
		want   string
	}{
		{"a provider's rejection", 400, "upstream_rejected", rejected, session.StopError, CodeModelError, "HTTP 400 upstream_rejected (" + rejected + ")"},
		{"a spent budget", 429, "budget_exhausted", "budget of the key spent for the window", session.StopBudget, "budget_exhausted", "HTTP 429 budget_exhausted (budget of the key spent for the window)"},
		{"a gateway's refusal", 403, "model_not_allowed", "the key selects 2 models", session.StopError, CodeModelError, "HTTP 403 model_not_allowed (the key selects 2 models)"},
		{"no detail", 400, "invalid_request", "", session.StopError, CodeModelError, "HTTP 400 invalid_request"},
		{"a detail past the bound", 400, "upstream_rejected", long, session.StopError, CodeModelError, "HTTP 400 upstream_rejected (" + cut + ")"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t, nil)
			ctx := t.Context()
			e.stub.Script(model, luxstub.Reply{Response: ir.Response{Model: model}, Fail: &luxstub.Failure{Status: c.status, Times: 9, Detail: c.detail,
				Body: `{"type":"error","error":{"type":"` + c.typ + `","message":"refused"}}`}})
			e.send(ctx, "Go.")
			if out := e.turn(ctx); out.StopReason != c.stop || out.Detail != c.code {
				t.Fatalf("outcome %+v", out)
			}
			errs := e.sessionErrors(ctx)
			if len(errs) != 1 || errs[0].Detail != c.want || !utf8.ValidString(errs[0].Detail) {
				t.Fatalf("session.error detail\n%q, want\n%q", errs[0].Detail, c.want)
			}
			if want := "models: HTTP " + strconv.Itoa(c.status) + ": " + c.typ + ": refused"; errs[0].Message != want {
				t.Fatalf("session.error message %q, want %q", errs[0].Message, want)
			}
		})
	}
}

// TestAFailedCompactionCarriesTheGatewaysDetail: a compaction whose
// summary request the gateway refuses ends the turn with
// compaction_failed, and its session.error's detail carries the answer's
// status, type and the gateway's developer detail.
func TestAFailedCompactionCarriesTheGatewaysDetail(t *testing.T) {
	e := setup(t, window(8000))
	ctx := t.Context()
	e.history(ctx, 8000, 8000, 8000, 8000, 40)
	e.stub.Script(model, luxstub.Reply{Response: ir.Response{Model: model}, Fail: &luxstub.Failure{Status: 400, Times: 9, Detail: "upstream status 413: too large",
		Body: `{"type":"error","error":{"type":"upstream_rejected","message":"The provider rejected this request."}}`}})
	if _, err := e.manage(ctx); err == nil {
		t.Fatal("a refused compaction went on")
	}
	errs := e.sessionErrors(ctx)
	if len(errs) != 1 || errs[0].Code != CodeCompactionFailed || errs[0].Detail != "HTTP 400 upstream_rejected (upstream status 413: too large)" {
		t.Fatalf("session.error %+v", errs)
	}
}
