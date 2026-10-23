// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package proposal is the trigger tool of spec 086: it lists the triggers
// the person who started a hosted session holds of the session's agent,
// proposes a new one on a schedule or on events, and changes, pauses,
// resumes or deletes one of them. A propose, a change, a resume and a
// delete wait for that person's allow every time and are done as theirs
// once they give it; a pause, which only stops work, runs at once. A
// proposal is written in the one shape every client reads as a routine,
// by latere.ai/x/sdk-go/agents/routine, and events no routine expresses
// in the same shape under a name of their own. The tool holds no
// credential: the session's server asks every question as the session's
// initiator, over the drive's runner.Triggers.
package proposal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"
	"latere.ai/x/sdk-go/agents/routine"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/manifest/trigger"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// Name is the tool's name.
const Name = session.ToolTrigger

// The tool's actions: a list, and each action of a runner.Proposal.
const (
	ActionList    = "list"
	ActionPropose = runner.TriggerPropose
	ActionChange  = runner.TriggerChange
	ActionPause   = runner.TriggerPause
	ActionResume  = runner.TriggerResume
	ActionDelete  = runner.TriggerDelete
)

// actions are the tool's actions in the order its schema lists them.
var actions = []string{ActionList, ActionPropose, ActionChange, ActionPause, ActionResume, ActionDelete}

