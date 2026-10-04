// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"

	"github.com/goccy/go-yaml"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/session"
)

// AttendedRule is the sentence the create route states the attended
// field by: what a client declares with it, and what a session that is
// not attended does with a question.
const AttendedRule = "The body's attended, true when a person answers the session's questions through a client that shows them, is the session's from its create on and nothing changes it: " +
	"an agent's question then waits, the session idle with stop_reason question, until a user.answer, a person's user.message or a user.interrupt closes it, holding no runner while it waits; " +
	"absent or false, a question is answered at once with outcome unanswered and the agent decides, so a client that shows no questions never meets one."

// AnswerRules are the send route's sentences on a user.answer, each
// bound stated from its constant, with an example of the body.
var AnswerRules = fmt.Sprintf("A user.answer answers the session's open question, the agent.tool_use named question that nothing closed, of which there is at most one; its payload is a UserAnswer, "+
	"{tool_use_id, answers}, one entry {selected, text} per question in the questions' order: selected the chosen labels, text the person's own words of at most %d characters. "+
	"An entry with text alone answers in the person's words, one with both is a choice and a note on it, an empty entry leaves its question to the agent, and an answer of empty entries leaves every question to it. "+
	"For example %s. It is asked of the authorizer as session.send with event_type user.answer, and its shape is checked before that question, as invalid_request. "+
	"After the allow, an answer that names no open question is conflict, the detail saying what closed the question; one with another number of entries than questions, a label that is no option of its question or one named twice, "+
	"or several labels on a question that does not take several, is invalid_request. Nothing is appended for a refused answer. An answer is final once appended: the reply and the stream carry it, and a correction is a message. "+
	"The runner renders it into the call's tool.result, outcome ok, or unanswered when every entry is empty, with a QuestionResultMeta.",
	session.MaxAnswerTextLength, exampleAnswerBody)

// questionInputSchema is the QuestionInput schema of the document: the
// tool's own input schema, its bounds the constants of package session,
// with the client's rules and an example.
func questionInputSchema() (yaml.MapSlice, error) {
	var schema yaml.MapSlice
	if err := yaml.UnmarshalWithOptions(harness.QuestionSchema, &schema, yaml.UseOrderedMap()); err != nil {
		return nil, fmt.Errorf("server: the question tool's schema: %w", err)
	}
	example, err := jsonExample(exampleQuestionInput)
	if err != nil {
		return nil, err
	}
	return append(yaml.MapSlice{
		{Key: "description", Value: "The input of a question call, as the agent.tool_use named question holds it once the runner checked it: no label is used twice in one question. " +
			"A client shows each question with header as its short label and question in full, the options in the order given, each with label and description; " +
			"marks each option whose recommended is true in its own words, the mark no part of the label; lets the person choose one option, or several where multiple is true, " +
			"and always answer in their own words beside or in place of the options; shows a preview only on a question that takes one option, each item one line of plain text in a monospace font, spaces kept, " +
			"and may collapse it, since the question is answerable without it; and treats every field as text a model wrote: no markup, and no link followed on its own."},
		{Key: "example", Value: example},
	}, schema...), nil
}

// userAnswer is the UserAnswer schema: the payload of user.answer.
var userAnswer = yaml.MapSlice{
	{Key: "type", Value: "object"},
	{Key: "description", Value: "The payload of user.answer, one entry per question of the open question call, in the questions' order. The server sets sender to the caller."},
	{Key: "required", Value: []string{"tool_use_id", "answers"}},
	{Key: "additionalProperties", Value: false},
	{Key: "properties", Value: yaml.MapSlice{
		{Key: "tool_use_id", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "description", Value: "The open question call's tool_use_id."}}},
		{Key: "answers", Value: yaml.MapSlice{
			{Key: "type", Value: "array"},
			{Key: "minItems", Value: 1},
			{Key: "maxItems", Value: session.MaxQuestions},
			{Key: "items", Value: yaml.MapSlice{
				{Key: "type", Value: "object"},
				{Key: "additionalProperties", Value: false},
				{Key: "properties", Value: yaml.MapSlice{
					{Key: "selected", Value: yaml.MapSlice{{Key: "type", Value: "array"}, {Key: "items", Value: yaml.MapSlice{{Key: "type", Value: "string"}}},
						{Key: "description", Value: "The chosen labels: at most one where the question does not take several, each a label of the question and none twice."}}},
					{Key: "text", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "maxLength", Value: session.MaxAnswerTextLength},
						{Key: "description", Value: "The person's own words, in characters."}}},
				}},
			}},
		}},
		{Key: "sender", Value: yaml.MapSlice{{Key: "type", Value: "object"}, {Key: "readOnly", Value: true}, {Key: "description", Value: "The caller, set by the server whatever the body says."}}},
	}},
}

// questionResultMeta is the QuestionResultMeta schema: the meta of a
// question call's tool.result.
var questionResultMeta = yaml.MapSlice{
	{Key: "type", Value: "object"},
	{Key: "description", Value: fmt.Sprintf("The meta of a question call's tool.result, which says why the question closed without reading the result's text. "+
		"The result's outcome is %s when an answer chose an option or held words, %s, which is not an error, when every entry of the answer was empty, a person's message came in place of an answer, or nobody attends the session, "+
		"and %s when an interrupt dismissed the question.", tools.OutcomeOK, tools.OutcomeUnanswered, tools.OutcomeCanceled)},
	{Key: "properties", Value: yaml.MapSlice{
		{Key: "closed_by", Value: yaml.MapSlice{{Key: "type", Value: "string"},
			{Key: "enum", Value: []string{session.ClosedByAnswer, session.ClosedByMessage, session.ClosedByInterrupt, session.ClosedByUnattended}}}},
		{Key: "event_id", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "description", Value: "The id of the event that closed the call; absent when nobody attends the session."}}},
	}},
}
