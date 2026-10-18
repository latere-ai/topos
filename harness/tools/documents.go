// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"

	"latere.ai/x/topos/prompts"
)

// The bounds of an office document read reads as text (spec 079).
const (
	// ReadMaxDocument bounds the bytes of a document read reads as text;
	// a larger one is answered as a binary file.
	ReadMaxDocument = 32 << 20
	// ReadMaxDocumentInflate bounds the bytes a document's parts inflate
	// to together, each part as often as it is read, so an archive that
	// inflates past it is answered as a binary file, and the time a read
	// takes is bounded with it.
	ReadMaxDocumentInflate = 64 << 20
	// maxDocumentText bounds the text kept of one document; the text past
	// it is left out, as the lines past a read's limit are. A line is held
	// to it as it is built, so no repeat or join builds one past it, and
	// the read stops once the text is full.
	maxDocumentText = 8 << 20
	// checkEvery is how many XML tokens a read decodes between two looks
	// at its context.
	checkEvery = 1024
	// maxColumns is the most columns of a spreadsheet's row read keeps,
	// a spreadsheet's widest sheet.
	maxColumns = 16384
	// maxRepeat bounds an OpenDocument cell, row or run of spaces that a
	// repeat attribute multiplies.
	maxRepeat = 256
)

// zipMagic opens a zip archive's first local file header.
var zipMagic = []byte("PK\x03\x04")

// documentKinds are the office documents read reads as text, by the
// extension of their name without its dot.
var documentKinds = map[string]bool{"docx": true, "xlsx": true, "pptx": true, "odt": true, "ods": true, "odp": true}

// documentKind is the kind of office document p names by its extension,
// "" for none.
func documentKind(p string) string {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(p), "."))
	if documentKinds[ext] {
		return ext
	}
	return ""
}

// The failures that make a document read as a binary file, and the end
// of a read whose text is full.
var (
	errPartMissing  = errors.New("tools: the document holds no such part")
	errPartTooLarge = errors.New("tools: the document's parts inflate past their bound")
	errFull         = errors.New("tools: the document's text is full")
)

// documentText is the text of the office document data of kind, as read
// shows it, or an error for an archive or a part that does not read, and
// ctx's error once it ends.
func documentText(ctx context.Context, kind string, data []byte) (string, error) {
	d, err := openArchive(ctx, data)
	if err != nil {
		return "", err
	}
	return d.text(kind)
}

// openArchive opens data as a document's zip archive, read under ctx.
func openArchive(ctx context.Context, data []byte) (*archive, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	d := &archive{ctx: ctx, files: map[string]*zip.File{}, left: ReadMaxDocumentInflate, out: &textOut{}}
	for _, f := range zr.File {
		d.files[f.Name] = f
	}
	return d, nil
}

// text reads the archive's text as a document of kind. A read whose text
// fills is its text so far.
func (d *archive) text(kind string) (string, error) {
	var err error
	switch kind {
	case "docx":
		err = d.docx(d.out)
	case "xlsx":
		err = d.xlsx(d.out)
	case "pptx":
		err = d.pptx(d.out)
	case "odt", "ods", "odp":
		err = d.odf(kind, d.out)
	default:
		err = fmt.Errorf("tools: %q is no document kind", kind)
	}
	if err != nil && !errors.Is(err, errFull) {
		return "", err
	}
	return d.out.b.String(), nil
}

// textOut is a document's text as it is read, at most maxDocumentText
// bytes of whole lines; full once a line does not fit.
type textOut struct {
	b    strings.Builder
	full bool
}

// room is how many bytes a line may still hold, its newline aside.
func (o *textOut) room() int { return maxDocumentText - o.b.Len() - 1 }

// line adds one line of text.
func (o *textOut) line(s string) {
	if o.full || len(s) > o.room() {
		o.full = true
		return
	}
	o.b.WriteString(s)
	o.b.WriteByte('\n')
}

