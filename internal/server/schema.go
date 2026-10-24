// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/manifest/trigger"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// The document describes every JSON body by a schema derived from the
// Go type the handler decodes or encodes, so the schema cannot say what
// the type does not. A test decodes every example strictly into its
// route's type, which holds the examples to the same types.

// bodyShape is the Go types of a route's JSON bodies. A nil type is a body
// the route does not take or answer as JSON.
type bodyShape struct {
	request  reflect.Type
	response reflect.Type
	// list marks a response that is a page of response.
	list bool
}

func typeOf[T any]() reflect.Type { return reflect.TypeFor[T]() }

// shapes names the types of every route's JSON bodies, by operation.
var shapes = map[string]bodyShape{
	"applyAgent":         {request: typeOf[v1.Agent](), response: typeOf[v1.Agent]()},
	"listAgents":         {response: typeOf[v1.Agent](), list: true},
	"getAgent":           {response: typeOf[v1.Agent]()},
	"listAgentVersions":  {response: typeOf[agentVersion](), list: true},
	"getAgentVersion":    {response: typeOf[v1.Agent]()},
	"archiveAgent":       {request: typeOf[archiveBody](), response: typeOf[v1.Agent]()},
	"createSession":      {request: typeOf[createBody](), response: typeOf[session.Session]()},
	"listSessions":       {response: typeOf[session.Session](), list: true},
	"getSessionSummary":  {response: typeOf[session.Summary]()},
	"getSessionActivity": {response: typeOf[session.SenderActivity](), list: true},
	"searchSessions":     {response: typeOf[session.SearchResult](), list: true},
	"getSession":         {response: typeOf[session.Session]()},
	"updateSession":      {request: typeOf[updateBody](), response: typeOf[session.Session]()},
	"endSession":         {request: typeOf[endBody](), response: typeOf[session.Session]()},
	"forkSession":        {request: typeOf[forkBody](), response: typeOf[session.Session]()},
	"archiveSession":     {response: typeOf[session.Session]()},
	"unarchiveSession":   {response: typeOf[session.Session]()},
	"resumeSession":      {request: typeOf[resumeBody](), response: typeOf[session.Session]()},
	"listEvents":         {response: typeOf[session.Event](), list: true},
	"sendEvent":          {request: typeOf[sendBody](), response: typeOf[session.Event]()},
	"redactEvent":        {request: typeOf[redactBody]()},
	"applyTrigger":       {request: typeOf[v1.Trigger](), response: typeOf[v1.Trigger]()},
	"listTriggers":       {response: typeOf[v1.Trigger](), list: true},
	"getTrigger":         {response: typeOf[v1.Trigger]()},
	"fireTrigger":        {request: typeOf[trigger.Envelope](), response: typeOf[firing]()},
	"listFirings":        {response: typeOf[firing](), list: true},
	"getTriggerHold":     {response: typeOf[holdAnswer]()},
	"setTriggerHold":     {request: typeOf[holdBody](), response: typeOf[holdAnswer]()},
	"listTriggerRuns":    {response: typeOf[triggerRuns](), list: true},
}

