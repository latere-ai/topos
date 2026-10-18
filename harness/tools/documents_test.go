// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"archive/zip"
	"bytes"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// archiveOf is a zip archive of the parts, each written in the order
// given.
func archiveOf(t *testing.T, parts ...string) string {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for i := 0; i+1 < len(parts); i += 2 {
		f, err := w.Create(parts[i])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(parts[i+1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

const (
	nsW   = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:mc="http://schemas.openxmlformats.org/markup-compatibility/2006"`
	nsR   = `xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`
	nsS   = `xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"`
	nsP   = `xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"`
	nsRel = `xmlns="http://schemas.openxmlformats.org/package/2006/relationships"`
	nsODF = `xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" ` +
		`xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0" xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0"`
)

var docx = []string{"word/document.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:document ` + nsW + `><w:body>` +
	`<w:p><w:pPr><w:tabs><w:tab w:val="left" w:pos="720"/></w:tabs></w:pPr><w:r><w:t>Quarterly</w:t></w:r><w:r><w:t xml:space="preserve"> report</w:t></w:r></w:p>` +
	`<w:p><w:r><w:t>Total</w:t><w:tab/><w:t>42 &amp; more</w:t></w:r></w:p>` +
	`<w:p/>` +
	`<w:tbl><w:tr><w:tc><w:p><w:r><w:t>Month</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>Sales</w:t></w:r></w:p></w:tc></w:tr>` +
	`<w:tr><w:tc><w:p><w:r><w:t>July</w:t></w:r></w:p><w:p><w:r><w:t>(est.)</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>10</w:t></w:r></w:p></w:tc></w:tr></w:tbl>` +
	`<w:p><w:r><w:t>Line one</w:t><w:br/><w:t>Line two</w:t></w:r><w:del><w:r><w:delText>gone</w:delText></w:r></w:del></w:p>` +
	`<w:p><mc:AlternateContent><mc:Choice Requires="wps"><w:r><w:t>Choice</w:t></w:r></mc:Choice><mc:Fallback><w:r><w:t>Fallback</w:t></w:r></mc:Fallback></mc:AlternateContent></w:p>` +
	`</w:body></w:document>`}

var xlsx = []string{
	"xl/workbook.xml", `<workbook ` + nsS + ` ` + nsR + `><sheets><sheet name="Sales" sheetId="2" r:id="rId2"/><sheet name="Notes" sheetId="1" r:id="rId1"/></sheets></workbook>`,
	"xl/_rels/workbook.xml.rels", `<Relationships ` + nsRel + `><Relationship Id="rId1" Target="worksheets/sheet2.xml"/><Relationship Id="rId2" Target="/xl/worksheets/sheet1.xml"/>` +
		`<Relationship Id="rId9" Target="https://example.com/" TargetMode="External"/></Relationships>`,
	"xl/sharedStrings.xml", `<sst ` + nsS + `><si><t>Month</t></si><si><r><t>To</t></r><r><t>tal</t></r></si><si><t>日本語</t><rPh><t>にほんご</t></rPh></si></sst>`,
	"xl/worksheets/sheet1.xml", `<worksheet ` + nsS + `><sheetData>` +
		`<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row>` +
		`<row r="2"><c r="A2" t="inlineStr"><is><t>July</t></is></c><c r="C2"><v>12.5</v></c><c r="D2" t="b"><v>1</v></c><c r="E2" t="b"><v>0</v></c></row>` +
		`<row r="3"><c r="A3"/></row>` +
		`<row r="4"><c r="A4" t="str"><f>CONCAT("x")</f><v>x</v></c><c><v>7</v></c></row>` +
		`</sheetData></worksheet>`,
	"xl/worksheets/sheet2.xml", `<worksheet ` + nsS + `><sheetData><row r="1"><c r="A1" t="s"><v>2</v></c><c r="B1" t="s"><v>99</v></c></row></sheetData></worksheet>`,
}

var pptx = []string{
	"ppt/presentation.xml", `<p:presentation ` + nsP + ` ` + nsR + `><p:sldIdLst><p:sldId id="256" r:id="rId3"/><p:sldId id="257" r:id="rId2"/></p:sldIdLst></p:presentation>`,
	"ppt/_rels/presentation.xml.rels", `<Relationships ` + nsRel + `><Relationship Id="rId2" Target="slides/slide1.xml"/><Relationship Id="rId3" Target="slides/slide2.xml"/></Relationships>`,
	"ppt/slides/slide2.xml", `<p:sld ` + nsP + `><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>Agenda</a:t></a:r></a:p><a:p/><a:p><a:r><a:t>First</a:t></a:r><a:br/><a:r><a:t>Second</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`,
	"ppt/slides/slide1.xml", `<p:sld ` + nsP + `><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>Thanks</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`,
}

func odf(body string) []string {
	return []string{"mimetype", "application/vnd.oasis.opendocument", "content.xml", `<office:document-content ` + nsODF + `><office:body>` + body + `</office:body></office:document-content>`}
}

var (
	odt = odf(`<office:text><text:tracked-changes><text:changed-region><text:deletion><text:p>removed</text:p></text:deletion></text:changed-region></text:tracked-changes>` +
		`<text:h>Title</text:h><text:p>A<text:s text:c="2"/>B<text:tab/>C<text:line-break/>D</text:p><text:p/>` +
		`<table:table table:name="T1"><table:table-row><table:table-cell><text:p>x</text:p></table:table-cell><table:table-cell><text:p>y</text:p></table:table-cell></table:table-row></table:table></office:text>`)
	ods = odf(`<office:spreadsheet><table:table table:name="Budget">` +
		`<table:table-row><table:table-cell><text:p>Item</text:p></table:table-cell><table:table-cell table:number-columns-repeated="2"><text:p>v</text:p></table:table-cell><table:table-cell table:number-columns-repeated="1000"/></table:table-row>` +
		`<table:table-row table:number-rows-repeated="1048000"><table:table-cell table:number-columns-repeated="1024"/></table:table-row>` +
		`<table:table-row table:number-rows-repeated="2"><table:table-cell><text:p>same</text:p></table:table-cell></table:table-row></table:table></office:spreadsheet>`)
	odp = odf(`<office:presentation><draw:page draw:name="p1"><draw:frame><draw:text-box><text:p>Hello</text:p><text:p/></draw:text-box></draw:frame></draw:page>` +
		`<draw:page draw:name="p2"><draw:frame><draw:text-box><text:p>Bye</text:p></draw:text-box></draw:frame></draw:page></office:presentation>`)
)

// numbered is lines numbered from 1 as read shows them.
func numbered(lines ...string) string {
	var b strings.Builder
	for i, l := range lines {
		b.WriteString("     " + strconv.Itoa(i+1) + "\t" + l + "\n")
	}
	return b.String()
}

// TestReadReadsOfficeDocuments: read returns a Word, Excel or PowerPoint
// document's text and an OpenDocument file's, after the line that names
// its kind, as numbered lines that offset and limit page, with the
// file's own digest (spec 079).
func TestReadReadsOfficeDocuments(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	read := builtinTool(t, NameRead)
	for _, c := range []struct {
		name, kind string
		parts      []string
		lines      []string
	}{
		{"report.docx", "a Word document", docx, []string{"Quarterly report", "Total\t42 & more", "", "Month\tSales", "July (est.)\t10", "Line one", "Line two", "Choice"}},
		{"Sales.XLSX", "an Excel workbook", xlsx, []string{"[Sheet: Sales]", "Month\tTotal", "July\t\t12.5\tTRUE\tFALSE", "x\t7", "[Sheet: Notes]", "日本語"}},
		{"deck.pptx", "a PowerPoint presentation", pptx, []string{"[Slide 1]", "Agenda", "First", "Second", "[Slide 2]", "Thanks"}},
		{"notes.odt", "an OpenDocument text", odt, []string{"Title", "A  B\tC", "D", "", "x\ty"}},
		{"budget.ods", "an OpenDocument spreadsheet", ods, []string{"[Sheet: Budget]", "Item\tv\tv", "same", "same"}},
		{"talk.odp", "an OpenDocument presentation", odp, []string{"[Slide 1]", "Hello", "[Slide 2]", "Bye"}},
	} {
		body := archiveOf(t, c.parts...)
		p := filepath.Join(f.work, c.name)
		put(t, p, body)
		head := p + " is " + c.kind + "; its text follows, without formatting or images.\n"
		res := run(ctx, t, read, f.h, State{}, mustInput(t, map[string]string{"path": c.name}))
		if res.Outcome != OutcomeOK || text(res) != head+numbered(c.lines...) {
			t.Fatalf("%s: %s\n%s", c.name, res.Outcome, text(res))
		}
		if res.Meta == nil || res.Meta.Path != p || res.Meta.SHA256 != sum(body) {
			t.Fatalf("%s: meta %+v", c.name, res.Meta)
		}
	}

	res := run(ctx, t, read, f.h, State{}, `{"path":"Sales.XLSX","offset":2,"limit":1}`)
	want := filepath.Join(f.work, "Sales.XLSX") + " is an Excel workbook; its text follows, without formatting or images.\n" +
		"     2\tMonth\tTotal\n[lines 2 to 2 of 6; read on with offset 3]\n"
	if text(res) != want {
		t.Fatalf("offset and limit %q", text(res))
	}

	put(t, filepath.Join(f.work, "blank.docx"), archiveOf(t, "word/document.xml", `<w:document `+nsW+`><w:body></w:body></w:document>`))
	res = run(ctx, t, read, f.h, State{}, `{"path":"blank.docx"}`)
	if p := filepath.Join(f.work, "blank.docx"); res.Outcome != OutcomeOK || text(res) != p+" is a Word document; its text follows, without formatting or images.\n"+p+" is empty." {
		t.Fatalf("a document without text %q", text(res))
	}

	// A file named as a document that is no archive is read for what it
	// is.
	put(t, filepath.Join(f.work, "plain.docx"), "just text\n")
	if res := run(ctx, t, read, f.h, State{}, `{"path":"plain.docx"}`); text(res) != numbered("just text") {
		t.Fatalf("a text file named .docx %q", text(res))
	}
}

// TestReadFallsBackOnABrokenDocument: a document whose archive, parts or
// relationships do not read, or whose part inflates past its bound, is
// answered as a binary file is (spec 079).
func TestReadFallsBackOnABrokenDocument(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	read := builtinTool(t, NameRead)
	for name, body := range map[string]string{
		"truncated.docx": archiveOf(t, docx...)[:40],
		"empty.docx":     archiveOf(t, "other.xml", "<x/>"),
		"broken.docx":    archiveOf(t, "word/document.xml", "<w:document><w:body><w:p>"),
		"norels.xlsx":    archiveOf(t, xlsx[0], xlsx[1]),
		"unknown.pptx":   archiveOf(t, pptx[0], pptx[1], pptx[2], pptx[3]),
		"nocontent.odt":  archiveOf(t, "mimetype", "application/vnd.oasis.opendocument.text"),
		"badrels.xlsx":   archiveOf(t, xlsx[0], xlsx[1], `xl/_rels/workbook.xml.rels`, `<Relationships `+nsRel+`><Relationship Id="rId1" Target="worksheets/sheet2.xml"/></Relationships>`),
	} {
		p := filepath.Join(f.work, name)
		put(t, p, body)
		res := run(ctx, t, read, f.h, State{}, mustInput(t, map[string]string{"path": name}))
		if res.Outcome != OutcomeError || text(res) != p+" is a binary file; read shows text files and PNG, JPEG, GIF and WebP images. Inspect it with bash." || res.Meta != nil {
			t.Fatalf("%s: %s %q", name, res.Outcome, text(res))
		}
	}

	// A part that inflates past its bound is not read whole.
	c := &capped{r: strings.NewReader("12345"), d: &archive{left: 4}}
	if _, err := bytes.NewBuffer(nil).ReadFrom(c); err == nil {
		t.Fatal("a part past its bound read whole")
	}
	if _, err := documentText(t.Context(), "rtf", []byte(archiveOf(t, "x", "y"))); err == nil {
		t.Fatal("a kind read has no reader for was read")
	}
	o := &textOut{}
	o.line("kept")
	o.line(strings.Repeat("x", maxDocumentText))
	o.line("after")
	if o.b.String() != "kept\n" || !o.full {
		t.Fatalf("the text past its bound is %d bytes", o.b.Len())
	}
}

// TestCellReferencesNameTheirColumn: a cell reference names its column
// from 0, a reference past a sheet's widest is held there, and one with
// no letters names none.
func TestCellReferencesNameTheirColumn(t *testing.T) {
	for ref, want := range map[string]int{"A1": 0, "z9": 25, "AA3": 26, "AB": 27, "XFD1": 16383, "ZZZZ1": maxColumns} {
		if got, ok := column(ref); !ok || got != want {
			t.Errorf("%s: %d %t, want %d", ref, got, ok, want)
		}
	}
	if _, ok := column("12"); ok {
		t.Error("a reference without letters names a column")
	}
	if repeat("x") != 1 || repeat("0") != 1 || repeat("3") != 3 || repeat("100000") != maxRepeat {
		t.Error("repeat does not bound its count")
	}
}
