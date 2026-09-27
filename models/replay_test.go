// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/session"
)

// fixedEncoder encodes every request as its model name.
type fixedEncoder struct{ err error }

func (f fixedEncoder) Encode(r Request) ([]byte, error) { return []byte(r.Connection.Model), f.err }
func (fixedEncoder) Codec(d ir.Dialect) string          { return string(d) + "@v1" }

func TestReplayComparesEachRecordedRequest(t *testing.T) {
	sum := sha256.Sum256([]byte("m"))
	hash := hex.EncodeToString(sum[:])
	var log []session.Event
	for i, mr := range []session.ModelRequest{
		{Model: "m", Dialect: "openai-chat", Codec: "openai-chat@v1", RequestSHA256: hash, FoldSeq: 1, Outcome: "ok"},
		{Model: "m", Dialect: "openai-chat", Codec: "openai-chat@v0", RequestSHA256: hash, Outcome: "ok"},
		{Model: "m", Dialect: "openai-chat", Outcome: "error"},
	} {
		e, err := session.NewEvent(session.TypeModelRequest, mr, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		e.Seq = uint64(i + 2)
		log = append(log, e)
	}
	tomb := log[0]
	tomb.Payload, tomb.Seq = json.RawMessage(`{"tombstone":true}`), 9
	log = append(log, tomb)
	rebuild := func(_ context.Context, _ []session.Event, _ session.Event, mr session.ModelRequest) (Request, error) {
		return Request{Connection: Connection{Model: mr.Model}}, nil
	}
	steps, err := Replay(t.Context(), log, rebuild, fixedEncoder{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range steps {
		got = append(got, s.Outcome)
	}
	if fmt.Sprint(got) != fmt.Sprint([]string{ReplayMatch, CodeMismatch, ReplaySkipped, ReplaySkipped}) || steps[0].Replayed != hash {
		t.Fatalf("steps %+v", steps)
	}
	skip := func(context.Context, []session.Event, session.Event, session.ModelRequest) (Request, error) {
		return Request{}, fmt.Errorf("%w: gone", ErrNotRebuilt)
	}
	if steps, err := Replay(t.Context(), log[:1], skip, fixedEncoder{}); err != nil || steps[0].Outcome != ReplaySkipped || steps[0].Detail == "" {
		t.Fatalf("a step that cannot be built: %+v, %v", steps, err)
	}
	broken := errors.New("broken")
	fail := func(context.Context, []session.Event, session.Event, session.ModelRequest) (Request, error) {
		return Request{}, broken
	}
	if _, err := Replay(t.Context(), log[:1], fail, fixedEncoder{}); !errors.Is(err, broken) {
		t.Fatalf("a rebuild that failed: %v", err)
	}
	if _, err := Replay(t.Context(), log[:1], rebuild, fixedEncoder{err: broken}); !errors.Is(err, broken) {
		t.Fatalf("an encoding that failed: %v", err)
	}
	bad := log[0]
	bad.Payload = json.RawMessage(`{"model":`)
	if _, err := Replay(t.Context(), []session.Event{bad}, rebuild, fixedEncoder{}); err == nil {
		t.Fatal("an unreadable model.request replayed")
	}
}
