// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/manifest/trigger"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// example is the bodies the document shows for one route.
type example struct {
	// request is the JSON body a caller sends, and manifest the same
	// document as YAML on a route that takes either.
	request, manifest string
	// response is the JSON body of the route's answer, or the frames of
	// its stream. A route that answers no body, or bytes, has none.
	response string
}

// The examples follow one agent through the API. A person applies the
// agent release-notes and has a session with it: one turn, a change of
// the model, a second message, a fork, the end and the archive. A
// trigger of the agent then starts a session when a release is
// published. Every response is what its route answers to the request
// beside it at that point, built from the type the handler encodes, with
// these ids, this caller and these times in the place of minted ones.
const (
	exampleCaller        = "https://login.example|alice"
	exampleAgentID       = session.PrefixAgent + "01M34FSDG0Q60424XT3D96DRAJ"
	exampleSessionID     = session.PrefixSession + "01M34G2JF01B91B8399CD1G6YP"
	exampleStoppedID     = session.PrefixSession + "01M34FYX90GK6SQ477TKZDF4MH"
	exampleForkID        = session.PrefixSession + "01M34GMWD03E3B7MERD1RGEM7S"
	exampleFiredID       = session.PrefixSession + "01M34J2N81KBW8FS0WCBFSJCGZ"
	exampleFirstEventID  = session.PrefixEvent + "01M34G2JF1P53NNG7DN08D5QAG"
	exampleSecondEventID = session.PrefixEvent + "01M34G2KE8DR262Y3P678HNPM2"
	exampleThirdEventID  = session.PrefixEvent + "01M34G3SH0V6283G9W516VXR87"
	exampleSentEventID   = session.PrefixEvent + "01M34GFCM0MP2SWWWVFVJH1PW2"
	exampleTriggerID     = session.PrefixTrigger + "01M34HGBA0JR98WKKXWZGAX6VA"
	exampleFiringID      = session.PrefixFiring + "01M34J2N80QKPCFHVXPHHQRHGQ"
	exampleDelivery      = "123e4567-e89b-12d3-a456-426614174000"

	exampleFirstDigest   = "sha256:5cdca0a8167efffd9c058b14bffaaa39ad4473ed001e29f8fbd160e951e80681"
	exampleDigest        = "sha256:dac7fd51151199dc6af109f5da3680a4e7485daf2bc6191a047261ae75933aa9"
	exampleBundle        = "sha256:b5790eaab58e675da4a3732b7a53afea7b61c87bc3810b06772c64fae9f986ec"
	exampleTriggerDigest = "sha256:cd61c91df8739587137a731afc9dbd615baa74b314f2e8ece6854065790db151"
)

// exampleAgentManifest is the Agent a caller applies, as JSON and as
// YAML, and exampleAgentResolved what the resolver makes of it, every
// default written out.
const (
	exampleAgentManifest = `{"apiVersion":"topos.latere.ai/v1","kind":"Agent","metadata":{"name":"release-notes","displayName":"Release notes"},` +
		`"spec":{"model":{"name":"claude-haiku-4-5"},"instructions":"Write the release notes for a tag from the pull requests merged since the tag before it.","machine":{"kind":"cella"}}}`
	exampleAgentYAML = `apiVersion: topos.latere.ai/v1
kind: Agent
metadata:
  name: release-notes
  displayName: Release notes
spec:
  model:
    name: claude-haiku-4-5
  instructions: Write the release notes for a tag from the pull requests merged since the tag before it.
  machine:
    kind: cella
`
	exampleAgentResolved = `{"apiVersion":"topos.latere.ai/v1","kind":"Agent","metadata":{"name":"release-notes","displayName":"Release notes"},` +
		`"spec":{"model":{"name":"claude-haiku-4-5"},"instructions":"Write the release notes for a tag from the pull requests merged since the tag before it.",` +
		`"tools":["read","write","edit","bash","grep","glob","web_fetch","todo"],"approvals":{"mode":"confirm","thresholds":{"flagAt":0.3,"askAt":0.5,"blockAt":0.9}},` +
		`"threads":{"maxDepth":2,"maxConcurrent":8},"machine":{"kind":"cella","image":"base"},"limits":{"turnTimeout":"2h","maxAge":"168h"},"context":{"compactAt":0.8}}}`
)

// exampleTriggerManifest is the Trigger a caller applies, as JSON and as
// YAML, and exampleTriggerResolved what the resolver makes of it: the
// agent by its id and every default written out.
const (
	exampleTriggerManifest = `{"apiVersion":"topos.latere.ai/v1","kind":"Trigger","metadata":{"name":"on-release"},` +
		`"spec":{"agent":"release-notes","on":{"product":"github","verbs":["release.published"]},"session":{"message":"Write the release notes for {{event.resource}}."}}}`
	exampleTriggerYAML = `apiVersion: topos.latere.ai/v1
kind: Trigger
metadata:
  name: on-release
spec:
  agent: release-notes
  on:
    product: github
    verbs: [release.published]
  session:
    message: Write the release notes for {{event.resource}}.
`
	exampleTriggerResolved = `{"apiVersion":"topos.latere.ai/v1","kind":"Trigger","metadata":{"name":"on-release"},` +
		`"spec":{"agent":"` + exampleAgentID + `","timeZone":"UTC","on":{"product":"github","verbs":["release.published"]},` +
		`"session":{"message":"Write the release notes for {{event.resource}}.","endOnIdle":true},"skipIfActive":true,"maxAge":"1h","suspend":false}}`
)

