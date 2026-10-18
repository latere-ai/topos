// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"latere.ai/x/topos/session"
)

// logLine is one line of a task's log: an event as the log keeps it,
// without what the session gives it when it is written, its id, its
// session and its sequence.
type logLine struct {
	Type    session.Type    `json:"type"`
	Time    *time.Time      `json:"time"`
	Turn    int             `json:"turn"`
	Step    int             `json:"step"`
	Thread  string          `json:"thread"`
	Payload json.RawMessage `json:"payload"`
}

// readLog reads a task's log, with its placeholders replaced: one event
// per line, blank lines aside, each of a type schema v1 defines and with
// a payload that is a JSON object. Each event gets a fresh id, and at the
// time of now unless its line names one; the session and the sequence
// are the append's to set.
func readLog(path, workdir, serveURL string, now time.Time) ([]session.Event, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []session.Event
	sc := bufio.NewScanner(bytes.NewReader(expand(b, workdir, serveURL)))
	sc.Buffer(nil, 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		var l logLine
		if err := dec.Decode(&l); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		switch {
		case !session.Known[l.Type]:
			return nil, fmt.Errorf("line %d: %q is not an event type of schema v1", n, l.Type)
		case !json.Valid(l.Payload) || !bytes.HasPrefix(bytes.TrimSpace(l.Payload), []byte("{")):
			return nil, fmt.Errorf("line %d: the payload is not a JSON object", n)
		}
		at := now
		if l.Time != nil {
			at = *l.Time
		}
		out = append(out, session.Event{
			ID: session.NewID(session.PrefixEvent), Type: l.Type, Time: at.UTC(), Turn: l.Turn, Step: l.Step, Thread: l.Thread, Payload: l.Payload,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s holds no event", path)
	}
	return out, nil
}
