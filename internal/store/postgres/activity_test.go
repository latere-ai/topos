// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build postgres

package postgres

import (
	"strings"
	"testing"
	"time"
)

// TestActivityUsesTheTimeIndex: each count of a day's activity reads the
// day's rows through the index migration 0014 adds, so a day is counted
// without reading every event or every session (spec 085). The planner is
// told to avoid a sequential scan where it can, since an empty table makes
// any plan as cheap as another; what the test holds is that the queries'
// predicates let the indexes serve them.
func TestActivityUsesTheTimeIndex(t *testing.T) {
	st := fresh(t, Options{})
	ctx := t.Context()
	conn, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct{ name, query, index string }{
		{"messages", activityMessages, "events_activity"},
		{"sessions", activitySessions, "sessions_created"},
		{"answers", activityAnswers, "events_activity"},
	} {
		rows, err := conn.Query(ctx, "EXPLAIN "+c.query, from, from.Add(24*time.Hour), "", "")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan.String(), c.index) {
			t.Errorf("the %s count does not read %s:\n%s", c.name, c.index, plan.String())
		}
	}
}
