// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build postgres

package postgres

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/server"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/host"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/dialect"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// listening waits until st's listener connection listens.
func listening(t *testing.T, st *Store) {
	t.Helper()
	eventually(t, "the listener", func() bool { _, pid := connects(st); return pid != 0 })
}

// receiveDelta reads the next delta of ch, failing after ten seconds.
func receiveDelta(t *testing.T, ch <-chan session.Delta, what string) session.Delta {
	t.Helper()
	select {
	case d, open := <-ch:
		if !open {
			t.Fatalf("%s: the subscription closed", what)
		}
		return d
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: no delta", what)
	}
	return session.Delta{}
}

// TestDeltasCrossReplicas: a delta one replica publishes reaches the
// subscribers of its session on that replica and on another, in the
// order published, and no subscriber of another session.
func TestDeltasCrossReplicas(t *testing.T) {
	rs := replicas(t, Options{Poll: time.Hour})
	ids := sessions(t, rs[0], 2)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	local := rs[0].SubscribeDeltas(ctx, ids[0])
	remote := rs[1].SubscribeDeltas(ctx, ids[0])
	elsewhere := rs[1].SubscribeDeltas(ctx, ids[1])
	listening(t, rs[0])
	listening(t, rs[1])
	// Fewer than a subscriber's buffer, since the subscribers are read one
	// after the other, and long enough to take several notifications.
	var want []session.Delta
	for i := range session.DeltaBuffer - 6 {
		d := session.Delta{Turn: 1, Step: 1 + i/100, Block: i % 3, Kind: session.DeltaText, Text: strings.Repeat("x", 1+i*7%session.MaxDeltaText)}
		if i%100 == 99 {
			d = session.Delta{Turn: 1, Step: 1 + i/100, Reset: true}
		}
		want = append(want, d)
		rs[0].PublishDelta(ids[0], d)
	}
	for _, sub := range []struct {
		name string
		ch   <-chan session.Delta
	}{{"the runner's replica", local}, {"another replica", remote}} {
		for i, w := range want {
			if d := receiveDelta(t, sub.ch, sub.name); d != w {
				t.Fatalf("%s: delta %d is %+v, want %+v", sub.name, i, d, w)
			}
		}
	}
	select {
	case d := <-elsewhere:
		t.Fatalf("a subscriber of another session received %+v", d)
	case <-time.After(100 * time.Millisecond):
	}
	if n := rs[0].DroppedDeltas() + rs[1].DroppedDeltas(); n != 0 {
		t.Fatalf("%d dropped", n)
	}
}

// TestDeltasPackIntoNotifications: a batch packs into payloads under
// Postgres's limit, in order, each numbered apart, and a delta too large
// for any payload is dropped and counted.
func TestDeltasPackIntoNotifications(t *testing.T) {
	st := fresh(t, Options{})
	big := session.Delta{Turn: 1, Step: 1, Kind: session.DeltaText, Text: strings.Repeat("\x01", maxNotify)}
	var batch []queued
	for i := range 40 {
		batch = append(batch, queued{id: "ses_" + strings.Repeat("a", i%3+1), d: session.Delta{Turn: 1, Step: 1, Block: i, Kind: session.DeltaToolInput, Text: strings.Repeat("\"<\n", session.MaxDeltaText/3)}})
		if i == 7 {
			batch = append(batch, queued{id: "ses_big", d: big})
		}
	}
	payloads, n := st.sender.pack(batch)
	if n != 40 || st.DroppedDeltas() != 1 || len(payloads) < 2 {
		t.Fatalf("%d deltas in %d payloads, %d dropped", n, len(payloads), st.DroppedDeltas())
	}
	var got []queued
	numbers := map[uint64]bool{}
	for _, p := range payloads {
		if len(p) > maxNotify {
			t.Fatalf("a payload of %d bytes", len(p))
		}
		var note deltaNote
		if err := json.Unmarshal([]byte(p), &note); err != nil {
			t.Fatal(err)
		}
		numbers[note.N] = true
		for _, e := range note.D {
			got = append(got, queued{id: e.S, d: e.F})
		}
	}
	batch = slices.DeleteFunc(batch, func(q queued) bool { return q.id == "ses_big" })
	if !slices.Equal(got, batch) || len(numbers) != len(payloads) {
		t.Fatalf("the payloads hold %d deltas, %d numbers for %d payloads", len(got), len(numbers), len(payloads))
	}
}

