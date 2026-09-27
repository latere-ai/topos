// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"
)

var t0 = time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

func TestDigest(t *testing.T) {
	d := DigestOf([]byte("abc"))
	if d != "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" || !d.Valid() {
		t.Fatalf("DigestOf(abc) = %s", d)
	}
	if d.Hex() != string(d[7:]) {
		t.Fatal("Hex")
	}
	for _, bad := range []Digest{"", "sha256:", "sha256:ABC", Digest("md5:" + strings.Repeat("0", 64)), Digest("sha256:" + strings.Repeat("0", 63))} {
		if bad.Valid() || bad.Hex() != "" {
			t.Fatalf("%q is valid", bad)
		}
	}
}

func TestLockedError(t *testing.T) {
	var err error = &LockedError{Holder: Holder{Runner: "run_a", PID: 7, Host: "h", AcquiredAt: t0}}
	if !errors.Is(err, ErrLocked) {
		t.Fatal("LockedError is not ErrLocked")
	}
	if !strings.Contains(err.Error(), `runner "run_a" (pid 7 on h)`) {
		t.Fatalf("message %q", err)
	}
	if got := (&LockedError{}).Error(); !strings.Contains(got, "another holder") {
		t.Fatalf("message without a holder %q", got)
	}
}

func TestEventBlobsAndOrphans(t *testing.T) {
	a, b := DigestOf([]byte("a")), DigestOf([]byte("b"))
	e, err := NewEvent(TypeSessionMachine, SessionMachine{
		Reason:       "attached",
		Instructions: []Instructions{{Path: "AGENTS.md", Blob: a}, {Path: "x/AGENTS.md", Blob: b}, {Path: "again", Blob: a}},
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Blobs(); !slices.Equal(got, []Digest{a, b}) {
		t.Fatalf("Blobs = %v", got)
	}
	other, err := NewEvent(TypeModelRequest, ModelRequest{Outcome: "ok", RequestBlob: b}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if got := OrphanBlobs(e, []Event{e, other}); !slices.Equal(got, []Digest{a}) {
		t.Fatalf("OrphanBlobs = %v", got)
	}
	plain, err := NewEvent(TypeUserInterrupt, UserInterrupt{Sender: Sender{Subject: "u", Kind: SenderPerson}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if OrphanBlobs(plain, nil) != nil || (Event{Payload: json.RawMessage(`nope`)}).Blobs() != nil {
		t.Fatal("blobs from an event that names none")
	}
}

func TestNewEventRejectsUnencodable(t *testing.T) {
	if _, err := NewEvent(TypeUserMessage, map[string]any{"f": func() {}}, t0); err == nil {
		t.Fatal("encoded a func")
	}
	e := Event{ID: "evt_x", Type: TypeUserMessage, Payload: json.RawMessage(`[]`)}
	var p UserMessage
	if err := e.Decode(&p); err == nil {
		t.Fatal("decoded an array into an object")
	}
}

func TestMarshalKeepsHTMLAndNoNewline(t *testing.T) {
	b, err := Marshal(map[string]string{"s": "<a&b>"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"s":"<a&b>"}` {
		t.Fatalf("Marshal = %s", b)
	}
	if _, err := Marshal(func() {}); err == nil {
		t.Fatal("marshaled a func")
	}
}

func TestNewSessionDefaults(t *testing.T) {
	s := New(AgentRef{ID: NewID(PrefixAgent), Version: 1}, Sender{Subject: "u", Kind: SenderPerson}, RunnerHosted, Machine{Kind: MachineCella}, t0.Add(time.Hour).In(time.FixedZone("x", 3600)))
	if s.Schema != SchemaVersion || s.Status != StatusIdle || CheckID(PrefixSession, s.ID) != nil {
		t.Fatalf("New = %+v", s)
	}
	if s.CreatedAt.Location() != time.UTC || !s.ExpiresAt.Equal(s.CreatedAt.Add(DefaultMaxAge)) || s.Limits.TurnTimeout != "2h0m0s" {
		t.Fatalf("limits %+v, created %s, expires %s", s.Limits, s.CreatedAt, s.ExpiresAt)
	}
}

func TestCheckCreate(t *testing.T) {
	s := New(AgentRef{ID: NewID(PrefixAgent)}, Sender{Subject: "u"}, RunnerHosted, Machine{}, t0)
	for name, mut := range map[string]func(*Session){
		"schema":   func(s *Session) { s.Schema = 2 },
		"last seq": func(s *Session) { s.LastSeq = 3 },
		"metadata": func(s *Session) {
			s.Metadata = map[string]string{}
			for i := range MaxMetadata + 1 {
				s.Metadata[string(rune('a'+i))] = "v"
			}
		},
	} {
		c := s
		mut(&c)
		if err := CheckCreate(c, nil); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v, want ErrInvalid", name, err)
		}
	}
}

func TestCheckBatchValidatesEvents(t *testing.T) {
	id := NewID(PrefixSession)
	good := func() Event {
		e, err := NewEvent(TypeUserMessage, UserMessage{Content: []lux.Block{{Type: ir.BlockText, Text: "x"}}}, t0)
		if err != nil {
			t.Fatal(err)
		}
		e.SessionID, e.Seq = id, 1
		return e
	}
	none := func(uint64) ([]Event, error) { return nil, nil }
	for name, mut := range map[string]func(*[]Event){
		"bad id":      func(b *[]Event) { (*b)[0].ID = "evt_x" },
		"repeated id": func(b *[]Event) { e := (*b)[0]; e.Seq = 2; *b = append(*b, e) },
		"no type":     func(b *[]Event) { (*b)[0].Type = "" },
		"no time":     func(b *[]Event) { (*b)[0].Time = time.Time{} },
		"bad payload": func(b *[]Event) { (*b)[0].Payload = json.RawMessage(`{`) },
	} {
		batch := []Event{good()}
		mut(&batch)
		if _, err := CheckBatch(id, 0, 0, batch, none); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	failing := func(uint64) ([]Event, error) { return nil, errors.New("disk") }
	e := good()
	if _, err := CheckBatch(id, 1, 0, []Event{e}, failing); err == nil || errors.Is(err, ErrSequenceConflict) {
		t.Fatalf("a failing read of the stored log: %v", err)
	}
	if _, err := CheckBatch(id, 1, 0, []Event{e}, none); !errors.Is(err, ErrSequenceConflict) {
		t.Fatalf("a retry the log does not hold: %v", err)
	}
}

func TestSameEvent(t *testing.T) {
	e, err := NewEvent(TypeUserMessage, UserMessage{}, t0)
	if err != nil {
		t.Fatal(err)
	}
	spaced := e
	spaced.Payload = json.RawMessage(" {\"sender\" : {\"subject\":\"\",\"kind\":\"\"},\"content\":null} ")
	if !SameEvent(e, spaced) {
		t.Fatal("whitespace made events differ")
	}
	moved := e
	moved.Turn = 9
	bad := e
	bad.Payload = json.RawMessage(`{`)
	if SameEvent(e, moved) || SameEvent(e, bad) {
		t.Fatal("different events compare equal")
	}
}

func TestApplyBatchSkipsRedactedStatus(t *testing.T) {
	s := Session{Status: StatusRunning}
	e, err := NewEvent(TypeSessionStatus, SessionStatus{Status: StatusEnded, StopReason: StopCompleted}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.Seq = 4
	e.Payload = slices.Clone(tombstone)
	ApplyBatch(&s, []Event{e})
	if s.Status != StatusRunning || s.LastSeq != 4 {
		t.Fatalf("a redacted status changed the header: %+v", s)
	}
}

func TestTranscriptCheck(t *testing.T) {
	if (Transcript{}).Check() != nil {
		t.Fatal("an empty transcript is too new")
	}
	if err := (Transcript{Unknown: []string{"x"}}).Check(); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("Check = %v", err)
	}
}

func TestFoldOmittingRedactedAndUncompacted(t *testing.T) {
	msg := func(seq uint64, text string) Event {
		e, err := NewEvent(TypeUserMessage, UserMessage{Sender: Sender{Subject: "u", Kind: SenderPerson}, Content: []lux.Block{{Type: ir.BlockText, Text: text}}}, t0)
		if err != nil {
			t.Fatal(err)
		}
		e.Seq = seq
		return e
	}
	secret := msg(1, "token abc")
	secret.Payload = slices.Clone(tombstone)
	evs := []Event{secret, msg(2, "carry on")}
	if _, err := Fold(evs, ""); !errors.Is(err, ErrRedactionUncompacted) {
		t.Fatalf("Fold: %v", err)
	}
	tr, err := FoldOmittingRedacted(evs, "")
	if err != nil || len(tr.Messages) != 1 || tr.Messages[0].Blocks[0].Text != "carry on" {
		t.Fatalf("FoldOmittingRedacted %+v, %v", tr, err)
	}
	un, err := Uncompacted(evs, "")
	if err != nil || len(un) != 1 || un[0].Seq != 1 {
		t.Fatalf("Uncompacted %+v, %v", un, err)
	}
	c, err := NewEvent(TypeContextCompacted, ContextCompacted{Kind: CompactSummary, FromSeq: 1, ToSeq: 2, Summary: "s"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	c.Seq = 3
	if un, err := Uncompacted(append(evs, c), ""); err != nil || len(un) != 0 {
		t.Fatalf("a covered redaction is still uncompacted: %+v, %v", un, err)
	}
	bad, err := NewEvent(TypeContextCompacted, ContextCompacted{Kind: CompactSummary, FromSeq: 1, ToSeq: 9}, t0)
	if err != nil {
		t.Fatal(err)
	}
	bad.Seq = 3
	if _, err := Uncompacted(append(evs, bad), ""); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a malformed compaction: %v", err)
	}
}