// piece is a part of a line as it is read, a paragraph or a cell, which
// takes no more bytes than the text has room for: a piece that would pass
// it fills the text, since its line cannot fit.
type piece struct {
	b   strings.Builder
	out *textOut
}

func (p *piece) write(s string) {
	if p.fits(len(s)) {
		p.b.WriteString(s)
	}
}

func (p *piece) writeByte(c byte) {
	if p.fits(1) {
		p.b.WriteByte(c)
	}
}

// writeData adds the text of an XML token without a copy of its own.
func (p *piece) writeData(b []byte) {
	if p.fits(len(b)) {
		p.b.Write(b)
	}
}

// fits reports whether n more bytes fit the piece, and fills the text
// when they do not.
func (p *piece) fits(n int) bool {
	if p.out.full || p.b.Len()+n > p.out.room() {
		p.out.full = true
		return false
	}
	return true
}

// join adds s to a cell's text, a space between it and what is there.
func (p *piece) join(s string) {
	if s == "" {
		return
	}
	if p.b.Len() > 0 {
		p.write(" ")
	}
	p.write(s)
}

// row is a line of cells joined by tabs as it is read, counted as each
// cell is set, so a row that would pass the text's room fills the text
// before its line is ever joined.
type row struct {
	cells []string
	bytes int
	out   *textOut
}

// reset starts the next row.
func (r *row) reset() { r.cells, r.bytes = r.cells[:0], 0 }

// set puts v at column col, a column the row skips an empty cell. A
// column past maxColumns is left out.
func (r *row) set(col int, v string) {
	if col >= maxColumns || r.out.full {
		return
	}
	for len(r.cells) <= col {
		r.cells = append(r.cells, "")
		r.bytes++
	}
	r.bytes += len(v) - len(r.cells[col])
	r.cells[col] = v
	if r.bytes > r.out.room() {
		r.out.full = true
	}
}

// add puts v after the row's last cell.
func (r *row) add(v string) { r.set(len(r.cells), v) }

// end writes the row as a line, its empty cells at its end left out,
// times times, and nothing for a row that holds no value.
func (r *row) end(times int) {
	for len(r.cells) > 0 && r.cells[len(r.cells)-1] == "" {
		r.cells = r.cells[:len(r.cells)-1]
	}
	if len(r.cells) == 0 || r.out.full {
		return
	}
	line := strings.Join(r.cells, "\t")
	for range times {
		r.out.line(line)
	}
}

// archive is a document's zip archive by the name of each part, read
// under ctx into out, its parts inflating within left bytes together.
type archive struct {
	ctx   context.Context
	files map[string]*zip.File
	out   *textOut
	// left is the bytes the document's parts may still inflate to, and
	// inflated the bytes they did.
	left, inflated int64
}

// capped reads r within its archive's inflate budget and fails past it.
type capped struct {
	r io.Reader
	d *archive
}

func (c *capped) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.d.left -= int64(n)
	c.d.inflated += int64(n)
	if c.d.left < 0 {
		return n, errPartTooLarge
	}
	return n, err
}

// each calls fn with every XML token of the part name, in order, until
// the part ends, the text is full (errFull), or ctx ends.
func (d *archive) each(name string, fn func(xml.Token)) (err error) {
	f, ok := d.files[name]
	if !ok {
		return fmt.Errorf("%w: %s", errPartMissing, name)
	}
	if f.UncompressedSize64 > uint64(max(d.left, 0)) {
		return fmt.Errorf("%w: %s", errPartTooLarge, name)
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer func() {
		if cerr := rc.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	dec := xml.NewDecoder(&capped{r: rc, d: d})
	// fallback counts the open elements of a markup compatibility
	// Fallback, the older copy of content its Choice already holds,
	// whose text would otherwise be read twice.
	fallback := 0
	for n := 0; ; n++ {
		if n%checkEvery == 0 {
			if err := d.ctx.Err(); err != nil {
				return err
			}
		}
		if d.out.full {
			return errFull
		}
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tools: read %s: %w", name, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if fallback > 0 || (t.Name.Local == "Fallback" && t.Name.Space == markupCompatibility) {
				fallback++
				continue
			}
		case xml.EndElement:
			if fallback > 0 {
				fallback--
				continue
			}
		default:
			if fallback > 0 {
				continue
			}
		}
		fn(tok)
	}
}

