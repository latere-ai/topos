// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"
)

// The bounds of a search over sessions by what was said in them (spec
// 050). Each is named once, and the server and every store read it here.
const (
	// MaxSearchQuery is the longest query, in runes.
	MaxSearchQuery = 200
	// MaxSearchResults is how many sessions one page of a search holds
	// at most, and how many it holds when the caller names no limit.
	MaxSearchResults = 20
	// SearchExcerpt is the longest excerpt of a match, in runes, its
	// ellipses left out.
	SearchExcerpt = 160
	// MaxSearchMatches is how many of a session's matching messages a
	// result carries, newest first.
	MaxSearchMatches = 3
	// MaxSearchText is how much of a message's plain text is searched, in
	// bytes from its start. It keeps a message's index entry well inside
	// the 1 MB a Postgres tsvector holds, so no append fails on a long
	// message.
	MaxSearchText = 64 << 10
)

// Postgres's own limits on what a tsvector keeps, mirrored here so a
// store that scans finds what the Postgres store's index finds.
const (
	// maxLexeme: a word of 2047 bytes or more is not indexed, and takes
	// no position.
	maxLexeme = 2047
	// maxPositions: a word's position runs from 1 to 16383, so a message
	// is searched in its first 16383 words.
	maxPositions = 16383
	// maxLexemePositions: a word keeps its first 256 positions, so a
	// phrase is found among a word's first 256 occurrences.
	maxLexemePositions = 256
)

// excerptSnap is how far an excerpt's edge moves to fall between two
// words rather than inside one, in runes.
const excerptSnap = 16

// SearchedTypes are the events a search reads: what a person wrote, what
// the agent answered, and a person's answer to the agent's question.
var SearchedTypes = []Type{TypeUserMessage, TypeAgentMessage, TypeUserAnswer}

// Searched reports whether a search reads e: an event of SearchedTypes
// on the session's own thread that is not redacted. A subagent's thread,
// a tool's output, a file and a redacted event are never searched.
func Searched(e Event) bool {
	return slices.Contains(SearchedTypes, e.Type) && e.Thread == "" && !e.Redacted()
}

// SearchText is the text a search reads of e: the text blocks of a
// message, or the choices and the words of an answer, as plain text
// without markdown's marks and cut to MaxSearchText. ok is false for an
// event Searched does not read. A payload that does not decode holds no
// text a person saw, and is searched as empty.
func SearchText(e Event) (text string, ok bool) {
	if !Searched(e) {
		return "", false
	}
	var parts []string
	switch e.Type {
	case TypeUserMessage:
		var p UserMessage
		if e.Decode(&p) != nil {
			return "", true
		}
		parts = textBlocks(p.Content)
	case TypeAgentMessage:
		var p AgentMessage
		if e.Decode(&p) != nil {
			return "", true
		}
		parts = textBlocks(p.Message.Blocks)
	case TypeUserAnswer:
		var p UserAnswer
		if e.Decode(&p) != nil {
			return "", true
		}
		for _, a := range p.Answers {
			if len(a.Selected) > 0 {
				parts = append(parts, strings.Join(a.Selected, ", "))
			}
			if a.Text != "" {
				parts = append(parts, a.Text)
			}
		}
	}
	return plainText(strings.Join(parts, "\n\n")), true
}

// textBlocks is the text of a message's text blocks: no thinking, no
// tool call, no image.
func textBlocks(blocks []lux.Block) []string {
	var out []string
	for _, b := range blocks {
		if b.Type == ir.BlockText && b.Text != "" {
			out = append(out, b.Text)
		}
	}
	return out
}

// The marks of markdown that a plain-text reading drops: a code fence's
// line, a rule, a table's rule, a heading's, a quote's and a list item's
// marks at a line's start, a link's or an image's address, and the
// inline marks of emphasis and code.
var (
	fenceLine  = regexp.MustCompile("^\\s*(```|~~~)")
	ruleLine   = regexp.MustCompile(`^\s*(?:-{3,}|\*{3,}|_{3,}|={3,})\s*$`)
	tableRule  = regexp.MustCompile(`^[\s|:-]*-{3,}[\s|:-]*$`)
	lineMarker = regexp.MustCompile(`^\s*(?:(?:#{1,6}|>+|[-*+]|\d{1,9}[.)])\s+)+`)
	linked     = regexp.MustCompile(`!?\[([^\]\n]*)\]\([^)\n]*\)`)
	inlineMark = strings.NewReplacer("**", "", "__", "", "~~", "", "`", "")
)

