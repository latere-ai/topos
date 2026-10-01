// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runnerrole

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/internal/runnerapi"
	"latere.ai/x/topos/machine/host"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/dialect"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// waitDelta reads the next delta of ch, failing after ten seconds.
func waitDelta(t *testing.T, ch <-chan session.Delta) session.Delta {
	t.Helper()
	select {
	case d := <-ch:
		return d
	case <-time.After(10 * time.Second):
		t.Fatal("no delta")
	}
	return session.Delta{}
}

// TestARemoteRunnersDeltasReachTheServersStreams: a runner in the runner
// role sends the deltas of the session it drives over the internal
// routes, and the server publishes them to the session's subscribers.
func TestARemoteRunnersDeltasReachTheServersStreams(t *testing.T) {
	f := setup(t, 0)
	s := hosted(t, f.st, "Go.")
	sub := f.st.(session.DeltaSubscriber).SubscribeDeltas(t.Context(), s.ID)
	stub := luxstub.New(t)
	stub.Script("builder-model", luxstub.Reply{Response: ir.Response{Model: "builder-model", Blocks: []ir.Block{{Type: ir.BlockThinking, Text: "Remote.", Signature: "sig"}, {Type: ir.BlockText, Text: "done remotely"}}, StopReason: ir.StopEndTurn}})
	base := t.TempDir()
	r, err := runner.New(runner.Options{Store: f.client, ID: "run_remote", Kind: runner.KindRunner,
		Harness: func(ctx context.Context, s session.Session) (harness.Config, error) {
			m, err := host.Open(host.Options{Workdir: base, SpillDir: filepath.Join(base, ".spill"), Environ: []string{"PATH=" + os.Getenv("PATH")}})
			if err != nil {
				return harness.Config{}, err
			}
			return harness.Config{
				Model: &dialect.Model{}, Connection: models.Connection{BaseURL: stub.URL() + "/anthropic", Model: "builder-model", Family: models.FamilyAnthropic},
				Entry: models.Entry{InputWindow: 100_000, MaxOutputTokens: 8_000}, Machine: m, Tools: tools.NewRegistry(),
				Sleep: func(context.Context, time.Duration) error { return nil },
			}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Serve(ctx, f.client, 1, func(id string, err error) { t.Errorf("%s: %v", id, err) }) }()
	for _, want := range []session.Delta{
		{Turn: 1, Step: 1, Block: 0, Kind: session.DeltaThinking, Text: "Remote."},
		{Turn: 1, Step: 1, Block: 1, Kind: session.DeltaText, Text: "done remotely"},
	} {
		if d := waitDelta(t, sub); d != want {
			t.Fatalf("the delta %+v, want %+v", d, want)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := f.client.DroppedDeltas(); n != 0 {
		t.Fatalf("%d dropped", n)
	}
}

// TestTheDeltasRoute: a delta goes out under its session's live claim; a
// delta of a session the runner holds no claim on, past the queue, or of
// a send that fails is dropped and counted by the client; the server
// drops a delta whose generation is not the live claim's and refuses one
// a runner could not have sent.
func TestTheDeltasRoute(t *testing.T) {
	f := setup(t, 0)
	f.client.SetRenewInterval(time.Hour)
	s := hosted(t, f.st, "Go.")
	ctx := t.Context()
	sub := f.st.(session.DeltaSubscriber).SubscribeDeltas(ctx, s.ID)
	text := session.Delta{Turn: 1, Step: 1, Kind: session.DeltaText, Text: "held"}

	f.client.PublishDelta(s.ID, text)
	if f.client.DroppedDeltas() != 1 {
		t.Fatalf("a delta without a claim: %d dropped", f.client.DroppedDeltas())
	}
	claims, err := f.client.Claim(ctx, session.Holder{Runner: "run_a"}, 1, time.Second)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim %+v, %v", claims, err)
	}
	l := claims[0].Lease.(*lease)
	f.client.PublishDelta(s.ID, text)
	if d := waitDelta(t, sub); d != text {
		t.Fatalf("the delta %+v", d)
	}

	post := func(body string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url+runnerapi.Root+"/deltas", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer runner-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}
	stale := `{"deltas":[{"session_id":"` + s.ID + `","generation":999,"delta":{"turn":1,"step":1,"block":0,"kind":"text","text":"stale"}}]}`
	if code := post(stale); code != http.StatusNoContent || len(sub) != 0 {
		t.Fatalf("a stale generation: %d, %d delivered", code, len(sub))
	}
	for _, bad := range []string{
		`{"deltas":[{"session_id":"` + s.ID + `","generation":1,"delta":{"turn":1,"step":1,"block":0,"kind":"signature","text":"x"}}]}`,
		`{"deltas":[{"session_id":"` + s.ID + `","generation":1,"delta":{"turn":1,"step":1,"block":0,"kind":"text","text":"` + strings.Repeat("x", session.MaxDeltaText+1) + `"}}]}`,
		`{"deltas":[{"session_id":"` + s.ID + `","delta":{"turn":1,"step":1,"text":"x","color":"red"}}]}`,
	} {
		if code := post(bad); code != http.StatusBadRequest {
			t.Fatalf("an invalid delta: %d", code)
		}
	}

	// A send in flight that never ends: nothing drains the queue.
	f.client.mu.Lock()
	f.client.sending = true
	f.client.mu.Unlock()
	before := f.client.DroppedDeltas()
	for range deltaQueue + 3 {
		f.client.PublishDelta(s.ID, text)
	}
	if n := f.client.DroppedDeltas() - before; n != 3 {
		t.Fatalf("past the queue: %d dropped", n)
	}
	f.client.mu.Lock()
	f.client.deltas, f.client.sending = nil, false
	f.client.mu.Unlock()

	down, err := New("http://127.0.0.1:1", "runner-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	down.hold(&lease{c: down, id: s.ID, gen: 1})
	down.PublishDelta(s.ID, text)
	deadline := time.Now().Add(10 * time.Second)
	for down.DroppedDeltas() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("a failed send was not counted")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	before = f.client.DroppedDeltas()
	f.client.PublishDelta(s.ID, text)
	if f.client.DroppedDeltas() != before+1 {
		t.Fatal("a delta after the release went out")
	}
}
