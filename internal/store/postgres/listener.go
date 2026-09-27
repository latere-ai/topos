// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/pkg/retry"
	"latere.ai/x/pkg/wait"
)

// reconnect is the backoff between attempts to bring the listener's
// connection back. The attempts never run out: a store that serves
// watchers keeps trying for as long as it is open, and the watchers
// poll meanwhile.
var reconnect = retry.Policy{Base: 200 * time.Millisecond, Max: 10 * time.Second}

// closeTimeout bounds the goodbye on a listener connection that is
// being replaced or shut down.
const closeTimeout = 5 * time.Second

// listener is a store's one LISTEN connection and the watchers it
// wakes. A notification carries the id of the session that changed, and
// only that session's watchers wake; each then reads the log itself, so
// a notification is a hint and never the data.
//
// The connection is on the direct DSN, since a transaction-pooling
// proxy does not keep a LISTEN across transactions. It opens with the
// first watcher and closes with the store; one that drops is dialed
// again with backoff, and once it listens again every watcher wakes, so
// none misses an append made while it was down.
type listener struct {
	cfg     *pgx.ConnConfig
	log     *slog.Logger
	backoff retry.Policy

	mu       sync.Mutex
	watchers map[string]map[*watcher]struct{}
	cancel   context.CancelFunc
	done     chan struct{}
	stopped  bool
	// connects counts the connections that reached LISTEN, and pid is the
	// backend of the current one, zero while there is none.
	connects int
	pid      uint32
	// dropped, when set, runs after a connection failed and before the
	// next dial.
	dropped func()
}

// watcher is one Watch's wake signal. Its buffer of one coalesces any
// number of notifications that arrive while the watcher reads into one
// more read, so the listener never waits on a slow watcher.
type watcher struct {
	wake chan struct{}
	// wakes counts the signals sent to this watcher, coalesced or not.
	wakes int
}

func newListener(cfg *pgx.ConnConfig, log *slog.Logger) *listener {
	return &listener{cfg: cfg, log: log, backoff: reconnect, watchers: map[string]map[*watcher]struct{}{}}
}

// subscribe registers a watcher of session id and starts the connection
// when it is the first; the connection outlives ctx, whose values it
// keeps. A subscription after stop registers a watcher that no
// notification wakes; its poll finds the store closed.
func (l *listener) subscribe(ctx context.Context, id string) *watcher {
	w := &watcher{wake: make(chan struct{}, 1)}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.watchers[id] == nil {
		l.watchers[id] = map[*watcher]struct{}{}
	}
	l.watchers[id][w] = struct{}{}
	if l.cancel == nil && !l.stopped {
		ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		l.cancel, l.done = cancel, make(chan struct{})
		go l.run(ctx)
	}
	return w
}

// unsubscribe removes a watcher of session id.
func (l *listener) unsubscribe(id string, w *watcher) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.watchers[id], w)
	if len(l.watchers[id]) == 0 {
		delete(l.watchers, id)
	}
}

// signal wakes w without waiting; l.mu is held.
func (w *watcher) signal() {
	w.wakes++
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// route wakes the watchers of session id.
func (l *listener) route(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for w := range l.watchers[id] {
		w.signal()
	}
}

// wakeAll wakes every watcher once.
func (l *listener) wakeAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ws := range l.watchers {
		for w := range ws {
			w.signal()
		}
	}
}

// stop closes the connection and waits for its loop to end; no
// connection opens after it.
func (l *listener) stop() {
	l.mu.Lock()
	l.stopped = true
	cancel, done := l.cancel, l.done
	l.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// run keeps a connection listening until ctx ends.
func (l *listener) run(ctx context.Context) {
	defer close(l.done)
	failures := 0
	for {
		listened, err := l.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		if listened {
			failures = 0
		}
		failures++
		delay := l.backoff.Delay(failures)
		l.log.WarnContext(ctx, "postgres: the listener connection failed; watchers poll until it is back", "attempt", failures, "retry_in", delay, "err", err)
		l.mu.Lock()
		dropped := l.dropped
		l.mu.Unlock()
		if dropped != nil {
			dropped()
		}
		if err := wait.Sleep(ctx, delay); err != nil {
			return
		}
	}
}

// listen dials one connection, listens on it and routes its
// notifications until it fails or ctx ends. It reports whether the
// connection reached LISTEN.
func (l *listener) listen(ctx context.Context) (bool, error) {
	conn, err := pgx.ConnectConfig(ctx, l.cfg)
	if err != nil {
		return false, fmt.Errorf("postgres: connect to listen: %w", err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		defer cancel()
		if err := conn.Close(cctx); err != nil {
			l.log.WarnContext(ctx, "postgres: close the listener connection", "err", err)
		}
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return false, fmt.Errorf("postgres: listen: %w", err)
	}
	l.up(conn.PgConn().PID())
	defer l.down()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return true, fmt.Errorf("postgres: wait for a notification: %w", err)
		}
		l.route(n.Payload)
	}
}

// up records a connection that listens and wakes every watcher, since
// an append may have landed before it listened: while the previous
// connection was down, or before the first one was up. The wake follows
// the LISTEN, so an append after it is announced and one before it is
// found by the read the wake starts.
func (l *listener) up(pid uint32) {
	l.mu.Lock()
	l.connects++
	l.pid = pid
	l.mu.Unlock()
	l.wakeAll()
}

// down records that the connection stopped listening.
func (l *listener) down() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pid = 0
}
