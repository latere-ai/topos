// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/session"
)

// TestAnEditedMessageRunsOnAFork: through toposd, a person edits the
// second message of a conversation: one call forks the session before it
// with the edited text, a runner claims the fork at once and runs its
// turn on the history before the message and the edit, never the
// original, and the list reads the two sessions as one conversation
// standing by the fork.
func TestAnEditedMessageRunsOnAFork(t *testing.T) {
	edited := said("Four people: about 1200 euros.")
	edited.Expect = func(r *ir.Request) error {
		var texts []string
		for _, m := range r.Messages {
			for _, b := range m.Blocks {
				if b.Type == ir.BlockText {
					texts = append(texts, b.Text)
				}
			}
		}
		all := strings.Join(texts, "\n")
		switch {
		case !strings.Contains(all, "Plan the trip for three.") || !strings.Contains(all, "Planned for three."):
			return fmt.Errorf("the fork's request lacks the history before the edit: %q", texts)
		case strings.Contains(all, "And the budget?"):
			return fmt.Errorf("the fork's request carries the message it replaced: %q", texts)
		case !strings.HasSuffix(all, "And the budget for four?"):
			return fmt.Errorf("the fork's request does not end with the edit: %q", texts)
		}
		return nil
	}
	send, waitFor := serveOverTheStubs(t, said("Planned for three."), said("About 900 euros."), edited)
	code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"builder","title":"Trip","message":"Plan the trip for three."}`)
	var s session.Session
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &s) != nil {
		t.Fatalf("create: %d %s", code, body)
	}
	waitFor(s.ID, 1, session.StopEndTurn)
	code, body = send(http.MethodPost, "/v1/sessions/"+s.ID+"/events", `{"type":"user.message","payload":{"content":[{"type":"text","text":"And the budget?"}]}}`)
	var question session.Event
	if code != http.StatusOK || json.Unmarshal([]byte(body), &question) != nil {
		t.Fatalf("send: %d %s", code, body)
	}
	waitFor(s.ID, 2, session.StopEndTurn)

	code, body = send(http.MethodPost, "/v1/sessions/"+s.ID+"/fork",
		fmt.Sprintf(`{"before_seq":%d,"title":"Trip","message":{"content":[{"type":"text","text":"And the budget for four?"}]}}`, question.Seq))
	var fork session.Session
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &fork) != nil {
		t.Fatalf("fork: %d %s", code, body)
	}
	if fork.Root != s.ID || fork.Parent == nil || fork.Parent.Seq != question.Seq-1 || fork.Title != "Trip" || fork.LastSeq != question.Seq {
		t.Fatalf("the fork: %s", body)
	}
	done := waitFor(fork.ID, 2, session.StopEndTurn)
	if done.Budget.CarriedCostUSDMicro != fork.Budget.CarriedCostUSDMicro {
		t.Fatalf("the fork's carried spend moved from %d to %d", fork.Budget.CarriedCostUSDMicro, done.Budget.CarriedCostUSDMicro)
	}
	_, events := send(http.MethodGet, "/v1/sessions/"+fork.ID+"/events", "")
	if !strings.Contains(events, "Four people: about 1200 euros.") || strings.Contains(events, "About 900 euros.") {
		t.Fatalf("the fork's log: %s", events)
	}

	list := func(query string) []session.Session {
		t.Helper()
		code, body := send(http.MethodGet, "/v1/sessions?"+query, "")
		var page struct {
			Items []session.Session `json:"items"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(body), &page) != nil {
			t.Fatalf("list %s: %d %s", query, code, body)
		}
		return page.Items
	}
	var ids []string
	for _, item := range list("root=" + s.ID) {
		ids = append(ids, item.ID)
	}
	if !slices.Equal(ids, []string{fork.ID, s.ID}) {
		t.Fatalf("the conversation's sessions: %v", ids)
	}
	grouped := list("group=tree")
	if len(grouped) != 1 || grouped[0].ID != fork.ID || grouped[0].Tree == nil || *grouped[0].Tree != (session.Tree{Root: s.ID, Sessions: 2}) {
		t.Fatalf("the conversations: %+v", grouped)
	}
}
