// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// allocated is the bytes fn allocates and the time it takes.
func allocated(fn func()) (uint64, time.Duration) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	began := time.Now()
	fn()
	took := time.Since(began)
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc, took
}

// The bounds a crafted document is held to: well above what reading one
// within its bounds takes, well below what the crafted ones cost without
// them.
const (
	craftedAlloc = 48 << 20
	craftedTime  = 20 * time.Second
)

// TestARowIsBoundAsItIsBuilt: a row of a spreadsheet whose cells name
// one long shared string, and an OpenDocument row whose cell a repeat
// multiplies, are held to the text's bound as their cells are added, so
// a small archive never builds a line far past it, and the read stops
// once the text is full (spec 079).
func TestARowIsBoundAsItIsBuilt(t *testing.T) {
	long := strings.Repeat("s", 16<<10)
	cells := strings.Repeat(`<c t="s"><v>0</v></c>`, maxColumns)
	wide := archiveOf(t,
		xlsx[0], `<workbook `+nsS+` `+nsR+`><sheets><sheet name="Wide" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		xlsx[2], `<Relationships `+nsRel+`><Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/sharedStrings.xml", `<sst `+nsS+`><si><t>`+long+`</t></si></sst>`,
		"xl/worksheets/sheet1.xml", `<worksheet `+nsS+`><sheetData><row r="1">`+cells+`</row><row r="2"><c t="inlineStr"><is><t>after</t></is></c></row></sheetData></worksheet>`)
	rows := strings.Repeat(`<table:table-row table:number-rows-repeated="256"><table:table-cell table:number-columns-repeated="256"><text:p>`+strings.Repeat("o", 64<<10)+`</text:p></table:table-cell></table:table-row>`, 20)
	repeated := archiveOf(t, odf(`<office:spreadsheet><table:table table:name="Big">`+rows+`</table:table></office:spreadsheet>`)...)
	for name, c := range map[string]struct {
		kind, data string
	}{"a wide xlsx": {"xlsx", wide}, "a repeated ods": {"ods", repeated}} {
		var text string
		var err error
		bytes, took := allocated(func() { text, err = documentText(t.Context(), c.kind, []byte(c.data)) })
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(text) > maxDocumentText || bytes > craftedAlloc || took > craftedTime {
			t.Fatalf("%s of %d bytes: %d bytes of text, %d bytes allocated in %s", name, len(c.data), len(text), bytes, took)
		}
	}
}

// TestADocumentIsInflatedWithinOneBudget: the parts of one document
// inflate within one budget together, so sheets that each stay within it
// cannot add up past it; a part named many times is read once; and a
// document whose text is full stops being read (spec 079).
func TestADocumentIsInflatedWithinOneBudget(t *testing.T) {
	pad := strings.Repeat(" ", 4<<20)
	var sheets, rels strings.Builder
	parts := []string{}
	for i := range 20 {
		fmt.Fprintf(&sheets, `<sheet name="S%d" sheetId="%d" r:id="rId%d"/>`, i, i+1, i+1)
		fmt.Fprintf(&rels, `<Relationship Id="rId%d" Target="worksheets/sheet%d.xml"/>`, i+1, i+1)
		parts = append(parts, fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1), `<worksheet `+nsS+`><sheetData>`+pad+`</sheetData></worksheet>`)
	}
	many := archiveOf(t, append([]string{
		xlsx[0], `<workbook ` + nsS + ` ` + nsR + `><sheets>` + sheets.String() + `</sheets></workbook>`,
		xlsx[2], `<Relationships ` + nsRel + `>` + rels.String() + `</Relationships>`,
	}, parts...)...)
	if _, err := documentText(t.Context(), "xlsx", []byte(many)); !errors.Is(err, errPartTooLarge) {
		t.Fatalf("twenty sheets of 4 MiB: %v", err)
	}

	ids := strings.Repeat(`<p:sldId id="256" r:id="rId2"/>`, 200)
	once := archiveOf(t,
		pptx[0], `<p:presentation `+nsP+` `+nsR+`><p:sldIdLst>`+ids+`</p:sldIdLst></p:presentation>`,
		pptx[2], `<Relationships `+nsRel+`><Relationship Id="rId2" Target="slides/slide1.xml"/></Relationships>`,
		"ppt/slides/slide1.xml", `<p:sld `+nsP+`><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>Hello</a:t></a:r></a:p>`+pad+`</p:txBody></p:sp></p:spTree></p:cSld></p:sld>`)
	var text string
	var err error
	_, took := allocated(func() { text, err = documentText(t.Context(), "pptx", []byte(once)) })
	if err != nil || text != "[Slide 1]\nHello\n" || took > craftedTime {
		t.Fatalf("a slide named 200 times: %q %v in %s", text, err, took)
	}

	// Paragraphs past the text's bound are not read: the read stops at
	// the first that does not fit.
	para := `<w:p><w:r><w:t>` + strings.Repeat("w", 3<<20) + `</w:t></w:r></w:p>`
	full := archiveOf(t, "word/document.xml", `<w:document `+nsW+`><w:body>`+strings.Repeat(para, 6)+`</w:body></w:document>`)
	d, err := openArchive(t.Context(), []byte(full))
	if err != nil {
		t.Fatal(err)
	}
	text, err = d.text("docx")
	if err != nil || len(text) > maxDocumentText || !strings.HasPrefix(text, strings.Repeat("w", 3<<20)+"\n") {
		t.Fatalf("a full document: %d bytes of text, %v", len(text), err)
	}
	// Six paragraphs of 3 MiB are 18 MiB of markup; the text fills at the
	// third, about 9 MiB in, so a read that stops there stays well under
	// 16 MiB and one that reads on passes it.
	if d.inflated > 16<<20 {
		t.Fatalf("a full document was inflated %d bytes, past the paragraph that filled it", d.inflated)
	}
}

// TestADocumentReadStopsWithItsContext: a read whose context ends stops
// with the context's error (spec 079).
func TestADocumentReadStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := documentText(ctx, "docx", []byte(archiveOf(t, docx...))); !errors.Is(err, context.Canceled) {
		t.Fatalf("a read past its context's end: %v", err)
	}
	f := open(t)
	read := builtinTool(t, NameRead)
	put(t, f.work+"/report.docx", archiveOf(t, docx...))
	res := run(ctx, t, read, f.h, State{}, `{"path":"report.docx"}`)
	if res.Outcome != OutcomeError || strings.Contains(text(res), "binary file") {
		t.Fatalf("a read tool call past its context's end: %s %q", res.Outcome, text(res))
	}
}