// payloadTypes is the payload of every event type schema v1 defines.
var payloadTypes = map[session.Type]reflect.Type{
	session.TypeUserMessage:          typeOf[session.UserMessage](),
	session.TypeUserInterrupt:        typeOf[session.UserInterrupt](),
	session.TypeUserToolConfirmation: typeOf[session.UserToolConfirmation](),
	session.TypeUserToolResult:       typeOf[session.UserToolResult](),
	session.TypeUserAnswer:           typeOf[session.UserAnswer](),
	session.TypeAgentMessage:         typeOf[session.AgentMessage](),
	session.TypeAgentToolUse:         typeOf[session.AgentToolUse](),
	session.TypeToolResult:           typeOf[session.ToolResult](),
	session.TypeThreadStarted:        typeOf[session.ThreadStarted](),
	session.TypeThreadEnded:          typeOf[session.ThreadEnded](),
	session.TypeThreadMessage:        typeOf[session.ThreadMessage](),
	session.TypeContextCompacted:     typeOf[session.ContextCompacted](),
	session.TypeModelRequest:         typeOf[session.ModelRequest](),
	session.TypeSessionStatus:        typeOf[session.SessionStatus](),
	session.TypeSessionMachine:       typeOf[session.SessionMachine](),
	session.TypeMachineWaiting:       typeOf[session.SessionMachineWaiting](),
	session.TypeSessionError:         typeOf[session.SessionError](),
	session.TypeMemoryAttached:       typeOf[session.MemoryAttached](),
	session.TypeScopeChanged:         typeOf[session.ScopeChanged](),
	session.TypeModelChanged:         typeOf[session.ModelChanged](),
	session.TypePolicyChanged:        typeOf[session.PolicyChanged](),
	session.TypeTitleChanged:         typeOf[session.TitleChanged](),
	session.TypeSessionResumed:       typeOf[session.SessionResumed](),
	session.TypeMemorySynced:         typeOf[session.MemorySynced](),
	session.TypeEventRedacted:        typeOf[session.EventRedacted](),
	session.TypeSessionRewound:       typeOf[session.SessionRewound](),
	session.TypeAttachmentsDelivered: typeOf[session.AttachmentsDelivered](),
	session.TypeNetworkChanged:       typeOf[session.NetworkChanged](),
	session.TypeConnectionsChanged:   typeOf[session.ConnectionsChanged](),
	session.TypeConnectionRequested:  typeOf[session.ConnectionRequested](),
	session.TypeApprovalRequested:    typeOf[session.ApprovalRequested](),
	session.TypeApprovalDecided:      typeOf[session.ApprovalDecided](),
	session.TypeFilesKept:            typeOf[session.FilesKept](),
	session.TypeWorkSaved:            typeOf[session.WorkSaved](),
	session.TypeWakeSet:              typeOf[session.WakeSet](),
	session.TypeWakeCleared:          typeOf[session.WakeCleared](),
}

// sentPayloads is the payload a person sends for each type sendEvent
// takes. The server sets the sender of each, and of a message the
// blobs of its files, before it appends the event.
var sentPayloads = map[session.Type]reflect.Type{
	session.TypeUserMessage:          typeOf[messageBody](),
	session.TypeUserInterrupt:        typeOf[session.UserInterrupt](),
	session.TypeUserToolConfirmation: typeOf[session.UserToolConfirmation](),
	session.TypeUserToolResult:       typeOf[session.UserToolResult](),
	session.TypeUserAnswer:           typeOf[session.UserAnswer](),
}

// requestRequired is the members each request type requires, as its
// handler refuses a body without them. A response type's required
// members follow its tags, since encoding/json writes every member
// without omitempty; a request type's do not, so each is stated here,
// read from the handler's refusals.
var requestRequired = map[reflect.Type][]string{
	typeOf[createBody]():     {"agent"},
	typeOf[endBody]():        {"reason"},
	typeOf[holdBody]():       {"held"},
	typeOf[resumeBody]():     {},
	typeOf[redactBody]():     {},
	typeOf[archiveBody]():    {},
	typeOf[updateBody]():     {},
	typeOf[modelChange]():    {},
	typeOf[policyChange]():   {},
	typeOf[createBudget]():   {},
	typeOf[createLimits]():   {},
	typeOf[forkBody]():       {},
	typeOf[messageBody]():    {},
	typeOf[attachmentBody](): {"name"},
	typeOf[sourceBody]():     {"owner", "path"},
	typeOf[sendBody]():       {"type", "payload"},
	// A create's machine and its repositories, read by requestedMachine
	// and checkRepositories.
	typeOf[createMachine]():     {"kind", "environment", "workdir"},
	typeOf[repositoryRequest](): {"type", "url"},
}

// sentRequired is the members a person must send in each payload type
// sendEvent takes whose type is also an event's payload.
var sentRequired = map[reflect.Type][]string{
	typeOf[session.UserInterrupt]():        {},
	typeOf[session.UserToolConfirmation](): {"decision"},
	typeOf[session.UserToolResult]():       {"tool_use_id"},
}

// requestNotes say what a request's required members cannot: a choice
// between members, or the one value a member takes.
var requestNotes = map[reflect.Type]string{
	typeOf[messageBody]():                  "A message holds content, attachments, or both.",
	typeOf[session.UserToolConfirmation](): "Names exactly one of tool_use_id, a call that waits, and approval_id, an approval.requested that asks.",
	typeOf[endBody]():                      "reason is completed or canceled.",
	typeOf[createMachine]():                "kind is device: a person's own computer, the environment it serves and the folder on it the session works in.",
	typeOf[repositoryRequest]():            "type is repository: an https url with no credential in it, and a branch, a tag or a commit as ref.",
}

