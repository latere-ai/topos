// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/dialect"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

const titleModelName = "quick-model"

// withTitleModel connects titleModelName on the stub, as a hosted
// harness connects a model the session did not start on.
func withTitleModel(e **env) func(*Config) {
	return func(c *Config) {
		c.Connect = func(_ context.Context, name string) (models.Model, models.Connection, models.Entry, error) {
			if name != titleModelName {
				return nil, models.Connection{}, models.Entry{}, &models.Coded{Code: models.CodeUnknown, Message: name + " is not run here"}
			}
			return nil, models.Connection{BaseURL: (*e).stub.URL() + "/anthropic", Model: titleModelName, Family: models.FamilyAnthropic},
				models.Entry{Name: titleModelName, InputWindow: 100_000, MaxOutputTokens: 32, Pricing: &models.Pricing{Input: price("1"), Output: price("5")}}, nil
		}
	}
}

// ranTurn runs one turn a person started with text, answered with
// answer, and returns the session and its log as they stand after it.
func (e *env) ranTurn(ctx context.Context, text, answer string) (session.Session, []session.Event) {
	e.t.Helper()
	e.stub.Script(model, reply(ir.StopEndTurn, ir.Block{Type: ir.BlockThinking, Text: "thinking about it", Signature: "sig"}, ir.Block{Type: ir.BlockText, Text: answer}))
	e.send(ctx, text)
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		e.t.Fatalf("the turn ended %+v", out)
	}
	return e.now(ctx)
}

