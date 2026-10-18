// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/topos/harness/parsing"
	"latere.ai/x/topos/prompts"
)

// fakeParser answers each read with its document and error, and records
// the requests.
type fakeParser struct {
	doc  parsing.Document
	err  error
	reqs []parsing.Request
}

func (f *fakeParser) Read(_ context.Context, r parsing.Request) (parsing.Document, error) {
	f.reqs = append(f.reqs, r)
	return f.doc, f.err
}

// pdfBody is a small PDF's bytes, binary past its header.
const pdfBody = "%PDF-1.7\n%\xe2\xe3\xcf\xd3\n1 0 obj\n<< /Type /Catalog >>\nendobj\n\x00\x01\x02trailer\n%%EOF\n"

func cost(n int64) *int64 { return &n }

// TestReadReadsAPDFThroughAParser (spec 080): read with a parser reads a
// PDF by its leading bytes, whatever its name, sends the path in the
// working directory, the whole file's digest and the pages asked, and
// answers the head line, the Markdown as numbered lines, and the pages
// to read next; its meta records the parse, its cost is the parse's, a
// thread that recorded the parse sends its id, and pages is offered and
// checked.
func TestReadReadsAPDFThroughAParser(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	put(t, filepath.Join(f.work, "attachments", "e1", "report"), pdfBody)
	fp := &fakeParser{doc: parsing.Document{
		ID: "prs_1", Pages: 12, Shown: []int{1, 2, 3, 4, 5}, CostUSDMicro: cost(42),
		Text: "<!-- page 1 -->\n# Title\n\nFirst page.\n",
	}}
	read := Read(fp)
	if d := read.Definition(); d.Description != prompts.Text(prompts.ToolReadParsed) || !strings.Contains(string(d.InputSchema), `"pages"`) {
		t.Fatalf("the definition %s %s", d.Description, d.InputSchema)
	}
	th := &thread{id: "thr_1"}
	res := th.call(ctx, t, read, f.h, `{"path":"attachments/e1/report"}`)
	p := filepath.Join(f.work, "attachments", "e1", "report")
	want := p + " is a PDF of 12 pages, read as text through the parsing service. Pages 1-5 follow as Markdown, each page opened by its page mark, without images.\n" +
		"     1\t<!-- page 1 -->\n     2\t# Title\n     3\t\n     4\tFirst page.\n" +
		"[pages 1-5 of 12; read on with pages \"6-10\"]\n"
	if res.Outcome != OutcomeOK || text(res) != want {
		t.Fatalf("read %s %q", res.Outcome, text(res))
	}
	if len(fp.reqs) != 1 || fp.reqs[0] != (parsing.Request{Path: "attachments/e1/report", SHA256: sum(pdfBody)}) {
		t.Fatalf("the requests %+v", fp.reqs)
	}
	if res.Meta == nil || res.Meta.SHA256 != sum(pdfBody) || res.Meta.Path != p || res.Meta.Parse == nil ||
		*res.Meta.Parse != (ParseMeta{ID: "prs_1", Pages: 12, Shown: "1-5"}) || res.CostUSDMicro == nil || *res.CostUSDMicro != 42 {
		t.Fatalf("meta %+v, cost %v", res.Meta, res.CostUSDMicro)
	}
	raw, err := json.Marshal(res.Meta)
	if err != nil || !strings.Contains(string(raw), `"parse":{"id":"prs_1","pages":12,"shown":"1-5"}`) {
		t.Fatalf("the meta's JSON %s %v", raw, err)
	}

	// The thread's state now holds the parse, which the next read names.
	fp.doc = parsing.Document{ID: "prs_1", Pages: 12, More: false, Failed: 1, Shown: []int{11, 12}, Text: "<!-- page 12 -->\nLast.\n", Unpriced: true}
	res = th.call(ctx, t, read, f.h, `{"path":"attachments/e1/report","pages":"11-"}`)
	if fp.reqs[1] != (parsing.Request{Path: "attachments/e1/report", SHA256: sum(pdfBody), Pages: "11-", Known: "prs_1"}) {
		t.Fatalf("the second request %+v", fp.reqs[1])
	}
	if !strings.HasPrefix(text(res), p+" is a PDF of 12 pages, read as text through the parsing service; 1 of them could not be read and is left out. Pages 11-12 follow") ||
		strings.Contains(text(res), "read on with pages") || res.CostUSDMicro != nil || !res.Meta.Parse.Unpriced {
		t.Fatalf("the last pages %q %+v", text(res), res.Meta.Parse)
	}

	fp.doc = parsing.Document{ID: "prs_2", Pages: parsing.MaxPages, More: true, Shown: []int{1}, Text: "one\n"}
	res = run(ctx, t, read, f.h, State{}, `{"path":"attachments/e1/report","pages":"1"}`)
	if !strings.HasPrefix(text(res), fmt.Sprintf("%s is a PDF of %d pages or more, of which the first %d pages were read,", p, parsing.MaxPages, parsing.MaxPages)) || !res.Meta.Parse.More {
		t.Fatalf("a long PDF %q", text(res))
	}

	before := len(fp.reqs)
	res = run(ctx, t, read, f.h, State{}, `{"path":"attachments/e1/report","pages":"0-3"}`)
	if res.Outcome != OutcomeInvalidInput || !strings.Contains(text(res), "/pages:") || len(fp.reqs) != before {
		t.Fatalf("a malformed selection %s %q", res.Outcome, text(res))
	}

	fp.err, fp.doc = &parsing.PastEnd{Pages: 3}, parsing.Document{Pages: 3}
	res = run(ctx, t, read, f.h, State{}, `{"path":"attachments/e1/report","pages":"7-"}`)
	if res.Outcome != OutcomeError || text(res) != p+` has 3 pages; pages "7-" names none of them.` {
		t.Fatalf("pages past the end %s %q", res.Outcome, text(res))
	}
	fp.err = parsing.ErrTooMuchText
	res = run(ctx, t, read, f.h, State{}, `{"path":"attachments/e1/report"}`)
	if res.Outcome != OutcomeError || text(res) != fmt.Sprintf(`Pages "1-5" of %s hold more than %d MiB of text; read fewer pages at a time.`, p, parsing.MaxText>>20) {
		t.Fatalf("too much text %s %q", res.Outcome, text(res))
	}

	// A file that is not a PDF is read as before.
	put(t, filepath.Join(f.work, "notes.txt"), "plain\n")
	fp.reqs = nil
	if res := run(ctx, t, read, f.h, State{}, `{"path":"notes.txt"}`); text(res) != "     1\tplain\n" || len(fp.reqs) != 0 {
		t.Fatalf("a text file %q", text(res))
	}
}

