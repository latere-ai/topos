// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"unicode/utf8"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/parsing"
	"latere.ai/x/topos/prompts"
)

// Limits of read (spec 008).
const (
	ReadDefaultLimit = 2000
	ReadMaxLineChars = 2000
	ReadMaxImage     = 5 << 20
	// binarySniff is how many leading bytes decide whether a file is
	// binary, the rule grep uses: one NUL among them is.
	binarySniff = 8000
)

// MaxImages is the most images one message carries: the images of a
// person's message (spec 015), and the images kept from an agent's
// answer (spec 055), each at most ReadMaxImage bytes.
const MaxImages = 8

// SniffBytes is how many leading bytes of a file name its media type
// when its name's extension names none: what http.DetectContentType
// reads. The files route and a kept file read no more of a file to type
// it (specs 044 and 064).
const SniffBytes = 512

// ReadMaxPDF bounds the bytes of a PDF read sends to a parsing service
// (spec 082): the service's own bound on one file, which a file a person
// brings is held to as well (spec 081).
const ReadMaxPDF = parsing.MaxFileBytes

// pdfMagic opens every PDF.
var pdfMagic = []byte("%PDF-")

// readSchema states the default from ReadDefaultLimit, so the schema
// cannot drift from what runRead does.
var readSchema = fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "minLength": 1, "description": "The file to read: absolute, or relative to the working directory."},
    "offset": {"type": "integer", "minimum": 1, "description": "The first line to show, counted from 1."},
    "limit": {"type": "integer", "minimum": 1, "description": "How many lines to show; default %d."}
  },
  "required": ["path"],
  "additionalProperties": false
}`, ReadDefaultLimit)

// readParsedSchema is readSchema with pages, the schema of a read that
// reads a PDF through a parsing service (spec 082).
var readParsedSchema = fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "minLength": 1, "description": "The file to read: absolute, or relative to the working directory."},
    "offset": {"type": "integer", "minimum": 1, "description": "The first line to show, counted from 1."},
    "limit": {"type": "integer", "minimum": 1, "description": "How many lines to show; default %d."},
    "pages": {"type": "string", "minLength": 1, "maxLength": %d, "description": "For a PDF, the pages to read, such as 1-5, 1-3,7 or 9-; default %s."}
  },
  "required": ["path"],
  "additionalProperties": false
}`, ReadDefaultLimit, parsing.MaxSelection, parsing.DefaultSelection)

func readTool() Tool { return Read(nil) }

// Read is the read tool. With r, a PDF of the session's folder is read as
// text through r, a parsing service (spec 082), its description is
// version 3 and its schema takes pages; with nil it is the built-in of
// spec 008, which Builtins holds.
func Read(r parsing.Reader) Tool {
	props := Properties{Parallel: true, Effect: EffectRead}
	run := func(ctx context.Context, b *builtin, c Call) (Result, error) { return runRead(ctx, b, c, r) }
	if r == nil {
		return pathTool{newBuiltin(NameRead, prompts.Text(prompts.ToolRead), readSchema, props, run)}
	}
	return pathTool{newBuiltin(NameRead, prompts.Text(prompts.ToolReadParsed), readParsedSchema, props, run)}
}

type readInput struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
	Pages  string `json:"pages"`
}

