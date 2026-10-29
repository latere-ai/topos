// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/session"
)

// TestTheSessionAndEventRoutesStateTheCostRule (spec 096): a client
// reads how a session's spending counts its log, and how to keep it
// current from the stream, where it reads the session and its events.
func TestTheSessionAndEventRoutesStateTheCostRule(t *testing.T) {
	raw, err := os.ReadFile(committed)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Description string `yaml:"description"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/sessions/{id}", "/sessions/{id}/events", "/sessions/{id}/stream"} {
		if !strings.Contains(doc.Paths[path]["get"].Description, CostRule) {
			t.Errorf("GET %s does not state the cost rule", path)
		}
	}
}

// TestTodoResultMetaIsWhatTheTodoToolRecords (spec 096): the document's
// TodoResultMeta names every member a todo call's meta holds and nothing
// it does not, its statuses are the ones the tool takes, a refused call
// records no meta, and an emptied list is todos [].
func TestTodoResultMetaIsWhatTheTodoToolRecords(t *testing.T) {
	raw, err := os.ReadFile(committed)
	if err != nil {
		t.Fatal(err)
	}
	type prop struct {
		Type     string   `yaml:"type"`
		Enum     []string `yaml:"enum"`
		MaxItems int      `yaml:"maxItems"`
		Required []string `yaml:"required"`
		Items    struct {
			Required   []string        `yaml:"required"`
			Properties map[string]prop `yaml:"properties"`
		} `yaml:"items"`
	}
	var doc struct {
		Components struct {
			Schemas struct {
				TodoResultMeta struct {
					Required   []string        `yaml:"required"`
					Properties map[string]prop `yaml:"properties"`
				} `yaml:"TodoResultMeta"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	sc := doc.Components.Schemas.TodoResultMeta
	todos, ok := sc.Properties["todos"]
	if !ok || todos.Type != "array" || todos.MaxItems != tools.TodoLimit || !slices.Equal(sc.Required, []string{"todos"}) {
		t.Fatalf("the schema %+v", sc)
	}

	var todo tools.Tool
	for _, b := range tools.Builtins() {
		if b.Definition().Name == tools.NameTodo {
			todo = b
		}
	}
	if todo == nil {
		t.Fatal("no todo tool")
	}
	var input struct {
		Properties struct {
			Todos struct {
				Items struct {
					Properties struct {
						Status struct {
							Enum []string `json:"enum"`
						} `json:"status"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"todos"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(todo.Definition().InputSchema, &input); err != nil {
		t.Fatal(err)
	}
	if got, want := todos.Items.Properties["status"].Enum, input.Properties.Todos.Items.Properties.Status.Enum; !slices.Equal(got, want) {
		t.Fatalf("the document's statuses %v, the tool's %v", got, want)
	}

	run := func(in string) tools.Result {
		t.Helper()
		res, err := todo.Run(t.Context(), tools.Call{ID: "toolu_1", Input: json.RawMessage(in)})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := run(`{"todos":[{"id":"a","content":"Read the listings","status":"completed"},{"id":"b","content":"Compare the prices","status":"in_progress"}]}`)
	b, err := json.Marshal(res.Meta)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string][]map[string]any
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatalf("the meta %s: %v", b, err)
	}
	if got := slices.Sorted(maps.Keys(meta)); !slices.Equal(got, slices.Sorted(maps.Keys(sc.Properties))) {
		t.Fatalf("the meta holds %v, the schema names %v", got, slices.Sorted(maps.Keys(sc.Properties)))
	}
	for _, item := range meta["todos"] {
		if got, want := slices.Sorted(maps.Keys(item)), slices.Sorted(maps.Keys(todos.Items.Properties)); !slices.Equal(got, want) {
			t.Fatalf("an item holds %v, the schema names %v", got, want)
		}
		if !slices.Contains(todos.Items.Properties["status"].Enum, item["status"].(string)) {
			t.Fatalf("the status %v is not in the schema", item["status"])
		}
	}

	if res := run(`{"todos":[{"id":"a","content":"One","status":"pending"},{"id":"a","content":"Two","status":"pending"}]}`); res.Outcome != tools.OutcomeError || res.Meta != nil {
		t.Fatalf("a refused call: %s, meta %+v", res.Outcome, res.Meta)
	}
	emptied := run(`{"todos":[]}`)
	if b, err := json.Marshal(emptied.Meta); err != nil || string(b) != `{"todos":[]}` {
		t.Fatalf("an emptied list is recorded as %s (%v)", b, err)
	}
}

// TestASessionsSpendingKeptCurrentFromItsStream (spec 096): a client
// that read a session and adds the cost_usd_micro of each later
// model.request and tool.result, as the events route answers them, has
// the next read's spending; a redacted result's tombstone keeps its cost;
// and a fork's own spending is its spent less carried.
func TestASessionsSpendingKeptCurrentFromItsStream(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	f.turn(s.ID, 1, "One finding.", 1200)

	read := func(id string) session.Session {
		t.Helper()
		var got session.Session
		a := f.do(http.MethodGet, "/v1/sessions/"+id, "alice", "")
		if a.status != http.StatusOK {
			t.Fatalf("read: %d %s", a.status, a.body)
		}
		a.decode(t, &got)
		return got
	}
	// costs adds up the cost of the events from seq on as a client reads
	// them: the cost_usd_micro of each model.request and tool.result.
	costs := func(id string, from uint64) (total int64, results []string) {
		t.Helper()
		var page struct {
			Items []struct {
				ID      string          `json:"id"`
				Type    string          `json:"type"`
				Payload json.RawMessage `json:"payload"`
			} `json:"items"`
		}
		f.do(http.MethodGet, "/v1/sessions/"+id+"/events?limit=100&from_seq="+strconv.FormatUint(from, 10), "alice", "").decode(t, &page)
		for _, e := range page.Items {
			if e.Type != string(session.TypeModelRequest) && e.Type != string(session.TypeToolResult) {
				continue
			}
			var p struct {
				Cost *int64 `json:"cost_usd_micro"`
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.Cost != nil {
				total += *p.Cost
			}
			if e.Type == string(session.TypeToolResult) {
				results = append(results, e.ID)
			}
		}
		return total, results
	}

	first := read(s.ID)
	if first.Budget.SpentCostUSDMicro != 1200 {
		t.Fatalf("the first read spent %d", first.Budget.SpentCostUSDMicro)
	}
	search := int64(10000)
	f.appendTo(s.ID, 2,
		session.UserMessage{Sender: s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: "Find three flats."}}},
		session.SessionStatus{Status: session.StatusRunning},
		session.ModelRequest{Model: "claude-haiku-4-5", CostUSDMicro: new(int64(300)), Outcome: "ok"},
		session.AgentToolUse{ToolUseID: "toolu_s", Name: tools.NameWebSearch, Input: json.RawMessage(`{"query":"flats"}`), Verdict: "allow"},
		session.ToolResult{ToolUseID: "toolu_s", Content: []lux.Block{{Type: ir.BlockText, Text: "3 results"}}, Outcome: tools.OutcomeOK, CostUSDMicro: &search},
		session.ModelRequest{Model: "claude-haiku-4-5", Outcome: "ok"},
	)
	f.turn(s.ID, 2, "Three flats.", 500)

	added, results := costs(s.ID, first.LastSeq+1)
	second := read(s.ID)
	if added != 300+10000+500 || second.Budget.SpentCostUSDMicro != first.Budget.SpentCostUSDMicro+added {
		t.Fatalf("the stream added %d to %d, the next read spent %d", added, first.Budget.SpentCostUSDMicro, second.Budget.SpentCostUSDMicro)
	}

	if len(results) != 1 {
		t.Fatalf("the tool results %v", results)
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events/"+results[0]+"/redact", "alice", `{}`); a.status != http.StatusNoContent {
		t.Fatalf("redact: %d %s", a.status, a.body)
	}
	all, _ := costs(s.ID, 1)
	if third := read(s.ID); third.Budget.SpentCostUSDMicro != second.Budget.SpentCostUSDMicro || all != third.Budget.SpentCostUSDMicro {
		t.Fatalf("after the redaction the log adds up to %d, the read spent %d, before %d", all, third.Budget.SpentCostUSDMicro, second.Budget.SpentCostUSDMicro)
	}

	a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/fork", "alice", "")
	if a.status != http.StatusCreated {
		t.Fatalf("fork: %d %s", a.status, a.body)
	}
	var fork session.Session
	a.decode(t, &fork)
	if b := fork.Budget; b.SpentCostUSDMicro != second.Budget.SpentCostUSDMicro || b.CarriedCostUSDMicro != b.SpentCostUSDMicro {
		t.Fatalf("the fork's budget %+v, the parent spent %d", b, second.Budget.SpentCostUSDMicro)
	}
	f.appendTo(fork.ID, 3, session.UserMessage{Sender: fork.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: "And a fourth?"}}})
	f.turn(fork.ID, 3, "A fourth.", 50)
	own, _ := costs(fork.ID, fork.LastSeq+1)
	if b := read(fork.ID).Budget; b.SpentCostUSDMicro-b.CarriedCostUSDMicro != own || own != 50 {
		t.Fatalf("the fork's own spending %d, its events after the fork %d", b.SpentCostUSDMicro-b.CarriedCostUSDMicro, own)
	}
}
