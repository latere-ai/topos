// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// netEvent is an event of typ with payload p at seq.
func netEvent(t *testing.T, seq uint64, typ Type, p any) Event {
	t.Helper()
	e, err := NewEvent(typ, p, time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	e.Seq = seq
	return e
}

// TestANetworkChangeAppliesAndIsComputed: the change from one base network
// to another names the hosts added and removed and the mode and ask that
// moved, and applying it to the first gives the second; the same network
// is no change.
func TestANetworkChangeAppliesAndIsComputed(t *testing.T) {
	old := &Network{Mode: NetworkAllowlist, Hosts: []string{"a.example.com", "b.example.com"}, Source: NetworkFromAuthorizer}
	next := Network{Mode: NetworkAllowlist, Hosts: []string{"b.example.com", "c.example.com"}, Ask: true}
	c, changed := Change(old, next)
	if !changed || !reflect.DeepEqual(c.Added, []string{"c.example.com"}) || !reflect.DeepEqual(c.Removed, []string{"a.example.com"}) ||
		c.Mode != "" || c.Ask == nil || !*c.Ask || c.Source != NetworkFromAuthorizer {
		t.Fatalf("change %+v, %v", c, changed)
	}
	got := c.Apply(old)
	want := &Network{Mode: NetworkAllowlist, Hosts: []string{"b.example.com", "c.example.com"}, Ask: true, Source: NetworkFromAuthorizer}
	if !reflect.DeepEqual(got, want) || len(old.Hosts) != 2 || old.Hosts[0] != "a.example.com" {
		t.Fatalf("applied %+v, old %+v", got, old)
	}
	if _, changed := Change(want, *want); changed {
		t.Fatal("the same network is a change")
	}
	// A session that recorded none is an allowlist of no hosts.
	c, changed = Change(nil, Network{Mode: NetworkOpen})
	if !changed || c.Mode != NetworkOpen || c.Apply(nil).Mode != NetworkOpen {
		t.Fatalf("from none: %+v", c)
	}
	if got := JoinHosts([]string{"B.example.com.", " a.example.com"}, []string{"b.example.com", ""}); !reflect.DeepEqual(got, []string{"a.example.com", "b.example.com"}) {
		t.Fatalf("joined %q", got)
	}
}

// TestApprovalsAndTheReach: a request that asks waits until a confirmation
// or a person's message answers it, and is open until approval.decided
// settles it; the reach holds the hosts a person allowed and denied, and
// a fork's copied events are its parent's.
func TestApprovalsAndTheReach(t *testing.T) {
	ada := Sender{Subject: "usr_ada", Kind: SenderPerson}
	req := func(id, host, verdict string) ApprovalRequested {
		return ApprovalRequested{ApprovalID: id, ToolUseID: "toolu_1", Source: ApprovalFromEgress, Destination: Destination{Host: host, Port: 443}, Reason: ReasonConnectionOutside, Verdict: verdict}
	}
	evs := []Event{
		netEvent(t, 0, TypeUserMessage, UserMessage{Sender: ada}),
		netEvent(t, 1, TypeApprovalRequested, req("apr_1", "a.example.com", "ask")),
		netEvent(t, 2, TypeApprovalRequested, req("apr_2", "b.example.com", "ask")),
		netEvent(t, 3, TypeApprovalRequested, req("apr_3", "c.example.com", "block")),
		netEvent(t, 4, TypeUserToolConfirmation, UserToolConfirmation{Sender: ada, ApprovalID: "apr_1", Decision: DecisionAllow}),
	}
	if got := Awaiting(evs); !reflect.DeepEqual(got, map[string]Answer{"apr_2": AnswerConfirmation}) {
		t.Fatalf("awaiting %v", got)
	}
	as := Approvals(evs)
	if len(as) != 3 || as[0].Confirmation == nil || as[0].Waiting() || !as[0].Open() || !as[1].Waiting() || as[2].Open() {
		t.Fatalf("approvals %+v", as)
	}
	// A person's message answers the one still waiting, as a deny.
	evs = append(evs, netEvent(t, 5, TypeUserMessage, UserMessage{Sender: ada}))
	if as := Approvals(evs); as[1].Message == nil || as[1].Waiting() || len(Awaiting(evs)) != 0 {
		t.Fatalf("after a message %+v, awaiting %v", as, Awaiting(evs))
	}
	evs = append(evs,
		netEvent(t, 6, TypeNetworkChanged, NetworkChanged{Added: []string{"a.example.com"}, Source: NetworkFromPerson, ApprovalID: "apr_1"}),
		netEvent(t, 7, TypeApprovalDecided, ApprovalDecided{ApprovalID: "apr_1", Decision: DecisionAllow, By: ada}),
		netEvent(t, 8, TypeApprovalDecided, ApprovalDecided{ApprovalID: "apr_2", Decision: DecisionDeny, By: ada}),
		netEvent(t, 9, TypeNetworkChanged, NetworkChanged{Added: []string{"z.example.com"}, Source: NetworkFromAuthorizer}),
	)
	if as := Approvals(evs); as[0].Open() || as[1].Open() {
		t.Fatalf("settled approvals still open: %+v", as)
	}
	s := Session{Network: &Network{Mode: NetworkAllowlist, Hosts: []string{"base.example.com"}}}
	r := NetworkOf(s, evs)
	if !reflect.DeepEqual(r.Allowed, []string{"a.example.com"}) || !reflect.DeepEqual(r.Denied, []string{"b.example.com"}) ||
		!reflect.DeepEqual(r.Hosts(), []string{"a.example.com", "base.example.com"}) {
		t.Fatalf("reach %+v, hosts %q", r, r.Hosts())
	}
	fork := Session{Parent: &Parent{SessionID: "ses_x", Seq: 9}}
	if r := NetworkOf(fork, evs); len(r.Allowed) != 0 || len(r.Denied) != 0 || r.Base != nil || len(r.Hosts()) != 0 {
		t.Fatalf("a fork carried its parent's reach: %+v", r)
	}
}

// TestTheHeaderTakesTheAuthorizersNetwork: an authorizer's change replaces
// the header's base network; a person's allow and a fork's copied change
// leave it.
func TestTheHeaderTakesTheAuthorizersNetwork(t *testing.T) {
	s := Session{Network: &Network{Mode: NetworkAllowlist, Hosts: []string{"a.example.com"}, Source: NetworkFromAgent}}
	ApplyBatch(&s, []Event{
		netEvent(t, 1, TypeNetworkChanged, NetworkChanged{Added: []string{"p.example.com"}, Source: NetworkFromPerson, ToolUseID: "toolu_1"}),
		netEvent(t, 2, TypeNetworkChanged, NetworkChanged{Added: []string{"b.example.com"}, Removed: []string{"a.example.com"}, Ask: new(true), Source: NetworkFromAuthorizer}),
	})
	want := &Network{Mode: NetworkAllowlist, Hosts: []string{"b.example.com"}, Ask: true, Source: NetworkFromAuthorizer}
	if !reflect.DeepEqual(s.Network, want) || s.LastSeq != 2 {
		t.Fatalf("header %+v", s.Network)
	}
	fork := Session{Parent: &Parent{SessionID: "ses_x", Seq: 1}, Network: &Network{Mode: NetworkNone}}
	ApplyBatch(&fork, []Event{netEvent(t, 1, TypeNetworkChanged, NetworkChanged{Mode: NetworkOpen, Source: NetworkFromAuthorizer})})
	if fork.Network.Mode != NetworkNone {
		t.Fatalf("a fork took its parent's change: %+v", fork.Network)
	}
	// The payloads keep the wire shapes a client renders.
	b, err := Marshal(NetworkChanged{Added: []string{"a.example.com"}, Ask: new(false), Source: NetworkFromPerson, ApprovalID: "apr_1"})
	if err != nil || string(b) != `{"added":["a.example.com"],"ask":false,"source":"person","approval_id":"apr_1"}` {
		t.Fatalf("network_changed %s, %v", b, err)
	}
	b, err = Marshal(UserToolConfirmation{Sender: Sender{Subject: "usr_ada", Kind: SenderPerson}, ApprovalID: "apr_1", Decision: DecisionAllow})
	var m map[string]any
	if err != nil || json.Unmarshal(b, &m) != nil || m["tool_use_id"] != nil || m["approval_id"] != "apr_1" {
		t.Fatalf("confirmation %s, %v", b, err)
	}
}