// rawFields are the members a type holds as json.RawMessage, which its
// handler decodes on its own, each with the type it decodes the member
// as, so the document describes the member by that type's schema.
var rawFields = map[reflect.Type]map[string]reflect.Type{
	typeOf[createBody](): {
		"machine":   typeOf[createMachine](),
		"resources": typeOf[[]repositoryRequest](),
	},
	typeOf[trigger.Envelope](): {"payload": typeOf[map[string]any]()},
}

// repositoryRequest is a repository as a create names it in resources:
// the members of a session.Resource that checkRepositories lets a
// request set.
type repositoryRequest struct {
	Type string `json:"type"`
	URL  string `json:"url"`
	Ref  string `json:"ref,omitempty"`
}

// typeNames name the components whose Go names do not read well in a
// public document: the server's unexported request types, and payload
// types that share a name with another type.
var typeNames = map[reflect.Type]string{
	typeOf[agentVersion]():           "AgentVersion",
	typeOf[archiveBody]():            "ArchiveAgentRequest",
	typeOf[createBody]():             "CreateSessionRequest",
	typeOf[createBudget]():           "CreateSessionBudget",
	typeOf[createLimits]():           "CreateSessionLimits",
	typeOf[createMachine]():          "CreateSessionMachine",
	typeOf[repositoryRequest]():      "CreateSessionRepository",
	typeOf[updateBody]():             "UpdateSessionRequest",
	typeOf[modelChange]():            "ModelChange",
	typeOf[policyChange]():           "PolicyChange",
	typeOf[endBody]():                "EndSessionRequest",
	typeOf[holdBody]():               "TriggerHoldRequest",
	typeOf[holdAnswer]():             "TriggerHold",
	typeOf[triggerRuns]():            "TriggerRuns",
	typeOf[triggerRun]():             "TriggerRun",
	typeOf[runTarget]():              "TriggerRunTarget",
	typeOf[forkBody]():               "ForkSessionRequest",
	typeOf[resumeBody]():             "ResumeSessionRequest",
	typeOf[redactBody]():             "RedactEventRequest",
	typeOf[messageBody]():            "MessageRequest",
	typeOf[attachmentBody]():         "AttachmentRequest",
	typeOf[sourceBody]():             "AttachmentSourceRequest",
	typeOf[firing]():                 "Firing",
	typeOf[firingEvent]():            "FiringEvent",
	typeOf[sendBody]():               "SendEventRequest",
	typeOf[session.Type]():           "EventType",
	typeOf[session.SessionStatus]():  "SessionStatusEvent",
	typeOf[session.SessionMachine](): "SessionMachineEvent",
	typeOf[session.ToolResult]():     "ToolResultEvent",
	typeOf[lux.ToolResult]():         "ToolResultBlock",
	typeOf[v1.SessionLimits]():       "ManifestSessionLimits",
	typeOf[v1.SessionResource]():     "ManifestSessionResource",
	typeOf[v1.SessionMachine]():      "ManifestSessionMachine",
}

// packageLabels prefix a component's name when types of several
// packages share it, so the document names each type once and plainly,
// whichever is reached first.
var packageLabels = map[string]string{
	"latere.ai/x/topos/session":          "Session",
	"latere.ai/x/topos/manifest/v1":      "Manifest",
	"latere.ai/x/topos/manifest/trigger": "Trigger",
	"latere.ai/x/topos/internal/server":  "Request",
	"latere.ai/x/topos/internal/store":   "Store",
	"latere.ai/x/pkg/llmdialect/lux":     "Lux",
	"latere.ai/x/pkg/llmdialect/ir":      "Model",
}

// enums lists the values of the string types the document names them
// for. A test holds each list to the constants its package declares.
var enums = map[reflect.Type][]string{
	typeOf[session.Status]():     {string(session.StatusIdle), string(session.StatusRunning), string(session.StatusEnded)},
	typeOf[session.StopReason](): stopReasons(),
	typeOf[session.Type]():       eventTypes(),
}

