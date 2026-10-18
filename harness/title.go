// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// The bounds of a title request (spec 083).
const (
	// TitleMaxTokens is the max_tokens a title request asks, or the
	// model's output limit when that is lower: a line of a few words.
	TitleMaxTokens = 64
	// TitleTimeout bounds a title request, apart from any turn's limit.
	TitleTimeout = 20 * time.Second
	// TitleMessages is how many of the person's latest messages a title
	// request carries.
	TitleMessages = 6
	// TitleMessageChars is how many characters of each of the person's
	// messages a title request carries: the subject is in what the person
	// wrote first, and a pasted log or file adds cost and no subject.
	TitleMessageChars = 2000
	// TitleAnswerChars is how many characters of the agent's answer to
	// each message a title request carries.
	TitleAnswerChars = 600
	// TitleMaxLength is the most characters a title the harness writes
	// holds, which the title prompt states.
	TitleMaxLength = 60
)

// TitleKeep is the answer that keeps the session's title, compared
// without case and a closing period.
const TitleKeep = "KEEP"

// Why a title request whose model answered wrote no title.
const (
	// TitleKept is an answer of TitleKeep.
	TitleKept = "kept"
	// TitleEmpty is an answer with no text.
	TitleEmpty = "empty"
	// TitleInvalid is an answer session.CheckTitle refuses, such as one of
	// two lines.
	TitleInvalid = "invalid"
	// TitleTooLong is an answer longer than TitleMaxLength characters.
	TitleTooLong = "too_long"
	// TitleUnchanged is the title the session has.
	TitleUnchanged = "unchanged"
)

// TitleResult is what a title request did: Model is the model it ran on,
// Title the title it wrote, "" when it wrote none, and Kept why it wrote
// none though its model answered.
type TitleResult struct {
	Model string
	Title string
	Kept  string
}

// ErrNoTitleModel is a title request none of whose models could be
// connected: nothing was sent.
var ErrNoTitleModel = errors.New("harness: no model a title request may run on could be connected")

// ErrNothingToName is a session whose log holds no message of a person:
// a title request has nothing to name, and nothing was sent.
var ErrNothingToName = errors.New("harness: the session holds no message of a person to name")

// Title asks a model whether the session's title still names what its
// conversation is about, and gives it a new one when it does not (spec
// 083). It runs between turns, never as one: log is the session's log as
// it stands, and l the log it appends to. The request is built from the
// title prompt and the window titleWindow reads, with no tools and no
// reasoning setting, and runs on the first of names the installation
// runs, within TitleTimeout. Its response is read to its end and reaches
// no Observer: a client sees the title, not the model writing it.
//
// The request is recorded as a model.request of purpose title, whose
// cost the session spends, and a new title is appended in the same batch
// as a session.title_changed by session.HarnessSubject. An answer of
// TitleKeep, an empty one, one session.CheckTitle refuses, one longer
// than TitleMaxLength characters and the title the session has write no
// title, and Kept says which. A request that failed is recorded with
// outcome error and returned as the error; no session.error is written
// and no status changes. ErrNothingToName and ErrNoTitleModel send and
// record nothing.
func (h *Harness) Title(ctx context.Context, s session.Session, log []session.Event, l Log, names []string) (TitleResult, error) {
	current, msgs := titleWindow(s, log)
	if len(msgs) == 0 {
		return TitleResult{}, ErrNothingToName
	}
	model, conn, entry, err := h.titleModel(ctx, names)
	if err != nil {
		return TitleResult{}, err
	}
	out := TitleResult{Model: conn.Model}
	limit := min(int64(TitleMaxTokens), entry.MaxOutputTokens)
	req, err := titleRequest(conn.Model, current, msgs, limit)
	if err != nil {
		return out, err
	}
	mr := session.ModelRequest{
		Model: conn.Model, Family: conn.Family, Dialect: string(conn.EffectiveDialect()), PromptVersion: string(prompts.Title),
		Purpose: session.PurposeTitle, MaxTokens: limit, Attempts: 1,
	}
	if n := len(log); n > 0 {
		mr.FoldSeq = log[n-1].Seq
	}
	tctx, cancel := context.WithTimeout(ctx, TitleTimeout)
	defer cancel()
	began := h.c.Clock()
	res, err := quietly(tctx, model, models.Request{IR: req, Connection: conn, Capture: s.Capture.Requests})
	mr.LatencyMS = h.c.Clock().Sub(began).Milliseconds()
	// Whatever happens to the request, its record is appended: a request
	// the title's own deadline cut short is recorded too.
	record := context.WithoutCancel(ctx)
	if err != nil {
		mr.Outcome, mr.Error = "error", models.Described(err)
		if len(res.RawResponse) > 0 {
			blob, perr := l.PutBlob(record, bytes.NewReader(res.RawResponse))
			if perr != nil {
				return out, errors.Join(err, fmt.Errorf("harness: store the failed response: %w", perr))
			}
			mr.ResponseBlob = blob
		}
		return out, errors.Join(err, h.appendTitle(record, s, l, mr, nil))
	}
	if mr.ResponseBlob, err = l.PutBlob(record, bytes.NewReader(res.RawResponse)); err != nil {
		return out, fmt.Errorf("harness: store the response: %w", err)
	}
	if s.Capture.Requests && res.RequestBytes != nil {
		if mr.RequestBlob, err = l.PutBlob(record, bytes.NewReader(res.RequestBytes)); err != nil {
			return out, fmt.Errorf("harness: store the request: %w", err)
		}
	}
	mr.Codec, mr.RequestSHA256, mr.RequestBytes = res.Codec, res.RequestSHA256, res.RequestSize
	mr.Usage, mr.FirstTokenMS, mr.StopReason, mr.Loss, mr.Outcome = &res.Usage, res.FirstToken.Milliseconds(), res.StopReason, res.Loss, "ok"
	if cost, src, err := models.Cost(models.FromLux(res.Usage), entry); err == nil {
		mr.CostUSDMicro, mr.CostSource = &cost, src
	}
	answer := titleAnswer(res.Message)
	if out.Kept = keptTitle(answer, s.Title); out.Kept != "" {
		return out, h.appendTitle(record, s, l, mr, nil)
	}
	out.Title = answer
	changed := &session.TitleChanged{By: session.Sender{Subject: session.HarnessSubject, Kind: session.SenderService}, Old: s.Title, New: answer}
	if err := h.appendTitle(record, s, l, mr, changed); err != nil {
		return TitleResult{Model: out.Model}, err
	}
	return out, nil
}

