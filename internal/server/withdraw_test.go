// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/session"
)

// sendWhileRunning starts a turn of s and sends a message to it as alice,
// answering the appended event.
func (f *fixture) sendWhileRunning(id, body string) session.Event {
	f.t.Helper()
	f.appendTo(id, 1, session.SessionStatus{Status: session.StatusRunning})
	a := f.do(http.MethodPost, "/v1/sessions/"+id+"/events", "alice", body)
	if a.status != http.StatusOK {
		f.t.Fatalf("send: %d %s", a.status, a.body)
	}
	var e session.Event
	a.decode(f.t, &e)
	return e
}

func (f *fixture) withdraw(id, eventID string) answer {
	f.t.Helper()
	return f.do(http.MethodPost, "/v1/sessions/"+id+"/events", "alice", `{"type":"user.withdraw","payload":{"event_id":"`+eventID+`"}}`)
}

func TestWithdraw(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	evs, err := f.sessions.Events(t.Context(), s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	first := evs[0].ID
	m := f.sendWhileRunning(s.ID, `{"type":"user.message","payload":{"content":[{"type":"text","text":"Use port 9000."}],"send_now":true}}`)
	var sent session.UserMessage
	if err := m.Decode(&sent); err != nil || !sent.SendNow {
		t.Fatalf("a message sent now %+v, %v", sent, err)
	}
	f.authz.take()
	if a := f.withdraw(s.ID, m.ID); a.status != http.StatusOK {
		t.Fatalf("withdraw: %d %s", a.status, a.body)
	}
	if asked := f.authz.take(); len(asked) != 1 || asked[0] != authorizer.ActionSessionInterrupt {
		t.Fatalf("a withdrawal asked %v", asked)
	}
	all, err := f.sessions.Events(t.Context(), s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if w := session.WaitingMessages(all); len(w) != 1 || w[0].State != session.MessageWithdrawn {
		t.Fatalf("waiting %+v", w)
	}
	other := session.UserMessage{Sender: session.Sender{Subject: "usr_grace", Kind: session.SenderPerson}, Content: []lux.Block{{Type: ir.BlockText, Text: "Mine."}}}
	f.appendTo(s.ID, 1, other)
	all, err = f.sessions.Events(t.Context(), s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	theirs := all[len(all)-1].ID
	for _, c := range []struct {
		name, id, code, says string
	}{
		{"twice", m.ID, CodeConflict, "already withdrawn"},
		{"a message that started its turn", first, CodeConflict, "started its turn"},
		{"no message", "evt_none", CodeConflict, "no message of this session"},
		{"another sender's", theirs, auth.CodeForbidden, "only the person who sent"},
	} {
		a := f.withdraw(s.ID, c.id)
		if a.code() != c.code || !strings.Contains(string(a.body), c.says) {
			t.Errorf("%s: %d %s", c.name, a.status, a.body)
		}
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", `{"type":"user.withdraw","payload":{}}`); a.code() != CodeInvalidRequest {
		t.Fatalf("a withdrawal of nothing: %d %s", a.status, a.body)
	}
}

func TestAWithdrawalAfterTheReadIsRefused(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	m := f.sendWhileRunning(s.ID, `{"type":"user.message","payload":{"content":[{"type":"text","text":"Use port 9000."}]}}`)
	f.appendTo(s.ID, 1, session.MessagesRead{EventIDs: []string{m.ID}})
	if a := f.withdraw(s.ID, m.ID); a.code() != CodeConflict || !strings.Contains(string(a.body), "already read") {
		t.Fatalf("a withdrawal after the read: %d %s", a.status, a.body)
	}
}
