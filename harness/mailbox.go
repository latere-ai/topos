// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package harness

import (
	"fmt"
	"sync"
)

// Message is one parent↔sub-agent message. The primitive is topology-agnostic;
// who may message whom is configured, not baked into send/receive.
type Message struct {
	From string
	To   string
	Body []byte
}

// Topology answers "may from message to?". The default is hierarchical: workers
// message only their parent; the parent may message any of its children;
// child↔child is denied.
type Topology interface {
	CanSend(from, to string) bool
}

// Hierarchical is the safe default topology: a single parent and its children,
// where children may only message the parent and the parent may message any
// child. Sibling (child↔child) messaging is denied.
type Hierarchical struct {
	ParentID string
	children map[string]bool
}

// NewHierarchical returns a hierarchical topology rooted at parentID.
func NewHierarchical(parentID string, childIDs ...string) *Hierarchical {
	c := make(map[string]bool, len(childIDs))
	for _, id := range childIDs {
		c[id] = true
	}
	return &Hierarchical{ParentID: parentID, children: c}
}

// CanSend implements Topology.
func (h *Hierarchical) CanSend(from, to string) bool {
	switch {
	case from == h.ParentID && h.children[to]:
		return true // parent → child
	case h.children[from] && to == h.ParentID:
		return true // child → parent
	default:
		return false // siblings and outsiders cannot message
	}
}

// Mailbox is a topology-gated, in-pod message channel between the parent and its
// sub-agents. Safe for concurrent use.
type Mailbox struct {
	topo  Topology
	mu    sync.Mutex
	boxes map[string][]Message
}

// NewMailbox returns a Mailbox enforcing topo.
func NewMailbox(topo Topology) *Mailbox {
	return &Mailbox{topo: topo, boxes: map[string][]Message{}}
}

// ErrNotPermitted is returned when the topology forbids a send.
var ErrNotPermitted = fmt.Errorf("harness: mailbox send not permitted by topology")

// Send delivers a message to the recipient's box if the topology permits it.
func (m *Mailbox) Send(from, to string, body []byte) error {
	if !m.topo.CanSend(from, to) {
		return ErrNotPermitted
	}
	m.mu.Lock()
	m.boxes[to] = append(m.boxes[to], Message{From: from, To: to, Body: body})
	m.mu.Unlock()
	return nil
}

// Receive drains and returns the recipient's pending messages in order.
func (m *Mailbox) Receive(agentID string) []Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	msgs := m.boxes[agentID]
	delete(m.boxes, agentID)
	return msgs
}
