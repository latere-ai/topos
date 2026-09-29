// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/lux"
)

// TestAttachmentPath: a file goes under attachments/ by its name, and a
// name the session holds takes -2, -3 and on before its extension.
func TestAttachmentPath(t *testing.T) {
	taken := map[string]bool{}
	for _, c := range []struct{ name, want string }{
		{"sales.csv", "attachments/sales.csv"},
		{"sales.csv", "attachments/sales-2.csv"},
		{"sales.csv", "attachments/sales-3.csv"},
		{"archive.tar.gz", "attachments/archive.tar.gz"},
		{"archive.tar.gz", "attachments/archive.tar-2.gz"},
		{".env", "attachments/.env"},
		{".env", "attachments/.env-2"},
		{"README", "attachments/README"},
		{"README", "attachments/README-2"},
	} {
		if got := AttachmentPath(c.name, taken); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
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