// TestADeltaNeverWaitsOnTheDatabase: while a send is in flight the queue
// holds deltaQueue deltas and drops the rest at once; a send that fails
// drops its deltas; a closed store drops what it is handed; a
// notification that does not decode is counted. Every drop is counted.
func TestADeltaNeverWaitsOnTheDatabase(t *testing.T) {
	st := fresh(t, Options{})
	id := sessions(t, st, 1)[0]
	// A send in flight that never ends: nothing drains the queue.
	st.sender.mu.Lock()
	st.sender.running = true
	st.sender.mu.Unlock()
	start := time.Now()
	for range deltaQueue + 25 {
		st.PublishDelta(id, session.Delta{Turn: 1, Step: 1, Kind: session.DeltaText, Text: "x"})
	}
	if took := time.Since(start); took > time.Second || st.DroppedDeltas() != 25 {
		t.Fatalf("publishing past the queue took %s and dropped %d", took, st.DroppedDeltas())
	}

	failing := fresh(t, Options{})
	failing.pool.Close()
	failing.PublishDelta(id, session.Delta{Turn: 1, Step: 1, Kind: session.DeltaText, Text: "x"})
	eventually(t, "the failed send's drop", func() bool { return failing.DroppedDeltas() == 1 })

	closed := fresh(t, Options{})
	closed.Close()
	closed.PublishDelta(id, session.Delta{Turn: 1, Step: 1, Kind: session.DeltaText, Text: "x"})
	if closed.DroppedDeltas() != 1 {
		t.Fatalf("a closed store dropped %d", closed.DroppedDeltas())
	}

	garbled := fresh(t, Options{})
	ids := sessions(t, garbled, 1)
	sub := garbled.SubscribeDeltas(t.Context(), ids[0])
	listening(t, garbled)
	if _, err := garbled.pool.Exec(t.Context(), `SELECT pg_notify($1, 'not json')`, deltaChannel); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the garbled notification's drop", func() bool { return garbled.DroppedDeltas() == 1 })
	if len(sub) != 0 {
		t.Fatalf("a garbled notification delivered %d deltas", len(sub))
	}
}

// trickle streams a model's text two bytes at a time with a pause
// between, as a model that writes slowly does.
type trickle struct{ models.Model }

func (m trickle) Stream(ctx context.Context, req models.Request) (models.Stream, error) {
	s, err := m.Model.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	return &trickled{Stream: s}, nil
}

type trickled struct {
	models.Stream
	rest []ir.Event
}

// trickleGap is the pause before each piece.
const trickleGap = 3 * time.Millisecond

func (s *trickled) Next() (ir.Event, error) {
	if len(s.rest) > 0 {
		ev := s.rest[0]
		s.rest = s.rest[1:]
		time.Sleep(trickleGap)
		return ev, nil
	}
	ev, err := s.Stream.Next()
	if err != nil || ev.Type != ir.EventTextDelta {
		return ev, err
	}
	for i := 0; i < len(ev.Delta); i += 2 {
		piece := ev
		piece.Delta = ev.Delta[i:min(i+2, len(ev.Delta))]
		s.rest = append(s.rest, piece)
	}
	return s.Next()
}

// anyone is every bearer's caller, and allowAll the authorizer that
// allows each question: the test is about the stream, not the guard.
type anyone struct{}

func (anyone) Authenticate(*http.Request) (auth.Caller, error) {
	return auth.Caller{Subject: "usr_ada", Sub: "usr_ada"}, nil
}

type allowAll struct{}

func (allowAll) Authorize(context.Context, authz.Request) (authz.Decision, error) {
	return authz.Decision{Allow: true}, nil
}

// sseFrame is one Server-Sent Events frame.
type sseFrame struct{ id, event, data string }

// frames reads the stream of path on srv and hands its frames to the
// returned channel, which closes with the stream.
func frames(t *testing.T, ctx context.Context, srv *httptest.Server, path string) <-chan sseFrame {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer ada")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: %d", resp.StatusCode)
	}
	out := make(chan sseFrame, 256)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		var fr sseFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if fr.event != "" {
					out <- fr
				}
				fr = sseFrame{}
			case strings.HasPrefix(line, "id: "):
				fr.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				fr.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				fr.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return out
}

