// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package v1 is the topos.latere.ai/v1 manifest API (spec 003): the
// Agent, Trigger, MemoryStore and Connection kinds as Go types with the
// camelCase JSON form their files take. It holds types and constants
// only and imports nothing outside the standard library, so an embedder
// can read and write manifests without the resolver. Package manifest
// decodes, defaults, validates and resolves them.
//
// A field whose table in spec 003 names a fixed default carries no
// omitempty: the resolved spec writes the default out. Every other
// optional field is omitted when unset, so a field added to v1 later
// leaves the canonical JSON, and so the digest, of a manifest that does
// not set it unchanged.
package v1

import "time"

// APIVersion is the one apiVersion this package reads and writes.
const APIVersion = "topos.latere.ai/v1"

// ReservedPrefix is the label and annotation key prefix that belongs to
// the core; a manifest that sets a key under it is refused.
const ReservedPrefix = "topos.latere.ai/"

// The kinds of topos.latere.ai/v1.
const (
	KindAgent       = "Agent"
	KindTrigger     = "Trigger"
	KindMemoryStore = "MemoryStore"
	KindConnection  = "Connection"
)

// TypeMeta is the envelope every document starts with.
type TypeMeta struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
}

// ObjectMeta names an object. Name is a DNS label: lowercase letters,
// digits and hyphens, at most 63 characters.
type ObjectMeta struct {
	Name        string            `json:"name"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Status is written by the resolver and ignored on input. Digest is the
// sha256 of the resolved spec's canonical JSON; Version counts the
// distinct digests an object has had under its ID. Identity is the
// subject an Agent's key acts as (spec 018).
type Status struct {
	ID        string    `json:"id,omitempty"`
	Version   int       `json:"version,omitempty"`
	Digest    string    `json:"digest,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
	Identity  string    `json:"identity,omitempty"`
	// ArchivedAt is when the object was archived; a server writes it
	// when it answers, and a stored version never carries it.
	ArchivedAt *time.Time `json:"archivedAt,omitempty"`
}

// Agent is an agent definition.
type Agent struct {
	TypeMeta
	Metadata ObjectMeta `json:"metadata"`
	Spec     AgentSpec  `json:"spec"`
	Status   Status     `json:"status,omitzero"`
}

// Trigger is a schedule that starts sessions of an agent.
type Trigger struct {
	TypeMeta
	Metadata ObjectMeta  `json:"metadata"`
	Spec     TriggerSpec `json:"spec"`
	Status   Status      `json:"status,omitzero"`
}

// MemoryStore is a store of files an agent's sessions attach.
type MemoryStore struct {
	TypeMeta
	Metadata ObjectMeta      `json:"metadata"`
	Spec     MemoryStoreSpec `json:"spec"`
	Status   Status          `json:"status,omitzero"`
}

// Connection is how an agent reaches a third-party service. It names
// the credential and never holds it.
type Connection struct {
	TypeMeta
	Metadata ObjectMeta     `json:"metadata"`
	Spec     ConnectionSpec `json:"spec"`
	Status   Status         `json:"status,omitzero"`
}
