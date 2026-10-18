// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// The bounds of a query's answer from the history tool (spec 084).
const (
	// HistoryMatches is the most matching events one answer holds.
	HistoryMatches = 20
	// HistoryMatchChars is the most characters of an event's text an
	// answer shows around its first match.
	HistoryMatchChars = 400
)

// readsHistory reports whether the thread holds the history tool, whose
// texts its fold renders and whose compaction prompt it asks with. A
// client's tool of the name, which an agent stored before the name was
// reserved may hold, is that client's.
func (t *turn) readsHistory() bool {
	if t.reg == nil {
		return false
	}
	tool, ok := t.reg.Get(tools.NameHistory)
	return ok && !tool.Properties().Client
}

// history answers a read of the history tool from this thread's view of
// the log, through the event before the call's agent.tool_use, so the
// step's other calls and their results never change what it answers.
func (t *turn) history(_ context.Context, c tools.Call, r tools.HistoryRead) (string, error) {
	evs := t.events()
	before := uint64(1)
	if n := len(evs); n > 0 {
		before = evs[n-1].Seq + 1
	}
	for _, e := range evs {
		var p session.AgentToolUse
		if e.Type == session.TypeAgentToolUse && e.Thread == t.thread && !e.Redacted() && e.Decode(&p) == nil && p.ToolUseID == c.ID {
			before = e.Seq
			break
		}
	}
	entries, err := session.History(evs, t.thread, before)
	if err != nil {
		return "", err
	}
	return historyAnswer(entries, r, before-1, tools.DefaultOutputLimit)
}

// historyAnswer is the answer to r from entries, the thread's view
// through last: whole entries, at most limit bytes of them, closed with
// the input that reads on when the answer stops before its read ends.
func historyAnswer(entries []session.HistoryEntry, r tools.HistoryRead, last uint64, limit int) (string, error) {
	from, to := max(r.From, 1), last
	if r.To != 0 {
		to = min(r.To, last)
	}
	if r.ToolUseID != "" {
		var picked []session.HistoryEntry
		for _, e := range entries {
			if slices.Contains(e.Calls, r.ToolUseID) {
				picked = append(picked, e)
			}
		}
		if len(picked) == 0 {
			return prompts.Render(prompts.HistoryNoCall, prompts.Data{"ID": r.ToolUseID}), nil
		}
		end := picked[len(picked)-1].Seq
		more, err := readsOn(picked, func(e session.HistoryEntry) tools.HistoryRead { return tools.HistoryRead{From: e.Seq, To: end} })
		if err != nil {
			return "", err
		}
		return bounded(entryTexts(picked), more, limit), nil
	}
	var picked []session.HistoryEntry
	for _, e := range entries {
		if e.Seq >= from && e.Seq <= to {
			picked = append(picked, e)
		}
	}
	if r.Query != "" {
		return historyMatches(picked, r.Query, from, to, limit)
	}
	if len(picked) == 0 {
		return prompts.Render(prompts.HistoryNone, prompts.Data{"From": from, "To": r.To, "Last": last}), nil
	}
	more, err := readsOn(picked, func(e session.HistoryEntry) tools.HistoryRead { return tools.HistoryRead{From: e.Seq, To: to} })
	if err != nil {
		return "", err
	}
	return bounded(entryTexts(picked), more, limit), nil
}

// historyMatches answers a query over entries, the range from to to: the
// entries whose body holds the query, case folded, each shown with the
// text around its first match, at most HistoryMatches of them.
func historyMatches(entries []session.HistoryEntry, query string, from, to uint64, limit int) (string, error) {
	var texts []string
	var matched []session.HistoryEntry
	for _, e := range entries {
		excerpt, before, after, ok := around(e.Body, query, HistoryMatchChars)
		if !ok {
			continue
		}
		matched = append(matched, e)
		texts = append(texts, prompts.Render(prompts.HistoryMatch, prompts.Data{
			"Seq": e.Seq, "Time": stamp(e.Time), "Head": e.Head, "Excerpt": excerpt, "Before": before, "After": after,
		}))
	}
	if len(texts) == 0 {
		return prompts.Render(prompts.HistoryNoMatch, prompts.Data{"From": from, "To": to, "Query": query}), nil
	}
	shown := min(len(texts), HistoryMatches)
	more, err := readsOn(matched[:min(len(matched), shown+1)], func(e session.HistoryEntry) tools.HistoryRead {
		return tools.HistoryRead{Query: query, From: e.Seq, To: to}
	})
	if err != nil {
		return "", err
	}
	return bounded(texts[:shown], more, limit), nil
}