// appendTitle appends a title request's model.request, and the change of
// the title it made, when it made one, in one batch, on the session's own
// thread at the turn the session last ran.
func (h *Harness) appendTitle(ctx context.Context, s session.Session, l Log, mr session.ModelRequest, changed *session.TitleChanged) error {
	now := h.c.Clock()
	e, err := session.NewEvent(session.TypeModelRequest, mr, now)
	if err != nil {
		return err
	}
	batch := []session.Event{e}
	if changed != nil {
		c, err := session.NewEvent(session.TypeTitleChanged, *changed, now)
		if err != nil {
			return err
		}
		batch = append(batch, c)
	}
	for i := range batch {
		batch[i].Turn = s.Turn
	}
	if _, err := l.Append(ctx, batch); err != nil {
		return fmt.Errorf("harness: append the title: %w", err)
	}
	return nil
}

// titleModel connects the first of names the installation runs: the
// harness's own model as it is configured, the one the session last
// switched to as Connect answered it, and any other through Connect,
// which the harness's switch is not told of, so the next turn runs where
// it would have. It is ErrNoTitleModel, with why each name could not be
// connected, when none can.
func (h *Harness) titleModel(ctx context.Context, names []string) (models.Model, models.Connection, models.Entry, error) {
	var why []error
	for _, name := range names {
		if name == "" {
			continue
		}
		if name == h.c.Connection.Model {
			return h.c.Model, h.c.Connection, h.c.Entry, nil
		}
		sw := h.switched
		sw.mu.Lock()
		model, conn, entry, known := sw.model, sw.conn, sw.entry, sw.name == name
		sw.mu.Unlock()
		if known {
			return model, conn, entry, nil
		}
		if h.c.Connect == nil {
			why = append(why, fmt.Errorf("%s: this runner connects %s alone", name, h.c.Connection.Model))
			continue
		}
		model, conn, entry, err := h.c.Connect(ctx, name)
		if err == nil {
			err = conn.Validate()
		}
		if err == nil && (entry.MaxOutputTokens <= 0 || entry.InputWindow <= 0) {
			err = &models.Coded{Code: models.CodeUnknown, Message: fmt.Sprintf("no input window and output limit for %q", name)}
		}
		if err != nil {
			why = append(why, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if model == nil {
			model = h.c.Model
		}
		return model, conn, entry, nil
	}
	return nil, models.Connection{}, models.Entry{}, fmt.Errorf("%w: %w", ErrNoTitleModel, errors.Join(why...))
}

// quietly streams a request to its end and returns its result, telling
// no Observer and starting no machine.
func quietly(ctx context.Context, m models.Model, req models.Request) (models.Result, error) {
	s, err := m.Stream(ctx, req)
	if err != nil {
		return models.Result{}, err
	}
	for {
		_, err := s.Next()
		if errors.Is(err, io.EOF) {
			return s.Result(), s.Close()
		}
		if err != nil {
			return s.Result(), errors.Join(err, s.Close())
		}
	}
}

// titleMessage is one of the person's messages as a title request
// carries it: its text, the names of the files it carries, and the text
// of the agent's last message before the person's next one.
type titleMessage struct {
	Text   string
	Files  []string
	Answer string
}

// titleWindow is what a title request reads of a log (spec 083): the
// title the harness last gave the session, "" when the session's title is
// not the harness's, as for a session whose title its create gave or a
// person changed since; and the person's latest TitleMessages messages of
// the session's own thread, oldest first, each cut to TitleMessageChars
// characters and followed by the text of the agent's last message before
// the next of them, cut to TitleAnswerChars. Reasoning, calls, results,
// summaries, a thread's events and redacted events are left out. A
// fork's copied changes of the title were its parent's, so they name no
// title of the fork's; its copied messages are its conversation.
func titleWindow(s session.Session, log []session.Event) (string, []titleMessage) {
	current := ""
	var msgs []titleMessage
	for _, e := range log {
		if e.Redacted() || e.Thread != "" {
			continue
		}
		switch e.Type {
		case session.TypeTitleChanged:
			var p session.TitleChanged
			if s.Copied(e) || e.Decode(&p) != nil {
				continue
			}
			current = ""
			if p.By.Subject == session.HarnessSubject && p.By.Kind == session.SenderService {
				current = p.New
			}
		case session.TypeUserMessage:
			var p session.UserMessage
			if e.Decode(&p) != nil || p.Sender.Kind != session.SenderPerson {
				continue
			}
			m := titleMessage{Text: cutChars(blockText(p.Content, "\n"), TitleMessageChars)}
			for _, a := range p.Attachments {
				m.Files = append(m.Files, a.Name)
			}
			if m.Text != "" || len(m.Files) > 0 {
				msgs = append(msgs, m)
			}
		case session.TypeAgentMessage:
			var p session.AgentMessage
			if len(msgs) == 0 || e.Decode(&p) != nil {
				continue
			}
			if text := blockText(p.Message.Blocks, "\n"); text != "" {
				msgs[len(msgs)-1].Answer = cutChars(text, TitleAnswerChars)
			}
		}
	}
	if len(msgs) > TitleMessages {
		msgs = msgs[len(msgs)-TitleMessages:]
	}
	return current, msgs
}

// blockText is the text blocks of a message joined by sep and trimmed.
func blockText(blocks []lux.Block, sep string) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == ir.BlockText && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, sep))
}

