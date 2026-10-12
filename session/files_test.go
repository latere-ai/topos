// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"errors"
	"testing"
)

// TestARefIsAPlaceInTheFolder: a ref is a clean path under attachments/
// or results/, a directory and a name below it, and its directory is the
// first two segments a fork copies (spec 068).
func TestARefIsAPlaceInTheFolder(t *testing.T) {
	for _, ok := range []string{"attachments/evt_1/notes.txt", "results/3/out/report.pdf", KeptRef(3, "out/report.pdf")} {
		if err := CheckRef(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "notes.txt", "attachments/notes.txt", "other/1/a", "results/3/../a", "results/3//a", "results/3/./a", "/results/3/a", "results/3/a/", "results/3/a\x00b", "results/./3/a"} {
		if err := CheckRef(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if KeptRef(3, "out/report.pdf") != "results/3/out/report.pdf" {
		t.Errorf("KeptRef %q", KeptRef(3, "out/report.pdf"))
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
	none := Session{}
	ApplyBatch(&none, []Event{msg})
	if none.Folder != nil {
		t.Fatal("a session with no folder gained one")
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
