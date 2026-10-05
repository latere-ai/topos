// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"

	"latere.ai/x/topos/manifest"
	"latere.ai/x/topos/manifest/trigger"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

//go:generate go test -run TestOpenAPIIsGenerated -update .

// OpenAPI renders the API's OpenAPI document from the route and error
// tables, with server as its one server URL. api/openapi.yaml is this
// document for the server "/v1", and a test holds the two equal.
func OpenAPI(server string) ([]byte, error) {
	statuses := map[int][]string{}
	for code, row := range codes {
		statuses[row.status] = append(statuses[row.status], code)
	}
	var errorCodes []string
	for code := range codes {
		errorCodes = append(errorCodes, code)
	}
	slices.Sort(errorCodes)

	// head is the document's first members, which the route that serves
	// the document shows as its example.
	head := yaml.MapSlice{
		{Key: "openapi", Value: "3.1.0"},
		{Key: "info", Value: yaml.MapSlice{
			{Key: "title", Value: "Topos API"},
			{Key: "version", Value: "v1"},
			{Key: "description", Value: "Agents, their versions, the sessions people have with them, and the triggers that start and continue sessions on a schedule or on delivered events. Every route is verified and asked of the installation's authorizer; every error is one envelope with a code of the table under components.schemas.Error."},
		}},
		{Key: "servers", Value: []yaml.MapSlice{{{Key: "url", Value: server}}}},
	}
	first, err := asYAML(head)
	if err != nil {
		return nil, err
	}
	questionInput, err := questionInputSchema()
	if err != nil {
		return nil, err
	}
	shown, err := examples()
	if err != nil {
		return nil, err
	}
	shown["getOpenAPI"] = example{response: string(first)}

	paths := yaml.MapSlice{}
	byPath := map[string]yaml.MapSlice{}
	var order []string
	for _, rt := range table() {
		if _, ok := byPath[rt.path]; !ok {
			order = append(order, rt.path)
		}
		op, err := operation(rt, shown[rt.op])
		if err != nil {
			return nil, fmt.Errorf("server: the example of %s: %w", rt.op, err)
		}
		byPath[rt.path] = append(byPath[rt.path], yaml.MapItem{Key: strings.ToLower(rt.method), Value: op})
	}
	for _, p := range order {
		paths = append(paths, yaml.MapItem{Key: p, Value: byPath[p]})
	}
	doc := slices.Concat(head, yaml.MapSlice{
		{Key: "security", Value: []yaml.MapSlice{{{Key: "bearer", Value: []string{}}}}},
		{Key: "paths", Value: paths},
		{Key: "components", Value: yaml.MapSlice{
			{Key: "securitySchemes", Value: yaml.MapSlice{
				{Key: "bearer", Value: yaml.MapSlice{{Key: "type", Value: "http"}, {Key: "scheme", Value: "bearer"}, {Key: "bearerFormat", Value: "JWT"}}},
			}},
			{Key: "schemas", Value: yaml.MapSlice{
				{Key: "Delta", Value: deltaSchema},
				{Key: "QuestionInput", Value: questionInput},
				{Key: "UserAnswer", Value: userAnswer},
				{Key: "QuestionResultMeta", Value: questionResultMeta},
				{Key: "Error", Value: yaml.MapSlice{
					{Key: "type", Value: "object"},
					{Key: "required", Value: []string{"error"}},
					{Key: "properties", Value: yaml.MapSlice{{Key: "error", Value: yaml.MapSlice{
						{Key: "type", Value: "object"},
						{Key: "required", Value: []string{"code", "message"}},
						{Key: "properties", Value: yaml.MapSlice{
							{Key: "code", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "enum", Value: errorCodes}}},
							{Key: "message", Value: yaml.MapSlice{{Key: "type", Value: "string"}}},
							{Key: "details", Value: errorDetails},
						}},
					}}}},
				}},
			}},
			{Key: "responses", Value: errorResponses(statuses)},
		}},
	})
	return asYAML(doc)
}

// asYAML writes a part of the document as YAML, a string of several
// lines as a block. The encoder indents a block's empty lines, which
// read the same written empty, so the document ends no line in a space.
func asYAML(v any) ([]byte, error) {
	b, err := yaml.MarshalWithOptions(v, yaml.Indent(2), yaml.IndentSequence(true), yaml.UseLiteralStyleIfMultiline(true))
	if err != nil {
		return nil, err
	}
	return indentOnly.ReplaceAll(b, nil), nil
}

