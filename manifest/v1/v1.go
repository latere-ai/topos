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

// ObjectMeta names an object. Name is the object's identifier, a DNS
// label: lowercase letters, digits and hyphens, at most 63 characters.
// DisplayName is the name a person reads, free text of at most
// manifest.MaxDisplayName characters on one line; an object without one
// is shown by Name. Like the labels and annotations, it is not part of
// the spec, so it changes no digest and no version.
type ObjectMeta struct {
	Name        string            `json:"name"`
	DisplayName string            `json:"displayName,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Status is written by the resolver and ignored on input. Digest is the
// sha256 of the resolved spec's canonical JSON; Version counts the
// distinct digests an object has had under its ID. Identity is an
// Agent's subject at the installation's identity provider, which the
// server writes when it creates the identity and the resolver carries
// to every later version (spec 018); an agent run with no identity
// provider has none.
type Status struct {
	ID        string    `json:"id,omitempty"`
	Version   int       `json:"version,omitempty"`
	Digest    string    `json:"digest,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
	Identity  string    `json:"identity,omitempty"`
	// Owner is who the agent belongs to, as the authorizer named it when
	// the agent's identity was created: a person or an organization. It
	// is empty on an installation with no identity provider, where the
	// applier owns the agent.
	Owner *Owner `json:"owner,omitempty"`
	// ArchivedAt is when the object was archived; a server writes it
	// when it answers, and a stored version never carries it.
	ArchivedAt *time.Time `json:"archivedAt,omitempty"`
	// LastFiredAt, LastSessionID, NextFireAt and Counts are a Trigger's
	// firing record (spec 022), which a server writes when it answers:
	// when it last started or continued a session and which, when a
	// schedule next fires, and how many firings came to each outcome.
	LastFiredAt   *time.Time     `json:"lastFiredAt,omitempty"`
	LastSessionID string         `json:"lastSessionId,omitempty"`
	NextFireAt    *time.Time     `json:"nextFireAt,omitempty"`
	Counts        *TriggerCounts `json:"counts,omitempty"`
	// Origin is how a Trigger came to be when a session's call made it
	// (spec 086), which the server writes and keeps across later applies;
	// nil for a trigger a person or a program applied at the API.
	Origin *TriggerOrigin `json:"origin,omitempty"`
	// Paused is why and by whom a suspended Trigger was paused (spec 086),
	// which the server writes when an apply suspends it, keeps while it
	// stays suspended and clears when it resumes; nil while it runs.
	Paused *TriggerPause `json:"paused,omitempty"`
	// Spend is a Trigger's week as the server last counted it (spec 087),
	// which it writes when it answers; nil before a firing counted it.
	Spend *TriggerSpend `json:"spend,omitempty"`
}

// TriggerPause is the pause of a suspended Trigger (spec 086).
type TriggerPause struct {
	// Why is the sentence the agent gave when its trigger tool paused the
	// trigger; empty for a suspend applied at the API.
	Why string `json:"why,omitempty"`
	// By is the subject the suspend was applied as: the person who applied
	// it at the API, or the session's initiator for a session's call.
	By string `json:"by"`
	// SessionID and ToolUseID name the session call that paused the
	// trigger; absent for a suspend applied at the API.
	SessionID string `json:"sessionId,omitempty"`
	ToolUseID string `json:"toolUseId,omitempty"`
	// At is when the trigger was paused.
	At time.Time `json:"at"`
}

// TriggerOrigin is the session call that made a Trigger (spec 086).
type TriggerOrigin struct {
	// Kind is OriginProposal or OriginWake.
	Kind string `json:"kind"`
	// SessionID and ToolUseID are the session and its call.
	SessionID string `json:"sessionId"`
	ToolUseID string `json:"toolUseId"`
	// ConfirmedBy is the subject who allowed the call, the session's
	// initiator, who owns the trigger.
	ConfirmedBy string `json:"confirmedBy,omitempty"`
}

// The kinds of a TriggerOrigin: the trigger tool's propose, applied once
// the session's initiator allowed it, and the wake tool's set, a session's
// own next wake.
const (
	OriginProposal = "proposal"
	OriginWake     = "wake"
)

// TriggerCounts is the number of a trigger's firings that came to each
// outcome of spec 022.
type TriggerCounts struct {
	Started       int64 `json:"started"`
	Continued     int64 `json:"continued"`
	Held          int64 `json:"held"`
	Filtered      int64 `json:"filtered"`
	SkippedActive int64 `json:"skippedActive"`
	SkippedBusy   int64 `json:"skippedBusy"`
	SkippedLate   int64 `json:"skippedLate"`
	// SkippedEnded counts the firings whose session.target had ended or
	// was gone (spec 086).
	SkippedEnded int64 `json:"skippedEnded"`
	// SkippedBudget counts the firings that started nothing because the
	// spend of the trigger they count toward was at its allowance (spec
	// 087).
	SkippedBudget int64 `json:"skippedBudget"`
	Refused       int64 `json:"refused"`
	Failed        int64 `json:"failed"`
}

// TriggerSpend is a Trigger's week as the server last counted it, at a
// firing that counts toward it, before that firing's look (spec 087).
// The window ends 7 days after the oldest look SpentUSDMicro counts, or 7
// days after the firing when it counts none, so ProjectedUSDMicro is the
// spend of one window.
type TriggerSpend struct {
	// WindowDays is the window the spend is counted over: 7.
	WindowDays int `json:"windowDays"`
	// SpentUSDMicro is what the looks counted toward the trigger cost in
	// the last 7 days.
	SpentUSDMicro int64 `json:"spentUsdMicro"`
	// PerWeekUSDMicro is the trigger's allowance, nil for none.
	PerWeekUSDMicro *int64 `json:"perWeekUsdMicro,omitempty"`
	// PerLookUSDMicro is the mean cost of the window's latest
	// trigger.PaceLooks looks that ended; nil while none has.
	PerLookUSDMicro *int64 `json:"perLookUsdMicro,omitempty"`
	// ExpectedLooks are the looks expected from the firing to the window's
	// end, the firing's own included.
	ExpectedLooks int `json:"expectedLooks"`
	// ProjectedUSDMicro is SpentUSDMicro plus PerLookUSDMicro times
	// ExpectedLooks; nil with PerLookUSDMicro.
	ProjectedUSDMicro *int64 `json:"projectedUsdMicro,omitempty"`
}

// Owner is an agent's owner: Type is "user" or "organization".
type Owner struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Agent is an agent definition.
type Agent struct {
	TypeMeta
	Metadata ObjectMeta `json:"metadata"`
	Spec     AgentSpec  `json:"spec"`
	Status   Status     `json:"status,omitzero"`
}

// Trigger starts or continues sessions of an agent on a schedule or on
// delivered events.
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