// schema is the tool's input, its bounds read from their constants.
var schema = json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": [%q, %q, %q, %q, %q, %q], "description": "list to read the triggers the person holds of you; propose to offer a new one; change, resume or delete one of them, which waits for the person's answer; pause one, which runs at once."},
    "id": {"type": "string", "description": "For change, pause, resume and delete: the trigger's id, trg_ and the rest, as list answers it."},
    "name": {"type": "string", "maxLength": %d, "description": "For propose and change: the trigger's name, as the person would call it."},
    "ask": {"type": "string", "description": "For propose and change: what each run asks, as a message from the person to you, complete on its own."},
    "why": {"type": "string", "maxLength": %d, "description": "For propose and change: one sentence the person reads that says the schedule or the event back and what they get each time. For pause: why you pause it."},
    "schedule": {"type": "string", "description": "For propose and change: five cron fields of a minute and an hour, every day or on the days of the week listed, such as 0 9 * * 1."},
    "timeZone": {"type": "string", "description": "For propose and change with a schedule: the IANA time zone the schedule is read in, such as Europe/Berlin."},
    "on": {
      "type": "object",
      "description": "For propose and change, in place of schedule: the delivered events each run starts on.",
      "properties": {
        "product": {"type": "string", "description": "The product the events come from, lowercase, such as mail, webhook or github."},
        "verbs": {"type": "array", "items": {"type": "string"}, "description": "The verbs to run on, such as received or push; an entry ending in * matches the prefix before it."},
        "resources": {"type": "array", "items": {"type": "string"}, "description": "The resources to run on, such as a repository owner/name; absent runs on every one."},
        "match": {"type": "array", "items": {"type": "object", "properties": {"path": {"type": "string"}, "in": {"type": "array", "items": {"type": "string"}}}, "required": ["path", "in"], "additionalProperties": false}, "description": "Rules on the event's payload, each a path such as payload.ref and the values it may take."}
      },
      "required": ["product", "verbs"],
      "additionalProperties": false
    },
    "notify": {"type": "string", "enum": [%q, %q], "description": "For propose and change: always to bring every run's result to the person, judged to bring it only when you call notify in the run. always for a schedule and judged for events when absent."},
    "perWeekUsd": {"type": "number", "description": "For propose, and for change when the cost changes: your estimate, in USD and above zero, of what the trigger's runs cost in a week, such as 0.40, said back to the person in why. The server starts no run while a week's runs cost this much."}
  },
  "required": ["action"],
  "additionalProperties": false
}`, ActionList, ActionPropose, ActionChange, ActionPause, ActionResume, ActionDelete, routine.MaxTitle, trigger.MaxPurposeLength, v1.NotifyAlways, v1.NotifyJudged))

// The reasons a held call waits for its person, as its agent.tool_use
// records them.
const (
	confirmPropose = "makes a routine the person who started this conversation owns, which they approve every time"
	confirmChange  = "changes a trigger the person who started this conversation owns, which they approve every time"
	confirmResume  = "runs again a trigger the person who started this conversation owns, which they approve every time"
	confirmDelete  = "deletes a trigger the person who started this conversation owns, which they approve every time"
)

// Held is the reason a call of action waits for the person who started
// the session, "" for a list and a pause, which run at once.
func Held(action string) string {
	switch action {
	case ActionPropose:
		return confirmPropose
	case ActionChange:
		return confirmChange
	case ActionResume:
		return confirmResume
	case ActionDelete:
		return confirmDelete
	}
	return ""
}

// Tool is the trigger tool of one hosted session.
type Tool struct {
	t runner.Triggers
	// price is the catalog's price of the session's model, which the
	// description states so the agent estimates a proposal's weekly
	// allowance from it (spec 087); nil when the catalog prices it at
	// nothing.
	price *models.Pricing
}

// New is the tool over t, the session's reach to its server's triggers,
// for a session whose model the catalog prices at price, nil for none.
func New(t runner.Triggers, price *models.Pricing) *Tool { return &Tool{t: t, price: price} }

// Definition is what the model sees.
func (t *Tool) Definition() tools.Definition {
	return tools.Definition{Name: Name, Description: Description(t.price), InputSchema: schema}
}

// Description is the tool's description for a model the catalog prices
// at price: its input and output price per million tokens, which the
// agent estimates a proposal's allowance from, or that it is not stated.
func Description(price *models.Pricing) string {
	data := prompts.Data{"Priced": false, "Input": "", "Output": ""}
	if price != nil && price.Input != nil && price.Output != nil {
		data = prompts.Data{"Priced": true, "Input": "$" + price.Input.String(), "Output": "$" + price.Output.String()}
	}
	return prompts.Render(prompts.ToolTrigger, data)
}

// Properties: one call at a time. A list reads and a pause only stops
// work, so both run in every mode; a propose, a change, a resume and a
// delete are held for their person by Confirm whatever the tool's effect,
// and blocked in plan mode.
func (*Tool) Properties() tools.Properties { return tools.Properties{Effect: tools.EffectRead} }

// fields is a call's input as the model writes it.
type fields struct {
	Action   string          `json:"action"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Ask      string          `json:"ask"`
	Why      string          `json:"why"`
	Schedule string          `json:"schedule"`
	TimeZone string          `json:"timeZone"`
	On       json.RawMessage `json:"on"`
	Notify   string          `json:"notify"`
	// PerWeekUSD is the weekly allowance in USD as the model writes it,
	// read as a decimal (spec 087).
	PerWeekUSD json.Number `json:"perWeekUsd"`
}

// decode reads a call's input as the server reads it, the call's id
// beside it.
func decode(id string, raw json.RawMessage) (runner.Proposal, error) {
	var f fields
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return runner.Proposal{}, err
	}
	p := runner.Proposal{ToolUseID: id, Action: f.Action, ID: f.ID, Name: f.Name, Ask: f.Ask, Why: f.Why, Schedule: f.Schedule, TimeZone: f.TimeZone, Notify: f.Notify}
	if f.PerWeekUSD != "" {
		micro, err := models.ParsePrice(f.PerWeekUSD.String())
		if err != nil || micro <= 0 {
			return runner.Proposal{}, fmt.Errorf("perWeekUsd is %s; it is a USD amount above zero with at most six digits after the point, such as 0.40", f.PerWeekUSD)
		}
		p.PerWeekUSDMicro = new(int64(micro))
	}
	if len(f.On) > 0 && !bytes.Equal(bytes.TrimSpace(f.On), []byte("null")) {
		on := json.NewDecoder(bytes.NewReader(f.On))
		on.DisallowUnknownFields()
		if err := on.Decode(&p.On); err != nil {
			return runner.Proposal{}, fmt.Errorf("on: %w", err)
		}
	}
	return p, nil
}