// stopReasons is every session.StopReason, in the order session.go
// declares them.
func stopReasons() []string {
	return []string{
		string(session.StopEndTurn), string(session.StopToolConfirmation), string(session.StopToolResult),
		string(session.StopQuestion), string(session.StopBudget), string(session.StopTurnLimit),
		string(session.StopOutputLimit), string(session.StopInterrupted), string(session.StopError),
		string(session.StopMachineWaiting), string(session.StopCompleted), string(session.StopFailed),
		string(session.StopCanceled), string(session.StopExpired),
	}
}

// eventTypes is every event type schema v1 defines, in order.
func eventTypes() []string {
	out := make([]string, 0, len(session.Known))
	for t := range session.Known {
		out = append(out, string(t))
	}
	slices.Sort(out)
	return out
}

// written is the schema of a type whose JSON its own MarshalJSON
// writes, which the fields do not show.
var written = map[reflect.Type]yaml.MapSlice{
	typeOf[ir.LogProb](): {{Key: "type", Value: []string{"number", "null"}}, {Key: "description", Value: "A log probability; null when it is not finite."}},
	// Delta and UserAnswer are described by hand beside their handlers,
	// with the bounds and readings their types do not carry.
	typeOf[session.Delta]():      ref("Delta"),
	typeOf[session.UserAnswer](): ref("UserAnswer"),
	// An event is one of its type's variants, which events adds.
	typeOf[session.Event](): ref("Event"),
}

var (
	typeTime = typeOf[time.Time]()
	typeRaw  = typeOf[json.RawMessage]()
	typeJSON = typeOf[json.Marshaler]()
)

// schemas collects the components a document names.
type schemas struct {
	defs  map[string]yaml.MapSlice
	names map[reflect.Type]string
	// err is the first type the collector could not describe.
	err error
}

// newSchemas names every type reachable from roots: its own name when
// no other reachable type has it, its package's label and its name
// when another does.
func newSchemas(roots ...reflect.Type) *schemas {
	s := &schemas{defs: map[string]yaml.MapSlice{}, names: map[reflect.Type]string{}}
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		if t == nil || seen[t] {
			return
		}
		seen[t] = true
		// The standard library's types and those written by hand are
		// described whole, not by their fields.
		if _, ok := written[t]; ok || (t.PkgPath() != "" && !strings.Contains(t.PkgPath(), ".")) {
			return
		}
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			walk(t.Elem())
		case reflect.Struct:
			for field := range t.Fields() {
				walk(field.Type)
			}
			for _, stand := range rawFields[t] {
				walk(stand)
			}
		}
	}
	for _, t := range roots {
		walk(t)
	}
	byName := map[string][]reflect.Type{}
	for t := range seen {
		if t.Name() == "" || (t.Kind() != reflect.Struct && enums[t] == nil) || t == typeTime {
			continue
		}
		if n, ok := typeNames[t]; ok {
			s.names[t] = n
			continue
		}
		if !ast.IsExported(t.Name()) && s.err == nil {
			s.err = fmt.Errorf("server: %s is unexported; name its component in typeNames", t)
		}
		byName[t.Name()] = append(byName[t.Name()], t)
	}
	for n, ts := range byName {
		for _, t := range ts {
			if len(ts) == 1 {
				s.names[t] = n
				continue
			}
			label, ok := packageLabels[t.PkgPath()]
			if !ok && s.err == nil {
				s.err = fmt.Errorf("server: %s shares its name with another type; give its package a label", t)
			}
			s.names[t] = label + n
		}
	}
	owner := map[string]reflect.Type{}
	for t, n := range s.names {
		if other, ok := owner[n]; ok && s.err == nil {
			s.err = fmt.Errorf("server: %s and %s are both named %s; name one in typeNames", other, t, n)
		}
		owner[n] = t
	}
	return s
}

// ref is a reference to a component.
func ref(name string) yaml.MapSlice {
	return yaml.MapSlice{{Key: "$ref", Value: "#/components/schemas/" + name}}
}

// name is the component name of a named type.
func (s *schemas) name(t reflect.Type) string {
	if n, ok := s.names[t]; ok {
		return n
	}
	if s.err == nil {
		s.err = fmt.Errorf("server: %s was not reached from the document's roots", t)
	}
	return t.Name()
}