// TestARunnersDeltasReachAStreamOnAnotherReplica: two servers on one
// database, a runner on the first driving a session against a model
// that streams its text slowly. A stream with deltas=1 on either server
// receives the text as deltas with no id, fewer than the fragments the
// model streamed and joining into the step's text, and the turn's
// events as before; a stream without deltas=1 receives no delta.
func TestARunnersDeltasReachAStreamOnAnotherReplica(t *testing.T) {
	rs := replicas(t, Options{Poll: time.Hour})
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	var servers [2]*httptest.Server
	for i, st := range rs {
		api, err := server.New(server.Options{Sessions: st, Objects: st, Verifier: anyone{}, Guard: auth.Guard{Authorizer: allowAll{}}, PublicURL: "https://topos.example", Heartbeat: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		servers[i] = httptest.NewServer(api.Handler())
		t.Cleanup(servers[i].Close)
	}

	const model = "builder-model"
	const answer = "The quick brown fox jumps over the lazy dog, then over the fence."
	stub := luxstub.New(t)
	stub.Script(model, luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{{Type: ir.BlockText, Text: answer}}, StopReason: ir.StopEndTurn}})
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Options{
		Store: rs[0], ID: "run_replica_a", Kind: runner.KindServe,
		Harness: func(ctx context.Context, s session.Session) (harness.Config, error) {
			m, err := host.Open(host.Options{Workdir: base, SpillDir: filepath.Join(base, ".spill"), Environ: []string{"PATH=" + os.Getenv("PATH")}})
			if err != nil {
				return harness.Config{}, err
			}
			return harness.Config{
				Model: trickle{&dialect.Model{}}, Connection: models.Connection{BaseURL: stub.URL() + "/anthropic", Model: model, Family: models.FamilyAnthropic},
				Entry: models.Entry{InputWindow: 100_000, MaxOutputTokens: 8_000}, Machine: m, Tools: tools.NewRegistry(),
				Sleep: func(context.Context, time.Duration) error { return nil },
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := session.New(session.AgentRef{ID: session.NewID(session.PrefixAgent), Name: "builder", Version: 1},
		session.Sender{Subject: "usr_ada", Kind: session.SenderPerson}, session.RunnerHosted, session.Machine{Kind: machine.KindHost}, time.Now())
	if err := rs[0].Create(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	say(t, rs[0], s.ID, "Write one sentence.")

	path := "/v1/sessions/" + s.ID + "/stream"
	streams := map[string]<-chan sseFrame{
		"another replica":      frames(t, ctx, servers[1], path+"?deltas=1"),
		"the runner's replica": frames(t, ctx, servers[0], path+"?deltas=1"),
		"no deltas":            frames(t, ctx, servers[1], path),
	}
	for name, ch := range streams {
		if fr := <-ch; fr.id != "1" {
			t.Fatalf("%s: the replay began with %+v", name, fr)
		}
	}
	listening(t, rs[0])
	listening(t, rs[1])
	drove := make(chan error, 1)
	go func() {
		_, err := r.Drive(ctx, s.ID)
		drove <- err
	}()

	for name, ch := range streams {
		var text strings.Builder
		deltas, idle := 0, false
		for !idle || (name != "no deltas" && text.String() != answer) {
			var fr sseFrame
			select {
			case fr = <-ch:
			case <-ctx.Done():
				t.Fatalf("%s: idle %v, the deltas joined into %q", name, idle, text.String())
			}
			switch fr.event {
			case "delta":
				var d session.Delta
				if err := json.Unmarshal([]byte(fr.data), &d); err != nil {
					t.Fatal(err)
				}
				if fr.id != "" || d.Turn != 1 || d.Step != 1 || d.Block != 0 || d.Kind != session.DeltaText || d.Reset {
					t.Fatalf("%s: the delta %+v of the frame %+v", name, d, fr)
				}
				text.WriteString(d.Text)
				deltas++
			case string(session.TypeSessionStatus):
				var p session.SessionStatus
				if err := json.Unmarshal([]byte(fr.data), &struct {
					Payload *session.SessionStatus `json:"payload"`
				}{&p}); err != nil {
					t.Fatal(err)
				}
				idle = idle || p.Status == session.StatusIdle
			}
		}
		pieces := (len(answer) + 1) / 2
		switch {
		case name == "no deltas" && deltas != 0:
			t.Fatalf("a stream without deltas=1 carried %d", deltas)
		case name != "no deltas" && (deltas == 0 || deltas >= pieces):
			t.Fatalf("%s: %d deltas for %d fragments", name, deltas, pieces)
		}
		t.Logf("%s: %d deltas for %d fragments", name, deltas, pieces)
	}
	if err := <-drove; err != nil {
		t.Fatal(err)
	}
	if n := rs[0].DroppedDeltas() + rs[1].DroppedDeltas(); n != 0 {
		t.Fatalf("%d dropped", n)
	}
}
