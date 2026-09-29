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

func readTool() Tool {
	return newBuiltin(NameRead, prompts.Text(prompts.ToolRead), readSchema, Properties{Parallel: true, Effect: EffectRead}, runRead)
}

type readInput struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
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

func runRead(ctx context.Context, b *builtin, c Call) (res Result, err error) {
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
	if bytes.IndexByte(head, 0) >= 0 {
		return b.result(ctx, c, OutcomeError, prompts.Render(prompts.ReadBinary, prompts.Data{"Path": p}), nil)
	}
	offset := max(in.Offset, 1)
	limit := in.Limit
	if limit <= 0 {
		limit = ReadDefaultLimit
	}
	var out strings.Builder
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
		return b.result(ctx, c, OutcomeOK, prompts.Render(prompts.ReadEmpty, prompts.Data{"Path": p}), meta)
	case offset > n:
		return b.result(ctx, c, OutcomeError, prompts.Render(prompts.ReadPastEnd, prompts.Data{"Path": p, "Lines": plural(n, "line"), "Offset": offset}), meta)
	}
	if last := offset + shown - 1; last < n {
		out.WriteString(prompts.Render(prompts.ReadMoreLines, prompts.Data{"From": offset, "To": last, "Total": n, "Next": last + 1}) + "\n")
	}
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