// readsOn are the lines that close an answer stopping before each of
// entries but the first, with the input that reads on from it: line i
// for a stop before entry i, and none for an answer that ends after the
// last.
func readsOn(entries []session.HistoryEntry, from func(session.HistoryEntry) tools.HistoryRead) ([]string, error) {
	out := make([]string, len(entries)+1)
	for i := 1; i < len(entries); i++ {
		b, err := json.Marshal(from(entries[i]))
		if err != nil {
			return nil, fmt.Errorf("harness: the input that reads on: %w", err)
		}
		out[i] = prompts.Render(prompts.HistoryMore, prompts.Data{"Input": string(b)})
	}
	return out, nil
}

// entryTexts are the entries as the model reads them.
func entryTexts(entries []session.HistoryEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = prompts.Render(prompts.HistoryEntry, prompts.Data{"Seq": e.Seq, "Time": stamp(e.Time), "Head": e.Head, "Body": e.Body})
	}
	return out
}

// stamp is an event's time as an entry's head shows it.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// bounded joins texts, each whole, while they fit in limit bytes with
// more[i], the line that closes an answer stopping before text i, empty
// when nothing follows; more holds one line past the texts, for a read
// that goes on beyond them. A first text longer than that alone is cut,
// with a line that says where. The answer never passes limit, since a
// longer one would be spilled to a file on the machine.
func bounded(texts, more []string, limit int) string {
	closing := func(i int) string {
		if i >= len(more) || more[i] == "" {
			return ""
		}
		return "\n\n" + more[i]
	}
	var b strings.Builder
	for i, text := range texts {
		sep := ""
		if i > 0 {
			sep = "\n\n"
		}
		if b.Len()+len(sep)+len(text)+len(closing(i+1)) <= limit {
			b.WriteString(sep + text)
			continue
		}
		if i == 0 {
			return cut(text, limit-len(closing(1))) + closing(1)
		}
		return b.String() + closing(i)
	}
	return b.String() + closing(len(texts))
}

// cut is the start of text that fits in room bytes with the line that
// says where it was cut, which names text's bytes and the bytes kept.
func cut(text string, room int) string {
	line := func(kept int) string {
		return prompts.Render(prompts.HistoryCut, prompts.Data{"Total": len(text), "Kept": kept})
	}
	// The line for room is at least as long as the line for any fewer
	// bytes kept, so what it leaves fits.
	kept := max(room-len("\n")-len(line(room)), 0)
	for kept > 0 && !utf8.RuneStart(text[kept]) {
		kept--
	}
	return text[:kept] + "\n" + line(kept)
}

// around finds query in text, both case folded, and returns at most
// width characters of text around its first match, centered on it, and
// whether text goes on before and after them.
func around(text, query string, width int) (excerpt string, before, after, ok bool) {
	runes := []rune(text)
	folded, q := foldRunes(runes), foldRunes([]rune(query))
	if len(q) == 0 {
		return "", false, false, false
	}
	at := -1
	for i := 0; i+len(q) <= len(folded); i++ {
		if slices.Equal(folded[i:i+len(q)], q) {
			at = i
			break
		}
	}
	if at < 0 {
		return "", false, false, false
	}
	start := at
	if len(q) < width {
		start = max(at-(width-len(q))/2, 0)
	}
	end := min(start+width, len(runes))
	if len(q) < width {
		start = max(end-width, 0)
	}
	return string(runes[start:end]), start > 0, end < len(runes), true
}

// foldRunes maps each rune to the smallest rune of its simple case
// folding orbit, so two texts that differ in case alone map to the same
// runes, one for one, and a match's place in the folded text is its
// place in the text.
func foldRunes(rs []rune) []rune {
	out := make([]rune, len(rs))
	for i, r := range rs {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		out[i] = least
	}
	return out
}