var indentOnly = regexp.MustCompile(`(?m)^ +$`)

var pathParam = regexp.MustCompile(`\{([a-z_]+)\}`)

// OwnerRule is the sentence the document states an agent's owner by
// (spec 036), the one a client reads to know the core keeps an
// organization's agents.
const OwnerRule = "An agent belongs to the organization the caller's token names, or to the caller as a person when it names none."

// paramDescriptions say how the API reads a path parameter where a
// client needs to know it: a name is unique within its owner, so an
// agent's name reads among the agents of the caller's context and a
// trigger's among the caller's own, and an id reads its object whoever
// owns it.
var paramDescriptions = map[string]string{
	"name": "The name, unique within its owner. " + OwnerRule + " An agent's name is read among that owner's agents, a trigger's among the caller's own triggers. An apply acts on the object of the name and creates it when there is none; an object of the name another owner holds is neither read nor changed.",
	"ref":  "An id, which names its object whoever owns it, subject to the authorizer, or a name, read among the agents of the caller's context or the caller's own triggers. A name only another owner holds answers not_found, as one nobody holds does.",
}

// agentParam, runnerParam and archivedParam scope the sessions a list
// pages through and a summary counts; status is the list's alone.
var (
	agentParam = yaml.MapSlice{{Key: "name", Value: "agent"}, {Key: "in", Value: "query"}, {Key: "description", Value: "An agent's id, or a name among the agents of the caller's context."},
		{Key: "schema", Value: yaml.MapSlice{{Key: "type", Value: "string"}}}}
	runnerParam = yaml.MapSlice{{Key: "name", Value: "runner"}, {Key: "in", Value: "query"},
		{Key: "schema", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "enum", Value: []string{session.RunnerHosted, session.RunnerExternal}}}}}
	archivedParam = yaml.MapSlice{{Key: "name", Value: "archived"}, {Key: "in", Value: "query"}, {Key: "description", Value: "false or absent leaves archived sessions out, true lists only them, any lists both."},
		{Key: "schema", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "enum", Value: []string{"false", "true", "any"}}}}}
)

// queryParams are the query parameters of the routes that read any.
var queryParams = map[string][]yaml.MapSlice{
	"listSessions": {
		agentParam,
		{{Key: "name", Value: "status"}, {Key: "in", Value: "query"},
			{Key: "schema", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "enum", Value: []string{string(session.StatusIdle), string(session.StatusRunning), string(session.StatusEnded)}}}}},
		runnerParam,
		archivedParam,
	},
	"getSessionSummary": {agentParam, runnerParam, archivedParam},
	"getFile": {
		{{Key: "name", Value: "path"}, {Key: "in", Value: "query"}, {Key: "required", Value: true},
			{Key: "description", Value: "The file: an absolute path inside the session's working directory, as a write or edit tool.result's meta.path names it, or a path relative to the working directory."},
			{Key: "schema", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "minLength", Value: 1}}}},
	},
	"streamEvents": {
		{{Key: "name", Value: "from_seq"}, {Key: "in", Value: "query"}, {Key: "description", Value: "The sequence the replay starts at; 1 when absent. A Last-Event-ID header starts it after the sequence the header names instead."},
			{Key: "schema", Value: yaml.MapSlice{{Key: "type", Value: "integer"}, {Key: "minimum", Value: 1}}}},
		{{Key: "name", Value: "deltas"}, {Key: "in", Value: "query"}, {Key: "description", Value: "1 also carries the session's live deltas, each an event: delta frame with no id; 0 or absent carries the log's events alone."},
			{Key: "schema", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "enum", Value: []string{"0", "1"}}}}},
	},
}

// eventStreams are the routes that answer Server-Sent Events.
var eventStreams = map[string]string{
	"streamEvents": "Frames of `id: <seq>`, `event: <type>` and `data: <the event's JSON>`, a `: keepalive` comment line between them, and with deltas=1 frames of `event: delta` and `data: <a Delta>` with no id.",
}