// imageTypes are the image formats read returns as an image block, by
// their leading bytes.
var imageTypes = []struct {
	media string
	match func([]byte) bool
}{
	{"image/png", func(b []byte) bool { return bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")) }},
	{"image/jpeg", func(b []byte) bool { return bytes.HasPrefix(b, []byte("\xff\xd8\xff")) }},
	{"image/gif", func(b []byte) bool {
		return bytes.HasPrefix(b, []byte("GIF87a")) || bytes.HasPrefix(b, []byte("GIF89a"))
	}},
	{"image/webp", func(b []byte) bool {
		return len(b) >= 12 && bytes.HasPrefix(b, []byte("RIFF")) && string(b[8:12]) == "WEBP"
	}},
}

// ImageType is the media type of an image format read returns as an
// image block, from the file's leading bytes, or "" for any other file.
func ImageType(head []byte) string {
	for _, t := range imageTypes {
		if t.match(head) {
			return t.media
		}
	}
	return ""
}

func runRead(ctx context.Context, b *builtin, c Call, pr parsing.Reader) (res Result, err error) {
	var in readInput
	if r := b.decode(c, &in); r != nil {
		return *r, nil
	}
	p := resolve(c.Machine, in.Path)
	fi, err := c.Machine.Stat(ctx, p)
	if err != nil {
		return b.fail(ctx, c, p, err)
	}
	if fi.IsDir {
		return b.result(ctx, c, OutcomeError, prompts.Render(prompts.ReadDirectory, prompts.Data{"Path": p}), nil)
	}
	rc, err := c.Machine.ReadFile(ctx, p)
	if err != nil {
		return b.fail(ctx, c, p, err)
	}
	defer func() {
		if cerr := rc.Close(); cerr != nil && err == nil {
			res, err = Result{}, fmt.Errorf("tools: close %s: %w", p, cerr)
		}
	}()
	h := sha256.New()
	br := bufio.NewReaderSize(io.TeeReader(rc, h), 64<<10)
	head, perr := br.Peek(binarySniff)
	if perr != nil && !errors.Is(perr, io.EOF) && !errors.Is(perr, bufio.ErrBufferFull) {
		return b.fail(ctx, c, p, perr)
	}
	if media := ImageType(head); media != "" {
		return readImage(ctx, b, c, p, media, fi.Size, br, h)
	}
	if pr != nil && bytes.HasPrefix(head, pdfMagic) {
		return readPDF(ctx, b, c, p, in, pr, fi.Size, br, h)
	}
	if kind := documentKind(p); kind != "" && bytes.HasPrefix(head, zipMagic) {
		return readDocument(ctx, b, c, p, kind, in, br, h)
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return b.result(ctx, c, OutcomeError, prompts.Render(prompts.ReadBinary, prompts.Data{"Path": p}), nil)
	}
	return readLines(ctx, b, c, p, in, br, h, "", "")
}

// readDocument returns the text of an office document of kind (spec
// 079) as numbered lines after the line that names its kind, and its
// meta the file's own digest. A document past ReadMaxDocument bytes, and
// one whose archive or parts do not read, is answered as a binary file
// is: the document's text is a reading of the file read offers, and a
// file it cannot read is the file read cannot show.
func readDocument(ctx context.Context, b *builtin, c Call, p, kind string, in readInput, br io.Reader, h hash.Hash) (Result, error) {
	data, err := io.ReadAll(io.LimitReader(br, ReadMaxDocument+1))
	if err != nil {
		return b.fail(ctx, c, p, err)
	}
	text := ""
	if len(data) <= ReadMaxDocument {
		text, err = documentText(ctx, kind, data)
	}
	if cerr := ctx.Err(); cerr != nil {
		return b.fail(ctx, c, p, cerr)
	}
	if len(data) > ReadMaxDocument || err != nil {
		return b.result(ctx, c, OutcomeError, prompts.Render(prompts.ReadBinary, prompts.Data{"Path": p}), nil)
	}
	head := prompts.Render(prompts.ReadDocument, prompts.Data{"Path": p, "Kind": kind}) + "\n"
	return readLines(ctx, b, c, p, in, bufio.NewReader(strings.NewReader(text)), h, head, "")
}

// readLines returns the lines of br numbered from in's offset, at most
// its limit of them, each cut at ReadMaxLineChars, between head and
// tail, and its meta the digest h holds once br is read to its end.
func readLines(ctx context.Context, b *builtin, c Call, p string, in readInput, br *bufio.Reader, h hash.Hash, head, tail string) (Result, error) {
	offset := max(in.Offset, 1)
	limit := in.Limit
	if limit <= 0 {
		limit = ReadDefaultLimit
	}
	var out strings.Builder
	out.WriteString(head)
	n, shown := 0, 0
	for {
		line, long, more, rerr := readLine(br)
		if rerr != nil {
			return b.fail(ctx, c, p, rerr)
		}
		if !more {
			break
		}
		n++
		if n < offset || shown == limit {
			continue
		}
		shown++
		text := string(line)
		if utf8.RuneCountInString(text) > ReadMaxLineChars {
			text, long = cutRunes(text, ReadMaxLineChars), true
		}
		if long {
			text += " " + prompts.Render(prompts.ReadLineCut, prompts.Data{"Max": ReadMaxLineChars})
		}
		fmt.Fprintf(&out, "%6d\t%s\n", n, text)
	}
	meta := &Meta{Path: p, SHA256: hex.EncodeToString(h.Sum(nil))}
	switch {
	case n == 0:
		return b.result(ctx, c, OutcomeOK, head+prompts.Render(prompts.ReadEmpty, prompts.Data{"Path": p}), meta)
	case offset > n:
		return b.result(ctx, c, OutcomeError, prompts.Render(prompts.ReadPastEnd, prompts.Data{"Path": p, "Lines": plural(n, "line"), "Offset": offset}), meta)
	}
	if last := offset + shown - 1; last < n {
		out.WriteString(prompts.Render(prompts.ReadMoreLines, prompts.Data{"From": offset, "To": last, "Total": n, "Next": last + 1}) + "\n")
	}
	out.WriteString(tail)
	return b.result(ctx, c, OutcomeOK, out.String(), meta)
}

// readImage returns an image of at most ReadMaxImage bytes as an image
// block.
func readImage(ctx context.Context, b *builtin, c Call, p, media string, size int64, br io.Reader, h hash.Hash) (Result, error) {
	data, err := io.ReadAll(io.LimitReader(br, ReadMaxImage+1))
	if err != nil {
		return b.fail(ctx, c, p, err)
	}
	if len(data) > ReadMaxImage {
		return b.result(ctx, c, OutcomeError, prompts.Render(prompts.ReadImageTooLarge, prompts.Data{"Path": p, "Size": size, "Max": ReadMaxImage >> 20}), nil)
	}
	return Result{
		Outcome: OutcomeOK,
		Content: []lux.Block{{Type: ir.BlockImage, Image: &lux.Image{MediaType: media, Data: base64.StdEncoding.EncodeToString(data)}}},
		Meta:    &Meta{Path: p, SHA256: hex.EncodeToString(h.Sum(nil))},
	}, nil
}

// maxLineBytes bounds what readLine keeps of one line: enough bytes for
// ReadMaxLineChars characters of up to four bytes each, and one more.
const maxLineBytes = ReadMaxLineChars*utf8.UTFMax + 1

// readLine reads one line without its newline, keeping at most
// maxLineBytes of it; long reports a line cut there. more is false at the
// end of the input.
func readLine(br *bufio.Reader) (line []byte, long, more bool, err error) {
	seen := 0
	for {
		chunk, rerr := br.ReadSlice('\n')
		seen += len(chunk)
		chunk = bytes.TrimSuffix(chunk, []byte("\n"))
		if room := maxLineBytes - len(line); room > 0 {
			line = append(line, chunk[:min(len(chunk), room)]...)
			long = long || len(chunk) > room
		} else if len(chunk) > 0 {
			long = true
		}
		switch {
		case errors.Is(rerr, bufio.ErrBufferFull):
			continue
		case errors.Is(rerr, io.EOF):
			return line, long, seen > 0, nil
		case rerr != nil:
			return nil, false, false, rerr
		}
		return line, long, true, nil
	}
}

// cutRunes keeps the first n characters of s.
func cutRunes(s string, n int) string {
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}
