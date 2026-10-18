// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"unicode/utf8"

	"latere.ai/x/topos/session"
)

// maxCachedText bounds the texts of a person's message's files a harness
// keeps between requests (spec 079); a text past it is read again at the
// next request.
const maxCachedText = 16 << 20

// texts keeps the text of each file a request carries inline, by its
// digest, for the life of a harness, shared by the copies a turn makes
// of it, so a long session reads a file once and not at every step.
type texts struct {
	mu    sync.Mutex
	held  map[session.Digest]string
	bytes int
}

// get is the text kept for d; a harness built without texts keeps none.
func (c *texts) get(d session.Digest) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.held[d]
	return s, ok
}

// put keeps the text of d while the texts kept stay within
// maxCachedText.
func (c *texts) put(d session.Digest, s string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.held[d]; ok || c.bytes+len(s) > maxCachedText {
		return
	}
	if c.held == nil {
		c.held = map[session.Digest]string{}
	}
	c.held[d] = s
	c.bytes += len(s)
}

// fold is the thread's transcript of evs, with the text of each file a
// person's message marks inline that the turn can read (spec 079), every
// redacted event left out where omitRedacted is set, and the texts of a
// thread that holds the history tool where it does (spec 084). Every
// request the harness builds from a fold, a replay's and a summary's
// among them, folds here, so each carries the same texts.
func (t *turn) fold(ctx context.Context, evs []session.Event, omitRedacted bool) (session.Transcript, error) {
	files, err := session.InlineFiles(evs, t.thread)
	if err != nil {
		return session.Transcript{}, err
	}
	var held map[session.Digest]string
	for _, a := range files {
		s, ok := t.inlineText(ctx, a)
		if !ok {
			continue
		}
		if held == nil {
			held = map[session.Digest]string{}
		}
		held[a.Blob] = s
	}
	return session.FoldWith(evs, t.thread, session.FoldOptions{
		OmitRedacted: omitRedacted, Texts: held, Device: t.s.Machine.Kind == session.MachineDevice, History: t.readsHistory(),
	})
}

// inlineText is the text of the file a records, from the harness's
// texts, or read where the file is kept. A file that cannot be read is
// logged and named by its path alone for the rest of the turn, which
// runs, and the next turn tries it again.
func (t *turn) inlineText(ctx context.Context, a session.Attachment) (string, bool) {
	if s, ok := t.h.texts.get(a.Blob); ok {
		return s, true
	}
	t.sh.inlining.Lock()
	failed := t.sh.unreadable[a.Blob]
	t.sh.inlining.Unlock()
	if failed {
		return "", false
	}
	s, err := readInline(ctx, t.l, t.h.c.Files, a)
	if err != nil {
		t.sh.inlining.Lock()
		if t.sh.unreadable == nil {
			t.sh.unreadable = map[session.Digest]bool{}
		}
		t.sh.unreadable[a.Blob] = true
		t.sh.inlining.Unlock()
		t.h.c.Log.WarnContext(ctx, "a message's file is named to the model by its path alone: its text could not be read", "session", t.s.ID, "path", a.Path, "error", err)
		return "", false
	}
	t.h.texts.put(a.Blob, s)
	return s, true
}

// readInline reads the text of the file a records where it is kept, as
// the runner reads it to deliver it: the session's folder of a files
// store for a file with a ref, the session's blobs for one without. The
// bytes are the record's, at most session.InlineTextBytes of them, with
// its size and its digest, and UTF-8.
func readInline(ctx context.Context, blobs BlobReader, files Files, a session.Attachment) (string, error) {
	if a.Size < 0 || a.Size > session.InlineTextBytes {
		return "", fmt.Errorf("the record names %d bytes, and a file read inline is at most %d", a.Size, session.InlineTextBytes)
	}
	var rc io.ReadCloser
	var err error
	switch {
	case a.Ref != "" && files == nil:
		err = errNoFiles
	case a.Ref != "":
		rc, _, err = files.Open(ctx, a.Ref)
	default:
		rc, err = blobs.Blob(ctx, a.Blob)
	}
	if err != nil {
		return "", err
	}
	b, rerr := io.ReadAll(io.LimitReader(rc, a.Size+1))
	if err := errors.Join(rerr, rc.Close()); err != nil {
		return "", err
	}
	switch {
	case int64(len(b)) != a.Size:
		return "", fmt.Errorf("the file holds other than the %d bytes its record names", a.Size)
	case session.DigestOf(b) != a.Blob:
		return "", fmt.Errorf("the file's digest is not %s, its record's", a.Blob)
	case !utf8.Valid(b):
		return "", errors.New("the file is not UTF-8")
	}
	return string(b), nil
}