// The media types of the bodies the document describes.
const (
	mediaJSON   = "application/json"
	mediaYAML   = "application/yaml"
	mediaStream = "text/event-stream"
	mediaBytes  = "application/octet-stream"
)

// answerMedia are the routes whose answer is not JSON: a stream's
// frames, a blob's bytes and the document itself.
var answerMedia = map[string]string{"streamEvents": mediaStream, "getBlob": mediaBytes, "getFile": mediaBytes, "getOpenAPI": mediaYAML}

// emptyBodies are the routes that read a body only to refuse one that
// holds a member: a caller sends none, so the document describes none.
var emptyBodies = map[string]bool{"archiveSession": true, "unarchiveSession": true}

// The answers of a route that creates its object or finds it there.
const (
	answerCreated = "Created: the apply created the object."
	answerExisted = "OK: the object existed, and the apply changed it or left it as it was."
)

// deltaSchema is the data of a stream's event: delta frame (spec 015).
var deltaSchema = yaml.MapSlice{
	{Key: "type", Value: "object"},
	{Key: "description", Value: "One live frame of a session's response as it arrives, the data of an event: delta frame. A frame of text is {thread, turn, step, block, kind, text}: text is the next run of the content block's text, thinking or tool input, " +
		"to append to what the step's earlier deltas carried for that block. A reset is {thread, turn, step, reset}: the step's request is sent again, so what its deltas carried is discarded. " +
		"Deltas are best effort: never appended, never replayed, and dropped rather than slowing the turn; the step's agent.message is the record, so a client that misses deltas loses nothing, and a delta of a step whose agent.message the client holds is stale."},
	{Key: "required", Value: []string{"turn", "step"}},
	{Key: "properties", Value: yaml.MapSlice{
		{Key: "thread", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "description", Value: "The thread whose response it is; absent for the session's own thread, as on events."}}},
		{Key: "turn", Value: yaml.MapSlice{{Key: "type", Value: "integer"}, {Key: "minimum", Value: 1}}},
		{Key: "step", Value: yaml.MapSlice{{Key: "type", Value: "integer"}, {Key: "minimum", Value: 1}}},
		{Key: "block", Value: yaml.MapSlice{{Key: "type", Value: "integer"}, {Key: "minimum", Value: 0}, {Key: "description", Value: "The index of the response's content block the text belongs to."}}},
		{Key: "kind", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "enum", Value: []string{string(session.DeltaText), string(session.DeltaThinking), string(session.DeltaToolInput)}},
			{Key: "description", Value: "What the text is part of: a text block, a thinking block, or a tool call's arguments as the model writes them."}}},
		{Key: "text", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "description", Value: fmt.Sprintf("At most %d bytes; a longer run arrives as several deltas.", session.MaxDeltaText)}}},
		{Key: "reset", Value: yaml.MapSlice{{Key: "type", Value: "boolean"}, {Key: "const", Value: true}, {Key: "description", Value: "Present only on a reset."}}},
	}},
}

// errorDetails is the schema of an error's details: the developer
// detail, and the fields a code carries beside it. A not_found carries
// none, so a denied read answers as an absent object does.
var errorDetails = yaml.MapSlice{
	{Key: "type", Value: "object"},
	{Key: "description", Value: "What a developer reads about the refusal; message is the sentence a person reads. A not_found carries no details."},
	{Key: "properties", Value: yaml.MapSlice{
		{Key: "detail", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "description", Value: "What was refused and why, for a log or a verbose view."}}},
		{Key: "problems", Value: yaml.MapSlice{
			{Key: "type", Value: "array"},
			{Key: "description", Value: "A refused manifest's problems, each at its document (from 1) and field path."},
			{Key: "items", Value: yaml.MapSlice{
				{Key: "type", Value: "object"},
				{Key: "properties", Value: yaml.MapSlice{
					{Key: "document", Value: yaml.MapSlice{{Key: "type", Value: "integer"}}},
					{Key: "path", Value: yaml.MapSlice{{Key: "type", Value: "string"}}},
					{Key: "detail", Value: yaml.MapSlice{{Key: "type", Value: "string"}}},
				}},
			}},
		}},
		{Key: "reason", Value: yaml.MapSlice{
			{Key: "type", Value: "string"},
			{Key: "description", Value: "On forbidden, the installation's authorizer's reason for its deny, a stable snake_case token such as not_owner or agents_not_enabled, which a client may branch on. " +
				"The authorizer owns the vocabulary. It is absent when the authorizer gave none or gave text of another shape, and on a session create of another subject's agent the caller may not read."},
		}},
	}},
}

