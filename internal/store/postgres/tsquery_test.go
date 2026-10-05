// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"fmt"
	"strings"
	"testing"

	"latere.ai/x/topos/session"
)

func TestTSQueryNamesEveryTerm(t *testing.T) {
	for in, want := range map[string]string{
		"Pricing plans": `'pricing':* & 'plans':*`,
		"北京 api":        `('北' <-> '京') & 'api':*`,
		"京":             `'京'`,
		"コーヒー":          `('コ' <-> 'ー' <-> 'ヒ' <-> 'ー')`,
	} {
		q, err := session.ParseQuery(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := tsquery(q); got != want {
			t.Errorf("tsquery(%q) = %s, want %s", in, got, want)
		}
	}
	if got := tsquery(session.Query{}); got != "" {
		t.Fatalf("a query without terms: %q", got)
	}
	if got := lexemes.Replace(`it's a \ mark`); got != `it''s a \\ mark` {
		t.Fatalf("escaped %q", got)
	}
}

// The backfill reads the events the events_unsearched index holds, whose
// condition names session.SearchedTypes as literals.
func TestTheBackfillReadsTheSearchedTypes(t *testing.T) {
	quoted := make([]string, len(session.SearchedTypes))
	for i, typ := range session.SearchedTypes {
		quoted[i] = fmt.Sprintf("'%s'", typ)
	}
	list := "type IN (" + strings.Join(quoted, ", ") + ")"
	b, err := migrations.ReadFile("migrations/0007_session_search.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unsearched, list) || !strings.Contains(string(b), list) {
		t.Fatalf("the backfill or its index does not name %s", list)
	}
}
