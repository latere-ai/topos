// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package parsing is the contract of the parsing service read sends a
// PDF to (spec 082): the request of one read, the document it answers,
// the bounds of a read, the page selection a call names, and the Reader a
// runner hands the tool. It dials nothing (spec 001): the HTTP client of
// the service is toposd's, and an embedder may hand the tool any Reader.
// The contract names no provider.
package parsing

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The bounds of spec 082. The tool's schema and its description are
// rendered from them or held to them by a test.
const (
	// DefaultPages is how many pages from the first a read returns when
	// its call names none.
	DefaultPages = 5
	// MaxPages is the most pages of one document the service is asked to
	// read: a longer document is read up to it.
	MaxPages = 500
	// Timeout bounds one read, from finding the file to its text.
	Timeout = 5 * time.Minute
	// MaxText is the most bytes of a document's text one read takes.
	MaxText = 8 << 20
	// MaxSelection is the most bytes of a call's page selection.
	MaxSelection = 200
	// MaxFileBytes is the most bytes of one file the service is sent: a
	// parsing service's per-file bound by default, Lectio's among them.
	MaxFileBytes = 256 << 20
)

// DefaultSelection is the selection of a call that names none: the first
// DefaultPages pages.
var DefaultSelection = fmt.Sprintf("1-%d", DefaultPages)

// Request is one read of a PDF the session's machine holds.
type Request struct {
	// Path is the file's path relative to the working directory, slash
	// separated: where the session's folder keeps it, if it does.
	Path string
	// SHA256 is the hex digest of the bytes the machine holds, which the
	// folder's copy must equal.
	SHA256 string
	// Pages is the selection to return, DefaultSelection when empty.
	Pages string
	// Known is the id of the parse a result of the thread recorded for
	// these bytes, "" for none: a later drive reads it again instead of
	// parsing the bytes again.
	Known string
}

// Document is one read's answer.
type Document struct {
	// ID is the parse's id.
	ID string
	// Pages is how many pages the parse read: the document's count, or
	// MaxPages when More.
	Pages int
	// More reports a document of MaxPages pages or more, of which the
	// first MaxPages were read.
	More bool
	// Failed is how many of the pages read could not be read; the text
	// leaves them out.
	Failed int
	// Shown are the pages the text holds, in order.
	Shown []int
	// Text is the pages' Markdown.
	Text string
	// CostUSDMicro is what the parse's model reads cost, set whenever the
	// read ran a parse, a failed one included, and nil for a read that
	// reused a kept parse or whose cost could not be priced; Unpriced
	// marks the second.
	CostUSDMicro *int64
	Unpriced     bool
}

// Reader reads a PDF of the session's folder as text. A read that fails
// answers an error and, beside it, the Document's cost when it ran a
// parse.
type Reader interface {
	Read(ctx context.Context, r Request) (Document, error)
}

// ErrNotKept is a file the session's folder does not hold with the bytes
// the machine holds at its path.
var ErrNotKept = errors.New("parsing: the session's folder holds no file with these bytes at this path")

// ErrTooMuchText is a selection whose text passes MaxText.
var ErrTooMuchText = fmt.Errorf("parsing: the pages hold more than %d bytes of text", MaxText)

// ErrUnavailable is a read the service did not answer as the contract
// says: a 5xx, a refusal without the envelope, an answer that does not
// decode, a transport error, a credential that could not be had. A
// timeout wraps context.DeadlineExceeded instead.
var ErrUnavailable = errors.New("parsing: the parsing service is unavailable")

// Refused is a call the service refused with the error envelope: its
// status, its code and its sentence for the person.
type Refused struct {
	Status  int
	Code    string
	Message string
}

func (r *Refused) Error() string {
	return fmt.Sprintf("parsing: the service refused with %d %s: %s", r.Status, r.Code, r.Message)
}

// Failed is a parse that ended without its pages: its state, failed or
// canceled, and its error's code and detail.
type Failed struct {
	State, Code, Detail string
}

func (f *Failed) Error() string {
	return fmt.Sprintf("parsing: the parse ended %s: %s: %s", f.State, f.Code, f.Detail)
}

// PastEnd is a selection that names none of the document's pages.
type PastEnd struct {
	Pages int
	More  bool
}

func (p *PastEnd) Error() string {
	return fmt.Sprintf("parsing: the selection names none of the document's %d pages", p.Pages)
}

// span is one part of a selection: from lo to hi, hi 0 for an open range.
type span struct{ lo, hi int }

// spans reads a selection: parts joined by commas, each a page n, a
// closed range a-b with b at least a, or an open range a-; pages count
// from 1, and space around a part or a dash is allowed.
func spans(expr string) ([]span, error) {
	bad := func(why string) ([]span, error) {
		return nil, fmt.Errorf("parsing: the pages %q are not a selection such as 1-5,7 or 9-: %s", expr, why)
	}
	if len(expr) > MaxSelection {
		return bad(fmt.Sprintf("it is longer than %d bytes", MaxSelection))
	}
	if strings.TrimSpace(expr) == "" {
		return bad("it is empty")
	}
	var out []span
	for part := range strings.SplitSeq(expr, ",") {
		lo, hi, ranged := strings.Cut(strings.TrimSpace(part), "-")
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil || a < 1 {
			return bad(fmt.Sprintf("%q does not start with a page from 1", strings.TrimSpace(part)))
		}
		s := span{a, a}
		if ranged {
			s.hi = 0
			if hi = strings.TrimSpace(hi); hi != "" {
				b, err := strconv.Atoi(hi)
				if err != nil || b < a {
					return bad(fmt.Sprintf("%q does not end with a page from %d", strings.TrimSpace(part), a))
				}
				s.hi = b
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// CheckPages reports why expr is not a selection.
func CheckPages(expr string) error {
	_, err := spans(expr)
	return err
}

// Select is the pages of a document of total pages that expr names, in
// order and each once; a page past total is left out, so a selection may
// name none.
func Select(expr string, total int) ([]int, error) {
	ss, err := spans(expr)
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	for _, s := range ss {
		hi := s.hi
		if hi == 0 || hi > total {
			hi = total
		}
		for p := s.lo; p <= hi; p++ {
			seen[p] = true
		}
	}
	out := make([]int, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	slices.Sort(out)
	return out, nil
}

// Format writes pages, sorted and each once, as a selection: each run of
// consecutive pages as a-b, a lone page as itself, joined by commas.
func Format(pages []int) string {
	var b strings.Builder
	for i := 0; i < len(pages); {
		j := i
		for j+1 < len(pages) && pages[j+1] == pages[j]+1 {
			j++
		}
		if b.Len() > 0 {
			b.WriteString(",")
		}
		if j == i {
			b.WriteString(strconv.Itoa(pages[i]))
		} else {
			fmt.Fprintf(&b, "%d-%d", pages[i], pages[j])
		}
		i = j + 1
	}
	return b.String()
}

// Next is the selection that reads on after the last of shown: the next
// DefaultPages pages up to total, "" when shown ends the document.
func Next(shown []int, total int) string {
	if len(shown) == 0 || shown[len(shown)-1] >= total {
		return ""
	}
	from := shown[len(shown)-1] + 1
	to := min(from+DefaultPages-1, total)
	if from == to {
		return strconv.Itoa(from)
	}
	return fmt.Sprintf("%d-%d", from, to)
}