// maxSummaryWords bounds a route's summary. A reference lists an
// operation by its summary, in a column a sentence does not fit.
const maxSummaryWords = 4

// opDescriptions say what each route does, and what its body and its
// answer hold where a client needs to know: an apply's manifest names
// the agent twice, by the identifier and by the name a person reads, and
// only the spec versions. A route's summary names the action in a few
// words, the label a reference lists the operation by, so every
// sentence about it is here.
var opDescriptions = map[string]string{
	"listAgents":        "List the agents of the caller's context.",
	"getAgent":          "Get an agent's latest version by id, or by name among the agents of the caller's context.",
	"listAgentVersions": "List an agent's versions.",
	"getAgentVersion":   "Get one version of an agent.",
	"archiveAgent":      "Archive an agent; running sessions keep their version.",
	"createSession": "Create a session of an agent, named by id or by name among the agents of the caller's context. " + AttendedRule + " " +
		"The session runs its agent's model, and its model is absent from the answer. Where the installation's authorizer names another model for it, the session starts on that one: " +
		"its model is {name, via, effort}, name the model that runs and via the agent's own name for it, which a client that offers the choice shows. " +
		"The model that runs is checked after the authorizer is asked: one no source gives an input window and an output limit is model_unknown, and a gateway that does not answer model_unavailable.",
	"listSessions":  "List the sessions of the agents of the caller's context, filtered by agent, status, runner and archived.",
	"getSession":    "Get a session.",
	"resumeSession": "Resume a session idle on its budget once the cap is raised.",
	"deleteSession": "Delete a session, its log and its blobs, for good. The route asks session.read, so a caller who may not read the session hears not_found, and refuses a running session as conflict before it asks session.delete: " +
		"interrupt it and delete it once it is idle. A session a runner claims while the authorizer decides is conflict too, and a deny is forbidden. " +
		"A fork keeps its own copy of the log. The session's sandbox, its secrets and a checkpoint kept at the git host are the installation's to remove.",
	"listEvents": "List a session's events from a sequence.",
	"getBlob":    "Get a blob of a session.",
	"getFile": fmt.Sprintf("Get one file of the session's working directory as it is now, such as a file the agent wrote, by its path. "+
		"The answer is the file's bytes as a download: Content-Type from its extension or its first bytes, Content-Length, Content-Disposition attachment with its name, "+
		"X-Content-Type-Options nosniff, Content-Security-Policy %q and Cache-Control no-store. HEAD answers the same headers without the bytes. "+
		"The file is read from the session's machine while it runs, and the read never starts or creates one: a machine that is stopped, never opened, or gone is file_unavailable, "+
		"and the file can be read again once the session runs its machine. A file past %d bytes is file_too_large. "+
		"A path outside the working directory, a directory, or a path the credential deny-list names is invalid_request, and a file that does not exist not_found.", filePolicy, MaxFileBytes),
	"redactEvent": "Replace one event's content with a tombstone. The tombstone of a tool.result or a user.tool_result keeps its tool_use_id, so the call still reads as answered. " +
		"A user.answer is redactable, and redacting it redacts the tool.result the runner rendered from it in the same call; a runner that has not read it yet tells the agent the answer was removed. " +
		"The agent.tool_use of a question whose call has no tool.result yet is conflict: dismiss the question with user.interrupt first.",
	"listTriggers":  "List triggers.",
	"getTrigger":    "Get a trigger by id, or by name among the caller's own triggers, with its firing record.",
	"deleteTrigger": "Delete a trigger with its firings; the sessions it started keep running.",
	"listFirings":   "List a trigger's firings, newest first.",
	"getOpenAPI":    "This document.",
	"endSession": `End an idle session completed or canceled. The body is {"reason": "completed"} or {"reason": "canceled"}. ` +
		"The route asks session.read, so a caller who may not read the session hears not_found, and refuses a running or ended session as conflict before it asks session.end; " +
		"a session a runner claims while the authorizer decides is conflict too, and a deny is forbidden.",
	"applyAgent": fmt.Sprintf("Apply an Agent manifest to the agent of the name in the caller's context; a changed spec creates a version. "+
		"The body is one Agent manifest of topos.latere.ai/v1. metadata.name is the agent's identifier, a DNS label equal to the path's name. "+
		"metadata.displayName, optional, is the name a person reads: text on one line of at most %d characters, without control characters, line breaks or bidirectional controls; "+
		"an agent without one is shown by its name. A changed spec creates the next version. A change to the metadata alone (the display name, labels, annotations) creates none: "+
		"it replaces the latest version's metadata, and every later read returns it.", manifest.MaxDisplayName),
	"sendEvent": fmt.Sprintf("The body is one user event, {\"type\", \"payload\"}: user.message, user.interrupt, user.tool_confirmation, user.tool_result or user.answer. "+
		"A user.message's payload holds content, text blocks {\"type\":\"text\",\"text\"} and inline images {\"type\":\"image\",\"image\":{\"media_type\",\"data\"}} (PNG, JPEG, GIF or WebP, base64, at most %d of at most %d bytes each), "+
		"and attachments, files {\"name\",\"media_type\",\"data\"} (base64, at most %d of at most %d bytes each, the name one path segment of at most %d bytes). "+
		"The server stores each file as a blob of the session and records it as {name, media_type, size, blob, path}, path attachments/<event id>/<name>, in a directory of the message's own, and no two files of one message share a name; "+
		"the runner writes it at that path in the working directory when the session's machine opens, or before the next step when it is open, and the model reads the paths in the message. "+
		"An image reaches a model whose figures say it takes images, and is a note that it cannot see it otherwise. An image or a file past its limit is attachment_too_large; the body is at most %d bytes. "+
		"The authorizer's allow of a message, a confirmation or a result may name another model than the one the session runs: session.model_changed {by, old, new} is then appended straight before the event, its by the service {subject: %s, kind: service} and not the sender, "+
		"the session's model is the new one with the name it was asked by as via, and the turn the event starts runs on it. A turn already running keeps its model. A model the installation does not run refuses the send as model_unknown or model_unavailable. "+
		"A user.tool_confirmation, a user.tool_result and a user.answer each answer one call that waits for exactly that answer, and are appended only after the log they were checked against: "+
		"of two sent at once one is appended and the other is conflict, and one that names a call nothing waits on, or one something else answered, is conflict. "+
		"A person's user.message denies every call that waits for a confirmation, with the message's text as the person's note, and closes an open question in place of an answer. "+
		AnswerRules,
		MaxImages, MaxImageBytes, MaxAttachments, MaxAttachmentBytes, MaxAttachmentName, MaxEventBody, session.AuthorizerSubject),
	"applyTrigger": fmt.Sprintf("Apply a Trigger manifest to the caller's own trigger of the name; the caller becomes its owner. "+
		"The body is one Trigger manifest of topos.latere.ai/v1. It fires on spec.schedule, a five-field cron expression or @hourly, @daily, @weekly read in spec.timeZone, "+
		"or on the events spec.on selects: product exactly, verbs and resources each exact or a prefix ending in *, and match rules {path, in} on the payload. "+
		"spec.session.message, title and key, and a repository's url and ref, are templates of {{event.*}}, {{trigger.id}}, {{trigger.name}}, {{firing.id}} and {{firing.time}}; "+
		"each value is cut at %d bytes and a message past %d bytes refuses its firing. spec.session.policy new starts a session per firing, skipped while the key's session is active under skipIfActive; "+
		"continue sends each firing to the key's open session, held while it waits for a person. At most spec.maxActive sessions, %d by default and %d at most, are active at once. "+
		"The caller becomes the trigger's owner, the initiator of every session it starts, and a firing is asked of the authorizer as the owner in the context the caller's token names.",
		trigger.MaxValueBytes, trigger.MaxMessageBytes, trigger.DefaultMaxActive, trigger.MaxActiveCeiling),
	"fireTrigger": fmt.Sprintf("An event trigger takes one envelope {id, product, verb, resource, actor, subject, time, payload}: id is the producer's id of the delivery, at most %d characters, unique per product, "+
		"product lowercase, time RFC 3339, payload an object. A schedule trigger takes an empty body and fires now; send an Idempotency-Key to fire it once. "+
		"The answer is the firing {id, trigger_id, origin, event, key, outcome, reason, session_id, received_at}, where outcome is started, continued, held, filtered, skipped_active, skipped_busy, skipped_late, refused or failed. "+
		"A redelivery of an event answers its first firing and starts nothing; a failed firing answers 503 and a redelivery runs it again. An event outside spec.on is filtered, counted and stored nowhere. "+
		"A suspended trigger answers conflict. The firing's session.create and session.send are asked as the trigger's owner.", trigger.MaxEventID),
	"streamEvents": fmt.Sprintf("Server-Sent Events: every event of the log from from_seq, or after the sequence a Last-Event-ID header names, then each new one as it is appended, from any replica, "+
		"each a frame of id: <seq>, event: <type> and data: <the event's JSON>. A comment line is sent every %d seconds, and the stream closes after the event that ends the session. "+
		"With deltas=1 the stream also carries the session's live output while a response arrives, best effort: frames of event: delta whose data is a Delta, with no id, "+
		"so a reconnect with the browser's last event id resumes the log where it was. A delta is never appended and never replayed. A subject holds at most %d streams open at once on one replica; the next is rate_limited.",
		int(DefaultHeartbeat.Seconds()), StreamsPerSubject),
	"forkSession": "Start a new session from a session's log at a turn boundary, an ended or expired session included. " +
		"The body is {\"at_seq\": N, \"attended\": true}, or empty. attended is the fork's own declaration that a person answers its questions, as at a create, absent false; " +
		"a fork made while a question is open copies the open call, and the fork's first message closes it in place of an answer. at_seq is the sequence of a turn boundary, a session.status of the session's own thread that is idle, whatever its stop reason, " +
		"or that is ended completed straight after the thread's running, the end of the turn that ended a session created with end_on_idle; absent, the last boundary, which for a session ended while idle is that idle. " +
		"Another sequence, or a session that never finished a turn, as one ended failed, canceled or expired while its only turn ran, is invalid_fork_point. The answer is the new Session, 201: a new id, parent {session_id, seq}, the same agent version, repositories and capture, " +
		"the forked session's title marked as its continuation (\"Notes\" gives \"Notes (continued)\", which gives \"Notes (continued 2)\"; no title gives none), " +
		"status idle with the stop reason at the fork point, end_turn at an end, a lifetime and a budget of its own from now, the caller as initiator, " +
		"and the model the forked session ran at the fork point, its via included, which is the model the fork is checked by. " +
		"Its log starts as a copy of events 1 to seq, ids included, a fork point that is an end copied as idle end_turn, with every blob they name, " +
		"so its first turn has the forked session's history; its spend starts at what the copied model requests cost. The fork point's checkpoint is restored into its working directory when its first machine opens and the runner can reach it, " +
		"recorded as session.machine reason restored. The route asks session.read, so a caller who may not read the session hears not_found, then session.fork with the fields of a create for the new session and owner, parent and seq of the forked one. " +
		"A session of an archived agent is conflict. " + ForkKeptFiles,
	"getSessionSummary": `The answer is {"sessions": {"running", "waiting_for_approval", "idle", "ended"}, "agents"}, counts of the sessions GET /sessions would list for the caller under the same agent, runner and archived filters, archived sessions left out unless archived asks for them. ` +
		"The four counts are disjoint: running and ended are the sessions of that status, waiting_for_approval the idle sessions whose stop_reason is tool_confirmation, where a call or an approval waits for a person, and idle every other idle session, " +
		"those idle on question, where a question waits for a person's answer, among them; " +
		"agents is the number of distinct agents among the sessions counted. The route asks session.list with the list's fields and applies the owners its decision narrows to, as the list does; an agent name the caller holds no agent of answers every count zero.",
	"archiveSession": "The body is empty. An ended session gets archived_at and leaves the lists unless they ask for archived sessions; it stays readable, streamable and forkable by id, and nothing is appended to its log. " +
		"An idle or running session is conflict: end it first. Archiving an archived session keeps its archived_at. The route asks session.read, then, of an ended session, session.update with session_id and archived true; a deny is forbidden.",
	"unarchiveSession": "The body is empty. The session's archived_at is cleared and it returns to the lists; a session that is not archived is answered as it is. The route asks session.read, then session.update with session_id and archived false; a deny is forbidden.",
	"updateSession": fmt.Sprintf(`The body is {"model": {"name": "<model>", "effort": "<effort>"}, "policy": {"mode": "<mode>"}}: the model the session's next turn runs and its reasoning effort, either member or both, and the approval mode its next steps decide calls under, one of %s. `+
		"The body names model, policy or both; any other member is refused. "+
		"A member left out keeps what the session runs. effort is one of %s, or empty to return to the agent's own; it holds across a change of the model, and a model that takes no reasoning effort ignores it. "+
		"The agent's own model's name is the agent's spec.model as it names it, and any other name is that model through the installation's model connection. "+
		"The authorizer is asked session.update with session_id, model when the body names one, and effort, the effort the next turn runs at, when the body names one, and a deny is forbidden. "+
		"Its allow may name the model to run in place of the one the body names: the session then runs that model, and its model's via is the name the body asked, which a client that offers the choice shows; via is absent when the session runs the name asked. "+
		"The model that runs is checked after the authorizer is asked: one no source gives an input window and an output limit is model_unknown, and a gateway that does not answer model_unavailable. "+
		"An allowed change appends session.model_changed {by, old, new}, each {name, via, effort}, and answers the Session, whose model is the new one; a change to the model and the effort the session runs appends nothing. "+
		"A turn already running keeps its model and its effort: the change takes effect at the next turn. "+
		"A change of the mode asks the same session.update with approval_mode, current_approval_mode, the mode the session runs, and agent_approval_mode, the mode its agent names and the session started in. "+
		"It appends session.policy_changed {by, old, new}, each {mode}, in the same batch as a change of the model, and the Session's policy.mode is the new one; its lists and thresholds do not change. "+
		"The mode holds from the next step, in the turn that runs: a call decided before keeps its verdict, and a call waiting for a confirmation keeps waiting for the person's answer whichever way the mode moved. "+
		"A thread runs no looser than the modes its own agents name, and a host with no operating-system sandbox decides progressive as confirm.", strings.Join(modes, ", "), strings.Join(v1.Efforts, ", ")),
}