// Check refuses, from a proposal alone, what the server would refuse: a
// propose the routine's rules or the resolver's grammar refuse, and a
// change, pause, resume or delete that names no trigger, or a change that
// names nothing to change or a field those rules refuse. The server
// checks a proposal by it again before it asks anything.
func Check(p runner.Proposal) error {
	verb := p.Verb()
	if Held(verb) == "" && verb != ActionPause {
		return fmt.Errorf("action is %q; it is one of %s", p.Action, strings.Join(actions, ", "))
	}
	if verb != ActionPropose && session.CheckID(session.PrefixTrigger, p.ID) != nil {
		return fmt.Errorf("id is %q; it is a trigger's id, trg_ and the rest, as list answers it", p.ID)
	}
	if p.Notify != "" && p.Notify != v1.NotifyAlways && p.Notify != v1.NotifyJudged {
		return fmt.Errorf("notify is %q; it is %s or %s", p.Notify, v1.NotifyAlways, v1.NotifyJudged)
	}
	switch verb {
	case ActionPropose:
		_, err := Routine(p)
		return err
	case ActionChange:
		if err := checkWhy(p.Why); err != nil {
			return err
		}
		switch {
		case p.Name == "" && p.Ask == "" && p.Schedule == "" && p.On == nil && p.Notify == "" && p.PerWeekUSDMicro == nil:
			return errors.New("a change names what it changes: name, ask, schedule, on, notify or perWeekUsd")
		case p.Schedule != "" && p.On != nil:
			return errors.New("a trigger runs on a schedule or on events: name schedule or on, not both")
		}
		if p.Name != "" {
			if _, err := routine.CheckTitle(p.Name); err != nil {
				return err
			}
		}
		if p.Ask != "" {
			if _, err := routine.CheckInstruction(p.Ask, routine.OnSchedule); err != nil {
				return err
			}
		}
		if p.Schedule != "" {
			if _, err := schedule(p.Schedule, p.TimeZone); err != nil {
				return err
			}
		}
		if p.On != nil {
			return filter(*p.On)
		}
		return nil
	case ActionPause:
		return checkWhy(p.Why)
	}
	return nil
}

// invalid is a call answered without running it.
func invalid(detail string) *tools.Result {
	r := tools.Text(tools.OutcomeInvalidInput, prompts.Render(prompts.TriggerInvalid, prompts.Data{"Detail": detail}))
	return &r
}

// Check refuses, from its input alone, a call that names no action the
// tool takes, and a proposal Check refuses, before anyone is asked to
// allow it.
func (*Tool) Check(c tools.Call) *tools.Result {
	in, err := decode(c.ID, c.Input)
	if err != nil {
		return invalid(err.Error())
	}
	if in.Action == ActionList {
		return nil
	}
	if in.Action == "" {
		return invalid(fmt.Sprintf("action is required; it is one of %s", strings.Join(actions, ", ")))
	}
	if err := Check(in); err != nil {
		return invalid(err.Error())
	}
	return nil
}

// Probe asks the server whether the person could have a held call done,
// before they are asked to allow it: a proposal the installation's
// authorizer would refuse, such as one past what the person may hold, or
// a change of a trigger that is not theirs to change from here, is
// answered with why and asks nobody. A server that could not be asked
// leaves the call to be decided.
func (t *Tool) Probe(ctx context.Context, c tools.Call) *tools.Result {
	in, err := decode(c.ID, c.Input)
	if err != nil || Held(in.Action) == "" {
		return nil
	}
	err = t.t.CheckProposal(ctx, in)
	re, refused := errors.AsType[*runner.TriggerRefusal](err)
	if !refused {
		return nil
	}
	text := prompts.Render(prompts.TriggerRefused, prompts.Data{"Code": re.Code, "Message": re.Message})
	if in.Action != ActionPropose {
		text = prompts.Render(prompts.TriggerRefusedChange, prompts.Data{"Action": in.Action, "Code": re.Code, "Message": re.Message})
	}
	r := tools.Text(tools.OutcomeInvalidInput, text)
	return &r
}

