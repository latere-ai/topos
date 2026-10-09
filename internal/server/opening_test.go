// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/internal/triggers"
	"latere.ai/x/topos/session"
)

// allows is an authorizer whose allows of the questions a session's model
// is read at carry the limits answer gives, on top of the owner policy,
// and none when it gives the zero limits. It keeps those questions.
type allows struct {
	mu     sync.Mutex
	answer func(authz.Request) authorizer.WireLimits
	asked  []authz.Request
}

// allowBy makes f's authorizer one whose allows carry what answer gives.
func (f *fixture) allowBy(answer func(authz.Request) authorizer.WireLimits) *allows {
	a := &allows{answer: answer}
	next := f.authz.next
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		d, err := next.Authorize(context.Background(), req)
		if err != nil || !d.Allow {
			return d, err
		}
		switch req.Action {
		case authorizer.ActionSessionCreate, authorizer.ActionSessionFork, authorizer.ActionSessionUpdate, authorizer.ActionSessionSend:
		default:
			return d, nil
		}
		a.mu.Lock()
		a.asked = append(a.asked, req)
		l := a.answer(req)
		a.mu.Unlock()
		raw, err := json.Marshal(l)
		if err != nil || string(raw) == "{}" {
			return d, err
		}
		d.Limits = raw
		return d, nil
	}
	return a
}

// by answers every question by answer from here on.
func (a *allows) by(answer func(authz.Request) authorizer.WireLimits) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.answer = answer
}

// to answers every question with l from here on.
func (a *allows) to(l authorizer.WireLimits) {
	a.by(func(authz.Request) authorizer.WireLimits { return l })
}

// last is the last question of action asked.
func (a *allows) last(t *testing.T, action string) authz.Request {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, v := range slices.Backward(a.asked) {
		if v.Action == action {
			return v
		}
	}
	t.Fatalf("%s was never asked", action)
	return authz.Request{}
}

// ceiling sets TOPOS_AUTHORIZER_MESSAGE_TEXT on a fixture's server.
func ceiling(on bool) func(*Options) { return func(o *Options) { o.MessageText = on } }

