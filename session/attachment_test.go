// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/lux"
)

// TestAttachmentPath: a file goes under attachments/ in its message's
// own directory, so two messages' files of one name have two paths.
func TestAttachmentPath(t *testing.T) {
	if a, b := AttachmentPath("evt_1", "sales.csv"), AttachmentPath("evt_2", "sales.csv"); a != "attachments/evt_1/sales.csv" || b != "attachments/evt_2/sales.csv" {
		t.Fatalf("paths %q and %q", a, b)
	}
}

// TestPendingAttachments: a file is pending until an
// attachments.delivered after the latest session.machine names it, a
// redacted message's files are gone, and a path is listed once.
func TestPendingAttachments(t *testing.T) {
	at := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	ev := func(typ Type, p any) Event {
		e, err := NewEvent(typ, p, at)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	a, b := Attachment{Name: "a", Path: "attachments/a"}, Attachment{Name: "b", Path: "attachments/b"}
	redacted := ev(TypeUserMessage, UserMessage{Content: []lux.Block{}, Attachments: []Attachment{{Name: "c", Path: "attachments/c"}}})
	redacted.Payload = []byte(`{"tombstone":true}`)
	log := []Event{
		ev(TypeUserMessage, UserMessage{Content: []lux.Block{}, Attachments: []Attachment{a, b, a}}),
		redacted,
		ev(TypeSessionMachine, SessionMachine{Reason: "attached"}),
		ev(TypeAttachmentsDelivered, AttachmentsDelivered{Paths: []string{"attachments/a"}}),
	}
	if got := PendingAttachments(log); len(got) != 1 || got[0] != b {
		t.Fatalf("pending %+v, want b", got)
	}
	log = append(log, ev(TypeSessionMachine, SessionMachine{Reason: "handoff"}))
	if got := PendingAttachments(log); len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("pending after a handoff %+v, want a and b", got)
	}
	if got := Attachments(log); len(got) != 3 {
		t.Fatalf("attachments %+v", got)
	}
}

// TestWhatAModelMayReadInline: a file is inlinable when it is text by
// its media type or its name, UTF-8 without a NUL, and at most
// InlineTextBytes; the record carries its source and its mark only when
// set (spec 079).
func TestWhatAModelMayReadInline(t *testing.T) {
	text := []byte("month,total\n")
	for _, c := range []struct {
		name, media string
		data        []byte
		want        bool
	}{
		{"sales.csv", "text/csv", text, true},
		{"notes", "text/plain; charset=utf-8", text, true},
		{"data", "application/json", text, true},
		{"feed", "application/xml", text, true},
		{"cfg", "application/yaml", text, true},
		{"cfg", "application/x-yaml", text, true},
		{"README.MD", "application/octet-stream", text, true},
		{"rows.tsv", "", text, true},
		{"page.yml", "", text, true},
		{"bundle", "application/octet-stream", text, false},
		{"photo.png", "image/png", text, false},
		{"a.txt", "text/plain", []byte("a\x00b"), false},
		{"a.txt", "text/plain", []byte("caf\xe9"), false},
		{"a.txt", "text/plain", bytes.Repeat([]byte("x"), InlineTextBytes), true},
		{"a.txt", "text/plain", bytes.Repeat([]byte("x"), InlineTextBytes+1), false},
		{"a.bin", "not a media type", text, false},
	} {
		if got := Inlinable(c.name, c.media, c.data); got != c.want {
			t.Errorf("%s %q %d bytes: %t, want %t", c.name, c.media, len(c.data), got, c.want)
		}
	}
	plain, err := json.Marshal(Attachment{Name: "a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "source") || strings.Contains(string(plain), "inline") {
		t.Fatalf("a file of no source and no mark is %s", plain)
	}
	read, err := json.Marshal(Attachment{Name: "a.txt", Source: AttachmentSource{Owner: "me", Path: "files/a.txt", Version: 2}, Inline: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(read), `"source":{"owner":"me","path":"files/a.txt","version":2},"inline":true`) {
		t.Fatalf("a read file is %s", read)
	}
}