// of is the schema of a value of type t.
func (s *schemas) of(t reflect.Type) yaml.MapSlice {
	if w, ok := written[t]; ok {
		return w
	}
	if t == typeOf[v1.Tool]() {
		// A tool is its bare name when it sets nothing else, and an
		// object of its fields otherwise.
		if _, done := s.defs["ToolSettings"]; !done {
			s.defs["ToolSettings"] = s.object(t)
		}
		return yaml.MapSlice{
			{Key: "description", Value: "A tool the agent may call: its bare name, or an object of the name and its settings."},
			{Key: "oneOf", Value: []yaml.MapSlice{{{Key: "type", Value: "string"}}, ref("ToolSettings")}},
		}
	}
	if t.Kind() != reflect.Pointer && t != typeTime && t != typeRaw && t.Implements(typeJSON) && s.err == nil {
		s.err = fmt.Errorf("server: %s writes its own JSON; describe it in written", t)
	}
	switch t {
	case typeTime:
		return yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "format", Value: "date-time"}}
	case typeRaw:
		return yaml.MapSlice{{Key: "description", Value: "Any JSON value."}}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return s.of(t.Elem())
	case reflect.String:
		if values, ok := enums[t]; ok {
			n := s.name(t)
			if _, done := s.defs[n]; !done {
				s.defs[n] = yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "enum", Value: values}}
			}
			return ref(n)
		}
		return yaml.MapSlice{{Key: "type", Value: "string"}}
	case reflect.Bool:
		return yaml.MapSlice{{Key: "type", Value: "boolean"}}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return yaml.MapSlice{{Key: "type", Value: "integer"}}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return yaml.MapSlice{{Key: "type", Value: "integer"}, {Key: "minimum", Value: 0}}
	case reflect.Float32, reflect.Float64:
		return yaml.MapSlice{{Key: "type", Value: "number"}}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "contentEncoding", Value: "base64"}}
		}
		return yaml.MapSlice{{Key: "type", Value: "array"}, {Key: "items", Value: s.of(t.Elem())}}
	case reflect.Map:
		return yaml.MapSlice{{Key: "type", Value: "object"}, {Key: "additionalProperties", Value: s.of(t.Elem())}}
	case reflect.Interface:
		return yaml.MapSlice{{Key: "description", Value: "Any JSON value."}}
	case reflect.Struct:
		if t.Name() == "" {
			return s.object(t)
		}
		n := s.name(t)
		if _, done := s.defs[n]; !done {
			s.defs[n] = nil // reserved, so a type that holds itself ends
			s.defs[n] = s.object(t)
		}
		return ref(n)
	}
	if s.err == nil {
		s.err = fmt.Errorf("server: no schema for %s", t)
	}
	return yaml.MapSlice{}
}

// object is the schema of a struct as encoding/json writes it, with a
// request type's required members as its handler requires them.
func (s *schemas) object(t reflect.Type) yaml.MapSlice {
	var props yaml.MapSlice
	var required []string
	s.fields(t, &props, &required)
	if r, ok := requestRequired[t]; ok {
		required = r
	}
	out := yaml.MapSlice{{Key: "type", Value: "object"}}
	if note, ok := requestNotes[t]; ok {
		out = append(out, yaml.MapItem{Key: "description", Value: note})
	}
	if len(required) > 0 {
		out = append(out, yaml.MapItem{Key: "required", Value: required})
	}
	if len(props) > 0 {
		out = append(out, yaml.MapItem{Key: "properties", Value: props})
	}
	return out
}

// fields adds the members of struct t, those of an embedded struct
// promoted as encoding/json promotes them.
func (s *schemas) fields(t reflect.Type, props *yaml.MapSlice, required *[]string) {
	for f := range t.Fields() {
		tag := f.Tag.Get("json")
		if tag == "-" || (!f.IsExported() && !f.Anonymous) {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		ft := f.Type
		if f.Anonymous && name == "" {
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				s.fields(ft, props, required)
				continue
			}
		}
		if name == "" {
			name = f.Name
		}
		optional := strings.Contains(opts, "omitempty") || strings.Contains(opts, "omitzero")
		described := ft
		if stand, ok := rawFields[t][name]; ok {
			described = stand
		}
		schema := s.of(described)
		if strings.Contains(opts, "string") {
			schema = yaml.MapSlice{{Key: "type", Value: "string"}}
		}
		if ft.Kind() == reflect.Pointer && !optional {
			schema = yaml.MapSlice{{Key: "anyOf", Value: []yaml.MapSlice{schema, {{Key: "type", Value: "null"}}}}}
		}
		*props = append(*props, yaml.MapItem{Key: name, Value: schema})
		if !optional {
			*required = append(*required, name)
		}
	}
}