// markupCompatibility is the namespace of an Office document's
// alternate content.
const markupCompatibility = "http://schemas.openxmlformats.org/markup-compatibility/2006"

// attr is the value of the attribute of e named local in any namespace.
func attr(e xml.StartElement, local string) string {
	for _, a := range e.Attr {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

// relID is the relationship id an element names, its r:id: the id
// attribute in a namespace, which an element's own id is not.
func relID(e xml.StartElement) string {
	for _, a := range e.Attr {
		if a.Name.Local == "id" && a.Name.Space != "" {
			return a.Value
		}
	}
	return ""
}

// rels are the targets of the relationships of the part at base, by id,
// each resolved against base's directory; an external target is left
// out.
func (d *archive) rels(base string) (map[string]string, error) {
	dir, file := path.Split(base)
	out := map[string]string{}
	err := d.each(dir+"_rels/"+file+".rels", func(tok xml.Token) {
		e, ok := tok.(xml.StartElement)
		if !ok || e.Name.Local != "Relationship" || attr(e, "TargetMode") == "External" {
			return
		}
		target := attr(e, "Target")
		if t, abs := strings.CutPrefix(target, "/"); abs {
			target = t
		} else {
			target = path.Join(dir, target)
		}
		out[attr(e, "Id")] = target
	})
	return out, err
}

// targets are the parts the ids name through rels, in the ids' order,
// each part once, and the index among ids of each.
func targets(ids []string, rels map[string]string) ([]string, []int, error) {
	var out []string
	var at []int
	seen := map[string]bool{}
	for i, id := range ids {
		t, ok := rels[id]
		if !ok {
			return nil, nil, fmt.Errorf("%w: the relationship %q", errPartMissing, id)
		}
		if !seen[t] {
			seen[t] = true
			out, at = append(out, t), append(at, i)
		}
	}
	return out, at, nil
}

// docx reads a Word document: each paragraph of its body a line, and
// each row of a table a line of its cells joined by tabs, a cell's
// paragraphs joined by spaces. A nested table's text joins the cell of
// the outer table it is in.
func (d *archive) docx(out *textOut) error {
	para, cell := &piece{out: out}, &piece{out: out}
	cells := &row{out: out}
	tables, runs := 0, 0
	inText := false
	return d.each("word/document.xml", func(tok xml.Token) {
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "tbl":
				tables++
			case "tr":
				if tables == 1 {
					cells.reset()
				}
			case "tc":
				if tables == 1 {
					cell.b.Reset()
				}
			case "r":
				runs++
			case "t":
				inText = runs > 0
			case "tab":
				// A tab stop of a paragraph's properties is a tab too, by
				// its name; only a run's is text.
				if runs > 0 {
					para.writeByte('\t')
				}
			case "br", "cr":
				if runs > 0 {
					para.writeByte(lineBreak(tables > 0))
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "r":
				runs--
			case "p":
				if tables > 0 {
					cell.join(para.b.String())
				} else {
					out.line(para.b.String())
				}
				para.b.Reset()
			case "tc":
				if tables == 1 {
					cells.add(cell.b.String())
				}
			case "tr":
				if tables == 1 {
					cells.end(1)
				}
			case "tbl":
				tables--
			}
		case xml.CharData:
			if inText {
				para.writeData(t)
			}
		}
	})
}

// lineBreak is a break inside a paragraph: a new line, or a space in a
// table's cell, whose row is one line.
func lineBreak(inTable bool) byte {
	if inTable {
		return ' '
	}
	return '\n'
}

// xlsx reads an Excel workbook: each sheet in the workbook's order under
// the line that names it, each row a line of its cells joined by tabs,
// a column the row skips an empty field. A workbook without shared
// strings has none.
func (d *archive) xlsx(out *textOut) error {
	const book = "xl/workbook.xml"
	var names, ids []string
	if err := d.each(book, func(tok xml.Token) {
		if e, ok := tok.(xml.StartElement); ok && e.Name.Local == "sheet" {
			names, ids = append(names, attr(e, "name")), append(ids, relID(e))
		}
	}); err != nil {
		return err
	}
	rels, err := d.rels(book)
	if err != nil {
		return err
	}
	parts, at, err := targets(ids, rels)
	if err != nil {
		return err
	}
	shared, err := d.sharedStrings()
	if err != nil {
		return err
	}
	for i, p := range parts {
		out.line(prompts.Render(prompts.ReadSheet, prompts.Data{"Name": names[at[i]]}))
		if err := d.sheet(p, shared, out); err != nil {
			return err
		}
	}
	return nil
}

// sharedStrings are a workbook's shared strings in order, each the text
// of its runs without its phonetic guide, held to the text's room.
func (d *archive) sharedStrings() ([]string, error) {
	var out []string
	cur := &piece{out: &textOut{}}
	phonetic := 0
	inText := false
	err := d.each("xl/sharedStrings.xml", func(tok xml.Token) {
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				cur.b.Reset()
				cur.out.full = false
			case "rPh":
				phonetic++
			case "t":
				inText = phonetic == 0
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				out = append(out, cur.b.String())
			case "rPh":
				phonetic--
			case "t":
				inText = false
			}
		case xml.CharData:
			if inText {
				cur.writeData(t)
			}
		}
	})
	if errors.Is(err, errPartMissing) {
		return nil, nil
	}
	return out, err
}