// plainText is raw as a person reads it rendered, near enough for a
// search and an excerpt: markdown's marks dropped, a table's cells apart,
// every run of whitespace one space, and cut to MaxSearchText bytes on a
// rune's start.
func plainText(raw string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(raw, "\n") {
		if fenceLine.MatchString(line) || ruleLine.MatchString(line) || tableRule.MatchString(line) {
			continue
		}
		line = lineMarker.ReplaceAllString(line, "")
		if strings.HasPrefix(strings.TrimSpace(line), "|") {
			line = strings.ReplaceAll(line, "|", " ")
		}
		line = linked.ReplaceAllString(line, "$1")
		b.WriteString(inlineMark.Replace(line))
		b.WriteByte(' ')
	}
	return cut(strings.Join(strings.Fields(b.String()), " "), MaxSearchText)
}

// cut is s in at most n bytes, ending on a rune's start.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// token is a word of a text: lowercased, with the runes it spans in the
// text, and whether it is a character of a script written without
// spaces between words.
type token struct {
	text       string
	start, end int
	spaceless  bool
}

// spaceless reports a character of a script written without spaces
// between words, Han, Hiragana and Katakana with its prolonged sound
// mark, which a search reads one character at a time.
func spaceless(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) ||
		r == 'ー' || r == 'ｰ'
}

// wordRune reports a letter, a digit or a mark, what a word is made of.
func wordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
}

// tokenize reads text's words: each run of letters, digits and marks is
// one, every character of a spaceless script is one of its own, and
// everything else is between words. Each is lowercased rune by rune, so
// its runes map onto the text's. A word Postgres would not index is left
// out, and the words stop at the last position Postgres keeps.
func tokenize(text []rune) []token {
	var out []token
	add := func(start, end int, alone bool) {
		lower := make([]rune, end-start)
		for i, r := range text[start:end] {
			lower[i] = unicode.ToLower(r)
		}
		if s := string(lower); len(s) < maxLexeme && len(out) < maxPositions {
			out = append(out, token{text: s, start: start, end: end, spaceless: alone})
		}
	}
	start := -1
	for i, r := range text {
		if len(out) == maxPositions {
			return out
		}
		switch {
		case spaceless(r):
			if start >= 0 {
				add(start, i, false)
				start = -1
			}
			add(i, i+1, true)
		case wordRune(r):
			if start < 0 {
				start = i
			}
		case start >= 0:
			add(start, i, false)
			start = -1
		}
	}
	if start >= 0 {
		add(start, len(text), false)
	}
	return out
}

// SearchDocument is what the Postgres store indexes of e: the words of
// its SearchText as a search reads them, lowercased and one space apart,
// so each character of a spaceless script is a word of its own and two
// words in a row stand at two positions in a row. ok is false where
// SearchText's is.
func SearchDocument(e Event) (doc string, ok bool) {
	text, ok := SearchText(e)
	if !ok {
		return "", false
	}
	toks := tokenize([]rune(text))
	words := make([]string, len(toks))
	for i, t := range toks {
		words[i] = t.text
	}
	return strings.Join(words, " "), true
}

// Term is one part of a query, which a matching message holds. A term
// that is not a phrase is one word, matched as the start of a word; a
// phrase is characters of a spaceless script typed in a row, matched as
// those characters in a row.
type Term struct {
	Words  []string
	Phrase bool
}

// Query is what a search looks for: every one of its terms.
type Query struct {
	terms []Term
}

// Terms are the query's terms, each once.
func (q Query) Terms() []Term { return slices.Clone(q.terms) }

// The refusals of ParseQuery.
var (
	ErrQueryTooLong = fmt.Errorf("%w: the query is longer than %d characters", ErrInvalid, MaxSearchQuery)
	ErrQueryEmpty   = fmt.Errorf("%w: the query holds no word to search for", ErrInvalid)
)

// ParseQuery reads a query (spec 050): its words, each a term, and each
// run of spaceless characters typed without a break between them one
// phrase. It refuses a query longer than MaxSearchQuery runes and one
// that holds no word.
func ParseQuery(s string) (Query, error) {
	if utf8.RuneCountInString(s) > MaxSearchQuery {
		return Query{}, ErrQueryTooLong
	}
	toks := tokenize([]rune(s))
	var q Query
	seen := map[string]bool{}
	add := func(t Term) {
		key := fmt.Sprintf("%t\x00%s", t.Phrase, strings.Join(t.Words, "\x00"))
		if !seen[key] {
			seen[key] = true
			q.terms = append(q.terms, t)
		}
	}
	for i := 0; i < len(toks); {
		if !toks[i].spaceless {
			add(Term{Words: []string{toks[i].text}})
			i++
			continue
		}
		j := i + 1
		for j < len(toks) && toks[j].spaceless && toks[j].start == toks[j-1].end {
			j++
		}
		words := make([]string, 0, j-i)
		for _, t := range toks[i:j] {
			words = append(words, t.text)
		}
		add(Term{Words: words, Phrase: true})
		i = j
	}
	if len(q.terms) == 0 {
		return Query{}, ErrQueryEmpty
	}
	return q, nil
}

