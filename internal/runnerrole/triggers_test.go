// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runnerrole

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"latere.ai/x/topos/internal/runnerapi"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/memory"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// recordedTriggers is a server's reach that keeps who asked what and
// refuses as refuse says.
type recordedTriggers struct {
	id     string
	asked  *[]string
	refuse error
}

func (r recordedTriggers) ListTriggers(context.Context) ([]runner.ListedTrigger, error) {
	*r.asked = append(*r.asked, r.id+" list")
	return []runner.ListedTrigger{{ID: "trg_1", Name: "routine-a", Origin: &v1.TriggerOrigin{Kind: v1.OriginProposal, SessionID: r.id}}}, nil
}

func (r recordedTriggers) CheckProposal(_ context.Context, p runner.Proposal) error {
	*r.asked = append(*r.asked, r.id+" check "+p.ToolUseID)
	return r.refuse
}

func (r recordedTriggers) ApplyProposal(_ context.Context, p runner.Proposal) (runner.ListedTrigger, error) {
	*r.asked = append(*r.asked, r.id+" apply "+p.ToolUseID)
	if r.refuse != nil {
		return runner.ListedTrigger{}, r.refuse
	}
	return runner.ListedTrigger{ID: "trg_2", Name: "routine-b", Title: p.Name}, nil
}

func (r recordedTriggers) SetWake(_ context.Context, w runner.Wake) (runner.ListedTrigger, error) {
	*r.asked = append(*r.asked, r.id+" wake "+w.ToolUseID)
	if r.refuse != nil {
		return runner.ListedTrigger{}, r.refuse
	}
	return runner.ListedTrigger{ID: "trg_3", Name: "wake-x", Title: w.Title, At: &w.At}, nil
}

func (r recordedTriggers) CancelWake(_ context.Context, toolUseID string) (string, error) {
	*r.asked = append(*r.asked, r.id+" unwake "+toolUseID)
	if r.refuse != nil {
		return "", r.refuse
	}
	return "trg_3", nil
}

// TestARemoteLeaseAsksTheTriggerQuestions: a remote runner's lease is the
// session's reach to its server's triggers (spec 086): a list, a check
// and an apply reach the server's own for the session the lease holds, a
// refusal comes back as a *runner.TriggerRefusal with its code and
// detail, and a stale generation is lease_lost and asks nothing.
func TestARemoteLeaseAsksTheTriggerQuestions(t *testing.T) {
	var asked []string
	var refuse error
	st := session.NewMemoryStore()
	srv, err := runnerapi.New(runnerapi.Options{Store: st, Queue: runner.NewQueue(st, 10*time.Millisecond), Tokens: []string{"runner-token"},
		Triggers: func(id string) runner.Triggers { return recordedTriggers{id: id, asked: &asked, refuse: refuse} }})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	c, err := New(hs.URL, "runner-token", hs.Client())
	if err != nil {
		t.Fatal(err)
	}
	c.SetRenewInterval(time.Hour)
	ctx := t.Context()
	s := hosted(t, st, "Go.")
	claims, err := c.Claim(ctx, session.Holder{Runner: "r1"}, 1, time.Second)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim %+v %v", claims, err)
	}
	l, ok := claims[0].Lease.(runner.Triggers)
	if !ok {
		t.Fatal("a remote lease reaches no triggers")
	}
	list, err := l.ListTriggers(ctx)
	if err != nil || len(list) != 1 || list[0].Origin == nil || list[0].Origin.SessionID != s.ID {
		t.Fatalf("the list %+v, %v", list, err)
	}
	p := runner.Proposal{ToolUseID: "toolu_1", Name: "Weekly", Ask: "Count.", Why: "Weekly.", Schedule: "0 9 * * 1", TimeZone: "UTC"}
	if err := l.CheckProposal(ctx, p); err != nil {
		t.Fatalf("the check: %v", err)
	}
	if made, err := l.ApplyProposal(ctx, p); err != nil || made.ID != "trg_2" || made.Title != "Weekly" {
		t.Fatalf("the apply %+v, %v", made, err)
	}
	at := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	if set, err := l.SetWake(ctx, runner.Wake{ToolUseID: "toolu_w", At: at, Title: "Look", Note: "Again."}); err != nil || set.ID != "trg_3" || set.At == nil || !set.At.Equal(at) {
		t.Fatalf("the wake %+v, %v", set, err)
	}
	if id, err := l.CancelWake(ctx, "toolu_c"); err != nil || id != "trg_3" {
		t.Fatalf("the cancel %q, %v", id, err)
	}
	refuse = &runner.TriggerRefusal{Code: "proposals_full", Message: "twenty"}
	for name, ask := range map[string]func() error{
		"check":  func() error { return l.CheckProposal(ctx, p) },
		"apply":  func() error { _, err := l.ApplyProposal(ctx, p); return err },
		"wake":   func() error { _, err := l.SetWake(ctx, runner.Wake{ToolUseID: "toolu_w", At: at}); return err },
		"unwake": func() error { _, err := l.CancelWake(ctx, "toolu_c"); return err },
	} {
		re, ok := errors.AsType[*runner.TriggerRefusal](ask())
		if !ok || re.Code != "proposals_full" || re.Message != "twenty" {
			t.Fatalf("a refused %s: %+v", name, re)
		}
	}
	want := []string{s.ID + " list", s.ID + " check toolu_1", s.ID + " apply toolu_1", s.ID + " wake toolu_w", s.ID + " unwake toolu_c"}
	if len(asked) != 9 || !slices.Equal(asked[:5], want) {
		t.Fatalf("the server was asked %q", asked)
	}
	held := claims[0].Lease.(*lease)
	stale := &lease{c: c, id: s.ID, gen: held.gen + 1, lost: make(chan struct{}), stop: make(chan struct{})}
	if _, err := stale.ListTriggers(ctx); !errors.Is(err, session.ErrLeaseLost) || len(asked) != 9 {
		t.Fatalf("another generation: %v, %d questions", err, len(asked))
	}
	if err := claims[0].Lease.Release(); err != nil {
		t.Fatal(err)
	}
}