// sheet reads one worksheet's rows: each cell at the column its
// reference names, or after the cell before it, its value resolved by
// its type, and a row that holds no value left out.
func (d *archive) sheet(name string, shared []string, out *textOut) error {
	fields := &row{out: out}
	value := &piece{out: out}
	col, next := 0, 0
	typ := ""
	inValue := false
	return d.each(name, func(tok xml.Token) {
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				fields.reset()
				next = 0
			case "c":
				typ, col = attr(t, "t"), next
				if c, ok := column(attr(t, "r")); ok {
					col = c
				}
				value.b.Reset()
			case "v", "t":
				inValue = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v", "t":
				inValue = false
			case "c":
				next = col + 1
				fields.set(col, cellValue(typ, value.b.String(), shared))
			case "row":
				fields.end(1)
			}
		case xml.CharData:
			if inValue {
				value.writeData(t)
			}
		}
	})
}

// column is the column a cell reference such as "AB12" names, counted
// from 0, false for a reference that names none.
func column(ref string) (int, bool) {
	n, letters := 0, 0
	for _, r := range ref {
		switch {
		case r >= 'A' && r <= 'Z':
			n = n*26 + int(r-'A'+1)
		case r >= 'a' && r <= 'z':
			n = n*26 + int(r-'a'+1)
		default:
			return n - 1, letters > 0
		}
		letters++
		if n > maxColumns {
			return maxColumns, true
		}
	}
	return n - 1, letters > 0
}

// cellValue is a cell's value as read shows it: a shared string by its
// index, a boolean as TRUE or FALSE, and any other, a number, an inline
// string, a formula's string or an error, as written.
func cellValue(typ, v string, shared []string) string {
	switch typ {
	case "s":
		i, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || i < 0 || i >= len(shared) {
			return ""
		}
		return shared[i]
	case "b":
		if strings.TrimSpace(v) == "1" {
			return "TRUE"
		}
		return "FALSE"
	}
	return v
}

