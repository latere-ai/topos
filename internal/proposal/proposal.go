// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package proposal is the trigger tool of spec 086: it lists the triggers
// the person who started a hosted session holds of the session's agent,
// and proposes a routine on a schedule, which waits for that person's
// allow every time and is applied as theirs once they give it. A routine
// is written in the one shape every client reads, by
// latere.ai/x/sdk-go/agents/routine. The tool holds no credential: the
// session's server asks every question as the session's initiator, over
// the drive's runner.Triggers.
package proposal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"latere.ai/x/sdk-go/agents/routine"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/manifest/trigger"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// Name is the tool's name.
const Name = session.ToolTrigger

// The tool's actions.
const (
	ActionList    = "list"
	ActionPropose = "propose"
)

// schema is the tool's input, its bounds read from their constants.
var schema = json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": [%q, %q], "description": "list to read the triggers the person holds of you; propose to offer a routine, which waits for the person's answer."},
    "name": {"type": "string", "maxLength": %d, "description": "For propose: the routine's name, as the person would call it."},
    "ask": {"type": "string", "description": "For propose: what each run asks, as a message from the person to you, complete on its own."},
    "why": {"type": "string", "maxLength": %d, "description": "For propose: one sentence the person reads that says the schedule back and what they get each time."},
    "schedule": {"type": "string", "description": "For propose: five cron fields of a minute and an hour, every day or on the days of the week listed, such as 0 9 * * 1."},
    "timeZone": {"type": "string", "description": "For propose: the IANA time zone the schedule is read in, such as Europe/Berlin."}
  },
  "required": ["action"],
  "additionalProperties": false
}`, ActionList, ActionPropose, routine.MaxTitle, trigger.MaxPurposeLength))

// confirmPropose is why a proposal waits for a person, as its
// agent.tool_use records it.
const confirmPropose = "makes a routine the person who started this conversation owns, which they approve every time"

// Tool is the trigger tool of one hosted session.
type Tool struct{ t runner.Triggers }

// New is the tool over t, the session's reach to its server's triggers.
func New(t runner.Triggers) *Tool { return &Tool{t: t} }

// Definition is what the model sees.
func (*Tool) Definition() tools.Definition {
	return tools.Definition{Name: Name, Description: prompts.Text(prompts.ToolTrigger), InputSchema: schema}
}

// Properties: one call at a time. A list reads, and a proposal is held
// for its person by Confirm whatever its effect, and blocked in plan mode.
func (*Tool) Properties() tools.Properties { return tools.Properties{Effect: tools.EffectRead} }

// input is a call's input.
type input struct {
	Action   string `json:"action"`
	Name     string `json:"name"`
	Ask      string `json:"ask"`
	Why      string `json:"why"`
	Schedule string `json:"schedule"`
	TimeZone string `json:"timeZone"`
}

func decode(raw json.RawMessage) (input, error) {
	var in input
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	err := dec.Decode(&in)
	return in, err
}

// proposal is the call's input as the server reads it.
func (in input) proposal(id string) runner.Proposal {
	return runner.Proposal{ToolUseID: id, Name: in.Name, Ask: in.Ask, Why: in.Why, Schedule: in.Schedule, TimeZone: in.TimeZone}
}

// Routine is the routine a proposal writes, checked by the routine's own
// rules and the tool's bounds: a why of one or two sentences, a schedule
// of a minute and an hour, every day or on the days listed, in a time
// zone this server knows, a name of one short line, and an ask. The
// server reads it again before it applies a proposal, so a check and an
// apply hold a proposal to one rule.
func Routine(p runner.Proposal) (routine.Fields, error) {
	why := strings.TrimSpace(p.Why)
	switch {
	case why == "":
		return routine.Fields{}, errors.New("why is required: one sentence the person reads of why you offer the routine")
	case utf8.RuneCountInString(why) > trigger.MaxPurposeLength:
		return routine.Fields{}, fmt.Errorf("why is longer than %d characters", trigger.MaxPurposeLength)
	case p.TimeZone == "":
		return routine.Fields{}, errors.New("timeZone is required: the IANA time zone the schedule is read in, such as Europe/Berlin")
	}
	if _, err := time.LoadLocation(p.TimeZone); err != nil {
		return routine.Fields{}, fmt.Errorf("timeZone %q is no time zone this server knows", p.TimeZone)
	}
	sched := routine.ParseSchedule(strings.TrimSpace(p.Schedule), p.TimeZone)
	if sched == nil {
		return routine.Fields{}, fmt.Errorf("schedule %q is not one a routine runs on: five fields of a minute and an hour, every day or on the days of the week listed, such as 0 9 * * 1", p.Schedule)
	}
	return routine.New(routine.Fields{Title: p.Name, Instruction: p.Ask, Schedule: sched})
}

// invalid is a call answered without running it.
func invalid(detail string) *tools.Result {
	r := tools.Text(tools.OutcomeInvalidInput, prompts.Render(prompts.TriggerInvalid, prompts.Data{"Detail": detail}))
	return &r
}

// Check refuses, from its input alone, a call that names no action the
// tool takes, and a proposal the routine's rules refuse, before anyone is
// asked to allow it.
func (*Tool) Check(c tools.Call) *tools.Result {
	in, err := decode(c.Input)
	if err != nil {
		return invalid(err.Error())
	}
	switch in.Action {
	case ActionList:
		return nil
	case ActionPropose:
		if _, err := Routine(in.proposal(c.ID)); err != nil {
			return invalid(err.Error())
		}
		return nil
	}
	return invalid(fmt.Sprintf("action is %q; it is %s or %s", in.Action, ActionList, ActionPropose))
}

// Probe asks the server whether the person could hold the proposed
// routine, before they are asked to allow it: a proposal the
// installation's authorizer would refuse, such as one past what the person
// may hold, is answered with why and asks nobody. A server that could not
// be asked leaves the call to be decided.
func (t *Tool) Probe(ctx context.Context, c tools.Call) *tools.Result {
	in, err := decode(c.Input)
	if err != nil || in.Action != ActionPropose {
		return nil
	}
	err = t.t.CheckProposal(ctx, in.proposal(c.ID))
	re, refused := errors.AsType[*runner.TriggerRefusal](err)
	if !refused {
		return nil
	}
	r := tools.Text(tools.OutcomeInvalidInput, prompts.Render(prompts.TriggerRefused, prompts.Data{"Code": re.Code, "Message": re.Message}))
	return &r
}

// Confirm holds every proposal for a person (spec 086): a routine is made
// only once the person who will own it allowed that call, whatever the
// agent's lists, an earlier answer, the mode or a decision service would
// run without asking.
func (*Tool) Confirm(c tools.Call) string {
	if in, err := decode(c.Input); err == nil && in.Action == ActionPropose {
		return confirmPropose
	}
	return ""
}

// Run lists the triggers, or applies an allowed proposal.
func (t *Tool) Run(ctx context.Context, c tools.Call) (tools.Result, error) {
	in, err := decode(c.Input)
	if err != nil {
		return *invalid(err.Error()), nil
	}
	if in.Action == ActionList {
		return t.list(ctx)
	}
	made, err := t.t.ApplyProposal(ctx, in.proposal(c.ID))
	if re, ok := errors.AsType[*runner.TriggerRefusal](err); ok {
		return tools.Text(tools.OutcomeError, prompts.Render(prompts.TriggerNotMade, prompts.Data{"Code": re.Code, "Message": re.Message})), nil
	}
	if err != nil {
		return tools.Result{}, err
	}
	return tools.Text(tools.OutcomeOK, prompts.Render(prompts.TriggerMade, prompts.Data{"Title": title(made), "Next": when(made.NextFireAt)})), nil
}

func (t *Tool) list(ctx context.Context) (tools.Result, error) {
	list, err := t.t.ListTriggers(ctx)
	if re, ok := errors.AsType[*runner.TriggerRefusal](err); ok {
		return tools.Text(tools.OutcomeError, prompts.Render(prompts.TriggerRefused, prompts.Data{"Code": re.Code, "Message": re.Message})), nil
	}
	if err != nil {
		return tools.Result{}, err
	}
	rows := make([]map[string]any, 0, len(list))
	for _, l := range list {
		rows = append(rows, map[string]any{"Title": title(l), "Name": l.Name, "Schedule": l.Schedule, "TimeZone": l.TimeZone, "Product": l.Product,
			"Paused": l.Suspended, "Next": when(l.NextFireAt), "Offered": l.Origin != nil})
	}
	return tools.Text(tools.OutcomeOK, prompts.Render(prompts.TriggerList, prompts.Data{"Triggers": rows})), nil
}

// title is the name a person reads of a trigger, its name when it has
// none.
func title(l runner.ListedTrigger) string {
	if l.Title != "" {
		return l.Title
	}
	return l.Name
}

// when is a fire time as the model reads it, in UTC; "" for none.
func when(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
