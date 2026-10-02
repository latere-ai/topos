// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package toposcli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/dir"
)

// decisionService is a fake decision service that records the paths it is
// asked and suggests an allow.
type decisionService struct {
	mu    sync.Mutex
	paths []string
}

func (d *decisionService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.paths = append(d.paths, r.URL.Path)
	d.mu.Unlock()
	if r.URL.Path == "/v1/predictions" {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"verdict": "allow", "approve": 0.97, "uncertainty": 0.1,
			"thresholds": map[string]float64{"allow_above": 0.9, "block_below": 0.25},
			"audit_rate": 0, "source": "learned/1", "reason": "approved often",
		})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// topos run reads TOPOS_DECISIONS_URL and TOPOS_DECISIONS_TOKEN, records
// the service's suggestion on the call, and refuses the URL without the
// token.
func TestDecisionServiceConfiguration(t *testing.T) {
	f := setup(t)
	f.vars["TOPOS_DECISIONS_URL"] = "http://127.0.0.1:1"
	if code, _, errOut := f.run("run", "--model", model, "Hi."); code != ExitUsage || !strings.Contains(errOut, "TOPOS_DECISIONS_TOKEN") {
		t.Fatalf("the URL without the token: exit %d %q", code, errOut)
	}

	svc := &decisionService{}
	srv := httptest.NewServer(svc)
	defer srv.Close()
	g := setup(t)
	g.vars["TOPOS_DECISIONS_URL"], g.vars["TOPOS_DECISIONS_TOKEN"] = srv.URL, "tok"
	g.stub.Script(model, reply(text("Reading."), toolUse("toolu_1", "write", `{"path":"a.txt","content":"x"}`)), reply(text("Done.")))
	if code, _, errOut := g.run("run", "--model", model, "Write a.txt."); code != ExitOK && code != ExitWaiting {
		t.Fatalf("exit %d %q", code, errOut)
	}
	svc.mu.Lock()
	paths := append([]string(nil), svc.paths...)
	svc.mu.Unlock()
	if len(paths) < 2 || paths[0] != "/v1/predictions" || paths[1] != "/v1/decisions" {
		t.Fatalf("the service was asked %v", paths)
	}
	list := g.sessions()
	if len(list) != 1 {
		t.Fatalf("sessions %+v", list)
	}
	var found bool
	st, err := dir.Open(g.vars["TOPOS_DATA_DIR"])
	if err != nil {
		t.Fatal(err)
	}
	events, err := st.Events(t.Context(), list[0].ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Type != session.TypeAgentToolUse {
			continue
		}
		var use session.AgentToolUse
		if err := e.Decode(&use); err != nil {
			t.Fatal(err)
		}
		found = true
		if use.Suggestion == nil || use.Suggestion.Source != "learned/1" || use.ReviewProbability == nil {
			t.Errorf("agent.tool_use %+v", use)
		}
	}
	if !found {
		t.Fatal("no agent.tool_use")
	}
}
