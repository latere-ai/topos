// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// ToolQuestion is the name of the tool that puts a decision to a person
// (spec 039). It is a harness tool, as spawn, message and advisor are:
// the harness adds it to the registry of the session's own thread when
// Config.Question is set, and no thread a spawn starts holds it.
const ToolQuestion = session.ToolQuestion

// QuestionSchema is the input schema of the question tool, with every
// bound stated from session's constants, so the schema cannot differ
// from what the server checks an answer against. The API document shows
// it as the input a question's agent.tool_use holds.
var QuestionSchema = json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "questions": {
      "type": "array",
      "minItems": 1,
      "maxItems": %d,
      "description": "The questions of this call, shown to the person together.",
      "items": {
        "type": "object",
        "properties": {
          "question": {"type": "string", "minLength": 1, "maxLength": %d, "description": "The full question, answerable without reading the conversation."},
          "header": {"type": "string", "minLength": 1, "maxLength": %d, "description": "A short label for the question, such as a tab's title."},
          "options": {
            "type": "array",
            "minItems": %d,
            "maxItems": %d,
            "description": "The choices, in the order they are shown, the one you would take first.",
            "items": {
              "type": "object",
              "properties": {
                "label": {"type": "string", "minLength": 1, "maxLength": %d, "description": "The option's name, unique in its question."},
                "description": {"type": "string", "minLength": 1, "maxLength": %d, "description": "What choosing the option means: what follows from it and what it costs."},
                "preview": {
                  "type": "array",
                  "maxItems": %d,
                  "items": {"type": "string", "maxLength": %d},
                  "description": "A mockup or a snippet as lines of plain text, shown in a monospace font. Only on a question that takes one option."
                },
                "recommended": {"type": "boolean", "description": "True on the option you would take."}
              },
              "required": ["label", "description"],
              "additionalProperties": false
            }
          },
          "multiple": {"type": "boolean", "description": "True when the person may choose several options."}
        },
        "required": ["question", "header", "options"],
        "additionalProperties": false
      }
    }
  },
  "required": ["questions"],
  "additionalProperties": false
}`,
	session.MaxQuestions, session.MaxQuestionLength, session.MaxQuestionHeaderLength,
	session.MinQuestionOptions, session.MaxQuestionOptions,
	session.MaxOptionLabelLength, session.MaxOptionDescriptionLength,
	session.MaxOptionPreviewLines, session.MaxOptionPreviewColumns))

// questionTool is the question tool of one turn of the session's own
// thread.
type questionTool struct{ t *turn }

func (q questionTool) Definition() tools.Definition {
	return tools.Definition{Name: ToolQuestion, Description: prompts.Text(prompts.ToolQuestion), InputSchema: QuestionSchema}
}

// Properties: the call changes nothing, so it scores 0.0 and is allowed
// in every mode, plan included, and it runs beside the step's other
// calls.
func (q questionTool) Properties() tools.Properties {
	return tools.Properties{Parallel: true, Effect: tools.EffectNone}
}

// Run does one of two things. In a session nobody attends it answers at
// once: nobody will answer, so the model decides and states what it
// assumed. In an attended session it returns the pause a thread's wait
// returns: the call keeps no result, the step's other calls run on, and
// the session goes idle with the stop reason question.
func (q questionTool) Run(context.Context, tools.Call) (tools.Result, error) {
	if !q.t.s.Attended {
		return unattended(), nil
	}
	return tools.Result{}, &errPause{reason: session.StopQuestion}
}

// unattended is the result of a question in a session nobody attends.
func unattended() tools.Result {
	res := tools.Text(tools.OutcomeUnanswered, prompts.Text(prompts.QuestionUnattended))
	res.Meta = &tools.Meta{ClosedBy: session.ClosedByUnattended}
	return res
}

// isQuestion reports whether a recorded call is a question: the name,
// on a call no client runs. An agent version stored before the name was
// reserved may declare a client tool of the name, which is that
// client's.
func isQuestion(use session.AgentToolUse) bool {
	return use.Name == ToolQuestion && !use.Client
}

// checkQuestion applies the two rules the schema cannot state (spec 039)
// to a call the schema accepted, and returns the answer of a call that
// breaks one: a step takes one question call, so MaxQuestions bounds
// what a person is shown at once, and no label is used twice in one
// question, since an answer names an option by its label. asked reports
// that the step already made a question call that is asked.
func checkQuestion(input []byte, asked bool) *tools.Result {
	if asked {
		res := tools.Text(tools.OutcomeInvalidInput, prompts.Render(prompts.QuestionSecondCall, prompts.Data{"Max": session.MaxQuestions}))
		return &res
	}
	var in session.QuestionInput
	if err := json.Unmarshal(input, &in); err != nil {
		// The registry validated the input against the schema, so this is
		// an input that bypassed validation, answered all the same.
		res := tools.Text(tools.OutcomeInvalidInput, prompts.Render(prompts.CallFailed, prompts.Data{"Error": err.Error()}))
		return &res
	}
	for i, q := range in.Questions {
		seen := map[string]bool{}
		for _, o := range q.Options {
			if seen[o.Label] {
				res := tools.Text(tools.OutcomeInvalidInput, prompts.Render(prompts.QuestionDuplicateLabel, prompts.Data{"Number": i + 1, "Label": o.Label}))
				return &res
			}
			seen[o.Label] = true
		}
	}
	return nil
}

// withoutConfirm is the policy a question is decided under: the
// session's, with no always_confirm pattern of the question tool. A
// question is itself put to a person, so it is never held for a
// confirmation first.
func withoutConfirm(p Policy) Policy {
	p.AlwaysConfirm = slices.DeleteFunc(slices.Clone(p.AlwaysConfirm), func(pattern string) bool {
		tool, _, _ := strings.Cut(pattern, "(")
		return tool == ToolQuestion
	})
	return p
}

// questionResult renders the tool.result of a question call from what
// closed it (spec 039): the answer as the model reads it, with the
// outcome and, in its meta, the cause and the id of the closing event.
// It reads the log alone, so any runner renders the same bytes.
func questionResult(in session.QuestionInput, c session.Closing) tools.Result {
	outcome, text := tools.OutcomeUnanswered, ""
	switch c.By {
	case session.ClosedByAnswer:
		var a session.UserAnswer
		switch {
		case c.Event.Redacted():
			text = prompts.Text(prompts.QuestionRemoved)
		case c.Event.Decode(&a) != nil || !a.Answered():
			text = prompts.Text(prompts.QuestionLeft)
		default:
			questions := make([]prompts.Data, len(in.Questions))
			for i, q := range in.Questions {
				// The server holds an answer to one entry per question; a
				// question without one is left to the agent.
				var e session.AnswerEntry
				if i < len(a.Answers) {
					e = a.Answers[i]
				}
				questions[i] = prompts.Data{
					"Number": i + 1, "Header": q.Header, "Question": q.Question,
					"Chosen": strings.Join(e.Selected, ", "), "Text": e.Text, "Left": e.Empty(),
				}
			}
			outcome, text = tools.OutcomeOK, prompts.Render(prompts.QuestionAnswered, prompts.Data{"Questions": questions})
		}
	case session.ClosedByInterrupt:
		outcome, text = tools.OutcomeCanceled, prompts.Text(prompts.QuestionCanceled)
	default:
		text = prompts.Text(prompts.QuestionMessage)
	}
	res := tools.Text(outcome, text)
	res.Meta = &tools.Meta{ClosedBy: c.By, EventID: c.EventID}
	return res
}

// settleQuestion appends the result of the question call id when an
// event closed it and its result is not in the log, and returns what
// closed it: empty for a call that is still open, or that the log does
// not hold.
func (t *turn) settleQuestion(ctx context.Context, id string) (string, error) {
	for _, a := range session.Questions(t.events()) {
		if a.ToolUseID != id {
			continue
		}
		if !a.Closing.Owed() {
			return "", nil
		}
		return a.Closing.By, t.result(ctx, id, questionResult(a.Input, a.Closing), 0)
	}
	return "", nil
}
