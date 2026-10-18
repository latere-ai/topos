// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/bridge"
	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/parsing"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/arcastub"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// readerModel is the model the stub service's reader reads page 1 with,
// one the embedded catalog prices.
const readerModel = "gemini/gemini-3-flash"

// lectio is a stub parsing service of one document of 7 pages, page 1 a
// model read, the rest from their text layer. A submit fetches its
// source with no credential and answers the parse succeeded.
type lectio struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	submits []map[string]any
	fetched []string
	bearers []string
	gets    int
}

func newLectio(t *testing.T) *lectio {
	l := &lectio{t: t}
	l.srv = httptest.NewServer(http.HandlerFunc(l.serve))
	t.Cleanup(l.srv.Close)
	return l
}

func (l *lectio) serve(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.bearers = append(l.bearers, r.Header.Get("Authorization"))
	view := map[string]any{"id": "prs_1", "state": "succeeded", "progress": map[string]any{"pages_total": 7}, "usage": map[string]any{"input_tokens": 1000, "output_tokens": 200}}
	var out any = view
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/parsing/parses":
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			l.t.Errorf("a submit's body: %v", err)
		}
		l.submits = append(l.submits, body)
		src, _ := body["source"].(map[string]any)
		u, _ := src["url"].(string)
		resp, err := http.Get(u)
		if err != nil {
			l.t.Errorf("fetch the source: %v", err)
			return
		}
		b, err := io.ReadAll(resp.Body)
		if cerr := resp.Body.Close(); err != nil || cerr != nil {
			l.t.Errorf("read the source: %v %v", err, cerr)
		}
		l.fetched = append(l.fetched, string(b))
	case r.URL.Path == "/v1/parsing/parses/prs_1":
		l.gets++
	case r.URL.Path == "/v1/parsing/parses/prs_1/pages":
		out = map[string]any{"pages": []map[string]any{{"number": 1, "source": "reader"}, {"number": 2, "source": "text_layer"}}}
	case r.URL.Path == "/v1/parsing/parses/prs_1/pages/1":
		out = map[string]any{"number": 1, "source": "reader", "model": readerModel, "usage": map[string]any{"input_tokens": 1000, "output_tokens": 200}}
	case r.URL.Path == "/v1/parsing/parses/prs_1/document":
		sel, err := parsing.Select(r.URL.Query().Get("pages"), 7)
		if err != nil {
			l.t.Errorf("the document's pages: %v", err)
		}
		for _, n := range sel {
			_, _ = fmt.Fprintf(w, "<!-- page %d -->\nThe tide on page %d.\n", n, n)
		}
		return
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"Not found."}}`)
		return
	}
	if err := json.NewEncoder(w).Encode(out); err != nil {
		l.t.Error(err)
	}
}

// TestReadParsesAPDFOfTheFolder (spec 080): over the stub Cella, the stub
// files API and a stub parsing service, a hosted session whose message
// attached a PDF reads it with read: the service is reached with the
// session's own token for lectio, fetches the file at the address the
// files API presigned with no credential, and records the file's origin
// in the session's folder; the model reads the pages as Markdown, the
// tool.result carries the cost of the page a model read, priced at the
// catalog's figures, and its meta the parse; a later turn reads other
// pages of the recorded parse with no second submit and no cost.
func TestReadParsesAPDFOfTheFolder(t *testing.T) {
	arca := arcastub.New(t)
	l := newLectio(t)
	const owner = "https://issuer.example|ada"
	c := newCloudOf(t, writerAgent, func(s *session.Session) {
		s.Folder = &session.Folder{URL: arca.URL(), Owner: owner, Path: "files/conversations/" + s.ID}
	})
	c.o.FilesAudience = "arca"
	c.o.ParsingURL = l.srv.URL + "/v1/parsing"
	c.creds = &issued{life: 15 * time.Minute}

	pdf := []byte("%PDF-1.7\n\x00\x01 a report of tides\n%%EOF\n")
	e, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	p := session.AttachmentPath(e.ID, "report.pdf")
	msg := session.UserMessage{
		Sender:      c.s.Initiator,
		Content:     []lux.Block{{Type: ir.BlockText, Text: "What does the report say?"}},
		Attachments: []session.Attachment{{Name: "report.pdf", MediaType: "application/pdf", Size: int64(len(pdf)), Blob: session.DigestOf(pdf), Path: p, Ref: p}},
	}
	if e.Payload, err = session.Marshal(msg); err != nil {
		t.Fatal(err)
	}
	folder := "files/conversations/" + c.s.ID
	arca.Put(owner, folder+"/"+p, arcastub.Object{Data: pdf, ContentType: "application/pdf"})

	c.driveAfter(session.StopEndTurn, []session.Event{e},
		tool(t, "toolu_1", "read", map[string]any{"path": p}),
		said("It is about tides."),
	)
	res := c.results()
	if len(res) != 1 || !strings.Contains(res[0], "is a PDF of 7 pages, read as text through the parsing service. Pages 1-5 follow") ||
		!strings.Contains(res[0], "The tide on page 5.") || strings.Contains(res[0], "page 6.") || !strings.Contains(res[0], `read on with pages "6-7"`) {
		t.Fatalf("the read %q", res)
	}
	if !c.readInAResult("The tide on page 1.") {
		t.Fatal("the model does not read the document's text")
	}
	cat, err := models.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := models.Cost(models.UsageFigures{Input: 1000, Output: 200}, cat.Overlay(readerModel))
	if err != nil || want <= 0 {
		t.Fatalf("the reader's price %d %v", want, err)
	}
	var tr session.ToolResult
	if err := c.events(session.TypeToolResult)[0].Decode(&tr); err != nil {
		t.Fatal(err)
	}
	var meta tools.Meta
	if err := json.Unmarshal(tr.Meta, &meta); err != nil || meta.Parse == nil || meta.Parse.ID != "prs_1" || meta.Parse.Pages != 7 || meta.Parse.Shown != "1-5" {
		t.Fatalf("the meta %s %v", tr.Meta, err)
	}
	if tr.CostUSDMicro == nil || *tr.CostUSDMicro != want {
		t.Fatalf("the cost %v, want %d", tr.CostUSDMicro, want)
	}
	if len(l.submits) != 1 || len(l.fetched) != 1 || l.fetched[0] != string(pdf) {
		t.Fatalf("%d submits, fetched %q", len(l.submits), l.fetched)
	}
	origin, _ := l.submits[0]["origin"].(map[string]any)
	if origin["store"] != OriginStore || origin["path"] != url.PathEscape(owner)+"/"+folder+"/"+p {
		t.Fatalf("the origin %v", origin)
	}
	for _, b := range l.bearers {
		if !strings.HasPrefix(b, "Bearer "+AudienceLectio+"-"+runner.WorkloadSession+"-") {
			t.Fatalf("the service was reached with %q", b)
		}
	}

	c.drive("And the last pages?", tool(t, "toolu_2", "read", map[string]any{"path": p, "pages": "6-"}), said("More tides."))
	res = c.results()
	if len(res) != 2 || !strings.Contains(res[1], "Pages 6-7 follow") || !strings.Contains(res[1], "The tide on page 7.") {
		t.Fatalf("the second read %q", res)
	}
	var again session.ToolResult
	if err := c.events(session.TypeToolResult)[1].Decode(&again); err != nil {
		t.Fatal(err)
	}
	if len(l.submits) != 1 || l.gets != 1 || again.CostUSDMicro != nil {
		t.Fatalf("the second read submitted %d, read the parse %d times, cost %v", len(l.submits), l.gets, again.CostUSDMicro)
	}
}

// TestTheParsingReaderNeedsItsSettings (spec 080): read is given no
// parser without TOPOS_PARSING_URL, a folder, a files store that
// presigns, or a token source.
func TestTheParsingReaderNeedsItsSettings(t *testing.T) {
	arca := arcastub.New(t)
	ctx := runner.WithTokens(t.Context(), runner.NewTokenSource(&issued{life: time.Minute}, nil, nil))
	s := session.Session{ID: session.NewID(session.PrefixSession), Folder: &session.Folder{URL: arca.URL(), Owner: "o", Path: "files/conversations/s"}}
	b := builder{o: Options{FilesAudience: "arca", ParsingURL: "https://parsing.example/v1/parsing"}}
	files, err := b.files(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if b.parsingReader(ctx, s, files) == nil {
		t.Fatal("a session with every setting has no parser")
	}
	unset := builder{o: Options{FilesAudience: "arca"}}
	for name, got := range map[string]parsing.Reader{
		"no URL":          unset.parsingReader(ctx, s, files),
		"no folder":       b.parsingReader(ctx, session.Session{ID: s.ID}, nil),
		"no presign":      b.parsingReader(ctx, s, nil),
		"no token source": b.parsingReader(t.Context(), s, files),
	} {
		if got != nil {
			t.Errorf("%s: a parser", name)
		}
	}
}

// readInAResult reports whether the session's last model request carries
// text in a tool result.
func (c *cloud) readInAResult(text string) bool {
	c.t.Helper()
	reqs := c.lux.Requests()
	for _, m := range reqs[len(reqs)-1].Request.Messages {
		for _, b := range m.Blocks {
			if b.ToolResult == nil {
				continue
			}
			for _, inner := range b.ToolResult.Blocks {
				if strings.Contains(inner.Text, text) {
					return true
				}
			}
		}
	}
	return false
}

// TestAReaderModelIsPricedAtTheDoorsFigures (spec 080): a model the
// embedded catalog prices is priced without a door; one it does not is
// priced at the figures the Lux door for OpenAI serves for it, read with
// the session's own model key; a model the door does not price, and an
// installation without a door, are unpriced.
func TestAReaderModelIsPricedAtTheDoorsFigures(t *testing.T) {
	l := luxstub.New(t)
	l.Models(bridge.Model{Name: "vendor/reader-9", ContextWindow: 100_000, MaxOutputTokens: 8_192, Pricing: &bridge.ModelPricing{Currency: "USD", Input: "0.3", Output: "2.5"}},
		bridge.Model{Name: "vendor/free-list", ContextWindow: 100_000, MaxOutputTokens: 8_192})
	cat, err := models.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	src := runner.NewTokenSource(&issued{life: 15 * time.Minute}, nil, nil)
	b := builder{o: Options{ModelsURL: l.URL() + "/v1/models", Doors: models.Doors{"openai": l.URL() + "/openai"}}, cat: cat}
	price := b.pricer(src)
	if e, err := price(t.Context(), readerModel); err != nil || !priced(e) {
		t.Fatalf("a catalog model: %+v %v", e, err)
	}
	e, err := price(t.Context(), "vendor/reader-9")
	if err != nil || !priced(e) || *e.Pricing.Input != 300_000 || *e.Pricing.Output != 2_500_000 {
		t.Fatalf("a model the door prices: %+v %v", e, err)
	}
	// The door lists the model to the session's own key alone.
	l.Select(func(key string) []string {
		if key == "lux-session-1" {
			return []string{"vendor/reader-9", "vendor/free-list"}
		}
		return nil
	})
	if _, err := b.pricer(src)(t.Context(), "vendor/reader-9"); err != nil {
		t.Fatalf("the session's key: %v", err)
	}
	if _, err := (builder{o: Options{ModelsURL: b.o.ModelsURL, Doors: b.o.Doors, ModelsKey: "installation"}, cat: cat}).pricer(nil)(t.Context(), "vendor/reader-9"); !errors.Is(err, models.ErrUnpriced) {
		t.Fatalf("another key: %v", err)
	}
	if _, err := price(t.Context(), "vendor/free-list"); !errors.Is(err, models.ErrUnpriced) {
		t.Fatalf("a model the door does not price: %v", err)
	}
	none := builder{o: Options{ModelsURL: "https://provider.example/v1"}, cat: cat}
	if _, err := none.pricer(nil)(t.Context(), "vendor/reader-9"); !errors.Is(err, models.ErrUnpriced) {
		t.Fatalf("no door: %v", err)
	}
}