// Confirm holds every propose, change, resume and delete for a person
// (spec 086): standing work is made, changed, run again or removed only
// once the person who owns it allowed that call, whatever the agent's
// lists, an earlier answer, the mode or a decision service would run
// without asking.
func (*Tool) Confirm(c tools.Call) string {
	in, err := decode(c.ID, c.Input)
	if err != nil {
		return ""
	}
	return Held(in.Action)
}

// Run lists the triggers, pauses one, or does an allowed call.
func (t *Tool) Run(ctx context.Context, c tools.Call) (tools.Result, error) {
	in, err := decode(c.ID, c.Input)
	if err != nil {
		return *invalid(err.Error()), nil
	}
	if in.Action == ActionList {
		return t.list(ctx)
	}
	done, err := t.t.ApplyProposal(ctx, in)
	if re, ok := errors.AsType[*runner.TriggerRefusal](err); ok {
		if in.Action == ActionPropose {
			return tools.Text(tools.OutcomeError, prompts.Render(prompts.TriggerNotMade, prompts.Data{"Code": re.Code, "Message": re.Message})), nil
		}
		return tools.Text(tools.OutcomeError, prompts.Render(prompts.TriggerNotDone, prompts.Data{"Action": in.Action, "Held": Held(in.Action) != "", "Code": re.Code, "Message": re.Message})), nil
	}
	if err != nil {
		return tools.Result{}, err
	}
	data := prompts.Data{"Title": title(done), "Next": when(done.NextFireAt)}
	var res tools.Result
	switch in.Action {
	case ActionChange:
		res = tools.Text(tools.OutcomeOK, prompts.Render(prompts.TriggerChanged, data))
	case ActionPause:
		return tools.Text(tools.OutcomeOK, prompts.Render(prompts.TriggerPaused, prompts.Data{"Title": title(done)})), nil
	case ActionResume:
		res = tools.Text(tools.OutcomeOK, prompts.Render(prompts.TriggerResumed, data))
	case ActionDelete:
		return tools.Text(tools.OutcomeOK, prompts.Render(prompts.TriggerDeleted, prompts.Data{"Title": title(done)})), nil
	default:
		res = tools.Text(tools.OutcomeOK, prompts.Render(prompts.TriggerMade, data))
	}
	// A trigger made, changed or run again while its owner holds every
	// trigger runs nothing until the release, which the model says (spec
	// 086).
	if done.Held {
		res.Content = append(res.Content, lux.Block{Type: ir.BlockText, Text: prompts.Text(prompts.TriggerHeld)})
	}
	return res, nil
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
		rows = append(rows, map[string]any{"ID": l.ID, "Title": title(l), "Name": l.Name, "Schedule": l.Schedule, "TimeZone": l.TimeZone, "Product": l.Product,
			"Ask": l.Ask, "At": when(l.At), "Target": l.Target, "TargetTitle": l.TargetTitle,
			"Paused": l.Suspended, "Next": when(l.NextFireAt), "Offered": l.Origin != nil, "PerWeek": usd(l.PerWeekUSDMicro), "Spent": usd(l.SpentUSDMicro)})
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

// usd is an amount of USD micros as the model reads it, "" for none.
func usd(micro *int64) string {
	if micro == nil {
		return ""
	}
	return session.USD(*micro)
}

// when is a fire time as the model reads it, in UTC; "" for none.
func when(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
