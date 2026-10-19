// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/session"
)

// TestTheReadbackLogHidesWhatHistoryFinds (spec 084): the conversation
// the context/readback task starts from folds, for a thread that holds
// history, to a transcript that names the summarized range and holds no
// build number, and history's read of it finds the number the person
// gave. The task's premise holds whatever model runs it.
func TestTheReadbackLogHidesWhatHistoryFinds(t *testing.T) {
	const value = "7Q4-88213-K"
	// The log is read and stamped as a run writes it: its events are the
	// session's first, numbered from 1.
	evs, err := readLog(filepath.Join("context", "readback", "log.jsonl"), "", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	session.Stamp("ses_readback", 0, evs)
	tr, err := session.FoldWith(evs, "", session.FoldOptions{History: true})
	if err != nil {
		t.Fatal(err)
	}
	folded, err := json.Marshal(tr.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(folded), value) || !strings.Contains(string(folded), "events #1 to #4") || len(tr.Unknown) > 0 {
		t.Fatalf("the transcript the agent starts from: %s", folded)
	}
	entries, err := session.History(evs, "", uint64(len(evs)+1))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 || !strings.Contains(entries[0].Body, value) || !strings.Contains(entries[0].Body, "build number") {
		t.Fatalf("history reads back %+v", entries)
	}
}
