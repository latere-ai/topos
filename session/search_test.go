// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"
)

func words(toks []token) []string {
	out := make([]string, len(toks))
	for i, t := range toks {
		out[i] = t.text
	}
	return out
}

func TestTokenizeReadsWordsAndSpacelessCharacters(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"Hello, World!", []string{"hello", "world"}},
		{"ÉCOLE straße v2.5", []string{"école", "straße", "v2", "5"}},
		{"snake_case and foo-bar", []string{"snake", "case", "and", "foo", "bar"}},
		{"我在北京工作", []string{"我", "在", "北", "京", "工", "作"}},
		{"API 定价abc", []string{"api", "定", "价", "abc"}},
		{"コーヒーを", []string{"コ", "ー", "ヒ", "ー", "を"}},
		{"서울 날씨", []string{"서울", "날씨"}},
		{"नमस्ते दुनिया", []string{"नमस्ते", "दुनिया"}},
		{"  ... 🙂 ", nil},
	} {
		got := words(tokenize([]rune(c.in)))
		if !slices.Equal(got, c.want) {
			t.Errorf("tokenize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTokenizeKeepsRuneOffsetsAndPostgresLimits(t *testing.T) {
	toks := tokenize([]rune("Ünïcode wörds"))
	if toks[1].start != 8 || toks[1].end != 13 || toks[0].end != 7 {
		t.Fatalf("offsets %+v", toks)
	}
	long := strings.Repeat("a", maxLexeme)
	if got := words(tokenize([]rune("one " + long + " two"))); !slices.Equal(got, []string{"one", "two"}) {
		t.Fatalf("a word Postgres does not index is kept: %d words", len(got))
	}
	many := strings.Repeat("w ", maxPositions+10)
	if n := len(tokenize([]rune(many))); n != maxPositions {
		t.Fatalf("%d words read, want the first %d", n, maxPositions)
	}
}

func TestPlainTextDropsMarkdownsMarks(t *testing.T) {
	raw := "# Pricing plans\n\n> **Note**: see [the table](https://example.test/t) and ![a chart](c.png).\n\n" +
		"- first `item`\n* second __item__\n1. third ~~item~~\n\n```go\nfmt.Println(\"code\")\n```\n---\n" +
		"| plan | price |\n|---|:---:|\n| basic | 5 |"
	want := `Pricing plans Note: see the table and a chart. first item second item third item fmt.Println("code") plan price basic 5`
	if got := plainText(raw); got != want {
		t.Fatalf("plainText:\n got %q\nwant %q", got, want)
	}
	long := strings.Repeat("é", MaxSearchText)
	got := plainText(long)
	if len(got) > MaxSearchText || !strings.HasPrefix(long, got) || len(got) < MaxSearchText-1 {
		t.Fatalf("cut to %d bytes, want at most %d on a rune's start", len(got), MaxSearchText)
	}
}

func TestParseQuery(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []Term
	}{
		{"Pricing  plans pricing", []Term{{Words: []string{"pricing"}}, {Words: []string{"plans"}}}},
		{"北京 上海", []Term{{Words: []string{"北", "京"}, Phrase: true}, {Words: []string{"上", "海"}, Phrase: true}}},
		{"北京，上海", []Term{{Words: []string{"北", "京"}, Phrase: true}, {Words: []string{"上", "海"}, Phrase: true}}},
		{"API定价", []Term{{Words: []string{"api"}}, {Words: []string{"定", "价"}, Phrase: true}}},
		{"京", []Term{{Words: []string{"京"}, Phrase: true}}},
	} {
		q, err := ParseQuery(c.in)
		if err != nil {
			t.Fatalf("ParseQuery(%q): %v", c.in, err)
		}
		if got := q.Terms(); !slices.EqualFunc(got, c.want, func(a, b Term) bool { return a.Phrase == b.Phrase && slices.Equal(a.Words, b.Words) }) {
			t.Errorf("ParseQuery(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
	for _, c := range []struct {
		in   string
		want error
	}{
		{"", ErrQueryEmpty},
		{"   ", ErrQueryEmpty},
		{"!?… 🙂", ErrQueryEmpty},
		{strings.Repeat("a", MaxSearchQuery+1), ErrQueryTooLong},
	} {
		if _, err := ParseQuery(c.in); !errors.Is(err, c.want) || !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseQuery(%q) = %v, want %v", c.in, err, c.want)
		}
	}
	if _, err := ParseQuery(strings.Repeat("a", MaxSearchQuery)); err != nil {
		t.Fatalf("a query of the longest length: %v", err)
	}
}

func query(t *testing.T, s string) Query {
	t.Helper()
	q, err := ParseQuery(s)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestQueryMatches(t *testing.T) {
	for _, c := range []struct {
		query, text string
		want        bool
	}{
		{"pric", "We compared the Pricing of three plans", true},
		{"icing", "We compared the pricing", false},
		{"pricing plans", "pricing of three plans", true},
		{"pricing tiers", "pricing of three plans", false},
		{"北京", "我在北京工作", true},
		{"北京", "北方的京城", false},
		{"京", "北方的京城", true},
		{"北京", "北，京", true},
		{"コーヒー", "朝はコーヒーを飲む", true},
		{"école", "Une ÉCOLE", true},
	} {
		if got := query(t, c.query).Matches(c.text); got != c.want {
			t.Errorf("%q in %q = %v, want %v", c.query, c.text, got, c.want)
		}
	}
	if (Query{}).Matches("anything") {
		t.Fatal("a query without terms matched")
	}
}

func TestAPhraseIsFoundAmongAWordsFirstOccurrences(t *testing.T) {
	text := strings.Repeat("北", maxLexemePositions) + "北京"
	if query(t, "北京").Matches(text) {
		t.Fatal("a phrase found past a word's kept positions, which the index does not find")
	}
	if !query(t, "北").Matches(text) || !query(t, "京").Matches(text) {
		t.Fatal("a single character is found by its presence")
	}
	if !query(t, "北京").Matches(strings.Repeat("北", maxLexemePositions-1) + "北京") {
		t.Fatal("a phrase within the kept positions is not found")
	}
}

func joined(fs []Fragment) (plain, marked string) {
	var p, m strings.Builder
	for _, f := range fs {
		p.WriteString(f.Text)
		if f.Match {
			m.WriteString("[" + f.Text + "]")
		} else {
			m.WriteString(f.Text)
		}
	}
	return p.String(), m.String()
}

func TestExcerptMarksEveryMatchInAShortText(t *testing.T) {
	_, marked := joined(Excerpt("We compared the pricing of three pricey plans", query(t, "pric plan")))
	if marked != "We compared the [pric]ing of three [pric]ey [plan]s" {
		t.Fatalf("excerpt %q", marked)
	}
	_, marked = joined(Excerpt("我在北京工作，北京很大", query(t, "北京")))
	if marked != "我在[北京]工作，[北京]很大" {
		t.Fatalf("excerpt %q", marked)
	}
	if got := Excerpt("", query(t, "x")); len(got) != 0 {
		t.Fatalf("an empty text's excerpt: %+v", got)
	}
}

func TestExcerptCutsAroundTheFirstMatchBetweenWords(t *testing.T) {
	filler := strings.Repeat("lorem ipsum dolor ", 30)
	text := filler + "the pricing of three plans " + filler
	fs := Excerpt(text, query(t, "pricing"))
	plain, marked := joined(fs)
	if !strings.HasPrefix(fs[0].Text, "…") || fs[0].Match || !strings.HasSuffix(fs[len(fs)-1].Text, "…") || fs[len(fs)-1].Match {
		t.Fatalf("a cut excerpt's ends: %+v", fs)
	}
	body := strings.TrimSuffix(strings.TrimPrefix(plain, "…"), "…")
	if n := len([]rune(body)); n > SearchExcerpt || n < SearchExcerpt-2*excerptSnap {
		t.Fatalf("excerpt of %d runes, want at most %d", n, SearchExcerpt)
	}
	if strings.HasPrefix(body, " ") || strings.HasSuffix(body, " ") || !strings.Contains(text, body) {
		t.Fatalf("excerpt does not fall between words: %q", body)
	}
	if !strings.Contains(marked, "[pricing]") {
		t.Fatalf("the match is not marked: %q", marked)
	}
	at := strings.Index(body, "pricing")
	if at < 20 || at > SearchExcerpt/2 {
		t.Fatalf("the match stands at %d of the excerpt, want about a third in", at)
	}
}

func TestExcerptEllipsisStandsApartFromAMatchAtACut(t *testing.T) {
	text := strings.Repeat("北", 200) + "京" + strings.Repeat("南", 400)
	fs := Excerpt(text, query(t, "南"))
	if fs[0].Text != "…" || fs[0].Match || !fs[1].Match {
		t.Fatalf("the start's ellipsis joins a match: %+v", fs[:2])
	}
	if last := fs[len(fs)-1]; last.Text != "…" || last.Match {
		t.Fatalf("the end's ellipsis joins a match: %+v", last)
	}
	plain, _ := joined(fs)
	if n := len([]rune(plain)) - 2; n != SearchExcerpt {
		t.Fatalf("a text without spaces is cut at %d runes, want %d", n, SearchExcerpt)
	}
}

func TestExcerptOfATextWithoutAMatchIsItsStart(t *testing.T) {
	text := strings.Repeat("plain words here ", 40)
	fs := Excerpt(text, query(t, "absent"))
	if len(fs) != 1 || fs[0].Match || !strings.HasPrefix(text, strings.TrimSuffix(fs[0].Text, "…")) || !strings.HasSuffix(fs[0].Text, "…") {
		t.Fatalf("excerpt %+v", fs)
	}
	text = strings.Repeat("x", 300) + " pricing"
	fs = Excerpt(text, query(t, "pricing"))
	if !fs[len(fs)-1].Match || fs[len(fs)-1].Text != "pricing" {
		t.Fatalf("a match at the text's end: %+v", fs)
	}
}

func event(t *testing.T, typ Type, payload any) Event {
	t.Helper()
	e, err := NewEvent(typ, payload, time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestSearchTextReadsWhatAPersonSaw(t *testing.T) {
	person := Sender{Subject: "usr_1", Kind: SenderPerson}
	msg := event(t, TypeUserMessage, UserMessage{Sender: person, Content: []lux.Block{{Type: ir.BlockText, Text: "Find **cheap** flights"}, {Type: ir.BlockText, Text: "to Lisbon"}}})
	answer := event(t, TypeAgentMessage, AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{
		{Type: ir.BlockThinking, Text: "secret reasoning"},
		{Type: ir.BlockText, Text: "Here are three"},
		{Type: ir.BlockToolUse, ToolUse: &lux.ToolUse{ID: "t1", Name: "search", Args: []byte(`{"q":"hidden"}`)}},
	}}})
	reply := event(t, TypeUserAnswer, UserAnswer{Sender: person, ToolUseID: "t1", Answers: []AnswerEntry{{Selected: []string{"Morning", "Evening"}}, {Text: "window seat"}}})
	for _, c := range []struct {
		e    Event
		want string
	}{
		{msg, "Find cheap flights to Lisbon"},
		{answer, "Here are three"},
		{reply, "Morning, Evening window seat"},
	} {
		got, ok := SearchText(c.e)
		if !ok || got != c.want {
			t.Errorf("SearchText(%s) = %q, %v; want %q", c.e.Type, got, ok, c.want)
		}
	}
	result := event(t, TypeToolResult, map[string]any{"tool_use_id": "t1", "content": "Lisbon flights"})
	threaded := msg
	threaded.Thread = "thr_1"
	redacted := msg
	redacted.Payload = tombstone
	for _, e := range []Event{result, threaded, redacted} {
		if _, ok := SearchText(e); ok {
			t.Errorf("an event a search does not read was read: %s thread %q", e.Type, e.Thread)
		}
	}
	broken := msg
	broken.Payload = []byte(`{"content": "not blocks"}`)
	if got, ok := SearchText(broken); !ok || got != "" {
		t.Fatalf("an undecodable message: %q, %v", got, ok)
	}
	doc, ok := SearchDocument(event(t, TypeUserMessage, UserMessage{Sender: person, Content: []lux.Block{{Type: ir.BlockText, Text: "Visit 北京, Today!"}}}))
	if !ok || doc != "visit 北 京 today" {
		t.Fatalf("SearchDocument = %q, %v", doc, ok)
	}
	if _, ok := SearchDocument(result); ok {
		t.Fatal("a tool result has a document")
	}
}

func TestMatchingIsNewestFirstAndBounded(t *testing.T) {
	person := Sender{Subject: "usr_1", Kind: SenderPerson}
	var evs []Event
	for i := range MaxSearchMatches + 2 {
		e := event(t, TypeUserMessage, UserMessage{Sender: person, Content: []lux.Block{{Type: ir.BlockText, Text: "lisbon again"}}})
		e.Seq = uint64(i + 1)
		evs = append(evs, e)
	}
	got := Matching(evs, query(t, "lisbon"))
	if len(got) != MaxSearchMatches || got[0].Seq != uint64(MaxSearchMatches+2) || got[1].Seq != uint64(MaxSearchMatches+1) {
		t.Fatalf("matching: %d events, first seq %d", len(got), got[0].Seq)
	}
	m := MatchOf(got[0], query(t, "lisbon"))
	if m.Seq != got[0].Seq || m.EventID != got[0].ID || m.Type != TypeUserMessage || !m.Time.Equal(got[0].Time) || len(m.Excerpt) != 2 || !m.Excerpt[0].Match {
		t.Fatalf("MatchOf = %+v", m)
	}
}

func TestSearchLimit(t *testing.T) {
	for in, want := range map[int]int{0: MaxSearchResults, -1: MaxSearchResults, 5: 5, MaxSearchResults: MaxSearchResults, MaxSearchResults + 1: MaxSearchResults} {
		if got := SearchLimit(in); got != want {
			t.Errorf("SearchLimit(%d) = %d, want %d", in, got, want)
		}
	}
}

// TestTheContractPageStatesTheBounds: docs/searching-sessions.md, the
// contract a client's author reads, states each bound with the value of
// its constant, so a bound that changes fails here until the page says
// it.
func TestTheContractPageStatesTheBounds(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "docs", "searching-sessions.md"))
	if err != nil {
		t.Fatal(err)
	}
	page := strings.Join(strings.Fields(string(raw)), " ")
	for _, want := range []string{
		fmt.Sprintf("required, at most %d characters", MaxSearchQuery),
		fmt.Sprintf("1 to %d; %d when absent", MaxSearchResults, MaxSearchResults),
		fmt.Sprintf("or is longer than %d characters is `invalid_request`, and so is a `limit` outside 1 to %d", MaxSearchQuery, MaxSearchResults),
		fmt.Sprintf("searched in its first %d KiB", MaxSearchText>>10),
		fmt.Sprintf("the session's %d newest matching events at most", MaxSearchMatches),
		fmt.Sprintf("at most %d characters of the message", SearchExcerpt),
	} {
		if !strings.Contains(page, want) {
			t.Errorf("docs/searching-sessions.md does not state %q", want)
		}
	}
}
