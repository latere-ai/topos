// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package trigger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The roots a template path starts from.
const (
	RootEvent   = "event"
	RootTrigger = "trigger"
	RootFiring  = "firing"
)

// Roots are the roots a path may start from, in the order spec 022 lists
// them.
var Roots = []string{RootEvent, RootTrigger, RootFiring}

// CutMark follows a value cut to MaxValueBytes.
const CutMark = "[cut]"

// Template is a parsed template: literal text and placeholders, in order.
// A template has no logic and calls nothing; rendering it looks paths up
// in the values it is handed.
type Template struct {
	parts []part
}

// part is literal text, or a placeholder's path when path is set.
type part struct {
	text string
	path []string
}

// Error is a template that does not parse: Detail says what, at byte
// Offset of the template's text.
type Error struct {
	Offset int
	Detail string
}

func (e *Error) Error() string { return fmt.Sprintf("byte %d: %s", e.Offset, e.Detail) }

// Parse reads a template. A placeholder is {{, a path, and }}, with
// optional spaces inside the braces; \{{ is a literal {{. A path is a
// root of Roots and at least one dotted segment of letters, digits, _
// and -, and a segment of digits indexes a list. A {{ that opens no
// placeholder and a path of another root are refused.
func Parse(s string) (Template, error) {
	var t Template
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			t.parts = append(t.parts, part{text: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], `\{{`):
			lit.WriteString("{{")
			i += 3
		case strings.HasPrefix(s[i:], "{{"):
			path, n, err := placeholder(s[i:])
			if err != nil {
				err.Offset += i
				return Template{}, err
			}
			flush()
			t.parts = append(t.parts, part{path: path})
			i += n
		default:
			lit.WriteByte(s[i])
			i++
		}
	}
	flush()
	return t, nil
}

// placeholder reads the placeholder s starts with and returns its path
// and its length in bytes.
func placeholder(s string) ([]string, int, *Error) {
	end := strings.Index(s, "}}")
	if end < 0 {
		return nil, 0, &Error{Detail: `a {{ opens no placeholder: it has no }}; write \{{ for a literal {{`}
	}
	raw := strings.Trim(s[2:end], " ")
	path, err := ParsePath(raw)
	if err != nil {
		return nil, 0, err
	}
	return path, end + 2, nil
}

// ParsePath reads a path: a root of Roots and at least one dotted
// segment of letters, digits, _ and -.
func ParsePath(raw string) ([]string, *Error) {
	segs := strings.Split(raw, ".")
	for _, seg := range segs {
		if seg == "" || strings.ContainsFunc(seg, notPathChar) {
			return nil, &Error{Detail: fmt.Sprintf(`a {{ opens no placeholder: %q is not a path of dotted segments of letters, digits, _ and -; write \{{ for a literal {{`, raw)}
		}
	}
	switch {
	case !slices.Contains(Roots, segs[0]):
		return nil, &Error{Detail: fmt.Sprintf("the path %q starts from an unknown root %q, not one of %s", raw, segs[0], strings.Join(Roots, ", "))}
	case len(segs) < 2:
		return nil, &Error{Detail: fmt.Sprintf("the path %q names its root alone; name a field of it, such as %s.id", raw, segs[0])}
	}
	return segs, nil
}

func notPathChar(r rune) bool {
	return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
}

// Uses reports whether a placeholder of t starts from root.
func (t Template) Uses(root string) bool {
	return slices.ContainsFunc(t.parts, func(p part) bool { return len(p.path) > 0 && p.path[0] == root })
}

// Literal reports whether t holds no placeholder.
func (t Template) Literal() bool {
	return !slices.ContainsFunc(t.parts, func(p part) bool { return p.path != nil })
}

// Values are what a template's paths name: each root's object, as JSON
// decodes it with numbers kept as json.Number.
type Values map[string]any

// Render writes t with each placeholder replaced by the value its path
// names in v: a string as itself, a number or a boolean as its JSON, an
// object or a list as compact JSON, and a path that names nothing, or
// names null, as nothing. Each value is cut to MaxValueBytes at a
// character boundary, with CutMark after it.
func (t Template) Render(v Values) string {
	var b strings.Builder
	for _, p := range t.parts {
		if p.path == nil {
			b.WriteString(p.text)
			continue
		}
		s, _ := Lookup(v, p.path)
		b.WriteString(Cut(s))
	}
	return b.String()
}

// Lookup renders the value path names in v as Render renders it, and
// reports whether the path names a scalar: a string, a number or a
// boolean. A path that names nothing, null, an object or a list reports
// false.
func Lookup(v Values, path []string) (string, bool) {
	var cur any = map[string]any(v)
	for _, seg := range path {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return "", false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(node) || strconv.Itoa(i) != seg {
				return "", false
			}
			cur = node[i]
		default:
			return "", false
		}
	}
	switch x := cur.(type) {
	case nil:
		return "", false
	case string:
		return x, true
	case json.Number:
		return x.String(), true
	case bool:
		return strconv.FormatBool(x), true
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), true
	}
	return compact(cur), false
}

// compact is v as compact JSON, with HTML left unescaped, or nothing for
// a value JSON cannot encode.
func compact(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// Cut is s cut to MaxValueBytes at a character boundary with CutMark
// after it, or s as it is when it fits.
func Cut(s string) string {
	if len(s) <= MaxValueBytes {
		return s
	}
	n := MaxValueBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + CutMark
}
