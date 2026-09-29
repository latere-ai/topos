// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer

import (
	"slices"

	"latere.ai/x/pkg/authz"
)

// Core is the name the shared contract carries this vocabulary under,
// and the one a refusal of an unknown action names.
const Core = "topos"

// The five resource kinds of spec 006. An action acts on exactly one of
// them, and a kind an action names never changes.
const (
	KindAgent       = "agent"
	KindSession     = "session"
	KindTrigger     = "trigger"
	KindCredential  = "credential"
	KindMemoryStore = "memory_store"
)

// The actions of spec 006's table: every question toposd asks an
// authorizer. A constant never changes its string and never disappears;
// a new action is a new row in that table first and a constant here
// second.
const (
	ActionAgentCreate  = "agent.create"
	ActionAgentRead    = "agent.read"
	ActionAgentList    = "agent.list"
	ActionAgentUpdate  = "agent.update"
	ActionAgentArchive = "agent.archive"

	ActionSessionCreate    = "session.create"
	ActionSessionRead      = "session.read"
	ActionSessionList      = "session.list"
	ActionSessionSend      = "session.send"
	ActionSessionInterrupt = "session.interrupt"
	ActionSessionEnd       = "session.end"
	ActionSessionDelete    = "session.delete"
	ActionSessionFork      = "session.fork"
	ActionSessionRewind    = "session.rewind"
	ActionSessionResume    = "session.resume"
	ActionSessionRedact    = "session.redact"
	ActionSessionAppend    = "session.append"
	ActionSessionHandoff   = "session.handoff"
	ActionSessionScope     = "session.scope"
	ActionSessionUpdate    = "session.update"

	ActionTriggerCreate = "trigger.create"
	ActionTriggerRead   = "trigger.read"
	ActionTriggerList   = "trigger.list"
	ActionTriggerUpdate = "trigger.update"
	ActionTriggerDelete = "trigger.delete"
	ActionTriggerFire   = "trigger.fire"

	ActionCredentialCreate = "credential.create"
	ActionCredentialRead   = "credential.read"
	ActionCredentialList   = "credential.list"
	ActionCredentialDelete = "credential.delete"

	ActionMemoryStoreCreate = "memory_store.create"
	ActionMemoryStoreRead   = "memory_store.read"
	ActionMemoryStoreList   = "memory_store.list"
	ActionMemoryStoreUpdate = "memory_store.update"
	ActionMemoryStoreDelete = "memory_store.delete"
	ActionMemoryStoreWrite  = "memory_store.write"
)

// table is spec 006's action table in its order, and the one place an
// action is paired with its kind.
var table = []authz.Action{
	{Name: ActionAgentCreate, Kind: KindAgent},
	{Name: ActionAgentRead, Kind: KindAgent},
	{Name: ActionAgentList, Kind: KindAgent},
	{Name: ActionAgentUpdate, Kind: KindAgent},
	{Name: ActionAgentArchive, Kind: KindAgent},

	{Name: ActionSessionCreate, Kind: KindSession},
	{Name: ActionSessionRead, Kind: KindSession},
	{Name: ActionSessionList, Kind: KindSession},
	{Name: ActionSessionSend, Kind: KindSession},
	{Name: ActionSessionInterrupt, Kind: KindSession},
	{Name: ActionSessionEnd, Kind: KindSession},
	{Name: ActionSessionDelete, Kind: KindSession},
	{Name: ActionSessionFork, Kind: KindSession},
	{Name: ActionSessionRewind, Kind: KindSession},
	{Name: ActionSessionResume, Kind: KindSession},
	{Name: ActionSessionRedact, Kind: KindSession},
	{Name: ActionSessionAppend, Kind: KindSession},
	{Name: ActionSessionHandoff, Kind: KindSession},
	{Name: ActionSessionScope, Kind: KindSession},
	{Name: ActionSessionUpdate, Kind: KindSession},

	{Name: ActionTriggerCreate, Kind: KindTrigger},
	{Name: ActionTriggerRead, Kind: KindTrigger},
	{Name: ActionTriggerList, Kind: KindTrigger},
	{Name: ActionTriggerUpdate, Kind: KindTrigger},
	{Name: ActionTriggerDelete, Kind: KindTrigger},
	{Name: ActionTriggerFire, Kind: KindTrigger},

	{Name: ActionCredentialCreate, Kind: KindCredential},
	{Name: ActionCredentialRead, Kind: KindCredential},
	{Name: ActionCredentialList, Kind: KindCredential},
	{Name: ActionCredentialDelete, Kind: KindCredential},

	{Name: ActionMemoryStoreCreate, Kind: KindMemoryStore},
	{Name: ActionMemoryStoreRead, Kind: KindMemoryStore},
	{Name: ActionMemoryStoreList, Kind: KindMemoryStore},
	{Name: ActionMemoryStoreUpdate, Kind: KindMemoryStore},
	{Name: ActionMemoryStoreDelete, Kind: KindMemoryStore},
	{Name: ActionMemoryStoreWrite, Kind: KindMemoryStore},
}

// labels is the heading a person reads for each kind, where a picker
// groups actions (a narrowed key's grants are chosen by kind).
var labels = map[string]string{
	KindAgent:       "Agents",
	KindSession:     "Sessions",
	KindTrigger:     "Triggers",
	KindCredential:  "Credentials",
	KindMemoryStore: "Memory stores",
}

// creates is the action of each kind that makes an object, the one
// action the owner policy allows on an object that does not exist yet.
var creates = map[string]string{
	KindAgent:       ActionAgentCreate,
	KindSession:     ActionSessionCreate,
	KindTrigger:     ActionTriggerCreate,
	KindCredential:  ActionCredentialCreate,
	KindMemoryStore: ActionMemoryStoreCreate,
}

// Vocabulary is the action table as the shared contract reads it: the
// client refuses an action outside it before the wire, the endpoint
// scaffold of latere.ai/x/pkg/authz/server answers 400 for one, the
// conformance suite drives a case per row, and a picker reads each
// kind's heading off Label. Each call returns a fresh copy.
func Vocabulary() authz.Vocabulary {
	return authz.Vocabulary{Core: Core, Actions: slices.Clone(table)}.WithLabels(labels)
}

// Actions lists every action, in the table's order.
func Actions() []string {
	out := make([]string, len(table))
	for i, a := range table {
		out[i] = a.Name
	}
	return out
}

// Kind is the kind an action acts on, and "" for a string outside the
// vocabulary.
func Kind(action string) string {
	for _, a := range table {
		if a.Name == action {
			return a.Kind
		}
	}
	return ""
}

// Known reports whether action is in the vocabulary.
func Known(action string) bool { return Kind(action) != "" }

// Create is the action that creates an object of kind, and "" for a
// kind outside the vocabulary.
func Create(kind string) string { return creates[kind] }
