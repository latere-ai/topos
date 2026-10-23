// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"errors"
	"strings"
	"testing"
)

// TestARefIsAPlaceInTheFolder: a ref is a clean path under attachments/
// or results/, a directory and a name below it, or under work/ with a
// name, and its directory is the first two segments a fork copies (specs
// 068 and 073).
func TestARefIsAPlaceInTheFolder(t *testing.T) {
	for _, ok := range []string{"attachments/evt_1/notes.txt", "results/3/out/report.pdf", KeptRef(3, "out/report.pdf"), "work/package.json", WorkRef("src/app.js")} {
		if err := CheckRef(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "notes.txt", "attachments/notes.txt", "other/1/a", "results/3/../a", "results/3//a", "results/3/./a", "/results/3/a", "results/3/a/", "results/3/a\x00b", "results/./3/a", "work", "work/", "work/../a", "work/a//b", "work/a\x00b"} {
		if err := CheckRef(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if KeptRef(3, "out/report.pdf") != "results/3/out/report.pdf" || WorkRef("src/app.js") != "work/src/app.js" {
		t.Errorf("KeptRef %q, WorkRef %q", KeptRef(3, "out/report.pdf"), WorkRef("src/app.js"))
	}
	for ref, want := range map[string]string{"attachments/evt_1/notes.txt": "attachments/evt_1", "results/3/out/report.pdf": "results/3", "results/3": ""} {
		if got := RefDir(ref); got != want {
			t.Errorf("RefDir(%q) = %q, want %q", ref, got, want)
		}
	}
}

// TestAnEventRecordsTheFilesItsEntriesPlace: the files an event records
// are its entries with a ref, a user.message's and a files.kept's alone,
// and a redacted event records none.
func TestAnEventRecordsTheFilesItsEntriesPlace(t *testing.T) {
	d, other := DigestOf([]byte("a")), DigestOf([]byte("b"))
	msg := mustEvent(t, TypeUserMessage, UserMessage{Attachments: []Attachment{{Name: "a", Blob: d, Path: "attachments/e/a", Ref: "attachments/e/a"}, {Name: "b", Blob: other, Path: "attachments/e/b"}}})
	kept := mustEvent(t, TypeFilesKept, FilesKept{Message: "evt", Files: []KeptFile{{Blob: other, Ref: "results/1/b"}, {Blob: d}}})
	if got := msg.Files(); len(got) != 1 || got[0] != (FileRef{Digest: d, Ref: "attachments/e/a"}) {
		t.Errorf("the message's files %v", got)
	}
	if got := kept.Files(); len(got) != 1 || got[0] != (FileRef{Digest: other, Ref: "results/1/b"}) {
		t.Errorf("the files.kept's files %v", got)
	}
	status := mustEvent(t, TypeSessionStatus, SessionStatus{Status: StatusIdle})
	if got := status.Files(); got != nil {
		t.Errorf("a status records %v", got)
	}
	red := kept
	red.Payload = []byte(`{"tombstone":true}`)
	if got := red.Files(); got != nil {
		t.Errorf("a tombstone records %v", got)
	}
	if ref, ok := FindFile([]Event{status, msg, kept}, other); !ok || ref != "results/1/b" {
		t.Errorf("FindFile %q %t", ref, ok)
	}
	if _, ok := FindFile([]Event{status}, d); ok {
		t.Error("FindFile found a file in a log that records none")
	}
	s := Session{Folder: &Folder{Owner: "o", Path: "files/p"}}
	shared := s.Folder
	ApplyBatch(&s, []Event{status})
	if s.Folder.Stored {
		t.Fatal("a status marked the folder stored")
	}
	ApplyBatch(&s, []Event{msg})
	if !s.Folder.Stored || shared.Stored {
		t.Fatalf("after a stored file: stored %t, the folder it was copied from %t", s.Folder.Stored, shared.Stored)
	}
	// A save of the working directory's copy stores files under work/,
	// and one that wrote none, such as a turn that changed nothing, does
	// not.
	saved := mustEvent(t, TypeWorkSaved, WorkSaved{Covers: 3, Written: []WorkFile{{Path: "app.py", Size: 1, Mode: 0o644, SHA256: "ab"}}})
	empty := mustEvent(t, TypeWorkSaved, WorkSaved{Covers: 2})
	work := Session{Folder: &Folder{Owner: "o", Path: "files/p"}}
	ApplyBatch(&work, []Event{empty})
	if work.Folder.Stored {
		t.Fatal("a save that wrote nothing marked the folder stored")
	}
	ApplyBatch(&work, []Event{saved})
	if !work.Folder.Stored {
		t.Fatal("a save that wrote the working directory's files left the folder unstored")
	}
	none := Session{}
	ApplyBatch(&none, []Event{msg})
	if none.Folder != nil {
		t.Fatal("a session with no folder gained one")
	}
}

// TestARefGoesOnceNoLiveEventNamesIt: the refs a redaction leaves
// unnamed are the ones the log recorded before it and no live event
// records after it; a ref a second event still names stays (spec 073).
func TestARefGoesOnceNoLiveEventNamesIt(t *testing.T) {
	d := DigestOf([]byte("a"))
	msg := mustEvent(t, TypeUserMessage, UserMessage{Attachments: []Attachment{{Name: "a", Blob: d, Path: "attachments/e/a", Ref: "attachments/e/a"}}})
	first := mustEvent(t, TypeFilesKept, FilesKept{Message: "evt_1", Files: []KeptFile{{Blob: d, Ref: "results/1/a"}}})
	second := mustEvent(t, TypeFilesKept, FilesKept{Message: "evt_2", Files: []KeptFile{{Blob: d, Ref: "results/1/a"}}})
	tomb := func(e Event) Event {
		e.Payload = []byte(`{"tombstone":true}`)
		return e
	}
	before := []Event{msg, first, second}
	if got := GoneRefs(before, []Event{msg, tomb(first), second}); len(got) != 0 {
		t.Errorf("a ref a second answer names is gone: %v", got)
	}
	if got := GoneRefs(before, []Event{tomb(msg), tomb(first), tomb(second)}); len(got) != 2 || got[0] != "attachments/e/a" || got[1] != "results/1/a" {
		t.Errorf("the refs nothing names: %v", got)
	}
	if got := LiveRefs([]Event{msg, tomb(first)}); len(got) != 1 || !got["attachments/e/a"] {
		t.Errorf("the live refs %v", got)
	}
}

func mustEvent(t *testing.T, typ Type, payload any) Event {
	t.Helper()
	e, err := NewEvent(typ, payload, t0)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// TestAFileIsFoundByItsFirstLiveRecord: a digest an attachment or a kept
// file names is found with the media type and size of the first live
// event that records it, whether or not a folder holds it; a redacted
// event and an event of another type record nothing.
func TestAFileIsFoundByItsFirstLiveRecord(t *testing.T) {
	d, other := DigestOf([]byte("a")), DigestOf([]byte("bb"))
	msg := mustEvent(t, TypeUserMessage, UserMessage{Attachments: []Attachment{{Name: "a", MediaType: "text/plain", Size: 1, Blob: d, Path: "attachments/e/a"}}})
	kept := mustEvent(t, TypeFilesKept, FilesKept{Message: "evt", Files: []KeptFile{{Blob: d, MediaType: "text/markdown", Size: 1}, {Blob: other, MediaType: "application/pdf", Size: 2, Ref: "results/1/b"}}})
	status := mustEvent(t, TypeSessionStatus, SessionStatus{Status: StatusIdle})
	if r, ok := FindRecord([]Event{status, msg, kept}, d); !ok || r != (FileRecord{MediaType: "text/plain", Size: 1}) {
		t.Errorf("the first record of a: %+v %t", r, ok)
	}
	if r, ok := FindRecord([]Event{status, msg, kept}, other); !ok || r != (FileRecord{MediaType: "application/pdf", Size: 2}) {
		t.Errorf("a file of the folder: %+v %t", r, ok)
	}
	red := msg
	red.Payload = []byte(`{"tombstone":true}`)
	if r, ok := FindRecord([]Event{red, kept}, d); !ok || r.MediaType != "text/markdown" {
		t.Errorf("past a redacted record: %+v %t", r, ok)
	}
	if _, ok := FindRecord([]Event{status, red}, d); ok {
		t.Error("a log of no live record found one")
	}
}

// TestABlobBodyStatesItsLength: a blob's body reads its bytes, writes the
// rest of them, and states their whole length however much was read.
func TestABlobBodyStatesItsLength(t *testing.T) {
	b := NewBlobBody([]byte("hello"))
	p := make([]byte, 2)
	if n, err := b.Read(p); err != nil || n != 2 || string(p) != "he" {
		t.Fatalf("read %d %q %v", n, p, err)
	}
	var rest strings.Builder
	if n, err := b.WriteTo(&rest); err != nil || n != 3 || rest.String() != "llo" {
		t.Fatalf("wrote %d %q %v", n, rest.String(), err)
	}
	if b.Size() != 5 {
		t.Fatalf("size %d", b.Size())
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
}