// recordedConversations is a server's reach to a session's folders of
// conversations that keeps who asked what and fails as fail says.
type recordedConversations struct {
	id    string
	asked *[]string
	fail  *error
}

func (r recordedConversations) ListConversations(_ context.Context, folder string) ([]memory.ConversationFile, bool, error) {
	*r.asked = append(*r.asked, r.id+" list "+folder)
	if *r.fail != nil {
		return nil, false, *r.fail
	}
	return []memory.ConversationFile{{Name: "2026-10-28-ses_2.md", Version: "v2", Modified: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)}}, true, nil
}

func (r recordedConversations) ReadConversation(_ context.Context, folder, name string) (memory.ConversationFile, []byte, error) {
	*r.asked = append(*r.asked, r.id+" read "+folder+"/"+name)
	if *r.fail != nil {
		return memory.ConversationFile{}, nil, *r.fail
	}
	if name != "2026-10-28-ses_2.md" {
		return memory.ConversationFile{}, nil, fmt.Errorf("%w: %s", fs.ErrNotExist, name)
	}
	return memory.ConversationFile{Name: name, Version: "v2", Size: 9}, []byte("# Tides\n潮\n"), nil
}

// TestTheLeaseReachesConversations: a remote runner's lease is the
// session's reach to its server's folders of conversations (spec 094): a
// list and a read reach the server's own for the session the lease holds,
// a file the server does not answer is fs.ErrNotExist, a refused list
// comes back as the *memory.Refusal the file tools say, and a stale
// generation is lease_lost and asks nothing.
func TestTheLeaseReachesConversations(t *testing.T) {
	var asked []string
	var fail error
	st := session.NewMemoryStore()
	srv, err := runnerapi.New(runnerapi.Options{Store: st, Queue: runner.NewQueue(st, 10*time.Millisecond), Tokens: []string{"runner-token"},
		Conversations: func(id string) memory.Conversations { return recordedConversations{id: id, asked: &asked, fail: &fail} }})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	c, err := New(hs.URL, "runner-token", hs.Client())
	if err != nil {
		t.Fatal(err)
	}
	c.SetRenewInterval(time.Hour)
	ctx := t.Context()
	s := hosted(t, st, "Go.")
	claims, err := c.Claim(ctx, session.Holder{Runner: "r1"}, 1, time.Second)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim %+v %v", claims, err)
	}
	l, ok := claims[0].Lease.(memory.Conversations)
	if !ok {
		t.Fatal("a remote lease reaches no conversations")
	}
	files, more, err := l.ListConversations(ctx, "chats")
	if err != nil || !more || len(files) != 1 || files[0].Name != "2026-10-28-ses_2.md" || files[0].Version != "v2" || !files[0].Modified.Equal(time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("the list %+v, %v, %v", files, more, err)
	}
	file, text, err := l.ReadConversation(ctx, "chats", "2026-10-28-ses_2.md")
	if err != nil || file.Name != "2026-10-28-ses_2.md" || file.Size != 9 || string(text) != "# Tides\n潮\n" {
		t.Fatalf("the read %+v %q, %v", file, text, err)
	}
	if _, _, err := l.ReadConversation(ctx, "chats", "2026-10-27-ses_1.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a file the server does not answer: %v", err)
	}
	fail = &memory.Refusal{Op: "listing", Status: 403, Code: "forbidden", Message: "This session may not read these conversations now.", Reason: "chats_off"}
	_, _, err = l.ListConversations(ctx, "chats")
	if re, ok := errors.AsType[*memory.Refusal](err); !ok || re.Code != "forbidden" || re.Reason != "chats_off" || re.Message != "This session may not read these conversations now." || re.Status != 403 {
		t.Fatalf("a refused list: %v", err)
	}
	fail = nil
	want := []string{s.ID + " list chats", s.ID + " read chats/2026-10-28-ses_2.md", s.ID + " read chats/2026-10-27-ses_1.md", s.ID + " list chats"}
	if !slices.Equal(asked, want) {
		t.Fatalf("the server was asked %q", asked)
	}
	held := claims[0].Lease.(*lease)
	stale := &lease{c: c, id: s.ID, gen: held.gen + 1, lost: make(chan struct{}), stop: make(chan struct{})}
	if _, _, err := stale.ListConversations(ctx, "chats"); !errors.Is(err, session.ErrLeaseLost) || len(asked) != len(want) {
		t.Fatalf("another generation: %v, %d questions", err, len(asked))
	}
	if err := claims[0].Lease.Release(); err != nil {
		t.Fatal(err)
	}
}