// span is the runes of a text a term matched.
type span struct{ start, end int }

// find answers the spans of every occurrence of each of q's terms among
// toks, and whether every term occurs. A phrase of two or more is found
// among each word's first maxLexemePositions occurrences, as the
// Postgres index finds it.
func (q Query) find(toks []token) ([]span, bool) {
	nth := make([]int, len(toks))
	seen := map[string]int{}
	for i, t := range toks {
		nth[i] = seen[t.text]
		seen[t.text]++
	}
	var spans []span
	all := true
	for _, term := range q.terms {
		found := false
		if !term.Phrase {
			w := term.Words[0]
			n := utf8.RuneCountInString(w)
			for _, t := range toks {
				if strings.HasPrefix(t.text, w) {
					found = true
					spans = append(spans, span{t.start, t.start + n})
				}
			}
		} else {
			k := len(term.Words)
			for i := 0; i+k <= len(toks); i++ {
				in := true
				for j, w := range term.Words {
					if toks[i+j].text != w || (k > 1 && nth[i+j] >= maxLexemePositions) {
						in = false
						break
					}
				}
				if in {
					found = true
					spans = append(spans, span{toks[i].start, toks[i+k-1].end})
				}
			}
		}
		all = all && found
	}
	return spans, all
}

// Matches reports whether text, a SearchText, holds every term of q.
func (q Query) Matches(text string) bool {
	if len(q.terms) == 0 {
		return false
	}
	_, all := q.find(tokenize([]rune(text)))
	return all
}

// Fragment is a piece of an excerpt, Match on a piece the query matched.
type Fragment struct {
	Text  string `json:"text"`
	Match bool   `json:"match,omitempty"`
}

// Excerpt is the part of text around its first match of q, at most
// SearchExcerpt runes, in fragments with every match in it marked. A cut
// start begins the first fragment with "…" and a cut end ends the last
// with it, each in a fragment that is no match. A text in which nothing
// matches is excerpted from its start.
func Excerpt(text string, q Query) []Fragment {
	runes := []rune(text)
	if len(runes) == 0 {
		return []Fragment{}
	}
	spans, _ := q.find(tokenize(runes))
	spans = merge(spans)
	from, to := excerptWindow(runes, spans)
	var out []Fragment
	at := from
	for _, s := range spans {
		if s.end <= from || s.start >= to {
			continue
		}
		a, b := max(s.start, from), min(s.end, to)
		if a > at {
			out = append(out, Fragment{Text: string(runes[at:a])})
		}
		out = append(out, Fragment{Text: string(runes[a:b]), Match: true})
		at = b
	}
	if at < to {
		out = append(out, Fragment{Text: string(runes[at:to])})
	}
	const ellipsis = "…"
	if from > 0 {
		if !out[0].Match {
			out[0].Text = ellipsis + out[0].Text
		} else {
			out = slices.Insert(out, 0, Fragment{Text: ellipsis})
		}
	}
	if to < len(runes) {
		if last := len(out) - 1; !out[last].Match {
			out[last].Text += ellipsis
		} else {
			out = append(out, Fragment{Text: ellipsis})
		}
	}
	return out
}

// merge sorts spans by their start and joins those that overlap or
// touch.
func merge(spans []span) []span {
	slices.SortFunc(spans, func(a, b span) int { return a.start - b.start })
	var out []span
	for _, s := range spans {
		if n := len(out); n > 0 && s.start <= out[n-1].end {
			out[n-1].end = max(out[n-1].end, s.end)
			continue
		}
		out = append(out, s)
	}
	return out
}

// excerptWindow is the runes an excerpt shows: the whole text when it fits,
// and otherwise SearchExcerpt runes that start a third of the room
// before the first match, moved to fall between words where one is
// near, never past the match.
func excerptWindow(runes []rune, spans []span) (from, to int) {
	n := len(runes)
	if n <= SearchExcerpt {
		return 0, n
	}
	if len(spans) == 0 {
		return 0, snapEnd(runes, SearchExcerpt, 0)
	}
	first := spans[0]
	lead := (SearchExcerpt - min(first.end-first.start, SearchExcerpt)) / 3
	from = max(first.start-lead, 0)
	to = from + SearchExcerpt
	if to > n {
		to, from = n, n-SearchExcerpt
	}
	if from > 0 {
		from = snapStart(runes, from, first.start)
	}
	if to < n {
		to = snapEnd(runes, to, min(first.end, to))
	}
	return from, to
}