// TestReadFallsBackWhenAPDFIsNotParsed (spec 080): a PDF the parser did
// not read is the binary answer and one sentence that says why, outcome
// error, with what the read cost; a PDF outside the working directory and
// one past ReadMaxPDF are not sent.
func TestReadFallsBackWhenAPDFIsNotParsed(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	p := filepath.Join(f.work, "a.pdf")
	put(t, p, pdfBody)
	binary := prompts.Render(prompts.ReadBinary, prompts.Data{"Path": p}) + " It is a PDF, and it could not be read as text: "
	for _, c := range []struct {
		err  error
		cost *int64
		why  string
	}{
		{parsing.ErrNotKept, nil, "only a PDF a person attached, or one that was in the working directory as it is now at the end of an earlier turn, is read as text, and this one is neither."},
		{&parsing.Refused{Status: 403, Code: "forbidden", Message: "Parsing is not part of this plan."}, nil, "the parsing service refused it: Parsing is not part of this plan."},
		{&parsing.Failed{State: "failed", Code: "budget_exhausted"}, cost(7), "the parsing service could not read it (budget_exhausted)."},
		{&parsing.Failed{State: "failed", Code: "deadline_exceeded"}, cost(9), "the parsing service did not finish within 5m0s; read it again to wait for it, and the pages it already read are not read again."},
		{fmt.Errorf("parser: %w", context.DeadlineExceeded), nil, "the parsing service did not finish within 5m0s; read it again to wait for it, and the pages it already read are not read again."},
		{fmt.Errorf("%w: 503", parsing.ErrUnavailable), nil, "the parsing service is unavailable."},
	} {
		fp := &fakeParser{err: c.err, doc: parsing.Document{CostUSDMicro: c.cost}}
		res := run(ctx, t, Read(fp), f.h, State{}, `{"path":"a.pdf"}`)
		if res.Outcome != OutcomeError || text(res) != binary+c.why || res.Meta != nil || res.CostUSDMicro != c.cost {
			t.Errorf("%v: %s %q, cost %v", c.err, res.Outcome, text(res), res.CostUSDMicro)
		}
	}

	fp := &fakeParser{}
	put(t, filepath.Join(f.outside, "b.pdf"), pdfBody)
	res := run(ctx, t, Read(fp), f.h, State{}, mustInput(t, map[string]any{"path": filepath.Join(f.outside, "b.pdf")}))
	if res.Outcome != OutcomeError || len(fp.reqs) != 0 {
		t.Fatalf("a PDF outside the working directory %s %q", res.Outcome, text(res))
	}

	big := filepath.Join(f.work, "big.pdf")
	put(t, big, pdfBody)
	if err := os.Truncate(big, ReadMaxPDF+1); err != nil {
		t.Fatal(err)
	}
	res = run(ctx, t, Read(fp), f.h, State{}, `{"path":"big.pdf"}`)
	want := prompts.Render(prompts.ReadBinary, prompts.Data{"Path": big}) + fmt.Sprintf(" It is a PDF, and it could not be read as text: it is larger than %d MiB, the most a PDF read as text may be.", ReadMaxPDF>>20)
	if res.Outcome != OutcomeError || text(res) != want || len(fp.reqs) != 0 {
		t.Fatalf("a PDF past the bound %s %q", res.Outcome, text(res))
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if res := run(canceled, t, Read(&fakeParser{err: context.Canceled}), f.h, State{}, `{"path":"a.pdf"}`); res.Outcome != OutcomeError || strings.Contains(text(res), "could not be read as text") {
		t.Fatalf("a canceled read %s %q", res.Outcome, text(res))
	}
}

// TestReadWithoutAParserIsUnchanged (spec 080): the built-in read has
// description version 2 and no pages, refuses pages, and answers a PDF
// as a binary file.
func TestReadWithoutAParserIsUnchanged(t *testing.T) {
	f := open(t)
	read := builtinTool(t, NameRead)
	d := read.Definition()
	if d.Description != prompts.Text(prompts.ToolRead) || strings.Contains(string(d.InputSchema), "pages") {
		t.Fatalf("the built-in's definition %s", d.InputSchema)
	}
	reg := NewRegistry()
	if err := reg.AddBuiltin(read); err != nil {
		t.Fatal(err)
	}
	if _, res := reg.Validate(NameRead, json.RawMessage(`{"path":"a.pdf","pages":"1"}`)); res == nil || res.Outcome != OutcomeInvalidInput {
		t.Fatalf("pages was taken: %+v", res)
	}
	put(t, filepath.Join(f.work, "a.pdf"), pdfBody)
	res := run(t.Context(), t, read, f.h, State{}, `{"path":"a.pdf"}`)
	if res.Outcome != OutcomeError || text(res) != prompts.Render(prompts.ReadBinary, prompts.Data{"Path": filepath.Join(f.work, "a.pdf")}) {
		t.Fatalf("a PDF without a parser %s %q", res.Outcome, text(res))
	}
}

// TestStateKeepsAParse (spec 080): StateOf keeps each digest's last
// parse, and a meta without a digest keeps none.
func TestStateKeepsAParse(t *testing.T) {
	th := &thread{id: "thr_1"}
	th.record(t, Result{Outcome: OutcomeOK, Meta: &Meta{Path: "/w/a.pdf", SHA256: "aa", Parse: &ParseMeta{ID: "prs_1", Pages: 3, Shown: "1-3"}}})
	th.record(t, Result{Outcome: OutcomeOK, Meta: &Meta{Path: "/w/b.pdf", SHA256: "bb", Parse: &ParseMeta{ID: "prs_2", Pages: 9, Shown: "1-5"}}})
	th.record(t, Result{Outcome: OutcomeOK, Meta: &Meta{Path: "/w/a.pdf", SHA256: "aa", Parse: &ParseMeta{ID: "prs_3", Pages: 3, Shown: "2"}}})
	th.record(t, Result{Outcome: OutcomeOK, Meta: &Meta{Parse: &ParseMeta{ID: "prs_4"}}})
	st := th.state()
	if len(st.Parses) != 2 || st.Parses["aa"].ID != "prs_3" || st.Parses["bb"].ID != "prs_2" {
		t.Fatalf("the parses %+v", st.Parses)
	}
	if other := StateOf(th.events, "thr_2"); len(other.Parses) != 0 {
		t.Fatalf("another thread's parses %+v", other.Parses)
	}
}
