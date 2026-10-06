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
	// created is the JSON body of the answer of a route that creates
	// its object, when it does.
	created string
}

// The examples follow one agent through the API. A person applies the
// agent release-notes, which creates it, and applies it again after it
// was changed, which makes its third version. They have a session with
// it: one turn, a change of the model, a second message, a fork, the end
// and the archive. A trigger of the agent, applied the same two times,
// then starts a session when a release is published. Every response is
// what its route answers to the request beside it at that point, built
// from the type the handler encodes, with these ids, this caller and
// these times in the place of minted ones.
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

// exampleQuestionInput is a question call's input, as a question's
// agent.tool_use holds it, and exampleAnswerBody a send that answers it.
var (
	exampleQuestionInput = mustText(session.QuestionInput{Questions: []session.Question{
		{Header: "Storage", Question: "Which database should the new service keep its records in?", Options: []session.QuestionOption{
			{Label: "Postgres", Recommended: true, Description: "The cluster the other services use. One more schema, no new operations work."},
			{Label: "SQLite", Description: "A file beside the binary. Nothing to operate, and one writer at a time.", Preview: []string{"data/", "  service.db", "  service.db-wal"}},
		}},
		{Header: "Regions", Multiple: true, Question: "Which regions does the first release serve?", Options: []session.QuestionOption{
			{Label: "Europe", Description: "Where the current customers are."},
			{Label: "North America", Description: "Two prospects asked for it."},
		}},
	}})
	exampleAnswerBody = mustText(sendBody{Type: session.TypeUserAnswer, Payload: json.RawMessage(mustText(answerPayload{
		ToolUseID: exampleQuestionCall,
		Answers:   []session.AnswerEntry{{Selected: []string{"SQLite"}, Text: "we have nobody to run a second schema"}, {}},
	}))})
)

// exampleQuestionCall is the tool_use_id of the example's question call.
const exampleQuestionCall = "toolu_01"

// answerPayload is a user.answer's payload as a client sends it: the
// server sets its sender.
type answerPayload struct {
	ToolUseID string                `json:"tool_use_id"`
	Answers   []session.AnswerEntry `json:"answers"`
}