// pptx reads a PowerPoint presentation: each slide in the
// presentation's order under the line that names its number, each
// paragraph that holds text a line.
func (d *archive) pptx(out *textOut) error {
	const pres = "ppt/presentation.xml"
	var ids []string
	if err := d.each(pres, func(tok xml.Token) {
		if e, ok := tok.(xml.StartElement); ok && e.Name.Local == "sldId" {
			ids = append(ids, relID(e))
		}
	}); err != nil {
		return err
	}
	rels, err := d.rels(pres)
	if err != nil {
		return err
	}
	slides, _, err := targets(ids, rels)
	if err != nil {
		return err
	}
	for i, s := range slides {
		out.line(prompts.Render(prompts.ReadSlide, prompts.Data{"Number": i + 1}))
		para := &piece{out: out}
		inText := false
		if err := d.each(s, func(tok xml.Token) {
			switch t := tok.(type) {
			case xml.StartElement:
				switch t.Name.Local {
				case "t":
					inText = true
				case "br":
					para.writeByte('\n')
				}
			case xml.EndElement:
				switch t.Name.Local {
				case "t":
					inText = false
				case "p":
					if strings.TrimSpace(para.b.String()) != "" {
						out.line(para.b.String())
					}
					para.b.Reset()
				}
			case xml.CharData:
				if inText {
					para.writeData(t)
				}
			}
		}); err != nil {
			return err
		}
	}
	return nil
}

// odf reads an OpenDocument file's content.xml: each paragraph and
// heading a line, each row of a table a line of its cells joined by
// tabs, a spreadsheet's tables under the line that names each as a
// sheet, and a presentation's pages under the line that names each as a
// slide. A text keeps its empty paragraphs; a spreadsheet and a
// presentation, whose empty ones are layout, do not. A cell or a row a
// repeat attribute multiplies is repeated at most maxRepeat times, and
// the tracked changes' record of deleted text is left out.
func (d *archive) odf(kind string, out *textOut) error {
	para, cell := &piece{out: out}, &piece{out: out}
	cells := &row{out: out}
	paras, tables, deleted, pages := 0, 0, 0, 0
	cellRepeat, rowRepeat := 1, 1
	keepEmpty := kind == "odt"
	return d.each("content.xml", func(tok xml.Token) {
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "tracked-changes":
				deleted++
			case "page":
				pages++
				out.line(prompts.Render(prompts.ReadSlide, prompts.Data{"Number": pages}))
			case "table":
				tables++
				if tables == 1 && kind == "ods" {
					out.line(prompts.Render(prompts.ReadSheet, prompts.Data{"Name": attr(t, "name")}))
				}
			case "table-row":
				if tables == 1 {
					cells.reset()
					rowRepeat = repeat(attr(t, "number-rows-repeated"))
				}
			case "table-cell", "covered-table-cell":
				if tables == 1 {
					cell.b.Reset()
					cellRepeat = repeat(attr(t, "number-columns-repeated"))
				}
			case "p", "h":
				if deleted == 0 {
					paras++
				}
			case "tab":
				if paras > 0 {
					para.writeByte('\t')
				}
			case "s":
				if paras > 0 {
					para.write(strings.Repeat(" ", repeat(attr(t, "c"))))
				}
			case "line-break":
				if paras > 0 {
					para.writeByte(lineBreak(tables > 0))
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "tracked-changes":
				deleted--
			case "table":
				tables--
			case "p", "h":
				if deleted > 0 {
					return
				}
				paras--
				if paras > 0 {
					return
				}
				switch s := para.b.String(); {
				case tables > 0:
					cell.join(s)
				case keepEmpty || strings.TrimSpace(s) != "":
					out.line(s)
				}
				para.b.Reset()
			case "table-cell", "covered-table-cell":
				if tables == 1 {
					for range cellRepeat {
						cells.add(cell.b.String())
					}
				}
			case "table-row":
				if tables == 1 {
					cells.end(rowRepeat)
				}
			}
		case xml.CharData:
			if paras > 0 {
				para.writeData(t)
			}
		}
	})
}

// repeat is a repeat attribute's count: 1 when it is absent or not a
// count, and at most maxRepeat.
func repeat(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 1
	}
	return min(n, maxRepeat)
}
