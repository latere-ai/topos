// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"html"
	"strings"
	"unicode"
	"unicode/utf8"
)

// dropped are the elements whose content is not text a reader sees:
// scripts, styles, templates and drawings.
var dropped = map[string]bool{"script": true, "style": true, "noscript": true, "template": true, "svg": true}

// paragraphs are the block elements set off by a blank line.
var paragraphBlocks = map[string]bool{
	"p": true, "pre": true, "blockquote": true, "table": true, "ul": true, "ol": true, "dl": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "figure": true, "hr": true,
}

// lines are the block elements set on a line of their own.
var lineBlocks = map[string]bool{
	"br": true, "div": true, "li": true, "tr": true, "dt": true, "dd": true, "section": true, "article": true,
	"header": true, "footer": true, "nav": true, "main": true, "aside": true, "form": true, "title": true,
	"figcaption": true, "address": true, "details": true, "summary": true, "fieldset": true, "caption": true,
	"body": true, "html": true, "head": true, "option": true,
}

// HTMLText converts an HTML page to the text a reader sees: scripts,
// styles and comments are dropped, entities are decoded, whitespace is
// collapsed outside pre, block elements become line breaks, headings are
// marked with # and list items with -. It is a tolerant scan, not a
// parser: it reads any input and never fails.
func HTMLText(src []byte) string {
	lower := asciiLower(src)
	w := &textWriter{}
	pre := 0
	for i := 0; i < len(src); {
		lt := bytes.IndexByte(src[i:], '<')
		if lt < 0 {
			w.text(html.UnescapeString(string(src[i:])), pre > 0)
			break
		}
		if lt > 0 {
			w.text(html.UnescapeString(string(src[i:i+lt])), pre > 0)
		}
		i += lt
		switch {
		case bytes.HasPrefix(src[i:], []byte("<!--")):
			end := bytes.Index(src[i+4:], []byte("-->"))
			if end < 0 {
				return w.String()
			}
			i += 4 + end + 3
			continue
		case bytes.HasPrefix(src[i:], []byte("<!")), bytes.HasPrefix(src[i:], []byte("<?")):
			end := bytes.IndexByte(src[i:], '>')
			if end < 0 {
				return w.String()
			}
			i += end + 1
			continue
		}
		name, closing, end := tag(lower, i)
		if name == "" {
			// A "<" that opens no tag is text.
			w.text("<", pre > 0)
			i++
			continue
		}
		i = end
		switch {
		case dropped[name] && !closing:
			// Skip to the element's end tag; an unclosed one runs to the
			// end of the page.
			stop := bytes.Index(lower[i:], []byte("</"+name))
			if stop < 0 {
				return w.String()
			}
			_, _, i = tag(lower, i+stop)
			continue
		case name == "pre":
			if closing {
				pre = max(pre-1, 0)
			} else {
				pre++
			}
		}
		switch {
		case paragraphBlocks[name]:
			w.breakLine(2)
			if !closing && len(name) == 2 && name[0] == 'h' && name[1] >= '1' && name[1] <= '6' {
				w.raw(strings.Repeat("#", int(name[1]-'0')) + " ")
			}
		case lineBlocks[name]:
			w.breakLine(1)
			if name == "li" && !closing {
				w.raw("- ")
			}
		case name == "td" || name == "th":
			w.space()
		}
	}
	return w.String()
}

// tag reads the tag that starts at src[i], a "<": its lower-case name,
// whether it closes an element, and the index past its ">". A "<" that
// is not followed by a tag name gives an empty name.
func tag(lower []byte, i int) (name string, closing bool, end int) {
	j := i + 1
	if j < len(lower) && lower[j] == '/' {
		closing = true
		j++
	}
	start := j
	for j < len(lower) && (lower[j] >= 'a' && lower[j] <= 'z' || j > start && (lower[j] >= '0' && lower[j] <= '9' || lower[j] == '-' || lower[j] == ':')) {
		j++
	}
	if j == start {
		return "", false, i + 1
	}
	name = string(lower[start:j])
	var quote byte
	for ; j < len(lower); j++ {
		switch c := lower[j]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return name, closing, j + 1
		}
	}
	return name, closing, len(lower)
}

// asciiLower lower-cases the ASCII letters of b and keeps every other
// byte, so indexes into the result are indexes into b.
func asciiLower(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}

// textWriter collects text with whitespace collapsed and line breaks
// merged, so markup never leaves runs of blank lines or spaces.
type textWriter struct {
	b strings.Builder
	// pending is a collapsed run of whitespace not yet written.
	pending bool
	// newlines is how many line breaks end the output.
	newlines int
}

func (w *textWriter) text(s string, pre bool) {
	if pre {
		w.raw(s)
		return
	}
	for _, r := range s {
		if unicode.IsSpace(r) {
			w.pending = true
			continue
		}
		if w.pending && w.b.Len() > 0 && w.newlines == 0 {
			w.b.WriteByte(' ')
		}
		w.pending = false
		w.b.WriteRune(r)
		w.newlines = 0
	}
}

// raw writes s as it is, as pre content and markers are.
func (w *textWriter) raw(s string) {
	if s == "" {
		return
	}
	if w.pending && w.b.Len() > 0 && w.newlines == 0 {
		w.b.WriteByte(' ')
	}
	w.pending = false
	w.b.WriteString(s)
	trimmed := strings.TrimRight(s, "\n")
	if trimmed == "" {
		w.newlines += len(s)
	} else {
		w.newlines = len(s) - len(trimmed)
	}
}

// space separates what follows by one space, as table cells are.
func (w *textWriter) space() {
	if w.b.Len() > 0 && w.newlines == 0 {
		w.pending = true
	}
}

// breakLine ends the output with at least n line breaks.
func (w *textWriter) breakLine(n int) {
	w.pending = false
	if w.b.Len() == 0 {
		return
	}
	for w.newlines < n {
		w.b.WriteByte('\n')
		w.newlines++
	}
}

// String is the text with trailing spaces trimmed from every line.
func (w *textWriter) String() string {
	s := strings.TrimSpace(w.b.String())
	if s == "" {
		return ""
	}
	var out strings.Builder
	for line := range strings.SplitSeq(s, "\n") {
		out.WriteString(strings.TrimRightFunc(line, unicode.IsSpace))
		out.WriteByte('\n')
	}
	if !utf8.ValidString(out.String()) {
		return strings.ToValidUTF8(out.String(), "\uFFFD")
	}
	return out.String()
}