// operation is one route as the document describes it, with shown as
// the example of its bodies. x-topos-actions names the questions the
// route asks the authorizer.
func operation(rt route, shown example) (yaml.MapSlice, error) {
	op := yaml.MapSlice{{Key: "operationId", Value: rt.op}, {Key: "summary", Value: rt.summary}, {Key: "description", Value: opDescriptions[rt.op]}}
	if len(rt.actions) > 0 {
		op = append(op, yaml.MapItem{Key: "x-topos-actions", Value: rt.actions})
	} else {
		op = append(op, yaml.MapItem{Key: "security", Value: []yaml.MapSlice{}})
	}
	var params []yaml.MapSlice
	for _, m := range pathParam.FindAllStringSubmatch(rt.path, -1) {
		param := yaml.MapSlice{{Key: "name", Value: m[1]}, {Key: "in", Value: "path"}, {Key: "required", Value: true}}
		if d, ok := paramDescriptions[m[1]]; ok {
			param = append(param, yaml.MapItem{Key: "description", Value: d})
		}
		params = append(params, append(param, yaml.MapItem{Key: "schema", Value: yaml.MapSlice{{Key: "type", Value: "string"}}}))
	}
	params = append(params, queryParams[rt.op]...)
	if len(params) > 0 {
		op = append(op, yaml.MapItem{Key: "parameters", Value: params})
	}
	// A route that reads a body takes one, which the document shows by
	// its example, but for the routes whose body is empty.
	if rt.body > 0 && !emptyBodies[rt.op] {
		v, err := jsonExample(shown.request)
		if err != nil {
			return nil, err
		}
		media := yaml.MapSlice{{Key: mediaJSON, Value: yaml.MapSlice{{Key: "example", Value: v}}}}
		if shown.manifest != "" {
			media = append(media, yaml.MapItem{Key: mediaYAML, Value: yaml.MapSlice{{Key: "example", Value: shown.manifest}}})
		}
		op = append(op, yaml.MapItem{Key: "requestBody", Value: yaml.MapSlice{{Key: "content", Value: media}}})
	}
	ok := yaml.MapSlice{{Key: "description", Value: http.StatusText(rt.status)}}
	if rt.creates {
		ok = yaml.MapSlice{{Key: "description", Value: answerExisted}}
	}
	if rt.status != http.StatusNoContent {
		content, err := answered(rt.op, shown.response)
		if err != nil {
			return nil, err
		}
		ok = append(ok, yaml.MapItem{Key: "content", Value: content})
	}
	responses := yaml.MapSlice{{Key: strconv.Itoa(rt.status), Value: ok}}
	if rt.creates {
		content, err := answered(rt.op, shown.created)
		if err != nil {
			return nil, err
		}
		responses = append(responses, yaml.MapItem{Key: strconv.Itoa(http.StatusCreated), Value: yaml.MapSlice{{Key: "description", Value: answerCreated}, {Key: "content", Value: content}}})
	}
	responses = append(responses, yaml.MapItem{Key: "default", Value: yaml.MapSlice{{Key: "$ref", Value: "#/components/responses/Error"}}})
	return append(op, yaml.MapItem{Key: "responses", Value: responses}), nil
}