// TestAnAllowAsksForTheOpening: an allow's message_text is kept on the
// session's model beside its route at a create, a PATCH and a send, in
// the header, in a read and on both sides of session.model_changed; an
// allow that names a model without it clears it, one that names no model
// changes nothing, a change of it alone at a send appends one change made
// by the service, a failover keeps it and a fork carries it. Keeping it
// does not wait for the operator's ceiling, which is off here (spec 063).
func TestAnAllowAsksForTheOpening(t *testing.T) {
	f, _ := checking(t)
	f.applyModel("auto", "name: "+auto)
	opens := authorizer.WireLimits{Model: haiku, Route: quickWay, MessageText: true}
	a := f.allowBy(func(authz.Request) authorizer.WireLimits { return opens })
	s := f.create("alice", "auto")
	asking := session.ModelRef{Name: haiku, Via: auto, Route: quickWay, MessageText: true}
	if s.Model == nil || *s.Model != asking || *f.header(s.ID).Model != asking {
		t.Fatalf("a create answered %+v", s.Model)
	}
	if read := f.do(http.MethodGet, "/v1/sessions/"+s.ID, "alice", ""); !strings.Contains(string(read.body), `"route":"tier/quick","message_text":true}`) {
		t.Fatalf("a read answers %s", read.body)
	}
	f.turn(s.ID, 1, "Answered.", 1)
	// The same model and route without the ask clears it, a change of its
	// own made by the service; the same allow again changes nothing.
	a.to(authorizer.WireLimits{Model: haiku, Route: quickWay})
	for range 2 {
		if r := f.send(s.ID, "Thanks."); r.status != http.StatusOK {
			t.Fatalf("send: %d %s", r.status, r.body)
		}
	}
	quiet := session.ModelRef{Name: haiku, Via: auto, Route: quickWay}
	changes := f.modelEvents(s.ID)
	if len(changes) != 1 || changes[0].Old != asking || changes[0].New != quiet || changes[0].By.Kind != session.SenderService {
		t.Fatalf("session.model_changed %+v", changes)
	}
	// Asked again, it is set again, another change of its own.
	a.to(opens)
	if r := f.send(s.ID, "A long task."); r.status != http.StatusOK {
		t.Fatalf("send: %d %s", r.status, r.body)
	}
	if changes = f.modelEvents(s.ID); len(changes) != 2 || changes[1].Old != quiet || changes[1].New != asking || *f.header(s.ID).Model != asking {
		t.Fatalf("session.model_changed %+v", changes)
	}
	// An allow that names no model changes nothing, the ask included.
	a.to(authorizer.WireLimits{Route: careWay})
	if r := f.send(s.ID, "Short."); r.status != http.StatusOK || len(f.modelEvents(s.ID)) != 2 || *f.header(s.ID).Model != asking {
		t.Fatalf("an allow with no model: %d, %+v", r.status, f.header(s.ID).Model)
	}
	// A PATCH whose allow names a model without the ask clears it, and
	// one that names it sets it, on both sides of its change.
	a.to(authorizer.WireLimits{Model: sonnet})
	var cleared session.Session
	f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"name":"`+quick+`"}}`).decode(t, &cleared)
	if cleared.Model == nil || *cleared.Model != (session.ModelRef{Name: sonnet, Via: quick}) || f.header(s.ID).Model.MessageText {
		t.Fatalf("a PATCH without the ask answered %+v", cleared.Model)
	}
	a.to(opens)
	var patched session.Session
	f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"name":"`+auto+`"}}`).decode(t, &patched)
	if patched.Model == nil || *patched.Model != asking {
		t.Fatalf("a PATCH with the ask answered %+v", patched.Model)
	}
	if changes = f.modelEvents(s.ID); changes[len(changes)-1].Old != (session.ModelRef{Name: sonnet, Via: quick}) || changes[len(changes)-1].New != asking {
		t.Fatalf("the PATCH's session.model_changed %+v", changes[len(changes)-1])
	}
	// A failover moves the model inside the turn and keeps the ask.
	a.by(func(req authz.Request) authorizer.WireLimits {
		if req.Action == authorizer.ActionSessionUpdate && req.Resource.String("failed_model") == haiku {
			return authorizer.WireLimits{Model: sonnet, Route: careWay}
		}
		return opens
	})
	next, err := f.api.Failover(t.Context(), s.ID, asking, asking, "", "")
	if err != nil || next != (session.ModelRef{Name: sonnet, Via: auto, Route: quickWay, MessageText: true}) {
		t.Fatalf("the failover answered %+v, %v", next, err)
	}
	// A fork starts on its parent's model at the fork point, the ask
	// included.
	f.turn(s.ID, 2, "Answered again.", 1)
	if fork := f.forked(s.ID, `{}`); fork.Model == nil || *fork.Model != asking {
		t.Fatalf("the fork starts on %+v", fork.Model)
	}
}

// TestAMessageCarriesItsOpeningOnlyWhenAsked: with the ceiling on and the
// session's model asking, the send question about a person's message
// carries its opening, the text of its text blocks joined by a blank
// line, its images not read, and a message without text carries none;
// with the ceiling off or the session not asking, no question carries
// one, and the question about any other event never does (spec 063).
func TestAMessageCarriesItsOpeningOnlyWhenAsked(t *testing.T) {
	for _, c := range []struct {
		name     string
		on, asks bool
	}{
		{"the ceiling on and the session asking", true, true},
		{"the ceiling off", false, true},
		{"the session not asking", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, _ := checking(t, ceiling(c.on))
			f.applyModel("auto", "name: "+auto)
			a := f.allowBy(func(authz.Request) authorizer.WireLimits {
				return authorizer.WireLimits{Model: haiku, Route: quickWay, MessageText: c.asks}
			})
			s := f.create("alice", "auto")
			image := `{"type":"image","image":{"media_type":"image/png","data":"` + b64(pngBytes) + `"}}`
			body := `{"type":"user.message","payload":{"content":[{"type":"text","text":"Here is the log."},` + image + `,{"type":"text","text":"Find why it fails."}]}}`
			if r := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", body); r.status != http.StatusOK {
				t.Fatalf("send: %d %s", r.status, r.body)
			}
			text, carried := a.last(t, authorizer.ActionSessionSend).Resource.Fields["message_text"]
			if want := c.on && c.asks; carried != want || (want && text != "Here is the log.\n\nFind why it fails.") {
				t.Fatalf("the send carried message_text %q, %v", text, carried)
			}
			if r := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", `{"type":"user.message","payload":{"content":[`+image+`]}}`); r.status != http.StatusOK {
				t.Fatalf("send an image: %d %s", r.status, r.body)
			}
			if fields := a.last(t, authorizer.ActionSessionSend).Resource.Fields; fields["message_text"] != nil {
				t.Fatalf("a message without text carried %v", fields)
			}
			f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", `{"type":"user.tool_confirmation","payload":{"tool_use_id":"toolu_9","decision":"allow"}}`)
			if confirm := a.last(t, authorizer.ActionSessionSend); confirm.Resource.String("event_type") != string(session.TypeUserToolConfirmation) || confirm.Resource.Fields["message_text"] != nil {
				t.Fatalf("a confirmation carried %v", confirm.Resource.Fields)
			}
		})
	}
}

