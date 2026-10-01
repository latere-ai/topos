// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"latere.ai/x/topos/session"
)

// deltaChannel is the notification channel live deltas cross replicas
// on, beside channel: a runner's replica sends them, and every replica's
// listener hands them to the streams that follow their session.
const deltaChannel = "topos_deltas"

// maxNotify is the most bytes a notification's payload holds: Postgres
// refuses a payload of 8000 bytes or more.
const maxNotify = 7999

// deltaQueue is how many deltas wait for the sender at most; the next
// one is dropped until it catches up.
const deltaQueue = 1024

// notifyTimeout bounds one send, so a pool that does not answer drops
// that send's deltas instead of holding the ones behind them.
const notifyTimeout = 2 * time.Second

// deltaNote is the payload of one notification on deltaChannel. N makes
// the payloads of one send distinct, since Postgres delivers identical
// payloads of one transaction once; D are the deltas with their
// sessions, in the order they were published.
type deltaNote struct {
	N uint64       `json:"n"`
	D []deltaEntry `json:"d"`
}

type deltaEntry struct {
	S string        `json:"s"`
	F session.Delta `json:"f"`
}

// noteOverhead is the bytes of a deltaNote around its entries at the
// largest N.
var noteOverhead = len(`{"n":` + strconv.FormatUint(^uint64(0), 10) + `,"d":[]}`)

// queued is one delta waiting for the sender.
type queued struct {
	id string
	d  session.Delta
}

// sender sends a store's deltas to every replica's listener as
// notifications, from the serving pool. One goroutine sends at a time:
// the first delta that finds none running starts it, and it ends when
// the queue is empty, so a store holds no connection and no goroutine
// for deltas while none are published, and at most one pooled
// connection, for one statement at a time, while they are. Everything
// the queue holds when a send ends goes in the next one, so a busy
// replica sends many deltas, of any number of sessions, per statement.
type sender struct {
	pool *pgxpool.Pool
	// hub counts and logs the deltas the sender drops.
	hub *session.DeltaHub

	mu      sync.Mutex
	queue   []queued
	running bool
	closed  bool
	done    sync.WaitGroup
	// seq numbers the payloads; only the running goroutine reads it.
	seq uint64
}

// publish queues one delta and starts the sender when none runs. It
// never waits: a full queue or a closed store drops the delta.
func (q *sender) publish(id string, d session.Delta) {
	q.mu.Lock()
	defer q.mu.Unlock()
	switch {
	case q.closed:
		q.hub.Drop(id, 1, "the store is closed", nil)
		return
	case len(q.queue) >= deltaQueue:
		q.hub.Drop(id, 1, "the send queue is full", nil)
		return
	}
	q.queue = append(q.queue, queued{id: id, d: d})
	if !q.running {
		q.running = true
		q.done.Add(1)
		go q.run()
	}
}

// run sends what the queue holds until it is empty.
func (q *sender) run() {
	defer q.done.Done()
	for {
		q.mu.Lock()
		batch := q.queue
		q.queue = nil
		if len(batch) == 0 {
			q.running = false
			q.mu.Unlock()
			return
		}
		q.mu.Unlock()
		q.send(batch)
	}
}

// send notifies the batch's payloads in one statement; a failure drops
// them.
func (q *sender) send(batch []queued) {
	payloads, n := q.pack(batch)
	if n == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()
	// unnest yields the payloads in order, and the notifications of one
	// transaction are delivered in the order they were made.
	if _, err := q.pool.Exec(ctx, `SELECT pg_notify($1, p) FROM unnest($2::text[]) AS p`, deltaChannel, payloads); err != nil {
		q.hub.Drop("", n, "the notification failed", err)
	}
}

// pack encodes the batch, in order, into payloads of at most maxNotify
// bytes, and reports how many deltas they hold. A delta that does not
// fit a payload alone is dropped; session.MaxDeltaText keeps every delta
// a runner sends well under it.
func (q *sender) pack(batch []queued) ([]string, int) {
	var payloads []string
	var entries [][]byte
	size, n := 0, 0
	flush := func() {
		if len(entries) == 0 {
			return
		}
		q.seq++
		var b bytes.Buffer
		b.WriteString(`{"n":` + strconv.FormatUint(q.seq, 10) + `,"d":[`)
		b.Write(bytes.Join(entries, []byte(",")))
		b.WriteString(`]}`)
		payloads = append(payloads, b.String())
		entries, size = nil, 0
	}
	for _, e := range batch {
		b, err := session.Marshal(deltaEntry{S: e.id, F: e.d})
		if err != nil {
			q.hub.Drop(e.id, 1, "the delta does not encode", err)
			continue
		}
		if noteOverhead+len(b) > maxNotify {
			q.hub.Drop(e.id, 1, "the delta does not fit a notification", nil)
			continue
		}
		if noteOverhead+size+1+len(b) > maxNotify {
			flush()
		}
		if len(entries) > 0 {
			size++
		}
		entries = append(entries, b)
		size += len(b)
		n++
	}
	flush()
	return payloads, n
}

// close stops taking deltas and waits for the send in flight.
func (q *sender) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.done.Wait()
}

// deliver hands the deltas of one notification to this process's
// subscribers. A payload is decoded only while some stream subscribes;
// one that does not decode is counted as one dropped delta.
func (l *listener) deliver(payload string) {
	if !l.hub.Subscribed() {
		return
	}
	var note deltaNote
	if err := json.Unmarshal([]byte(payload), &note); err != nil {
		l.hub.Drop("", 1, "a notification that does not decode", err)
		return
	}
	for _, e := range note.D {
		l.hub.PublishDelta(e.S, e.F)
	}
}

// PublishDelta sends a live delta to the subscribers of its session on
// every replica of the database, best effort (spec 016): it never waits,
// and a delta the queue has no room for, or that a failed notification
// carried, is dropped and counted.
func (s *Store) PublishDelta(id string, d session.Delta) { s.sender.publish(id, d) }

// SubscribeDeltas follows the live deltas of a session published on any
// replica, from the store's one listener, until ctx ends. A delta
// published before the listener listens is not seen.
func (s *Store) SubscribeDeltas(ctx context.Context, id string) <-chan session.Delta {
	s.listener.start(ctx)
	return s.hub.SubscribeDeltas(ctx, id)
}

// DroppedDeltas is how many deltas this store dropped: on their way out,
// or on their way to a subscriber that did not keep up.
func (s *Store) DroppedDeltas() uint64 { return s.hub.DroppedDeltas() }
