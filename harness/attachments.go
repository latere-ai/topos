// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// CodeAttachmentUnavailable is spec 015's code for a file a message
// carries that the runner could not write into the machine; it stays
// pending, and a later turn tries it again.
const CodeAttachmentUnavailable = "attachment_unavailable"

// maxExclude bounds how much of a repository's .git/info/exclude is read
// before the attachments line is added to it.
const maxExclude = 1 << 20

// DeliverAttachments writes into m's working directory each file of the
// log's messages that the session's latest machine has not been given
// (spec 015), at its path under session.AttachmentDir, and returns the
// events that record it: attachments.delivered naming the paths written,
// and a session.error attachment_unavailable naming the files that could
// not be written, which it also returns and which stay pending. A path
// in skip is not tried. When the working directory is a repository, the
// directory is added to its .git/info/exclude, so the session's commits
// take a file only when the agent adds it by force. Nothing pending
// returns no events.
func DeliverAttachments(ctx context.Context, m machine.Machine, blobs BlobReader, log []session.Event, skip map[string]bool, now time.Time) ([]session.Event, []string, error) {
	var written, failed []string
	var errs []error
	for _, a := range session.PendingAttachments(log) {
		if skip[a.Path] {
			continue
		}
		if err := writeAttachment(ctx, m, blobs, a); err != nil {
			failed = append(failed, a.Path)
			errs = append(errs, fmt.Errorf("%s: %w", a.Path, err))
			continue
		}
		written = append(written, a.Path)
	}
	var out []session.Event
	if len(written) > 0 {
		if err := excludeAttachments(ctx, m); err != nil {
			errs = append(errs, fmt.Errorf("exclude %s/ from git: %w", session.AttachmentDir, err))
		}
		e, err := session.NewEvent(session.TypeAttachmentsDelivered, session.AttachmentsDelivered{Paths: written}, now)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, e)
	}
	if err := errors.Join(errs...); err != nil {
		e, eerr := session.NewEvent(session.TypeSessionError, session.SessionError{Code: CodeAttachmentUnavailable, Message: err.Error(), Retryable: true}, now)
		if eerr != nil {
			return nil, nil, eerr
		}
		out = append(out, e)
	}
	return out, failed, nil
}

// writeAttachment writes one file from its blob.
func writeAttachment(ctx context.Context, m machine.Machine, blobs BlobReader, a session.Attachment) error {
	if !strings.HasPrefix(a.Path, session.AttachmentDir+"/") || path.Clean(a.Path) != a.Path {
		return fmt.Errorf("the path is not under %s/", session.AttachmentDir)
	}
	rc, err := blobs.Blob(ctx, a.Blob)
	if err != nil {
		return err
	}
	werr := m.WriteFile(ctx, a.Path, rc, 0o644)
	return errors.Join(werr, rc.Close())
}

// excludeAttachments adds the attachments directory to the working
// directory's .git/info/exclude when the working directory is a
// repository's top level and the line is not there yet.
func excludeAttachments(ctx context.Context, m machine.Machine) error {
	fi, err := m.Stat(ctx, ".git")
	switch {
	case errors.Is(err, fs.ErrNotExist), err == nil && !fi.IsDir:
		return nil
	case err != nil:
		return err
	}
	const exclude = ".git/info/exclude"
	line := "/" + session.AttachmentDir + "/"
	var body []byte
	rc, err := m.ReadFile(ctx, exclude)
	switch {
	case err == nil:
		body, err = io.ReadAll(io.LimitReader(rc, maxExclude))
		if err = errors.Join(err, rc.Close()); err != nil {
			return err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if slices.Contains(strings.Split(string(body), "\n"), line) {
		return nil
	}
	if len(body) > 0 && !bytes.HasSuffix(body, []byte("\n")) {
		body = append(body, '\n')
	}
	return m.WriteFile(ctx, exclude, bytes.NewReader(append(body, line+"\n"...)), 0)
}

// deliverAttachments writes, before a step's request, the files of the
// messages the request carries that the open machine has not been given,
// so a model that reads a message's paths finds them. A machine opened
// on demand and not open yet gets them when it opens, from the runner. A
// file that cannot be written is tried again at a later turn, not at
// every step of this one.
func (t *turn) deliverAttachments(ctx context.Context) error {
	m := t.h.c.Machine
	if d, ok := m.(*machine.Deferred); ok && d.Opened() == nil {
		return nil
	}
	t.sh.delivering.Lock()
	defer t.sh.delivering.Unlock()
	if len(session.PendingAttachments(t.events())) == 0 {
		return nil
	}
	evs, failed, err := DeliverAttachments(ctx, m, t.l, t.events(), t.sh.undeliverable, t.h.c.Clock())
	if err != nil || len(evs) == 0 {
		return err
	}
	for _, p := range failed {
		if t.sh.undeliverable == nil {
			t.sh.undeliverable = map[string]bool{}
		}
		t.sh.undeliverable[p] = true
	}
	for i := range evs {
		evs[i].Turn, evs[i].Step, evs[i].Thread = t.num, t.step, t.thread
	}
	return t.commit(ctx, evs...)
}
