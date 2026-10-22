// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runnerrole

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"latere.ai/x/topos/internal/runnerapi"
	v1 "latere.ai/x/topos/manifest/v1"
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
	refuse = &runner.TriggerRefusal{Code: "proposals_full", Message: "twenty"}
	for name, ask := range map[string]func() error{
		"check": func() error { return l.CheckProposal(ctx, p) },
		"apply": func() error { _, err := l.ApplyProposal(ctx, p); return err },
	} {
		re, ok := errors.AsType[*runner.TriggerRefusal](ask())
		if !ok || re.Code != "proposals_full" || re.Message != "twenty" {
			t.Fatalf("a refused %s: %+v", name, re)
		}
	}
	want := []string{s.ID + " list", s.ID + " check toolu_1", s.ID + " apply toolu_1"}
	if len(asked) != 5 || asked[0] != want[0] || asked[1] != want[1] || asked[2] != want[2] {
		t.Fatalf("the server was asked %q", asked)
	}
	held := claims[0].Lease.(*lease)
	stale := &lease{c: c, id: s.ID, gen: held.gen + 1, lost: make(chan struct{}), stop: make(chan struct{})}
	if _, err := stale.ListTriggers(ctx); !errors.Is(err, session.ErrLeaseLost) || len(asked) != 5 {
		t.Fatalf("another generation: %v, %d questions", err, len(asked))
	}
	if err := claims[0].Lease.Release(); err != nil {
		t.Fatal(err)
	}
}
