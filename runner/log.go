// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"latere.ai/x/topos/session"
)

// Log is the harness.Log a runner hands its harness: appends go to the
// session's store after its true last sequence, and whatever others
// appended in the meantime comes back to the harness.
type Log struct {
	st   session.Store
	id   string
	mu   sync.Mutex
	last uint64
	// lost is the lease's Lost channel; once it is closed every append
	// is refused with ErrLeaseLost.
	lost <-chan struct{}
	// fence is the lease's own fenced append, when its store offers one.
	fence session.Fence
	// beside are the events the runner appended beside the harness, and
	// the events those appends read, which the next Append hands the
	// harness, so its view of the log misses none of them.
	beside []session.Event
}

// NewLog returns the log of one session whose last sequence is last.
func NewLog(st session.Store, id string, last uint64) *Log {
	return &Log{st: st, id: id, last: last}
}

// Last is the last sequence the log has seen.
func (l *Log) Last() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

// Append reads the events appended since the last one it saw, then
// appends the batch after them, stamping it in place. It returns what
// others appended and what the runner appended beside it since the last
// Append, in sequence order.
func (l *Log) Append(ctx context.Context, batch []session.Event) ([]session.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	foreign, err := l.append(ctx, batch)
	if err != nil {
		return nil, err
	}
	if len(l.beside) > 0 {
		foreign = append(l.beside, foreign...)
		l.beside = nil
	}
	return foreign, nil
}

// appendBeside appends a batch the runner writes while the harness runs
// a turn, such as the session.machine of a machine opened on demand
// (spec 009). The batch and the events the append read reach the
// harness with its next Append.
func (l *Log) appendBeside(ctx context.Context, batch []session.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	foreign, err := l.append(ctx, batch)
	if err != nil {
		return err
	}
	l.beside = append(append(l.beside, foreign...), batch...)
	return nil
}

// append is Append's work under l.mu.
func (l *Log) append(ctx context.Context, batch []session.Event) ([]session.Event, error) {
	select {
	case <-l.lost:
		return nil, ErrLeaseLost
	default:
	}
	foreign, err := l.st.Events(ctx, l.id, l.last+1, 0)
	if err != nil {
		return nil, err
	}
	if n := len(foreign); n > 0 {
		l.last = foreign[n-1].Seq
	}
	session.Stamp(l.id, l.last, batch)
	appendTo := l.st.Append
	if l.fence != nil {
		appendTo = func(ctx context.Context, _ string, after uint64, evs []session.Event) (uint64, error) {
			return l.fence.Append(ctx, after, evs)
		}
	}
	last, err := appendTo(ctx, l.id, l.last, batch)
	if errors.Is(err, session.ErrLeaseLost) {
		return nil, fmt.Errorf("%w: %w", ErrLeaseLost, err)
	}
	if err != nil {
		return nil, err
	}
	l.last = last
	return foreign, nil
}

// PutBlob stores a blob of the session.
func (l *Log) PutBlob(ctx context.Context, r io.Reader) (session.Digest, error) {
	return l.st.PutBlob(ctx, l.id, r)
}

// Blob reads a blob of the session.
func (l *Log) Blob(ctx context.Context, d session.Digest) (io.ReadCloser, error) {
	return l.st.Blob(ctx, l.id, d)
}

// Why a title's batch was not appended whole (spec 083).
var (
	// errTitleRenamed is a title whose request was recorded and whose
	// change was left out: a person changed the session's title while it
	// was asked, and a person's name wins.
	errTitleRenamed = errors.New("runner: a person changed the session's title while its title was asked; the person's stands")
	// errTitleEnded is a title of which nothing was appended: the session
	// ended while it was asked, and an ended log takes no more events.
	errTitleEnded = errors.New("runner: the session ended while its title was asked; nothing was appended")
)

// titleAppends bounds how often a title's batch is read against the log
// again after another writer appended in the moment before it.
const titleAppends = 8

// appendTitle appends a title's batch as Append does, deciding what to
// append from the events others appended since after, the last sequence
// the title read, which are read in the same step as the store takes the
// batch after them: when one is a session.status that ended the session,
// nothing is appended and the answer is errTitleEnded; when one is a
// person's session.title_changed, the batch's session.title_changed is
// left out, the rest appended, and the answer is errTitleRenamed. A batch
// the store refuses for events appended between the read and the append
// is decided again over them, titleAppends times at most.
func (l *Log) appendTitle(ctx context.Context, batch []session.Event, after uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var err error
	for range titleAppends {
		select {
		case <-l.lost:
			return ErrLeaseLost
		default:
		}
		var foreign []session.Event
		if foreign, err = l.st.Events(ctx, l.id, l.last+1, 0); err != nil {
			return err
		}
		if n := len(foreign); n > 0 {
			l.last = foreign[n-1].Seq
		}
		l.beside = append(l.beside, foreign...)
		var renamed bool
		for _, e := range foreign {
			var st session.SessionStatus
			var tc session.TitleChanged
			switch {
			case e.Seq <= after || e.Redacted():
			case e.Type == session.TypeSessionStatus && e.Decode(&st) == nil && st.Status == session.StatusEnded:
				return errTitleEnded
			case e.Type == session.TypeTitleChanged && e.Decode(&tc) == nil && tc.By.Kind == session.SenderPerson:
				renamed = true
			}
		}
		kept := batch
		if renamed {
			kept = slices.DeleteFunc(slices.Clone(batch), func(e session.Event) bool { return e.Type == session.TypeTitleChanged })
		}
		if len(kept) == 0 {
			return errTitleRenamed
		}
		session.Stamp(l.id, l.last, kept)
		appendTo := l.st.Append
		if l.fence != nil {
			appendTo = func(ctx context.Context, _ string, after uint64, evs []session.Event) (uint64, error) {
				return l.fence.Append(ctx, after, evs)
			}
		}
		last, aerr := appendTo(ctx, l.id, l.last, kept)
		switch {
		case errors.Is(aerr, session.ErrSequenceConflict):
			err = aerr
			continue
		case errors.Is(aerr, session.ErrLeaseLost):
			return fmt.Errorf("%w: %w", ErrLeaseLost, aerr)
		case aerr != nil:
			return aerr
		}
		l.last = last
		l.beside = append(l.beside, kept...)
		if renamed {
			return errTitleRenamed
		}
		return nil
	}
	return err
}

// titleLog is the harness.Log a title appends through: its batch goes to
// Log.appendTitle, decided over what others appended since after.
type titleLog struct {
	l     *Log
	after uint64
}

func (t titleLog) Append(ctx context.Context, batch []session.Event) ([]session.Event, error) {
	return nil, t.l.appendTitle(ctx, batch, t.after)
}

func (t titleLog) PutBlob(ctx context.Context, r io.Reader) (session.Digest, error) {
	return t.l.PutBlob(ctx, r)
}

func (t titleLog) Blob(ctx context.Context, d session.Digest) (io.ReadCloser, error) {
	return t.l.Blob(ctx, d)
}
