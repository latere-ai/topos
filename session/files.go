// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// ResultsDir is the directory of a session's files store folder that the
// files an answer shows or links are written under, in a directory named
// after the answer's turn (spec 068). A person's attachments are under
// AttachmentDir, in a directory named after their message's event id.
const ResultsDir = "results"

// WorkDir is the directory of a session's files store folder that holds
// the copy of its working directory (spec 073): each file at its path
// inside the working directory, the latest state alone.
const WorkDir = "work"

// KeptRef is the ref of a file an answer of turn showed or linked, at
// rel, its path inside the working directory (spec 068).
func KeptRef(turn int, rel string) string {
	return path.Join(ResultsDir, strconv.Itoa(turn), rel)
}

// WorkRef is the ref of the copy of the working directory's file at rel,
// its path inside the working directory (spec 073).
func WorkRef(rel string) string { return WorkDir + "/" + rel }

// CheckRef reports why ref is not a file's place in a session's folder:
// it is a clean relative path under AttachmentDir or ResultsDir, a
// directory of its own below that and a name, or under WorkDir with a
// name, with no empty, "." or ".." segment and no control character.
func CheckRef(ref string) error {
	segs := strings.Split(ref, "/")
	switch {
	case len(segs) >= 2 && segs[0] == WorkDir:
	case len(segs) < 3 || (segs[0] != AttachmentDir && segs[0] != ResultsDir):
		return fmt.Errorf("%w: the ref %q is not under %s/ or %s/ with a directory and a name, nor under %s/ with a name", ErrInvalid, ref, AttachmentDir, ResultsDir, WorkDir)
	}
	if path.Clean(ref) != ref {
		return fmt.Errorf("%w: the ref %q is not a clean path", ErrInvalid, ref)
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return fmt.Errorf("%w: the ref %q holds the segment %q", ErrInvalid, ref, s)
		}
	}
	if strings.ContainsFunc(ref, unicode.IsControl) {
		return fmt.Errorf("%w: the ref %q holds a control character", ErrInvalid, ref)
	}
	return nil
}

// RefDir is the directory of a ref a fork copies whole: its first two
// segments, as attachments/<event id> and results/<turn>.
func RefDir(ref string) string {
	segs := strings.SplitN(ref, "/", 3)
	if len(segs) < 3 {
		return ""
	}
	return segs[0] + "/" + segs[1]
}

// FileRef is one file an event records in its session's folder: its
// digest and its ref.
type FileRef struct {
	Digest Digest
	Ref    string
}

// Files are the files e records in its session's folder (spec 068): the
// attachments of a user.message and the kept files of a files.kept that
// carry a ref, in order. A redacted event, and an event of any other
// type, records none.
func (e Event) Files() []FileRef {
	if e.Redacted() {
		return nil
	}
	var out []FileRef
	switch e.Type {
	case TypeUserMessage:
		var m UserMessage
		if e.Decode(&m) != nil {
			return nil
		}
		for _, a := range m.Attachments {
			if a.Ref != "" {
				out = append(out, FileRef{Digest: a.Blob, Ref: a.Ref})
			}
		}
	case TypeFilesKept:
		var k FilesKept
		if e.Decode(&k) != nil {
			return nil
		}
		for _, f := range k.Files {
			if f.Ref != "" {
				out = append(out, FileRef{Digest: f.Blob, Ref: f.Ref})
			}
		}
	}
	return out
}

// FileIndex is the optional interface of a store that keeps the digest
// index of spec 068, so a file of a session's folder is read by its
// digest. FileRef answers the ref of a file of the session id whose
// digest is d that a live event records, and ErrNotFound when none does.
type FileIndex interface {
	FileRef(ctx context.Context, id string, d Digest) (string, error)
}

// LiveRefs are the refs the live events of log record files at.
func LiveRefs(log []Event) map[string]bool {
	out := map[string]bool{}
	for _, e := range log {
		for _, f := range e.Files() {
			out[f.Ref] = true
		}
	}
	return out
}

// GoneRefs are the refs before records a file at that after no longer
// does, sorted: the files a redaction leaves no live event naming, which
// the session's folder then deletes (spec 073). Two answers of one turn
// that link one file share its ref, and so do a fork's copied message and
// the fork's own message that attaches the same file by its digest; a
// ref goes only once no live event names it.
func GoneRefs(before, after []Event) []string {
	live := LiveRefs(after)
	var out []string
	for ref := range LiveRefs(before) {
		if !live[ref] {
			out = append(out, ref)
		}
	}
	slices.Sort(out)
	return out
}

// FindFile is FileRef read from a log: the ref of the first live event
// that records a file whose digest is d, false when none does.
func FindFile(log []Event, d Digest) (string, bool) {
	for _, e := range log {
		for _, f := range e.Files() {
			if f.Digest == d {
				return f.Ref, true
			}
		}
	}
	return "", false
}

// FileRefOf answers FileRef from st: its own index where it keeps one,
// and otherwise its log read whole.
func FileRefOf(ctx context.Context, st Store, id string, d Digest) (string, error) {
	if ix, ok := st.(FileIndex); ok {
		return ix.FileRef(ctx, id, d)
	}
	log, err := st.Events(ctx, id, 1, 0)
	if err != nil {
		return "", err
	}
	if ref, ok := FindFile(log, d); ok {
		return ref, nil
	}
	return "", fmt.Errorf("%w: no file of %s has the digest %s", ErrNotFound, id, d)
}

// stored marks s's folder as holding a file when e records one there: a
// message's attachment or a kept file, or a file of the working
// directory's copy a work.saved wrote under work/ (specs 068 and 073).
func (s *Session) stored(e Event) {
	if s.Folder == nil || s.Folder.Stored || (len(e.Files()) == 0 && !wroteWork(e)) {
		return
	}
	f := *s.Folder
	f.Stored = true
	s.Folder = &f
}

// wroteWork reports whether e is a work.saved that wrote a file of the
// working directory's copy into the folder.
func wroteWork(e Event) bool {
	var w WorkSaved
	return e.Type == TypeWorkSaved && !e.Redacted() && e.Decode(&w) == nil && len(w.Written) > 0
}