// TestTheOpeningIsBounded: the opening is the whole text up to
// MaxMessageText code points, whatever their bytes, and past that its
// first MessageTextHead and its last MessageTextTail around a line that
// holds an ellipsis; the blank line between two blocks counts toward the
// bound, and a message with no text has none (spec 063).
func TestTheOpeningIsBounded(t *testing.T) {
	text := func(s string) lux.Block { return lux.Block{Type: ir.BlockText, Text: s} }
	image := lux.Block{Type: ir.BlockImage, Image: &lux.Image{MediaType: "image/png", Data: b64(pngBytes)}}
	// cut is the opening of a long text, counted in runes apart from the
	// code under test.
	cut := func(s string) string {
		r := []rune(s)
		return string(r[:MessageTextHead]) + "\n…\n" + string(r[len(r)-MessageTextTail:])
	}
	whole := strings.Repeat("ü", MaxMessageText)
	emoji := strings.Repeat("😀", MaxMessageText)
	long := strings.Repeat("h", MessageTextHead-1) + "ä" + "the middle" + strings.Repeat("t", MessageTextTail-1) + "€"
	half := strings.Repeat("x", MaxMessageText/2)
	for name, c := range map[string]struct {
		content []lux.Block
		want    string
	}{
		"a short text":                     {[]lux.Block{text("Find why it fails.")}, "Find why it fails."},
		"two bytes a code point, whole":    {[]lux.Block{text(whole)}, whole},
		"four bytes a code point, whole":   {[]lux.Block{text(emoji)}, emoji},
		"one code point past the bound":    {[]lux.Block{text(whole + "!")}, cut(whole + "!")},
		"a long text":                      {[]lux.Block{text(long)}, strings.Repeat("h", MessageTextHead-1) + "ä\n…\n" + strings.Repeat("t", MessageTextTail-1) + "€"},
		"blocks joined by a blank line":    {[]lux.Block{text("Here is the log."), image, text("Find why it fails.")}, "Here is the log.\n\nFind why it fails."},
		"the join counts toward the bound": {[]lux.Block{text(half), text(half)}, cut(half + "\n\n" + half)},
		"an image alone":                   {[]lux.Block{image}, ""},
		"no content":                       {nil, ""},
	} {
		if got := opening(c.content); got != c.want {
			t.Errorf("%s: the opening is %d code points %q..., want %d", name, len([]rune(got)), got[:min(len(got), 24)], len([]rune(c.want)))
		}
	}
	if n := len([]rune(cut(whole + "!"))); n != MessageTextHead+MessageTextTail+3 {
		t.Fatalf("a cut opening is %d code points", n)
	}
}