// page is the schema of a page of items of type t.
func (s *schemas) page(t reflect.Type) yaml.MapSlice {
	return yaml.MapSlice{
		{Key: "type", Value: "object"},
		{Key: "required", Value: []string{"items"}},
		{Key: "properties", Value: yaml.MapSlice{
			{Key: "items", Value: yaml.MapSlice{{Key: "type", Value: "array"}, {Key: "items", Value: s.of(t)}}},
			{Key: "next_cursor", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "description", Value: "The cursor of the next page; absent on the last."}}},
		}},
	}
}

// events adds Event, one variant per event type with its payload, and
// SendEvent, one variant per type a person sends, and answers their
// references.
func (s *schemas) events() (event, send yaml.MapSlice) {
	s.defs["EventEnvelope"] = s.object(typeOf[session.Event]())
	envelope := ref("EventEnvelope")
	var variants []yaml.MapSlice
	mapping := yaml.MapSlice{}
	for _, t := range eventTypes() {
		n := "Event." + t
		s.defs[n] = yaml.MapSlice{{Key: "allOf", Value: []yaml.MapSlice{envelope, {
			{Key: "type", Value: "object"},
			{Key: "properties", Value: yaml.MapSlice{
				{Key: "type", Value: yaml.MapSlice{{Key: "const", Value: t}}},
				{Key: "payload", Value: s.of(payloadTypes[session.Type(t)])},
			}},
		}}}}
		variants = append(variants, ref(n))
		mapping = append(mapping, yaml.MapItem{Key: t, Value: "#/components/schemas/" + n})
	}
	s.defs["Event"] = yaml.MapSlice{
		{Key: "description", Value: "One entry of a session's log. type names the payload's shape; a type a later schema adds keeps the envelope, and a reader keeps an event of a type it does not know."},
		{Key: "oneOf", Value: variants},
		{Key: "discriminator", Value: yaml.MapSlice{{Key: "propertyName", Value: "type"}, {Key: "mapping", Value: mapping}}},
	}
	var sent []string
	for t := range sentPayloads {
		sent = append(sent, string(t))
	}
	slices.Sort(sent)
	variants, mapping = nil, yaml.MapSlice{}
	for _, t := range sent {
		n := "SendEvent." + t
		s.defs[n] = yaml.MapSlice{
			{Key: "type", Value: "object"},
			{Key: "required", Value: []string{"type", "payload"}},
			{Key: "properties", Value: yaml.MapSlice{
				{Key: "type", Value: yaml.MapSlice{{Key: "const", Value: t}}},
				{Key: "payload", Value: s.sent(sentPayloads[session.Type(t)])},
			}},
		}
		variants = append(variants, ref(n))
		mapping = append(mapping, yaml.MapItem{Key: t, Value: "#/components/schemas/" + n})
	}
	s.defs["SendEvent"] = yaml.MapSlice{
		{Key: "description", Value: "An event a person sends. The server sets each payload's sender, and a message's file blobs, before it appends the event."},
		{Key: "oneOf", Value: variants},
		{Key: "discriminator", Value: yaml.MapSlice{{Key: "propertyName", Value: "type"}, {Key: "mapping", Value: mapping}}},
	}
	return ref("Event"), ref("SendEvent")
}

// requestSchema is the schema of op's JSON request body.
func (s *schemas) requestSchema(op string) yaml.MapSlice {
	if op == "sendEvent" {
		return ref("SendEvent")
	}
	t := shapes[op].request
	if t == nil {
		if s.err == nil {
			s.err = fmt.Errorf("server: %s takes a body and names no request type in shapes", op)
		}
		return yaml.MapSlice{}
	}
	return s.of(t)
}