// now is the session and its log as they stand.
func (e *env) now(ctx context.Context) (session.Session, []session.Event) {
	e.t.Helper()
	s, err := e.store.Get(ctx, e.s.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return s, evs
}

func titleReply(answer string) luxstub.Reply {
	return luxstub.Reply{Response: ir.Response{Model: titleModelName, Blocks: []ir.Block{text(answer)}, StopReason: ir.StopEndTurn, Usage: ir.Usage{InputTokens: 200, OutputTokens: 6}}}
}

// TestATitleRequestNamesTheConversation: after a turn, Title sends one
// request to the first model of the names the installation runs, with
// the title prompt over the person's messages and the agent's answers,
// no system prompt, no tools, no reasoning setting, max_tokens 64 or the
// model's lower limit, and reads its response to the end without telling
// the observer. The answer is appended as a session.title_changed by the
// harness after its model.request of purpose title in one batch, which
// the session spends and which leaves its update time alone (spec 083).
func TestATitleRequestNamesTheConversation(t *testing.T) {
	var e *env
	seen := &recorder{}
	e = setup(t, func(c *Config) {
		withTitleModel(&e)(c)
		c.Observer = seen
	})
	ctx := t.Context()
	s, evs := e.ranTurn(ctx, "Draft the release note for v1.4.", "The note lists the new flags.")
	turned := seen.deltas
	e.stub.Script(titleModelName, titleReply(`"Release note for v1.4"`))
	res, err := e.h.Title(ctx, s, evs, e.log, TitleAsk{Names: []string{"tier/quick", titleModelName, model}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != titleModelName || res.Title != "Release note for v1.4" || res.Kept != "" {
		t.Fatalf("result %+v", res)
	}
	if seen.deltas != turned || seen.resets != 0 {
		t.Fatalf("the observer saw %d deltas of the title request", seen.deltas-turned)
	}
	reqs := e.stub.Requests()
	req := reqs[len(reqs)-1]
	if req.Model != titleModelName || len(req.Request.System) != 0 || len(req.Request.Tools) != 0 || req.Request.Reasoning != nil || *req.Request.MaxTokens != 32 || len(req.Request.Messages) != 1 {
		t.Fatalf("the title request %+v", req.Request)
	}
	asked := req.Request.Messages[0].Blocks[0].Text
	for _, want := range []string{"Draft the release note for v1.4.", "The note lists the new flags.", "Give a title", "at most 60 characters"} {
		if !strings.Contains(asked, want) {
			t.Errorf("the title request lacks %q:\n%s", want, asked)
		}
	}
	if strings.Contains(asked, "thinking about it") || strings.Contains(asked, "KEEP") || req.Request.Messages[0].Blocks[0].CacheHint {
		t.Fatalf("the first naming carries reasoning, offers KEEP or marks a breakpoint:\n%s", asked)
	}
	after, all := e.now(ctx)
	mr, changed := all[len(all)-2], all[len(all)-1]
	var p session.ModelRequest
	var c session.TitleChanged
	if mr.Type != session.TypeModelRequest || changed.Type != session.TypeTitleChanged || mr.Decode(&p) != nil || changed.Decode(&c) != nil || changed.Seq != mr.Seq+1 {
		t.Fatalf("the title's batch is %s then %s", mr.Type, changed.Type)
	}
	if p.Purpose != session.PurposeTitle || p.PromptVersion != string(prompts.Title) || p.Model != titleModelName || p.MaxTokens != 32 || p.FoldSeq != evs[len(evs)-1].Seq ||
		p.Outcome != "ok" || p.RequestSHA256 == "" || p.ResponseBlob == "" || p.CostUSDMicro == nil || *p.CostUSDMicro != 230 || p.Attempts != 1 {
		t.Fatalf("the title's model.request %+v", p)
	}
	if c.By != (session.Sender{Subject: session.HarnessSubject, Kind: session.SenderService}) || c.Old != "" || c.New != "Release note for v1.4" {
		t.Fatalf("the title change %+v", c)
	}
	if mr.Thread != "" || mr.Turn != s.Turn || changed.Turn != s.Turn {
		t.Fatalf("the title's events are on thread %q at turns %d and %d, want the session's turn %d", mr.Thread, mr.Turn, changed.Turn, s.Turn)
	}
	if after.Title != "Release note for v1.4" || !after.UpdatedAt.Equal(s.UpdatedAt) || after.Budget.SpentCostUSDMicro != s.Budget.SpentCostUSDMicro+230 || after.Status != session.StatusIdle {
		t.Fatalf("the header after the title %+v", after)
	}

	// The next naming offers KEEP with the title the harness wrote, and
	// carries the conversation as it stands.
	s, evs = e.ranTurn(ctx, "Now add the migration steps.", "Added them.")
	e.stub.Script(titleModelName, titleReply("KEEP"))
	if res, err = e.h.Title(ctx, s, evs, e.log, TitleAsk{Names: []string{titleModelName}}); err != nil || res.Kept != TitleKept || res.Title != "" {
		t.Fatalf("KEEP: %+v, %v", res, err)
	}
	reqs = e.stub.Requests()
	asked = reqs[len(reqs)-1].Request.Messages[0].Blocks[0].Text
	if !strings.Contains(asked, "Release note for v1.4\n") || !strings.Contains(asked, "KEEP to keep the title") || !strings.Contains(asked, "Now add the migration steps.") {
		t.Fatalf("the second naming:\n%s", asked)
	}
}

// TestATitleAnswerThatWritesNoTitle: an answer of KEEP, an empty one,
// one session.CheckTitle refuses, one longer than TitleMaxLength and the
// title the session has write no title and say why, each beside the
// model.request that asked; one pair of quotes around an answer is
// removed (spec 083).
func TestATitleAnswerThatWritesNoTitle(t *testing.T) {
	for _, c := range []struct {
		answer, kept, title string
	}{
		{"KEEP", TitleKept, ""},
		{" keep. ", TitleKept, ""},
		{"", TitleEmpty, ""},
		{`""`, TitleEmpty, ""},
		{"Two\nlines", TitleInvalid, ""},
		{strings.Repeat("x", TitleMaxLength+1), TitleTooLong, ""},
		{"Notes", TitleUnchanged, ""},
		{"\u201cMigration plan\u201d", "", "Migration plan"},
		{"'Plan d\u2019action'", "", "Plan d\u2019action"},
		{"\u300c\u79fb\u884c\u8a08\u753b\u300d", "", "\u79fb\u884c\u8a08\u753b"},
		{strings.Repeat("\u00e9", TitleMaxLength), "", strings.Repeat("\u00e9", TitleMaxLength)},
	} {
		t.Run(fmt.Sprintf("%q", c.answer), func(t *testing.T) {
			e := setupSession(t, nil, func(s *session.Session) { s.Title = "Notes" })
			ctx := t.Context()
			s, evs := e.ranTurn(ctx, "Write the plan.", "Written.")
			e.stub.Script(model, reply(ir.StopEndTurn, text(c.answer)))
			res, err := e.h.Title(ctx, s, evs, e.log, TitleAsk{Names: []string{model}})
			if err != nil || res.Kept != c.kept || res.Title != c.title {
				t.Fatalf("result %+v, %v; want kept %q title %q", res, err, c.kept, c.title)
			}
			after, all := e.now(ctx)
			last := all[len(all)-1]
			if c.kept != "" {
				if last.Type != session.TypeModelRequest || session.Purpose(last) != session.PurposeTitle || after.Title != "Notes" {
					t.Fatalf("a kept title left %s last and the title %q", last.Type, after.Title)
				}
				return
			}
			if last.Type != session.TypeTitleChanged || after.Title != c.title {
				t.Fatalf("the title is %q after %s", after.Title, last.Type)
			}
		})
	}
}

// TestAFailedTitleRequestIsRecorded: a request that fails, or that is cut
// short, is recorded as a model.request of purpose title with outcome
// error and returned as the error, and writes no session.error, no
// status and no title; a session with no message of a person, and names
// none of which connects, send and record nothing (spec 083).
func TestAFailedTitleRequestIsRecorded(t *testing.T) {
	var e *env
	e = setup(t, withTitleModel(&e))
	ctx := t.Context()
	s, evs := e.ranTurn(ctx, "Write the plan.", "Written.")
	// The stub fails the first request and holds the second, which the
	// test then cuts short.
	cut, cancel := context.WithCancel(ctx)
	held := make(chan struct{})
	e.stub.Script(titleModelName, luxstub.Reply{
		Fail:     &luxstub.Failure{Status: http.StatusBadRequest, Body: `{"error":{"type":"invalid_request","message":"no"}}`},
		Response: titleReply("Late").Response,
		Hold: func(ir.Event) {
			cancel()
			<-held
		},
	})
	if _, err := e.h.Title(ctx, s, evs, e.log, TitleAsk{Names: []string{titleModelName}}); err == nil {
		t.Fatal("a failed title request answered no error")
	}
	_, err := e.h.Title(cut, s, evs, e.log, TitleAsk{Names: []string{titleModelName}})
	close(held)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a title request cut short: %v", err)
	}
	after, all := e.now(ctx)
	var outcomes []string
	for _, ev := range all[len(evs):] {
		var p session.ModelRequest
		if ev.Type != session.TypeModelRequest || ev.Decode(&p) != nil || p.Purpose != session.PurposeTitle || p.Error == "" {
			t.Fatalf("a failed title appended %s", ev.Type)
		}
		outcomes = append(outcomes, p.Outcome)
	}
	if strings.Join(outcomes, " ") != "error canceled" || after.Status != session.StatusIdle || after.Title != "" {
		t.Fatalf("the failed requests are recorded %v, the session %s titled %q", outcomes, after.Status, after.Title)
	}
	if _, err := e.h.Title(ctx, s, evs, e.log, TitleAsk{Names: []string{"tier/none", "", "other"}}); !errors.Is(err, ErrNoTitleModel) || !strings.Contains(err.Error(), "tier/none is not run here") {
		t.Fatalf("no model to run on: %v", err)
	}
	if _, err := e.h.Title(ctx, s, evs[:0], e.log, TitleAsk{Names: []string{model}}); !errors.Is(err, ErrNothingToName) {
		t.Fatalf("nothing to name: %v", err)
	}
	if _, now := e.now(ctx); len(now) != len(all) {
		t.Fatal("a title that sent nothing appended something")
	}
}

// TestATitleRunsOnTheFirstModelThatConnects: the configured model needs
// no connection, the model the session switched to is the one Connect
// answered, a name Connect cannot have, or one without figures, passes to
// the next, and a runner without Connect runs its own model alone.
func TestATitleRunsOnTheFirstModelThatConnects(t *testing.T) {
	connects := 0
	h, err := New(Config{
		Model: &dialect.Model{}, Connection: models.Connection{BaseURL: "http://m.test", Model: model}, Machine: fakeMachine{}, Tools: tools.NewRegistry(),
		Entry: models.Entry{InputWindow: 10, MaxOutputTokens: 10},
		Connect: func(_ context.Context, name string) (models.Model, models.Connection, models.Entry, error) {
			connects++
			switch name {
			case "bare":
				return nil, models.Connection{BaseURL: "http://m.test", Model: name}, models.Entry{}, nil
			case "fine":
				return nil, models.Connection{BaseURL: "http://m.test", Model: name}, models.Entry{InputWindow: 10, MaxOutputTokens: 10}, nil
			}
			return nil, models.Connection{}, models.Entry{}, errors.New("not run")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for _, c := range []struct {
		names []string
		want  string
	}{{[]string{model}, model}, {[]string{"gone", "bare", "fine"}, "fine"}, {[]string{"", "gone", model}, model}} {
		_, conn, _, err := h.titleModel(ctx, c.names)
		if err != nil || conn.Model != c.want {
			t.Fatalf("%v runs on %q, %v", c.names, conn.Model, err)
		}
	}
	// A turn's switch connects on a copy of the harness, and keeps the
	// model it connected for the harness and its copies.
	scoped := *h
	if err := scoped.on(ctx, "fine"); err != nil {
		t.Fatal(err)
	}
	before := connects
	if _, conn, _, err := h.titleModel(ctx, []string{"fine"}); err != nil || conn.Model != "fine" || connects != before {
		t.Fatalf("the switched model connected again: %q, %v, %d connects", conn.Model, err, connects-before)
	}
	if _, _, _, err := h.titleModel(ctx, []string{"bare"}); !errors.Is(err, ErrNoTitleModel) || !strings.Contains(err.Error(), "no input window") {
		t.Fatalf("a model without figures: %v", err)
	}
	h.c.Connect = nil
	if _, _, _, err := h.titleModel(ctx, []string{"other"}); !errors.Is(err, ErrNoTitleModel) || !strings.Contains(err.Error(), "connects "+model+" alone") {
		t.Fatalf("a runner without Connect: %v", err)
	}
}

// TestTheTitleWindow: the window is the person's latest TitleMessages
// messages of the session's own thread, cut to TitleMessageChars, the
// names of their files, and the text of the agent's last message before
// each next one, cut to TitleAnswerChars; a trigger's message, a
// thread's events, reasoning and redacted events are left out. The title
// it carries is the harness's last, and none once a person renamed the
// session since or for a fork whose copied change was its parent's.
func TestTheTitleWindow(t *testing.T) {
	ev := func(typ session.Type, thread string, p any) session.Event {
		t.Helper()
		e, err := session.NewEvent(typ, p, t0)
		if err != nil {
			t.Fatal(err)
		}
		e.Thread = thread
		return e
	}
	person := session.Sender{Subject: "usr_ada", Kind: session.SenderPerson}
	said := func(sender session.Sender, text string, files ...string) session.Event {
		m := session.UserMessage{Sender: sender, Content: []lux.Block{{Type: ir.BlockText, Text: text}, {Type: ir.BlockImage}}}
		for _, f := range files {
			m.Attachments = append(m.Attachments, session.Attachment{Name: f})
		}
		return ev(session.TypeUserMessage, "", m)
	}
	answered := func(thread string, blocks ...lux.Block) session.Event {
		return ev(session.TypeAgentMessage, thread, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: blocks}, StopReason: ir.StopEndTurn})
	}
	renamed := func(by session.Sender, title string) session.Event {
		return ev(session.TypeTitleChanged, "", session.TitleChanged{By: by, New: title})
	}
	harness := session.Sender{Subject: session.HarnessSubject, Kind: session.SenderService}
	long := strings.Repeat("a", TitleAnswerChars+50)
	var log []session.Event
	for i := range TitleMessages + 1 {
		log = append(log, said(person, fmt.Sprintf("message %d", i)), answered("", lux.Block{Type: ir.BlockThinking, Text: "hidden"}, lux.Block{Type: ir.BlockText, Text: fmt.Sprintf("answer %d", i)}))
	}
	redacted := said(person, "removed")
	redacted.Payload = []byte(`{"tombstone":true}`)
	log = append(log,
		renamed(harness, "Plans"),
		said(session.Sender{Subject: "trigger:trg_1", Kind: session.SenderTrigger}, "a routine's message"),
		redacted,
		said(person, strings.Repeat("\u00fc", TitleMessageChars+10), "plan.md", "data.csv"),
		answered("thr_1", lux.Block{Type: ir.BlockText, Text: "a thread's words"}),
		answered("", lux.Block{Type: ir.BlockText, Text: "first part"}),
		answered("", lux.Block{Type: ir.BlockText, Text: long}),
	)
	for i := range log {
		log[i].Seq = uint64(i + 1)
	}
	current, msgs := titleWindow(session.Session{}, log)
	if current != "Plans" || len(msgs) != TitleMessages {
		t.Fatalf("title %q, %d messages", current, len(msgs))
	}
	if msgs[0].Text != "message 2" || msgs[0].Answer != "answer 2" || msgs[4].Text != "message 6" || msgs[4].Answer != "answer 6" {
		t.Fatalf("the window %+v", msgs[:5])
	}
	last := msgs[TitleMessages-1]
	if last.Text != strings.Repeat("\u00fc", TitleMessageChars) || strings.Join(last.Files, ",") != "plan.md,data.csv" || last.Answer != long[:TitleAnswerChars] {
		t.Fatalf("the last message %q with %v, answered %d characters", last.Text[:10], last.Files, len(last.Answer))
	}
	if current, _ := titleWindow(session.Session{}, append(log, renamed(person, "Mine"))); current != "" {
		t.Fatalf("a person's rename left the harness's title %q", current)
	}
	fork := session.Session{Parent: &session.Parent{SessionID: "ses_parent", Seq: uint64(len(log))}}
	if current, msgs := titleWindow(fork, log); current != "" || len(msgs) != TitleMessages {
		t.Fatalf("a fork reads its parent's title %q and %d messages", current, len(msgs))
	}
	if _, msgs := titleWindow(session.Session{}, []session.Event{answered("", lux.Block{Type: ir.BlockText, Text: "alone"}), said(person, " ")}); len(msgs) != 0 {
		t.Fatalf("an answer before any message, or a message of no text, is in the window: %+v", msgs)
	}
}

// TestRebuildATitleRequest: a title request is built again from the log
// through its fold_seq with the prompt it names, to the bytes whose hash
// it recorded, beside the steps of the turns around it, and never folded
// as a step; one that names a prompt this build does not hold is skipped
// (spec 083).
func TestRebuildATitleRequest(t *testing.T) {
	var e *env
	e = setup(t, withTitleModel(&e))
	ctx := t.Context()
	s, evs := e.ranTurn(ctx, "Draft the release note.", "Drafted.")
	e.stub.Script(titleModelName, titleReply("Release note"))
	if _, err := e.h.Title(ctx, s, evs, e.log, TitleAsk{Names: []string{titleModelName}}); err != nil {
		t.Fatal(err)
	}
	s, evs = e.ranTurn(ctx, "Now the migration.", "Migrated.")
	e.stub.Script(titleModelName, titleReply("KEEP"))
	if _, err := e.h.Title(ctx, s, evs, e.log, TitleAsk{Names: []string{titleModelName}}); err != nil {
		t.Fatal(err)
	}
	_, log := e.now(ctx)
	steps := e.replay(ctx, e.h, log)
	if got := strings.Join(outcomes(steps), " "); got != "match match match match" {
		t.Fatalf("the replay is %s: %+v", got, steps)
	}
	for i, ev := range log {
		if session.Purpose(ev) != session.PurposeTitle {
			continue
		}
		var mr session.ModelRequest
		if err := ev.Decode(&mr); err != nil {
			t.Fatal(err)
		}
		mr.PromptVersion = "title/title-v99"
		b, err := session.Marshal(mr)
		if err != nil {
			t.Fatal(err)
		}
		changed := append([]session.Event(nil), log...)
		changed[i].Payload = b
		steps := e.replay(ctx, e.h, changed)
		if steps[1].Outcome != models.ReplaySkipped || !strings.Contains(steps[1].Detail, "title/title-v99") {
			t.Fatalf("a title prompt this build does not hold: %+v", steps[1])
		}
		break
	}
}

// TestTheTitlePromptStatesItsBound: the title prompt states the most
// characters a title holds with the value of TitleMaxLength, holds no
// other number, and offers KEEP only when it carries a title.
func TestTheTitlePromptStatesItsBound(t *testing.T) {
	for _, current := range []string{"", "Release note"} {
		req, err := titleRequest(model, current, []titleMessage{{Text: "Draft the note.", Answer: "Drafted."}}, TitleMaxTokens)
		if err != nil {
			t.Fatal(err)
		}
		asked := req.Messages[0].Blocks[0].Text
		if !strings.Contains(asked, fmt.Sprintf("at most %d characters", TitleMaxLength)) {
			t.Fatalf("the prompt does not state the bound:\n%s", asked)
		}
		for _, f := range strings.FieldsFunc(asked, func(r rune) bool { return r < '0' || r > '9' }) {
			if f != fmt.Sprint(TitleMaxLength) {
				t.Fatalf("the prompt holds the number %s:\n%s", f, asked)
			}
		}
		if offered := strings.Contains(asked, TitleKeep); offered != (current != "") {
			t.Fatalf("with the title %q the prompt offers KEEP %v", current, offered)
		}
	}
	if req, err := titleRequest(model, "", nil, 9); err != nil || *req.MaxTokens != 9 || req.Model != model {
		t.Fatalf("the request %+v, %v", req, err)
	}
	if TitleTimeout != 15*time.Second || TitleRecordTimeout != 2*time.Second || TitleMaxTokens != 64 || TitleMessages != 6 || TitleAnswerChars != 600 || TitleMaxLength != 60 {
		t.Fatal("the bounds of spec 083 changed")
	}
}

// brokenLog is a Log whose blob writes or appends fail, as a store that
// is down or a lease that was lost refuses them.
type brokenLog struct {
	Log
	put, append error
}

func (l brokenLog) PutBlob(ctx context.Context, r io.Reader) (session.Digest, error) {
	if l.put != nil {
		return "", l.put
	}
	return l.Log.PutBlob(ctx, r)
}

func (l brokenLog) Append(ctx context.Context, batch []session.Event) ([]session.Event, error) {
	if l.append != nil {
		return nil, l.append
	}
	return l.Log.Append(ctx, batch)
}

// TestATitleThatCannotBeRecorded: a title whose response cannot be
// stored or whose batch cannot be appended returns why and writes
// nothing, and a session that captures its requests keeps the title
// request's bytes beside its response.
func TestATitleThatCannotBeRecorded(t *testing.T) {
	e := setupSession(t, nil, func(s *session.Session) { s.Capture.Requests = true })
	ctx := t.Context()
	s, evs := e.ranTurn(ctx, "Write the plan.", "Written.")
	down, lost := errors.New("the store is down"), errors.New("the lease was lost")
	for _, c := range []struct {
		l    Log
		fail luxstub.Reply
		want error
	}{
		{brokenLog{Log: e.log, put: down}, reply(ir.StopEndTurn, text("Plan")), down},
		{brokenLog{Log: e.log, append: lost}, reply(ir.StopEndTurn, text("Plan")), lost},
		{brokenLog{Log: e.log, put: down}, luxstub.Reply{Fail: &luxstub.Failure{Status: http.StatusBadRequest, Cut: true}, Response: reply(ir.StopEndTurn, text("Plan")).Response}, down},
	} {
		e.stub.Script(model, c.fail)
		if res, err := e.h.Title(ctx, s, evs, c.l, TitleAsk{Names: []string{model}}); !errors.Is(err, c.want) || res.Title != "" {
			t.Fatalf("%+v, %v; want %v", res, err, c.want)
		}
	}
	if _, now := e.now(ctx); len(now) != len(evs) {
		t.Fatalf("a title that could not be recorded appended %d events", len(now)-len(evs))
	}
	e.stub.Script(model, reply(ir.StopEndTurn, text("Plan")))
	if res, err := e.h.Title(ctx, s, evs, e.log, TitleAsk{Names: []string{model}}); err != nil || res.Title != "Plan" {
		t.Fatalf("%+v, %v", res, err)
	}
	_, all := e.now(ctx)
	var mr session.ModelRequest
	if err := all[len(all)-2].Decode(&mr); err != nil || mr.RequestBlob == "" || mr.ResponseBlob == "" {
		t.Fatalf("a captured title request %+v, %v", mr, err)
	}
}

// TestATitleThatSpentItsCapReasoningIsAskedAgain: a response that spent
// TitleMaxTokens reasoning and wrote no answer is asked once more at
// TitleReasoningTokens, the first request recorded as escalated beside
// the second in the title's batch, and both are built again by a replay;
// a second such response writes no title, and a title asked without
// Retry, or on a model whose output limit leaves no more room, is asked
// once (spec 083).
func TestATitleThatSpentItsCapReasoningIsAskedAgain(t *testing.T) {
	var e *env
	e = setup(t, withTitleModel(&e))
	ctx := t.Context()
	s, evs := e.ranTurn(ctx, "Plan the migration.", "Planned.")
	reasoned := reply(ir.StopMaxTokens, ir.Block{Type: ir.BlockThinking, Text: "Is this a new subject", Signature: "sig"})
	e.stub.Script(model, reasoned, reply(ir.StopEndTurn, text("Migration plan")))
	res, err := e.h.Title(ctx, s, evs, e.log, TitleAsk{Names: []string{model}, Retry: true})
	if err != nil || res.Title != "Migration plan" {
		t.Fatalf("result %+v, %v", res, err)
	}
	_, all := e.now(ctx)
	var asked []session.ModelRequest
	for _, ev := range all[len(evs):] {
		var mr session.ModelRequest
		if session.Purpose(ev) == session.PurposeTitle && ev.Decode(&mr) == nil {
			asked = append(asked, mr)
		}
	}
	if len(asked) != 2 || asked[0].Outcome != "escalated" || asked[0].MaxTokens != TitleMaxTokens || asked[1].Outcome != "ok" || asked[1].MaxTokens != TitleReasoningTokens ||
		all[len(all)-1].Type != session.TypeTitleChanged || len(all)-len(evs) != 3 {
		t.Fatalf("the title's batch %+v, then %s", asked, all[len(all)-1].Type)
	}
	reqs := e.stub.Requests()
	if *reqs[len(reqs)-2].Request.MaxTokens != TitleMaxTokens || *reqs[len(reqs)-1].Request.MaxTokens != TitleReasoningTokens {
		t.Fatal("the title was not asked again with room to reason")
	}
	if got := strings.Join(outcomes(e.replay(ctx, e.h, all)), " "); got != "match match match" {
		t.Fatalf("the replay is %s", got)
	}

	s, evs = e.now(ctx)
	e.stub.Script(model, reasoned, reasoned)
	if res, err := e.h.Title(ctx, s, evs, e.log, TitleAsk{Names: []string{model}, Retry: true}); err != nil || res.Kept != TitleEmpty {
		t.Fatalf("two answers of reasoning alone: %+v, %v", res, err)
	}
	// A second request that fails is recorded beside the first.
	s, evs = e.now(ctx)
	e.stub.Script(model, reasoned, luxstub.Reply{Fail: &luxstub.Failure{Status: http.StatusBadRequest}, Response: reasoned.Response})
	if _, err := e.h.Title(ctx, s, evs, e.log, TitleAsk{Names: []string{model}, Retry: true}); err == nil {
		t.Fatal("a failed second request answered no error")
	}
	if _, now := e.now(ctx); len(now) != len(evs)+2 || session.Purpose(now[len(now)-1]) != session.PurposeTitle {
		t.Fatalf("a failed second request appended %d events", len(now)-len(evs))
	}
	// Without Retry, a response of reasoning alone is asked once.
	s, evs = e.now(ctx)
	e.stub.Script(model, reasoned)
	before := len(e.stub.Requests())
	if res, err := e.h.Title(ctx, s, evs, e.log, TitleAsk{Names: []string{model}}); err != nil || res.Kept != TitleEmpty || len(e.stub.Requests()) != before+1 {
		t.Fatalf("a title asked without Retry: %+v, %v, %d requests", res, err, len(e.stub.Requests())-before)
	}
	s, evs = e.now(ctx)
	before = len(e.stub.Requests())
	e.stub.Script(titleModelName, luxstub.Reply{Response: ir.Response{Model: titleModelName, Blocks: []ir.Block{{Type: ir.BlockThinking, Text: "x", Signature: "sig"}}, StopReason: ir.StopMaxTokens}})
	if res, err := e.h.Title(ctx, s, evs, e.log, TitleAsk{Names: []string{titleModelName}, Retry: true}); err != nil || res.Kept != TitleEmpty || len(e.stub.Requests()) != before+1 {
		t.Fatalf("a model with no room to reason: %+v, %v, %d requests", res, err, len(e.stub.Requests())-before)
	}
}
