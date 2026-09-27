// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

// Values of ConnectionSpec.Mode.
const (
	ConnectionPerson = "person"
	ConnectionAgent  = "agent"
)

// Values of SessionResource.Type.
const (
	ResourceMemoryStore = "memoryStore"
	ResourceRepository  = "repository"
)

// TriggerSpec is a schedule and the session each firing starts (spec
// 022).
type TriggerSpec struct {
	// Agent references the agent the sessions run; the resolved spec
	// holds its id, so the trigger runs the agent's latest version.
	Agent string `json:"agent"`
	// Schedule is a five-field cron expression, or @hourly, @daily or
	// @weekly.
	Schedule     string         `json:"schedule"`
	TimeZone     string         `json:"timeZone"`
	Session      TriggerSession `json:"session"`
	SkipIfActive *bool          `json:"skipIfActive"`
	MaxAge       string         `json:"maxAge"`
	Suspend      bool           `json:"suspend"`
}

// TriggerSession is the session a firing creates. A field left out is
// the agent's.
type TriggerSession struct {
	Message   string            `json:"message"`
	Title     string            `json:"title,omitempty"`
	Machine   *SessionMachine   `json:"machine,omitempty"`
	Resources []SessionResource `json:"resources,omitempty"`
	Budget    *Budget           `json:"budget,omitempty"`
	Limits    *SessionLimits    `json:"limits,omitempty"`
	EndOnIdle *bool             `json:"endOnIdle"`
}

// SessionMachine is the machine a triggered session runs on.
type SessionMachine struct {
	Kind        string `json:"kind,omitempty"`
	Image       string `json:"image,omitempty"`
	Environment string `json:"environment,omitempty"`
}

// SessionResource is one attachment of a triggered session: a memory
// store by name or mem_ id with its access, or a git repository at a
// ref (spec 004).
type SessionResource struct {
	Type        string `json:"type"`
	MemoryStore string `json:"memoryStore,omitempty"`
	Access      string `json:"access,omitempty"`
	URL         string `json:"url,omitempty"`
	Ref         string `json:"ref,omitempty"`
}

// SessionLimits are a triggered session's wall-clock bounds; one left
// out is the agent's.
type SessionLimits struct {
	TurnTimeout string `json:"turnTimeout,omitempty"`
	MaxAge      string `json:"maxAge,omitempty"`
}

// MemoryStoreSpec describes a memory store to the model that reads it.
type MemoryStoreSpec struct {
	Description string `json:"description"`
}

// ConnectionSpec is how a credential reaches a service: the hosts it is
// injected toward and the header it goes in. Credential names a
// Credential by name or cred_ id and is set only in agent mode; in
// person mode the credential is that of the person running the agent.
type ConnectionSpec struct {
	Service    string   `json:"service"`
	Mode       string   `json:"mode"`
	Hosts      []string `json:"hosts"`
	Credential string   `json:"credential,omitempty"`
	Inject     Inject   `json:"inject"`
}

// Inject is the header a credential is injected in. Format contains
// {value}, where the credential goes.
type Inject struct {
	Header string `json:"header"`
	Format string `json:"format"`
}