// responseSchema is the schema of op's JSON answer.
func (s *schemas) responseSchema(op string) yaml.MapSlice {
	sh := shapes[op]
	if sh.response == nil {
		if s.err == nil {
			s.err = fmt.Errorf("server: %s answers JSON and names no response type in shapes", op)
		}
		return yaml.MapSlice{}
	}
	if sh.list {
		return s.page(sh.response)
	}
	return s.of(sh.response)
}

// sent is the schema of a payload a person sends: its type's fields
// but sender, which the server sets whatever the body says. A payload
// type with its own component keeps it.
func (s *schemas) sent(t reflect.Type) yaml.MapSlice {
	if w, ok := written[t]; ok {
		return w
	}
	var props yaml.MapSlice
	var required []string
	s.fields(t, &props, &required)
	props = slices.DeleteFunc(props, func(m yaml.MapItem) bool { return m.Key == "sender" })
	if r, ok := sentRequired[t]; ok {
		required = r
	} else if r, ok := requestRequired[t]; ok {
		required = r
	} else if s.err == nil {
		s.err = fmt.Errorf("server: %s is sent and states no required members in sentRequired", t)
	}
	out := yaml.MapSlice{{Key: "type", Value: "object"}}
	if note, ok := requestNotes[t]; ok {
		out = append(out, yaml.MapItem{Key: "description", Value: note})
	}
	if len(required) > 0 {
		out = append(out, yaml.MapItem{Key: "required", Value: required})
	}
	return append(out, yaml.MapItem{Key: "properties", Value: props})
}

// components is the collected schemas, by name.
func (s *schemas) components() yaml.MapSlice {
	names := make([]string, 0, len(s.defs))
	for n := range s.defs {
		names = append(names, n)
	}
	slices.Sort(names)
	out := make(yaml.MapSlice, 0, len(names))
	for _, n := range names {
		out = append(out, yaml.MapItem{Key: n, Value: s.defs[n]})
	}
	return out
}

// pageParams are the parameters of every route that answers a page.
var pageParams = []yaml.MapSlice{
	{{Key: "name", Value: "limit"}, {Key: "in", Value: "query"},
		{Key: "description", Value: fmt.Sprintf("How many items a page holds at most; %d when absent.", session.DefaultListLimit)},
		{Key: "schema", Value: yaml.MapSlice{{Key: "type", Value: "integer"}, {Key: "minimum", Value: 1}, {Key: "maximum", Value: MaxLimit}}}},
	{{Key: "name", Value: "cursor"}, {Key: "in", Value: "query"},
		{Key: "description", Value: "The next_cursor of the page before. The answer's Link header names the next page's URL as well."},
		{Key: "schema", Value: yaml.MapSlice{{Key: "type", Value: "string"}}}},
}

// ownPaging reports a route that documents its own limit and cursor,
// with bounds of its own, as the search does.
func ownPaging(op string) bool {
	return slices.ContainsFunc(queryParams[op], func(p yaml.MapSlice) bool {
		return len(p) > 0 && p[0].Value == "limit"
	})
}

// headerParams are the request headers a route reads, by operation.
var headerParams = map[string][]yaml.MapSlice{
	"streamEvents": {{{Key: "name", Value: "Last-Event-ID"}, {Key: "in", Value: "header"},
		{Key: "description", Value: "The sequence of the last event a client received. The stream starts after it, in place of from_seq, so a client that reconnects resumes where it left off; the browser's EventSource sends it on its own."},
		{Key: "schema", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "pattern", Value: "^[1-9][0-9]*$"}}}}},
}

// idempotencyParam is the header every POST reads.
var idempotencyParam = yaml.MapSlice{{Key: "name", Value: "Idempotency-Key"}, {Key: "in", Value: "header"},
	{Key: "description", Value: fmt.Sprintf("A key the caller makes unique to the request, so it can send the request again without doing it twice. "+
		"A repeat from the same caller with the same key, route and body within %s answers the first answer again, with the header Idempotent-Replayed: true. "+
		"The same key with another route or body is idempotency_conflict (409). A repeat while the first request is still being answered is conflict (409), and the first request still completes. "+
		"An answer of 5xx keeps nothing, so the request may be sent again with the same key.", spoken(IdempotencyTTL))},
	{Key: "schema", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "minLength", Value: 1}}}}
