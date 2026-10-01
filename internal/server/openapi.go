// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
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

	paths := yaml.MapSlice{}
	byPath := map[string]yaml.MapSlice{}
	var order []string
	for _, rt := range table() {
		if _, ok := byPath[rt.path]; !ok {
			order = append(order, rt.path)
		}
		byPath[rt.path] = append(byPath[rt.path], yaml.MapItem{Key: strings.ToLower(rt.method), Value: operation(rt)})
	}
	for _, p := range order {
		paths = append(paths, yaml.MapItem{Key: p, Value: byPath[p]})
	}
	doc := yaml.MapSlice{
		{Key: "openapi", Value: "3.1.0"},
		{Key: "info", Value: yaml.MapSlice{
			{Key: "title", Value: "Topos API"},
			{Key: "version", Value: "v1"},
			{Key: "description", Value: "Agents, their versions, the sessions people have with them, and the triggers that start and continue sessions on a schedule or on delivered events. Every route is verified and asked of the installation's authorizer; every error is one envelope with a code of the table under components.schemas.Error."},
		}},
		{Key: "servers", Value: []yaml.MapSlice{{{Key: "url", Value: server}}}},
		{Key: "security", Value: []yaml.MapSlice{{{Key: "bearer", Value: []string{}}}}},
		{Key: "paths", Value: paths},
		{Key: "components", Value: yaml.MapSlice{
			{Key: "securitySchemes", Value: yaml.MapSlice{
				{Key: "bearer", Value: yaml.MapSlice{{Key: "type", Value: "http"}, {Key: "scheme", Value: "bearer"}, {Key: "bearerFormat", Value: "JWT"}}},
			}},
			{Key: "schemas", Value: yaml.MapSlice{
				{Key: "Delta", Value: deltaSchema},
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
	}
	return yaml.MarshalWithOptions(doc, yaml.Indent(2), yaml.IndentSequence(true))
}

var pathParam = regexp.MustCompile(`\{([a-z_]+)\}`)

// paramDescriptions say how the API reads a path parameter where a
// client needs to know it: a name is unique within its owner, so a name
// reads among the caller's own objects, and an id reads its object
// whoever owns it.
var paramDescriptions = map[string]string{
	"name": "The name, unique within its owner, the subject that applied it. An apply acts on the caller's own object of the name and creates it when the caller holds none; an object of the name another subject holds is neither read nor changed.",
	"ref":  "An id, which names its object whoever owns it, subject to the authorizer, or a name, read among the caller's own objects. A name only another subject holds answers not_found, as one nobody holds does.",
}

// queryParams are the query parameters of the routes that read any.
var queryParams = map[string][]yaml.MapSlice{
	"listSessions": {
		{{Key: "name", Value: "agent"}, {Key: "in", Value: "query"}, {Key: "description", Value: "An agent's id, or a name among the caller's own agents."},
			{Key: "schema", Value: yaml.MapSlice{{Key: "type", Value: "string"}}}},
		{{Key: "name", Value: "status"}, {Key: "in", Value: "query"},
			{Key: "schema", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "enum", Value: []string{string(session.StatusIdle), string(session.StatusRunning), string(session.StatusEnded)}}}}},
		{{Key: "name", Value: "runner"}, {Key: "in", Value: "query"},
			{Key: "schema", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "enum", Value: []string{session.RunnerHosted, session.RunnerExternal}}}}},
		{{Key: "name", Value: "archived"}, {Key: "in", Value: "query"}, {Key: "description", Value: "false or absent leaves archived sessions out, true lists only them, any lists both."},
			{Key: "schema", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "enum", Value: []string{"false", "true", "any"}}}}},
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