// cutChars is s cut to at most n characters.
func cutChars(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.TrimSpace(string([]rune(s)[:n]))
}

// titleRequest is the request that asks model for a session's title: the
// title prompt rendered over the window as one message of the user, no
// system prompt, no tools, no reasoning setting and no cache breakpoint,
// since nothing of it is sent again.
func titleRequest(model, current string, msgs []titleMessage, maxTokens int64) (ir.Request, error) {
	text := prompts.Render(prompts.Title, prompts.Data{"Title": current, "Messages": msgs, "Max": TitleMaxLength})
	return irOf(lux.Request{
		Model: model, Messages: []lux.Message{{Role: ir.RoleUser, Blocks: []lux.Block{{Type: ir.BlockText, Text: text}}}},
		MaxTokens: &maxTokens, Stream: true,
	})
}

// quotes are the pairs of quotes a title answer may stand between.
var quotes = [][2]string{{`"`, `"`}, {"'", "'"}, {"“", "”"}, {"‘", "’"}, {"«", "»"}, {"「", "」"}}

// titleAnswer is the text of a title response, trimmed, with one pair of
// quotes around it removed.
func titleAnswer(m lux.Message) string {
	a := blockText(m.Blocks, "")
	for _, q := range quotes {
		if len(a) >= len(q[0])+len(q[1]) && strings.HasPrefix(a, q[0]) && strings.HasSuffix(a, q[1]) {
			return strings.TrimSpace(a[len(q[0]) : len(a)-len(q[1])])
		}
	}
	return a
}

// keptTitle is why an answer writes no title over the session's title,
// "" when it is the new title.
func keptTitle(answer, title string) string {
	switch {
	case strings.EqualFold(strings.TrimSuffix(answer, "."), TitleKeep):
		return TitleKept
	case answer == "":
		return TitleEmpty
	case session.CheckTitle(answer) != "":
		return TitleInvalid
	case utf8.RuneCountInString(answer) > TitleMaxLength:
		return TitleTooLong
	case answer == title:
		return TitleUnchanged
	}
	return ""
}
