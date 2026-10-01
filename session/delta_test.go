// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// TestADeltaFrame: a delta of text is {thread, turn, step, block, kind,
// text}, with block 0 written, a reset is {thread, turn, step, reset}
// alone, the session's own thread is left out as on events, and both
// read back as they were written. Marshal keeps HTML as it is.
func TestADeltaFrame(t *testing.T) {
	for _, c := range []struct {
		d    Delta
		want string
	}{
		{Delta{Turn: 2, Step: 1, Block: 0, Kind: DeltaText, Text: "<b>Hi</b>"}, `{"turn":2,"step":1,"block":0,"kind":"text","text":"<b>Hi</b>"}`},
		{Delta{Thread: "evt_1", Turn: 2, Step: 3, Block: 1, Kind: DeltaToolInput, Text: `{"a":`}, `{"thread":"evt_1","turn":2,"step":3,"block":1,"kind":"tool_input","text":"{\"a\":"}`},
		{Delta{Turn: 2, Step: 1, Block: 4, Kind: DeltaText, Text: "dropped", Reset: true}, `{"turn":2,"step":1,"reset":true}`},
	} {
		b, err := Marshal(c.d)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Errorf("%+v is %s, want %s", c.d, b, c.want)
		}
		var back Delta
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		want := c.d
		if want.Reset {
			want = Delta{Thread: c.d.Thread, Turn: c.d.Turn, Step: c.d.Step, Reset: true}
		}
		if back != want {
			t.Errorf("%s reads back as %+v", b, back)
		}
	}
}

func TestDeltaValid(t *testing.T) {
	for _, c := range []struct {
		d    Delta
		want bool
	}{
		{Delta{Turn: 1, Step: 1, Kind: DeltaThinking, Text: "x"}, true},
		{Delta{Turn: 1, Step: 1, Reset: true}, true},
		{Delta{Turn: 1, Step: 1, Kind: DeltaText, Text: strings.Repeat("x", MaxDeltaText)}, true},
		{Delta{Turn: 1, Step: 1, Kind: DeltaText, Text: strings.Repeat("x", MaxDeltaText+1)}, false},
		{Delta{Turn: 1, Step: 1, Kind: DeltaText}, false},
		{Delta{Turn: 1, Step: 1, Kind: "signature", Text: "x"}, false},
		{Delta{Turn: 1, Step: 1, Block: -1, Kind: DeltaText, Text: "x"}, false},
		{Delta{Turn: 0, Step: 1, Kind: DeltaText, Text: "x"}, false},
		{Delta{Turn: 1, Step: 0, Reset: true}, false},
	} {
		if got := c.d.Valid(); got != c.want {
			t.Errorf("%+v valid %v, want %v", c.d, got, c.want)
		}
	}
}

// TestAFullSubscriberDropsAndCounts: a subscriber that does not read
// holds DeltaBuffer deltas, the publisher goes on without waiting, and
// each delta it could not take is counted and logged at debug level.
func TestAFullSubscriberDropsAndCounts(t *testing.T) {
	var logged bytes.Buffer
	h := &DeltaHub{Log: slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	if h.Subscribed() {
		t.Fatal("a new hub has subscribers")
	}
	ctx, cancel := context.WithCancel(t.Context())
	ch := h.SubscribeDeltas(ctx, "ses_a")
	if !h.Subscribed() {
		t.Fatal("the subscriber is not counted")
	}
	const extra = 10
	for range DeltaBuffer + extra {
		h.PublishDelta("ses_a", Delta{Turn: 1, Step: 1, Kind: DeltaText, Text: "x"})
	}
	h.PublishDelta("ses_b", Delta{Turn: 1, Step: 1, Kind: DeltaText, Text: "nobody follows this session"})
	if n := h.DroppedDeltas(); n != extra {
		t.Fatalf("%d dropped, want %d", n, extra)
	}
	if !strings.Contains(logged.String(), "dropped live deltas") || !strings.Contains(logged.String(), "session=ses_a") {
		t.Fatalf("the drop was not logged: %s", logged.String())
	}
	cancel()
	n := 0
	for range ch {
		n++
	}
	if n != DeltaBuffer || h.Subscribed() {
		t.Fatalf("%d held, want %d; subscribed after the end: %v", n, DeltaBuffer, h.Subscribed())
	}
}
