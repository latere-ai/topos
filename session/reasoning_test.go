// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"encoding/json"
	"strings"
	"testing"
)

// oldModelRef is a session's model as a release before the rename stored
// and read it: the level under effort, and no other name for it.
type oldModelRef struct {
	Name   string `json:"name"`
	Via    string `json:"via,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// TestAHeaderKeepsItsLevelAcrossTheRename: a header stored before the
// rename names its level under effort and keeps it, and a header this
// release stores names it under effort alone, so an earlier release
// reads it with its level (spec 048).
func TestAHeaderKeepsItsLevelAcrossTheRename(t *testing.T) {
	var stored Session
	if err := json.Unmarshal([]byte(`{"schema":1,"id":"ses_1","model":{"name":"vendor/model-a","via":"tier/quick","effort":"high"}}`), &stored); err != nil {
		t.Fatal(err)
	}
	if m := stored.Model; m == nil || m.Level() != "high" || m.Effort != "high" || m.Via != "tier/quick" {
		t.Fatalf("the stored header's model reads %+v", m)
	}

	written, err := Marshal(Session{ID: "ses_2", Model: &ModelRef{Name: "vendor/model-a", Via: "tier/quick", Effort: "medium"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), `"model":{"name":"vendor/model-a","via":"tier/quick","effort":"medium"}`) || strings.Contains(string(written), "reasoning") {
		t.Fatalf("the header is stored as %s", written)
	}
	var earlier struct {
		Model *oldModelRef `json:"model"`
	}
	if err := json.Unmarshal(written, &earlier); err != nil {
		t.Fatal(err)
	}
	if earlier.Model == nil || *earlier.Model != (oldModelRef{Name: "vendor/model-a", Via: "tier/quick", Effort: "medium"}) {
		t.Fatalf("an earlier release reads %+v", earlier.Model)
	}

	// A session.model_changed is stored the same way.
	ev, err := NewEvent(TypeModelChanged, ModelChanged{Old: ModelRef{Name: "a"}, New: ModelRef{Name: "a", Effort: "low"}}, stored.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ev.Payload), `"new":{"name":"a","effort":"low"}`) || strings.Contains(string(ev.Payload), "reasoning") {
		t.Fatalf("the change is stored as %s", ev.Payload)
	}
}

// TestAModelRefAnswersItsLevelAsReasoning: the API's form of a model
// names the level under reasoning alone, whichever name held it.
func TestAModelRefAnswersItsLevelAsReasoning(t *testing.T) {
	for _, m := range []ModelRef{{Name: "a", Effort: "high"}, {Name: "a", Reasoning: "high"}} {
		if got := m.Answered(); got != (ModelRef{Name: "a", Reasoning: "high"}) || m.Level() != "high" {
			t.Errorf("%+v answers %+v", m, got)
		}
	}
	b, err := json.Marshal(ModelRef{Name: "a", Via: "tier/quick", Effort: "low"}.Answered())
	if err != nil || string(b) != `{"name":"a","via":"tier/quick","reasoning":"low"}` {
		t.Fatalf("answered as %s, %v", b, err)
	}
	if own := (ModelRef{Name: "a"}); own.Answered() != own {
		t.Fatalf("the model's own default moved: %+v", own.Answered())
	}
}