// mustText renders an example value, which is a value of this package and
// always encodes; a failure is a defect the document's tests catch.
func mustText(v any) string {
	b, err := session.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// exampleTime is a time of the examples' day.
func exampleTime(hour, minute, second int) time.Time {
	return time.Date(2026, time.September, 22, hour, minute, second, 0, time.UTC)
}

// examples builds the example of every route that takes or answers a
// body, by operation.
func examples() (map[string]example, error) {
	var failed error
	text := func(v any) string {
		v, err := asAnswer(v)
		if err != nil && failed == nil {
			failed = err
		}
		b, err := session.Marshal(v)
		if err != nil && failed == nil {
			failed = err
		}
		return string(b)
	}
	list := func(item any, last string) string { return text(page{Items: []any{item}, NextCursor: last}) }

	// The agent as the apply that creates it answers it, and as the apply
	// that changes it back does: a third version of the first's digest.
	var agent v1.Agent
	if err := json.Unmarshal([]byte(exampleAgentResolved), &agent); err != nil {
		return nil, err
	}
	agent.Status = v1.Status{ID: exampleAgentID, Version: 1, Digest: exampleDigest, CreatedAt: exampleTime(12, 0, 0)}
	newAgent := agent
	agent.Status.Version, agent.Status.CreatedAt = 3, exampleTime(12, 2, 0)
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
		Attended:  true,
		Network:   &session.Network{Mode: session.NetworkAllowlist, Source: session.NetworkFromAgent},
		CreatedAt: exampleTime(12, 5, 0),
		UpdatedAt: exampleTime(12, 5, 0),
		ExpiresAt: exampleTime(12, 5, 0).Add(session.DefaultMaxAge),
	}
	turned := created
	turned.StopReason, turned.Turn, turned.LastSeq = session.StopEndTurn, 1, 5
	turned.Budget.SpentCostUSDMicro, turned.UpdatedAt = 1200, exampleTime(12, 5, 41)
	switched := turned
	switched.Model, switched.LastSeq, switched.UpdatedAt = &session.ModelRef{Name: "claude-sonnet-4-5", Effort: "high"}, 6, exampleTime(12, 10, 0)
	// The change of the model and the mode in one body appends the two
	// events in one batch (spec 041).
	updated := switched
	updated.Policy = &session.Policy{Mode: v1.ModeProgressive, Thresholds: created.Policy.Thresholds}
	updated.LastSeq = 7
	progressive := v1.ModeProgressive
	// The fork edits the message sent after the first turn: it copies the
	// log before it, the model change among it, and runs on the edited
	// message under the same title (spec 054).
	forked := turned
	forked.ID, forked.Title, forked.Model = exampleForkID, "Notes for v1.4.0", switched.Model
	forked.Parent, forked.Root, forked.LastSeq = &session.Parent{SessionID: exampleSessionID, Seq: 7}, exampleSessionID, 8
	forked.Budget.CarriedCostUSDMicro = turned.Budget.SpentCostUSDMicro
	forked.CreatedAt, forked.UpdatedAt, forked.ExpiresAt = exampleTime(12, 15, 0), exampleTime(12, 15, 0), exampleTime(12, 15, 0).Add(session.DefaultMaxAge)
	ended := updated
	ended.Status, ended.StopReason, ended.LastSeq, ended.UpdatedAt = session.StatusEnded, session.StopCompleted, 9, exampleTime(12, 20, 0)
	filed := ended
	filed.ArchivedAt = new(exampleTime(12, 25, 0))
	// A session of the agent that stopped on its budget and is resumed
	// with a higher one.
	resumed := created
	resumed.ID, resumed.Title, resumed.StopReason, resumed.Turn, resumed.LastSeq, resumed.Attended = exampleStoppedID, "", session.StopBudget, 1, 5, false
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
	sent := event(exampleSentEventID, 8, session.TypeUserMessage, exampleTime(12, 12, 0), 0,
		session.UserMessage{Sender: person, Content: said("Add a section for the breaking changes.")})
	// A search for the notes finds the session by its answer and its first
	// message.
	notes, err := session.ParseQuery("release notes")
	if err != nil {
		return nil, err
	}
	var frames bytes.Buffer
	for _, ev := range []session.Event{first, second, third} {
		ev, err := eventAnswer(ev)
		if err != nil {
			return nil, err
		}
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
	newTrigger := applied
	applied.Status.Version, applied.Status.CreatedAt = 3, exampleTime(12, 32, 0)
	fired := applied
	fired.Status.LastFiredAt, fired.Status.LastSessionID, fired.Status.Counts = new(exampleTime(12, 40, 0)), exampleFiredID, &v1.TriggerCounts{Started: 2}
	delivery := trigger.Envelope{ID: exampleDelivery, Product: "github", Verb: "release.published", Resource: "example/widgets@v1.4.0", Actor: "ann",
		Time: exampleTime(12, 40, 0), Payload: json.RawMessage(`{"tag":"v1.4.0"}`)}
	started := firing{ID: exampleFiringID, TriggerID: exampleTriggerID, Origin: store.OriginEvent,
		Event: &firingEvent{Product: delivery.Product, Verb: delivery.Verb, Resource: delivery.Resource, ID: delivery.ID},
		Key:   delivery.Resource, Outcome: store.OutcomeStarted, SessionID: exampleFiredID, ReceivedAt: exampleTime(12, 40, 0)}

	out := map[string]example{
		"applyAgent":        {request: exampleAgentManifest, manifest: exampleAgentYAML, response: text(agent), created: text(newAgent)},
		"listAgents":        {response: list(agent, store.Cursor(exampleAgentID))},
		"getAgent":          {response: text(agent)},
		"listAgentVersions": {response: list(agentVersion{Version: 1, Digest: exampleDigest, CreatedBy: exampleCaller, CreatedAt: exampleTime(12, 0, 0)}, store.Cursor("1"))},
		"getAgentVersion":   {response: text(agent)},
		"archiveAgent":      {request: text(archiveBody{Permanent: true}), response: text(archived)},
		"createSession":     {request: text(createBody{Agent: agent.Metadata.Name, Title: created.Title, Message: "Write the release notes for v1.4.0.", Attended: true}), response: text(created)},
		"listSessions":      {response: list(turned, exampleSessionID)},
		"getSessionSummary": {response: text(session.Summary{Sessions: session.Counts{Idle: 2}, Agents: 1})},
		"searchSessions":    {response: list(session.SearchResult{Session: turned, Matches: []session.Match{session.MatchOf(third, notes), session.MatchOf(first, notes)}}, "")},
		"getSession":        {response: text(turned)},
		"updateSession":     {request: text(updateBody{Model: &modelChange{Name: &switched.Model.Name, Reasoning: &switched.Model.Effort}, Policy: &policyChange{Mode: &progressive}}), response: text(updated)},
		"endSession":        {request: text(endBody{Reason: session.StopCompleted}), response: text(ended)},
		"forkSession": {request: text(forkBody{BeforeSeq: new(forked.Parent.Seq + 1), Title: &forked.Title, Attended: true,
			Message: &messageBody{Content: said("Add a section for the breaking changes, and one for the fixes.")}}), response: text(forked)},
		"archiveSession":   {response: text(filed)},
		"unarchiveSession": {response: text(ended)},
		"resumeSession":    {request: text(resumeBody{Reason: "budget_raised", MaxCostUSDMicro: resumed.Budget.MaxCostUSDMicro}), response: text(resumed)},
		"listEvents":       {response: list(first, cursorSeq(first.Seq))},
		"sendEvent":        {request: text(sendBody{Type: session.TypeUserMessage, Payload: json.RawMessage(text(messageBody{Content: said("Add a section for the breaking changes.")}))}), response: text(sent)},
		"streamEvents":     {response: frames.String()},
		"redactEvent":      {request: text(redactBody{Reason: "The message held an access token."})},
		"applyTrigger":     {request: exampleTriggerManifest, manifest: exampleTriggerYAML, response: text(applied), created: text(newTrigger)},
		"listTriggers":     {response: list(applied, store.Cursor(exampleTriggerID))},
		"getTrigger":       {response: text(fired)},
		"fireTrigger":      {request: text(delivery), response: text(started)},
		"listFirings":      {response: list(started, store.Cursor(exampleFiringID))},
	}
	return out, failed
}
