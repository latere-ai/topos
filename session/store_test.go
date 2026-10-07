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

// TestTheHeaderTakesAModelChangeWhole: the header's model is the new
// model of the last session.model_changed with the name that was asked
// beside the one that runs, a header that named a model at its create
// keeps it until a change, and both names are on the wire.
func TestTheHeaderTakesAModelChangeWhole(t *testing.T) {
	s := Session{Model: &ModelRef{Name: "vendor/model-a", Via: "tier/quick"}}
	ApplyBatch(&s, nil)
	if s.Model == nil || *s.Model != (ModelRef{Name: "vendor/model-a", Via: "tier/quick"}) {
		t.Fatalf("the model a create named is %+v", s.Model)
	}
	by := Sender{Subject: AuthorizerSubject, Kind: SenderService}
	e, err := NewEvent(TypeModelChanged, ModelChanged{By: by, Old: *s.Model, New: ModelRef{Name: "vendor/model-b", Via: "tier/quick", Effort: "high"}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.Seq = 1
	if want := `{"by":{"subject":"service:authorizer","kind":"service"},"old":{"name":"vendor/model-a","via":"tier/quick"},"new":{"name":"vendor/model-b","via":"tier/quick","effort":"high"}}`; string(e.Payload) != want {
		t.Fatalf("session.model_changed is %s", e.Payload)
	}
	ApplyBatch(&s, []Event{e})
	if s.Model == nil || *s.Model != (ModelRef{Name: "vendor/model-b", Via: "tier/quick", Effort: "high"}) {
		t.Fatalf("the header's model is %+v", s.Model)
	}
	plain, err := NewEvent(TypeModelChanged, ModelChanged{By: by, Old: *s.Model, New: ModelRef{Name: "vendor/model-c"}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	plain.Seq = 2
	ApplyBatch(&s, []Event{plain})
	if b, err := Marshal(s.Model); err != nil || string(b) != `{"name":"vendor/model-c"}` {
		t.Fatalf("a model that runs the name asked is %s, %v", b, err)
	}
}

// TestThePolicyFollowsItsChanges: the header's policy takes the mode of
// the last session.policy_changed and keeps its lists and thresholds,
// Mode reads the same mode from the log, a fork's copied change moves
// neither, and a session that records no policy keeps none while Mode
// still reads the change for the harness (spec 041).
func TestThePolicyFollowsItsChanges(t *testing.T) {
	by := Sender{Subject: "https://login.example|alice", Kind: SenderPerson}
	change := func(seq uint64, from, to string) Event {
		t.Helper()
		e, err := NewEvent(TypePolicyChanged, PolicyChanged{By: by, Old: PolicyRef{Mode: from}, New: PolicyRef{Mode: to}}, t0)
		if err != nil {
			t.Fatal(err)
		}
		e.Seq = seq
		return e
	}
	first := change(3, "confirm", "progressive")
	if want := `{"by":{"subject":"https://login.example|alice","kind":"person"},"old":{"mode":"confirm"},"new":{"mode":"progressive"}}`; string(first.Payload) != want {
		t.Fatalf("session.policy_changed is %s", first.Payload)
	}
	policy := Policy{Mode: "confirm", AlwaysConfirm: []string{"bash(git push*)"}, Thresholds: Thresholds{FlagAt: 0.3, AskAt: 0.5, BlockAt: 0.9}}
	s := Session{Policy: &policy}
	log := []Event{first, change(5, "progressive", "plan")}
	ApplyBatch(&s, log)
	if s.Policy.Mode != "plan" || !slices.Equal(s.Policy.AlwaysConfirm, policy.AlwaysConfirm) || s.Policy.Thresholds != policy.Thresholds {
		t.Fatalf("the header's policy is %+v", s.Policy)
	}
	if policy.Mode != "confirm" {
		t.Fatal("applying a change wrote through to the policy the header was built with")
	}
	if mode, ok := Mode(s, log); !ok || mode != "plan" {
		t.Fatalf("Mode = %q, %v", mode, ok)
	}
	if _, ok := Mode(s, nil); ok {
		t.Fatal("a log with no change names a mode")
	}

	fork := Session{Policy: &Policy{Mode: "confirm"}, Parent: &Parent{SessionID: "ses_parent", Seq: 5}}
	ApplyBatch(&fork, log)
	if fork.Policy.Mode != "confirm" {
		t.Fatalf("a fork's copied change moved its mode to %s", fork.Policy.Mode)
	}
	if _, ok := Mode(fork, log); ok {
		t.Fatal("Mode read a fork's copied change")
	}
	own := change(7, "confirm", "progressive")
	ApplyBatch(&fork, []Event{own})
	if mode, ok := Mode(fork, append(log, own)); fork.Policy.Mode != "progressive" || !ok || mode != "progressive" {
		t.Fatalf("a fork's own change: header %s, Mode %q %v", fork.Policy.Mode, mode, ok)
	}

	local := Session{}
	ApplyBatch(&local, log)
	if local.Policy != nil {
		t.Fatalf("a session that records no policy holds %+v", local.Policy)
	}
	if mode, ok := Mode(local, log); !ok || mode != "plan" {
		t.Fatalf("Mode of a session that records no policy = %q, %v", mode, ok)
	}
	if Redactable(TypePolicyChanged) {
		t.Fatal("session.policy_changed is redactable")
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

func TestHasPendingInput(t *testing.T) {
	ev := func(typ Type) Event { return Event{Type: typ} }
	for name, c := range map[string]struct {
		evs  []Event
		want bool
	}{
		"a new session":             {nil, false},
		"a first message":           {[]Event{ev(TypeUserMessage)}, true},
		"answered":                  {[]Event{ev(TypeUserMessage), ev(TypeSessionStatus), ev(TypeAgentMessage), ev(TypeSessionStatus)}, false},
		"a message after the turn":  {[]Event{ev(TypeUserMessage), ev(TypeSessionStatus), ev(TypeUserMessage)}, true},
		"a confirmation":            {[]Event{ev(TypeSessionStatus), ev(TypeUserToolConfirmation)}, true},
		"a client result":           {[]Event{ev(TypeSessionStatus), ev(TypeUserToolResult)}, true},
		"an interrupt resumes none": {[]Event{ev(TypeSessionStatus), ev(TypeUserInterrupt)}, false},
		"an answer":                 {[]Event{ev(TypeSessionStatus), ev(TypeUserAnswer)}, true},
	} {
		if got := HasPendingInput(c.evs); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

// TestAMessageClosesAnAsk: a person's message appended after a call that
// waits for a confirmation denies it (spec 012), so the call awaits no
// confirmation after it; a message before the call, a trigger's message
// and a client's call are left as they were.
func TestAMessageClosesAnAsk(t *testing.T) {
	ev := func(typ Type, p any) Event {
		e, err := NewEvent(typ, p, t0)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	person := Sender{Subject: "usr_ada", Kind: SenderPerson}
	trigger := Sender{Subject: TriggerSubjectPrefix + "trg_1", Kind: SenderTrigger}
	log := []Event{
		ev(TypeUserMessage, UserMessage{Sender: person}),
		ev(TypeAgentToolUse, AgentToolUse{ToolUseID: "ask", Verdict: "ask"}),
		ev(TypeAgentToolUse, AgentToolUse{ToolUseID: "client", Client: true}),
		ev(TypeUserMessage, UserMessage{Sender: trigger}),
	}
	if got := Awaiting(log); len(got) != 2 || got["ask"] != AnswerConfirmation || got["client"] != AnswerResult {
		t.Fatalf("before a person's message: %v", got)
	}
	log = append(log, ev(TypeUserMessage, UserMessage{Sender: person}), ev(TypeAgentToolUse, AgentToolUse{ToolUseID: "later", Verdict: "ask"}))
	if got := Awaiting(log); len(got) != 2 || got["later"] != AnswerConfirmation || got["client"] != AnswerResult {
		t.Fatalf("after a person's message: %v", got)
	}
}

// TestARedactedResultKeepsItsCall: the tombstone of a tool.result and of
// a user.tool_result names the call it answered and nothing of what it
// held, any other event's tombstone holds the mark alone, and a call
// whose result was redacted awaits no answer.
func TestARedactedResultKeepsItsCall(t *testing.T) {
	ev := func(typ Type, p any) Event {
		e, err := NewEvent(typ, p, t0)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	by := Sender{Subject: "usr_ada", Kind: SenderPerson}
	for typ, payload := range map[Type]any{
		TypeToolResult:     ToolResult{ToolUseID: "toolu_1", Content: []lux.Block{{Type: ir.BlockText, Text: "a secret"}}},
		TypeUserToolResult: UserToolResult{ToolUseID: "toolu_1", Content: []lux.Block{{Type: ir.BlockText, Text: "a secret"}}},
	} {
		tomb, red, err := Tombstone(ev(typ, payload), 7, by, "it held a secret", t0)
		if err != nil {
			t.Fatal(err)
		}
		if !tomb.Redacted() || tomb.Answers() != "toolu_1" || string(tomb.Payload) != `{"tombstone":true,"tool_use_id":"toolu_1"}` || red.Seq != 8 {
			t.Fatalf("%s: tombstone %s, redaction at %d", typ, tomb.Payload, red.Seq)
		}
	}
	tomb, _, err := Tombstone(ev(TypeUserMessage, UserMessage{Sender: by}), 7, by, "", t0)
	if err != nil {
		t.Fatal(err)
	}
	if string(tomb.Payload) != string(tombstone) || tomb.Answers() != "" {
		t.Fatalf("a message's tombstone: %s", tomb.Payload)
	}
	old := ev(TypeToolResult, ToolResult{ToolUseID: "toolu_1"})
	old.Payload = slices.Clone(tombstone)
	if old.Answers() != "" || (Event{Type: TypeToolResult, Payload: json.RawMessage(`[`)}).Answers() != "" {
		t.Fatal("a tombstone without an id, or a payload that does not decode, names a call")
	}
	ask := ev(TypeAgentToolUse, AgentToolUse{ToolUseID: "toolu_1", Verdict: "ask"})
	result, _, err := Tombstone(ev(TypeToolResult, ToolResult{ToolUseID: "toolu_1"}), 2, by, "", t0)
	if err != nil {
		t.Fatal(err)
	}
	if got := Awaiting([]Event{ask, result}); len(got) != 0 {
		t.Fatalf("a call whose result was redacted awaits %v", got)
	}
}

func TestAwaitingAndRedactable(t *testing.T) {
	ev := func(typ Type, p any) Event {
		e, err := NewEvent(typ, p, t0)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	log := []Event{
		ev(TypeAgentToolUse, AgentToolUse{ToolUseID: "ask", Verdict: "ask"}),
		ev(TypeAgentToolUse, AgentToolUse{ToolUseID: "client", Client: true}),
		ev(TypeAgentToolUse, AgentToolUse{ToolUseID: "ran", Verdict: "allow"}),
		ev(TypeAgentToolUse, AgentToolUse{ToolUseID: "confirmed", Verdict: "ask"}),
		ev(TypeUserToolConfirmation, UserToolConfirmation{ToolUseID: "confirmed", Decision: DecisionAllow}),
		ev(TypeAgentToolUse, AgentToolUse{ToolUseID: "resulted", Client: true}),
		ev(TypeUserToolResult, UserToolResult{ToolUseID: "resulted"}),
		ev(TypeAgentToolUse, AgentToolUse{ToolUseID: "closed", Verdict: "ask"}),
		ev(TypeToolResult, ToolResult{ToolUseID: "closed"}),
		{Type: TypeAgentToolUse, Payload: json.RawMessage(`[`)},
		{Type: TypeUserToolConfirmation, Payload: json.RawMessage(`[`)},
		{Type: TypeUserToolResult, Payload: json.RawMessage(`[`)},
		{Type: TypeToolResult, Payload: json.RawMessage(`[`)},
	}
	redacted := ev(TypeAgentToolUse, AgentToolUse{ToolUseID: "gone", Verdict: "ask"})
	redacted.Payload = slices.Clone(tombstone)
	log = append(log, redacted)
	got := Awaiting(log)
	if len(got) != 2 || got["ask"] != AnswerConfirmation || got["client"] != AnswerResult {
		t.Fatalf("Awaiting = %v", got)
	}
	for typ, want := range map[Type]bool{TypeUserMessage: true, TypeUserAnswer: true, TypeToolResult: true, TypeAgentToolUse: true, TypeModelRequest: false, TypeSessionStatus: false, TypeUserToolConfirmation: false, TypeScopeChanged: false, TypeFilesKept: true} {
		if Redactable(typ) != want {
			t.Errorf("Redactable(%s) = %v", typ, !want)
		}
	}
}

// TestApplyBatchFoldsATitleChange: the header takes the new title of the
// last session.title_changed and keeps its update time; a fork's copied
// change and a redacted one move nothing, and the event is not
// redactable (spec 054).
func TestApplyBatchFoldsATitleChange(t *testing.T) {
	by := Sender{Subject: "https://login.example|alice", Kind: SenderPerson}
	rename := func(seq uint64, from, to string, at time.Time) Event {
		t.Helper()
		e, err := NewEvent(TypeTitleChanged, TitleChanged{By: by, Old: from, New: to}, at)
		if err != nil {
			t.Fatal(err)
		}
		e.Seq = seq
		return e
	}
	e := rename(3, "Notes", "Release notes", t0.Add(time.Hour))
	if want := `{"by":{"subject":"https://login.example|alice","kind":"person"},"old":"Notes","new":"Release notes"}`; string(e.Payload) != want {
		t.Fatalf("session.title_changed is %s", e.Payload)
	}
	s := Session{Title: "Notes", UpdatedAt: t0, LastSeq: 2}
	ApplyBatch(&s, []Event{e})
	if s.Title != "Release notes" || s.LastSeq != 3 || !s.UpdatedAt.Equal(t0) {
		t.Fatalf("the header after a rename is %q at %d, updated %s", s.Title, s.LastSeq, s.UpdatedAt)
	}
	redacted := rename(4, "Release notes", "Gone", t0)
	redacted.Payload = slices.Clone(tombstone)
	ApplyBatch(&s, []Event{redacted})
	if s.Title != "Release notes" {
		t.Fatalf("a redacted rename moved the title to %q", s.Title)
	}
	fork := Session{Title: "Release notes (continued)", Parent: &Parent{SessionID: "ses_parent", Seq: 3}}
	ApplyBatch(&fork, []Event{rename(3, "Notes", "Release notes", t0)})
	if fork.Title != "Release notes (continued)" {
		t.Fatalf("a fork's copied rename moved its title to %q", fork.Title)
	}
	ApplyBatch(&fork, []Event{rename(4, "Release notes (continued)", "Mine", t0)})
	if fork.Title != "Mine" {
		t.Fatalf("a fork's own rename left its title %q", fork.Title)
	}
	if Redactable(TypeTitleChanged) || !Known[TypeTitleChanged] {
		t.Fatal("session.title_changed is redactable or unknown")
	}
}

// TestCheckTitle: a title is refused empty, past MaxTitleLength
// characters, or with a control character or a line separator, and
// counted in characters, not bytes.
func TestCheckTitle(t *testing.T) {
	for _, ok := range []string{"Notes", strings.Repeat("é", MaxTitleLength), "发布说明", "a b"} {
		if why := CheckTitle(ok); why != "" {
			t.Errorf("CheckTitle(%q) = %q", ok, why)
		}
	}
	for _, bad := range []string{"", strings.Repeat("a", MaxTitleLength+1), "a\nb", "a\tb", "a\u0000b", "a b", "a b", "a\u007fb"} {
		if CheckTitle(bad) == "" {
			t.Errorf("CheckTitle(%q) passed", bad)
		}
	}
}