// answered is the content of a success answer of op whose body is
// body: JSON with its example as a value, and for the answers of
// another media type a string, its example the text as it is on the
// wire, or a blob's bytes, which have no example.
func answered(op, body string) (yaml.MapSlice, error) {
	switch media := answerMedia[op]; media {
	case "":
		v, err := jsonExample(body)
		if err != nil {
			return nil, err
		}
		return yaml.MapSlice{{Key: mediaJSON, Value: yaml.MapSlice{{Key: "example", Value: v}}}}, nil
	case mediaBytes:
		return yaml.MapSlice{{Key: media, Value: yaml.MapSlice{{Key: "schema", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "format", Value: "binary"}}}}}}, nil
	case mediaStream:
		return yaml.MapSlice{{Key: media, Value: yaml.MapSlice{
			{Key: "schema", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "description", Value: eventStreams[op]}}},
			{Key: "example", Value: body},
		}}}, nil
	default:
		return yaml.MapSlice{{Key: media, Value: yaml.MapSlice{
			{Key: "schema", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "description", Value: "The document. The example is its first lines."}}},
			{Key: "example", Value: body},
		}}}, nil
	}
}

// jsonExample reads a JSON body as the value the document shows, its
// members in the order the body has them.
func jsonExample(body string) (any, error) {
	if body == "" {
		return nil, errors.New("no example")
	}
	var v any
	if err := yaml.UnmarshalWithOptions([]byte(body), &v, yaml.UseOrderedMap()); err != nil {
		return nil, err
	}
	return v, nil
}