// exampleTime is a time of the examples' day.
func exampleTime(hour, minute, second int) time.Time {
	return time.Date(2026, time.September, 22, hour, minute, second, 0, time.UTC)
}

// examples builds the example of every route that takes or answers a
// body, by operation.
func examples() (map[string]example, error) {
	var failed error
	text := func(v any) string {
		b, err := session.Marshal(v)
		if err != nil && failed == nil {
			failed = err
		}
		return string(b)
	}
	list := func(item any, last string) string { return text(page{Items: []any{item}, NextCursor: last}) }

	// The agent at its second version, the one the examples apply.
	var agent v1.Agent
	if err := json.Unmarshal([]byte(exampleAgentResolved), &agent); err != nil {
		return nil, err
	}
	agent.Status = v1.Status{ID: exampleAgentID, Version: 2, Digest: exampleDigest, CreatedAt: exampleTime(12, 2, 0)}
	archived := agent
	archived.Status.ArchivedAt = new(exampleTime(13, 0, 0))

	// The session as its create answers it, and as each later route does.
	created := session.Session{
		Schema:    session.SchemaVersion,
		ID:        exampleSessionID,
		Agent:     session.AgentRef{ID: exampleAgentID, Name: agent.Metadata.Name, Version: agent.Status.Version, Digest: exampleDigest, Bundle: exampleBundle},
		Title:     "Notes for v1.4.0",
		Initiator: session.Sender{Subject: exampleCaller, Kind: session.SenderPerson},
		Runner:    session.RunnerHosted,
		Status:    session.StatusIdle,
		LastSeq:   1,
		Machine:   session.Machine{Kind: session.MachineCella, Image: "base"},
		Policy:    &session.Policy{Mode: "confirm", Thresholds: session.Thresholds{FlagAt: 0.3, AskAt: 0.5, BlockAt: 0.9}},
		Limits:    session.Limits{TurnTimeout: session.DefaultTurnTimeout.String(), MaxAge: session.DefaultMaxAge.String()},
		CreatedAt: exampleTime(12, 5, 0),
		UpdatedAt: exampleTime(12, 5, 0),
		ExpiresAt: exampleTime(12, 5, 0).Add(session.DefaultMaxAge),
	}
	turned := created
	turned.StopReason, turned.Turn, turned.LastSeq = session.StopEndTurn, 1, 5
	turned.Budget.SpentCostUSDMicro, turned.UpdatedAt = 1200, exampleTime(12, 5, 41)
	switched := turned
	switched.Model, switched.LastSeq, switched.UpdatedAt = &session.ModelRef{Name: "claude-sonnet-4-5", Effort: "high"}, 6, exampleTime(12, 10, 0)
	forked := turned
	forked.ID, forked.Title, forked.Parent = exampleForkID, "Notes for v1.4.0 (continued)", &session.Parent{SessionID: exampleSessionID, Seq: 5}
	forked.CreatedAt, forked.UpdatedAt, forked.ExpiresAt = exampleTime(12, 15, 0), exampleTime(12, 15, 0), exampleTime(12, 15, 0).Add(session.DefaultMaxAge)
	ended := switched
	ended.Status, ended.StopReason, ended.LastSeq, ended.UpdatedAt = session.StatusEnded, session.StopCompleted, 8, exampleTime(12, 20, 0)
	filed := ended
	filed.ArchivedAt = new(exampleTime(12, 25, 0))
	// A session of the agent that stopped on its budget and is resumed
	// with a higher one.
	resumed := created
	resumed.ID, resumed.Title, resumed.StopReason, resumed.Turn, resumed.LastSeq = exampleStoppedID, "", session.StopBudget, 1, 5
	resumed.Budget = session.Budget{MaxCostUSDMicro: new(int64(5000000)), SpentCostUSDMicro: 1200}
	resumed.CreatedAt, resumed.UpdatedAt, resumed.ExpiresAt = exampleTime(12, 3, 0), exampleTime(12, 28, 0), exampleTime(12, 3, 0).Add(session.DefaultMaxAge)

	// The log's first events, and the message sent after the first turn.
	person := session.Sender{Subject: exampleCaller, Kind: session.SenderPerson}
	said := func(words string) []lux.Block { return []lux.Block{{Type: ir.BlockText, Text: words}} }
	event := func(id string, seq uint64, typ session.Type, at time.Time, turn int, payload any) session.Event {
		return session.Event{ID: id, Seq: seq, SessionID: exampleSessionID, Type: typ, Time: at, Turn: turn, Payload: json.RawMessage(text(payload))}
	}
	first := event(exampleFirstEventID, 1, session.TypeUserMessage, exampleTime(12, 5, 0), 0,
		session.UserMessage{Sender: person, Content: said("Write the release notes for v1.4.0.")})
	second := event(exampleSecondEventID, 2, session.TypeSessionStatus, exampleTime(12, 5, 1), 1, session.SessionStatus{Status: session.StatusRunning})
	third := event(exampleThirdEventID, 3, session.TypeAgentMessage, exampleTime(12, 5, 40), 1,
		session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: said("The release notes for v1.4.0 are in NOTES.md.")}, StopReason: ir.StopEndTurn})
	sent := event(exampleSentEventID, 7, session.TypeUserMessage, exampleTime(12, 12, 0), 0,
		session.UserMessage{Sender: person, Content: said("Add a section for the breaking changes.")})
	var frames bytes.Buffer
	for _, ev := range []session.Event{first, second, third} {
		if err := frame(&frames, ev); err != nil {
			return nil, err
		}
	}

	// The trigger as its apply answers it, as it reads after it fired,
	// and the firing.
	var applied v1.Trigger
	if err := json.Unmarshal([]byte(exampleTriggerResolved), &applied); err != nil {
		return nil, err
	}
	applied.Status = v1.Status{ID: exampleTriggerID, Version: 1, Digest: exampleTriggerDigest, CreatedAt: exampleTime(12, 30, 0), Counts: &v1.TriggerCounts{}}
	fired := applied
	fired.Status.LastFiredAt, fired.Status.LastSessionID, fired.Status.Counts = new(exampleTime(12, 40, 0)), exampleFiredID, &v1.TriggerCounts{Started: 2}
	delivery := trigger.Envelope{ID: exampleDelivery, Product: "github", Verb: "release.published", Resource: "example/widgets@v1.4.0", Actor: "ann",
		Time: exampleTime(12, 40, 0), Payload: json.RawMessage(`{"tag":"v1.4.0"}`)}
	started := firing{ID: exampleFiringID, TriggerID: exampleTriggerID, Origin: store.OriginEvent,
		Event: &firingEvent{Product: delivery.Product, Verb: delivery.Verb, Resource: delivery.Resource, ID: delivery.ID},
		Key:   delivery.Resource, Outcome: store.OutcomeStarted, SessionID: exampleFiredID, ReceivedAt: exampleTime(12, 40, 0)}

	out := map[string]example{
		"applyAgent":        {request: exampleAgentManifest, manifest: exampleAgentYAML, response: text(agent)},
		"listAgents":        {response: list(agent, store.Cursor(exampleAgentID))},
		"getAgent":          {response: text(agent)},
		"listAgentVersions": {response: list(agentVersion{Version: 1, Digest: exampleFirstDigest, CreatedBy: exampleCaller, CreatedAt: exampleTime(12, 0, 0)}, store.Cursor("1"))},
		"getAgentVersion":   {response: text(agent)},
		"archiveAgent":      {request: text(archiveBody{Permanent: true}), response: text(archived)},
		"createSession":     {request: text(createBody{Agent: agent.Metadata.Name, Title: created.Title, Message: "Write the release notes for v1.4.0."}), response: text(created)},
		"listSessions":      {response: list(turned, exampleSessionID)},
		"getSessionSummary": {response: text(session.Summary{Sessions: session.Counts{Idle: 2}, Agents: 1})},
		"getSession":        {response: text(turned)},
		"updateSession":     {request: text(updateBody{Model: &modelChange{Name: &switched.Model.Name, Effort: &switched.Model.Effort}}), response: text(switched)},
		"endSession":        {request: text(endBody{Reason: session.StopCompleted}), response: text(ended)},
		"forkSession":       {request: text(forkBody{AtSeq: &forked.Parent.Seq}), response: text(forked)},
		"archiveSession":    {response: text(filed)},
		"unarchiveSession":  {response: text(ended)},
		"resumeSession":     {request: text(resumeBody{Reason: "budget_raised", MaxCostUSDMicro: resumed.Budget.MaxCostUSDMicro}), response: text(resumed)},
		"listEvents":        {response: list(first, cursorSeq(first.Seq))},
		"sendEvent":         {request: text(sendBody{Type: session.TypeUserMessage, Payload: json.RawMessage(text(messageBody{Content: said("Add a section for the breaking changes.")}))}), response: text(sent)},
		"streamEvents":      {response: frames.String()},
		"redactEvent":       {request: text(redactBody{Reason: "The message held an access token."})},
		"applyTrigger":      {request: exampleTriggerManifest, manifest: exampleTriggerYAML, response: text(applied)},
		"listTriggers":      {response: list(applied, store.Cursor(exampleTriggerID))},
		"getTrigger":        {response: text(fired)},
		"fireTrigger":       {request: text(delivery), response: text(started)},
		"listFirings":       {response: list(started, store.Cursor(exampleFiringID))},
	}
	return out, failed
}