// opDescriptions say what a route's body holds where a client needs more
// than its summary: an apply's manifest names the agent twice, by the
// identifier and by the name a person reads, and only the spec versions.
var opDescriptions = map[string]string{
	"applyAgent": fmt.Sprintf("The body is one Agent manifest of topos.latere.ai/v1. metadata.name is the agent's identifier, a DNS label equal to the path's name. "+
		"metadata.displayName, optional, is the name a person reads: text on one line of at most %d characters, without control characters, line breaks or bidirectional controls; "+
		"an agent without one is shown by its name. A changed spec creates the next version. A change to the metadata alone (the display name, labels, annotations) creates none: "+
		"it replaces the latest version's metadata, and every later read returns it.", manifest.MaxDisplayName),
	"sendEvent": fmt.Sprintf("The body is one user event, {\"type\", \"payload\"}: user.message, user.interrupt, user.tool_confirmation or user.tool_result. "+
		"A user.message's payload holds content, text blocks {\"type\":\"text\",\"text\"} and inline images {\"type\":\"image\",\"image\":{\"media_type\",\"data\"}} (PNG, JPEG, GIF or WebP, base64, at most %d of at most %d bytes each), "+
		"and attachments, files {\"name\",\"media_type\",\"data\"} (base64, at most %d of at most %d bytes each, the name one path segment of at most %d bytes). "+
		"The server stores each file as a blob of the session and records it as {name, media_type, size, blob, path}, path attachments/<event id>/<name>, in a directory of the message's own, and no two files of one message share a name; "+
		"the runner writes it at that path in the working directory when the session's machine opens, or before the next step when it is open, and the model reads the paths in the message. "+
		"An image reaches a model whose figures say it takes images, and is a note that it cannot see it otherwise. An image or a file past its limit is attachment_too_large; the body is at most %d bytes.",
		MaxImages, MaxImageBytes, MaxAttachments, MaxAttachmentBytes, MaxAttachmentName, MaxEventBody),
	"applyTrigger": fmt.Sprintf("The body is one Trigger manifest of topos.latere.ai/v1. It fires on spec.schedule, a five-field cron expression or @hourly, @daily, @weekly read in spec.timeZone, "+
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
	"forkSession": "The body is {\"at_seq\": N}, or empty. at_seq is the sequence of a session.status idle of the session's own thread, the end of a turn; absent, the last one, which for an ended or expired session is the one before its end. " +
		"Another sequence, or a session that never finished a turn, is invalid_fork_point. The answer is the new Session, 201: a new id, parent {session_id, seq}, the same agent version, repositories and capture, " +
		"the forked session's title marked as its continuation (\"Notes\" gives \"Notes (continued)\", which gives \"Notes (continued 2)\"; no title gives none), " +
		"status idle with the stop reason at the fork point, a lifetime and a budget of its own from now, and the caller as initiator. Its log starts as a copy of events 1 to seq, ids included, with every blob they name, " +
		"so its first turn has the forked session's history; its spend starts at what the copied model requests cost. The fork point's checkpoint is restored into its working directory when its first machine opens and the runner can reach it, " +
		"recorded as session.machine reason restored. The route asks session.read, so a caller who may not read the session hears not_found, then session.fork with the fields of a create for the new session and owner, parent and seq of the forked one. " +
		"A session of an archived agent is conflict.",
	"archiveSession": "The body is empty. An ended session gets archived_at and leaves the lists unless they ask for archived sessions; it stays readable, streamable and forkable by id, and nothing is appended to its log. " +
		"An idle or running session is conflict: end it first. Archiving an archived session keeps its archived_at. The route asks session.read, then session.update with session_id and archived true; a deny is forbidden.",
	"unarchiveSession": "The body is empty. The session's archived_at is cleared and it returns to the lists; a session that is not archived is answered as it is. The route asks session.read, then session.update with session_id and archived false; a deny is forbidden.",
	"updateSession": fmt.Sprintf(`The body is {"model": {"name": "<model>", "effort": "<effort>"}}, the model the session's next turn runs and its reasoning effort, either member or both; any other member is refused. `+
		"A member left out keeps what the session runs. effort is one of %s, or empty to return to the agent's own; it holds across a change of the model, and a model that takes no reasoning effort ignores it. "+
		"The agent's own model's name is the agent's spec.model as it names it, and any other name is that model through the installation's model connection. "+
		"A model no source gives an input window and an output limit is model_unknown, and a gateway that does not answer model_unavailable; the authorizer is asked session.update with session_id, model when the body names one, after the model resolved, "+
		"and effort, the effort the next turn runs at, when the body names one, and a deny is forbidden. "+
		"An allowed change appends session.model_changed {by, old, new}, each {name, effort}, and answers the Session, whose model is the new one; a change to the model and the effort the session runs appends nothing. "+
		"A turn already running keeps its model and its effort: the change takes effect at the next turn.", strings.Join(v1.Efforts, ", ")),
}

// operation is one route as the document describes it. x-topos-actions
// names the questions the route asks the authorizer.
func operation(rt route) yaml.MapSlice {
	op := yaml.MapSlice{{Key: "operationId", Value: rt.op}, {Key: "summary", Value: rt.summary}}
	if d, ok := opDescriptions[rt.op]; ok {
		op = append(op, yaml.MapItem{Key: "description", Value: d})
	}
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
	if rt.body > 0 {
		media := yaml.MapSlice{{Key: "application/json", Value: yaml.MapSlice{}}}
		if rt.op == "applyAgent" || rt.op == "applyTrigger" {
			media = append(media, yaml.MapItem{Key: "application/yaml", Value: yaml.MapSlice{}})
		}
		op = append(op, yaml.MapItem{Key: "requestBody", Value: yaml.MapSlice{{Key: "content", Value: media}}})
	}
	ok := yaml.MapSlice{{Key: "description", Value: http.StatusText(rt.status)}}
	if d, stream := eventStreams[rt.op]; stream {
		ok = append(ok, yaml.MapItem{Key: "content", Value: yaml.MapSlice{{Key: "text/event-stream", Value: yaml.MapSlice{
			{Key: "schema", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "description", Value: d}}},
		}}}})
	}
	responses := yaml.MapSlice{{Key: strconv.Itoa(rt.status), Value: ok}}
	responses = append(responses, yaml.MapItem{Key: "default", Value: yaml.MapSlice{{Key: "$ref", Value: "#/components/responses/Error"}}})
	return append(op, yaml.MapItem{Key: "responses", Value: responses})
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