// errorResponses is the one error response every route may answer, with
// the codes each status carries.
func errorResponses(statuses map[int][]string) yaml.MapSlice {
	var ss []int
	for s := range statuses {
		ss = append(ss, s)
	}
	slices.Sort(ss)
	var lines []string
	for _, s := range ss {
		cs := statuses[s]
		slices.Sort(cs)
		lines = append(lines, strconv.Itoa(s)+": "+strings.Join(cs, ", "))
	}
	return yaml.MapSlice{{Key: "Error", Value: yaml.MapSlice{
		{Key: "description", Value: "An error of the table. By status: " + strings.Join(lines, "; ") + "."},
		{Key: "content", Value: yaml.MapSlice{{Key: "application/json", Value: yaml.MapSlice{
			{Key: "schema", Value: yaml.MapSlice{{Key: "$ref", Value: "#/components/schemas/Error"}}},
		}}}},
	}}}
}

// openAPI is GET /openapi.yaml, with this installation's API root as
// its server, so every path of the document joined to it is the address
// a client outside reaches.
func (c *call) openAPI() error {
	b, err := OpenAPI(c.s.rootURL)
	if err != nil {
		return err
	}
	c.w.Header().Set("Content-Type", "application/yaml")
	c.w.WriteHeader(http.StatusOK)
	_, err = c.w.Write(b)
	return err
}