// TestEveryQuestionAboutAMessageCarriesItsOpening: a trigger's firing into
// an open session and a fork sent its message carry the opening under the
// same rule as the send route, the fork on its parent's model at the fork
// point; a create's first message carries none, since the session has no
// allow yet (spec 063).
func TestEveryQuestionAboutAMessageCarriesItsOpening(t *testing.T) {
	f, _ := checking(t, ceiling(true))
	f.applyModel("auto", "name: "+auto)
	a := f.allowBy(func(authz.Request) authorizer.WireLimits {
		return authorizer.WireLimits{Model: haiku, Route: quickWay, MessageText: true}
	})
	s := f.create("alice", "auto")
	if created := a.last(t, authorizer.ActionSessionCreate); created.Resource.Fields["message_text"] != nil || created.Resource.Fields["message_chars"] == nil {
		t.Fatalf("a create with its first message carried %v", created.Resource.Fields)
	}
	trigger := store.Trigger{ID: session.NewID(session.PrefixTrigger), Owner: "https://login.example|alice"}
	if err := (actor{f.api}).Send(t.Context(), triggers.Send{Trigger: trigger, SessionID: s.ID, Message: "The nightly build failed."}); err != nil {
		t.Fatal(err)
	}
	if fired := a.last(t, authorizer.ActionSessionSend); fired.Resource.String("message_text") != "The nightly build failed." ||
		fired.Resource.String("sender") != session.TriggerSubjectPrefix+trigger.ID {
		t.Fatalf("a trigger's firing carried %v", fired.Resource.Fields)
	}
	f.turn(s.ID, 1, "Answered.", 1)
	f.forked(s.ID, `{"message":{"content":[{"type":"text","text":"Try it another way."}]}}`)
	if forked := a.last(t, authorizer.ActionSessionSend); forked.Resource.String("message_text") != "Try it another way." || forked.Resource.String("model_route") != quickWay {
		t.Fatalf("a fork's message carried %v", forked.Resource.Fields)
	}
}

// TestAMessageForwardsItsWordToTheAuthorizer: a user.message at the send
// route and in a fork's message may carry askable and answers, which the
// send question carries under the same names, askable only when true, and
// no event of the log keeps; a value of another type, and an answers that
// is empty, past MaxAnswers bytes or has space around it, is refused as
// invalid_request (spec 063).
func TestAMessageForwardsItsWordToTheAuthorizer(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	a := f.allowBy(func(authz.Request) authorizer.WireLimits { return authorizer.WireLimits{} })
	s := f.create("alice", "reviewer")
	message := func(text, word string) string {
		return `{"content":[{"type":"text","text":"` + text + `"}]` + word + `}`
	}
	send := func(word string) answer {
		return f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", `{"type":"user.message","payload":`+message("Run it on the costly one.", word)+`}`)
	}
	kept := func(id string) {
		t.Helper()
		raw, err := json.Marshal(f.log(id))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"askable"`) || strings.Contains(string(raw), `"answers"`) {
			t.Fatalf("the log of %s keeps the message's word: %s", id, raw)
		}
	}
	bound := strings.Repeat("a", MaxAnswers)
	if r := send(`,"askable":true,"answers":"` + bound + `"`); r.status != http.StatusOK {
		t.Fatalf("send: %d %s", r.status, r.body)
	}
	if sent := a.last(t, authorizer.ActionSessionSend); sent.Resource.Fields["askable"] != true || sent.Resource.String("answers") != bound {
		t.Fatalf("the send carried %v", sent.Resource.Fields)
	}
	if r := send(`,"askable":false`); r.status != http.StatusOK {
		t.Fatalf("send: %d %s", r.status, r.body)
	}
	if sent := a.last(t, authorizer.ActionSessionSend); sent.Resource.Fields["askable"] != nil || sent.Resource.Fields["answers"] != nil {
		t.Fatalf("a message that is not askable carried %v", sent.Resource.Fields)
	}
	kept(s.ID)
	f.turn(s.ID, 1, "Answered.", 1)
	fork := f.forked(s.ID, `{"message":`+message("Again, on the costly one.", `,"askable":true,"answers":"ask_1"`)+`}`)
	if sent := a.last(t, authorizer.ActionSessionSend); sent.Resource.Fields["askable"] != true || sent.Resource.String("answers") != "ask_1" {
		t.Fatalf("the fork's message carried %v", sent.Resource.Fields)
	}
	kept(fork.ID)
	for name, word := range map[string]string{
		"askable a string":       `,"askable":"yes"`,
		"askable a number":       `,"askable":1`,
		"answers a number":       `,"answers":3`,
		"answers a list":         `,"answers":["ask_1"]`,
		"answers empty":          `,"answers":""`,
		"answers with space":     `,"answers":"ask_1 "`,
		"answers past the bound": `,"answers":"` + bound + `b"`,
	} {
		if r := send(word); r.status != http.StatusBadRequest || r.code() != CodeInvalidRequest {
			t.Errorf("%s at the send route: %d %s", name, r.status, r.body)
		}
		if r := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/fork", "alice", `{"message":`+message("Again.", word)+`}`); r.status != http.StatusBadRequest || r.code() != CodeInvalidRequest {
			t.Errorf("%s in a fork's message: %d %s", name, r.status, r.body)
		}
	}
}