// snapStart moves an excerpt's start to just after a space within
// excerptSnap runes, before limit.
func snapStart(runes []rune, from, limit int) int {
	for i := from; i < min(from+excerptSnap, limit); i++ {
		if runes[i] == ' ' {
			return i + 1
		}
	}
	return from
}

// snapEnd moves an excerpt's end back to a space within excerptSnap
// runes, not before limit.
func snapEnd(runes []rune, to, limit int) int {
	for i := to; i > max(to-excerptSnap, limit); i-- {
		if runes[i-1] == ' ' {
			return i - 1
		}
	}
	return to
}

// Match is one message a search found in a session: its place in the
// log, so a client can show it in the conversation, and the excerpt
// around what matched.
type Match struct {
	Seq     uint64     `json:"seq"`
	EventID string     `json:"event_id"`
	Type    Type       `json:"type"`
	Time    time.Time  `json:"time"`
	Excerpt []Fragment `json:"excerpt"`
}

// MatchOf is e as a search answers it for q.
func MatchOf(e Event, q Query) Match {
	text, _ := SearchText(e)
	return Match{Seq: e.Seq, EventID: e.ID, Type: e.Type, Time: e.Time.UTC(), Excerpt: Excerpt(text, q)}
}

// SearchResult is one session a search found, with its matches newest
// first.
type SearchResult struct {
	Session Session `json:"session"`
	Matches []Match `json:"matches"`
}

// SearchOptions are a search: the sessions it looks in, filtered as
// List filters them, paged by List's Limit and Cursor, and the query.
type SearchOptions struct {
	ListOptions
	Query Query
}

// SearchHit is a session that holds a match, with its matching events,
// newest first and at most MaxSearchMatches.
type SearchHit struct {
	Session Session
	Events  []Event
}

// Searcher is the optional interface of a store that searches its own
// index (spec 050). It answers the sessions of o's scope that hold an
// event matching o's query, newest first by id as List orders them, and
// the cursor of the next page, as ScanSearch does.
type Searcher interface {
	Search(ctx context.Context, o SearchOptions) ([]SearchHit, string, error)
}

// Search searches st: by its own index where it has one, and otherwise
// by reading each session's log.
func Search(ctx context.Context, st Store, o SearchOptions) ([]SearchHit, string, error) {
	if s, ok := st.(Searcher); ok {
		return s.Search(ctx, o)
	}
	return ScanSearch(ctx, st, o)
}

// SearchLimit is a search's page size: limit, or MaxSearchResults when
// limit is unset or larger.
func SearchLimit(limit int) int {
	if limit <= 0 || limit > MaxSearchResults {
		return MaxSearchResults
	}
	return limit
}

// searchScanPage is how many sessions ScanSearch lists at a time.
const searchScanPage = 100

// ScanSearch searches a store without an index of its own: it lists the
// sessions of o's scope newest first and reads each one's log. A session
// deleted between its list and its log is passed over.
func ScanSearch(ctx context.Context, st Store, o SearchOptions) ([]SearchHit, string, error) {
	limit := SearchLimit(o.Limit)
	lo := o.ListOptions
	lo.Limit = searchScanPage
	var hits []SearchHit
	for {
		page, next, err := st.List(ctx, lo)
		if err != nil {
			return nil, "", err
		}
		for _, s := range page {
			evs, err := st.Events(ctx, s.ID, 1, 0)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, "", err
			}
			found := Matching(evs, o.Query)
			if len(found) == 0 {
				continue
			}
			if len(hits) == limit {
				return hits, hits[limit-1].Session.ID, nil
			}
			hits = append(hits, SearchHit{Session: s, Events: found})
		}
		if next == "" {
			return hits, "", nil
		}
		lo.Cursor = next
	}
}

// Matching is the events of a log that hold q, newest first and at most
// MaxSearchMatches.
func Matching(evs []Event, q Query) []Event {
	var out []Event
	for _, e := range slices.Backward(evs) {
		if text, ok := SearchText(e); ok && q.Matches(text) {
			out = append(out, e)
			if len(out) == MaxSearchMatches {
				break
			}
		}
	}
	return out
}
