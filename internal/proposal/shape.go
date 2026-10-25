// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package proposal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"latere.ai/x/sdk-go/agents/routine"

	"latere.ai/x/topos/manifest/trigger"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/runner"
)

// Starts is how a routine starts on on, FromLink or FromEmail, and ""
// for a filter no routine expresses, whose proposal is written under the
// SDK's EventName.
func Starts(on v1.TriggerOn) routine.Start {
	starts, ok := routine.StartsOn(on.Product, on.Verbs, len(on.Resources) > 0 || len(on.Match) > 0)
	if !ok {
		return ""
	}
	return starts
}

// TriggerName is the name a propose of the call toolUseID in session
// sessionID applies its trigger under, so a retry of the call applies the
// same trigger: a routine's name, or for events no routine expresses the
// SDK's EventName of the same key.
func TriggerName(sessionID, toolUseID string, p runner.Proposal) string {
	key := routine.OfferKey(sessionID, toolUseID)
	if p.On != nil && Starts(*p.On) == "" {
		return routine.EventName(key)
	}
	return routine.Name(key)
}

// escape makes s literal in a trigger's template, as a routine's
// instruction and title are.
func escape(s string) string { return strings.ReplaceAll(s, "{{", `\{{`) }

// checkWhy holds why to one or two sentences a person reads.
func checkWhy(why string) error {
	why = strings.TrimSpace(why)
	switch {
	case why == "":
		return errors.New("why is required: one sentence the person reads of why you offer the trigger, or pause it")
	case utf8.RuneCountInString(why) > trigger.MaxPurposeLength:
		return fmt.Errorf("why is longer than %d characters", trigger.MaxPurposeLength)
	}
	return nil
}

// schedule reads a routine's schedule in its zone, refusing a zone this
// server does not know and a schedule no routine runs on.
func schedule(cron, zone string) (*routine.Schedule, error) {
	if zone == "" {
		return nil, errors.New("timeZone is required: the IANA time zone the schedule is read in, such as Europe/Berlin")
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return nil, fmt.Errorf("timeZone %q is no time zone this server knows", zone)
	}
	sched := routine.ParseSchedule(strings.TrimSpace(cron), zone)
	if sched == nil {
		return nil, fmt.Errorf("schedule %q is not one a routine runs on: five fields of a minute and an hour, every day or on the days of the week listed, such as 0 9 * * 1", cron)
	}
	return sched, nil
}

// filter refuses an event filter the resolver would.
func filter(on v1.TriggerOn) error {
	if ps := trigger.CheckFilter(on); len(ps) > 0 {
		return fmt.Errorf("on.%s: %s", ps[0].Path, ps[0].Detail)
	}
	return nil
}

// Routine is the routine a propose writes, checked by the routine's own
// rules and the tool's bounds: a why of one or two sentences, a name of
// one short line, an ask, and exactly one of a schedule of a minute and
// an hour, every day or on the days listed, in a time zone this server
// knows, and an event filter the resolver takes. A filter no routine
// expresses answers the routine's fields with Starts OnSchedule and no
// schedule, which Manifest writes the filter beside. The server reads it
// again before it applies a proposal, so a check and an apply hold a
// proposal to one rule.
func Routine(p runner.Proposal) (routine.Fields, error) {
	if err := checkWhy(p.Why); err != nil {
		return routine.Fields{}, err
	}
	switch {
	case p.On != nil && p.Schedule != "":
		return routine.Fields{}, errors.New("a trigger runs on a schedule or on events: name schedule or on, not both")
	case p.On != nil:
		if err := filter(*p.On); err != nil {
			return routine.Fields{}, err
		}
		starts := Starts(*p.On)
		if starts == "" {
			title, err := routine.CheckTitle(p.Name)
			if err != nil {
				return routine.Fields{}, err
			}
			ask, err := routine.CheckInstruction(p.Ask, routine.FromLink)
			if err != nil {
				return routine.Fields{}, err
			}
			return routine.Fields{Title: title, Instruction: ask, Starts: routine.OnSchedule}, nil
		}
		return routine.New(routine.Fields{Title: p.Name, Instruction: p.Ask, Starts: starts})
	}
	sched, err := schedule(p.Schedule, p.TimeZone)
	if err != nil {
		return routine.Fields{}, err
	}
	return routine.New(routine.Fields{Title: p.Name, Instruction: p.Ask, Schedule: sched})
}

// Manifest is the Trigger manifest a propose applies, of the agent named
// agent, under name: the routine latere.ai/x/sdk-go/agents/routine
// writes, and for events no routine expresses the same manifest with the
// filter as its on and the SDK's EventMessage, the ask and the event
// after it, as its message.
func Manifest(agent, name string, p runner.Proposal) (json.RawMessage, error) {
	f, err := Routine(p)
	if err != nil {
		return nil, err
	}
	body, err := routine.Manifest(agent, name, nil, f)
	if err != nil {
		return nil, err
	}
	events := p.On != nil && Starts(*p.On) == ""
	if !events && p.Notify != v1.NotifyJudged && p.PerWeekUSDMicro == nil {
		return body, nil
	}
	return edit(body, func(spec, session map[string]any) {
		if events {
			spec["on"] = *p.On
			session["message"] = routine.EventMessage(f.Instruction)
		}
		writeNotify(session, p.Notify)
		writeBudget(spec, p.PerWeekUSDMicro)
	})
}

// writeBudget writes a proposal's weekly allowance into a trigger's spec
// as its budget.perWeek (spec 087); nil keeps the one it has.
func writeBudget(spec map[string]any, perWeek *int64) {
	if perWeek != nil {
		spec["budget"] = map[string]any{"perWeek": *perWeek}
	}
}

// writeNotify writes a proposal's notify into a trigger's session: judged
// as it is, and always as the default, which a trigger leaves unset so
// its digest is the one it had before spec 086 (spec 003).
func writeNotify(session map[string]any, notify string) {
	switch notify {
	case v1.NotifyJudged:
		session["notify"] = v1.NotifyJudged
	case v1.NotifyAlways:
		delete(session, "notify")
	}
}

// edit decodes a trigger manifest, lets fn change its spec and its
// session in place, and writes it again without its status. Numbers stay
// as they were written.
func edit(doc json.RawMessage, fn func(spec, session map[string]any)) (json.RawMessage, error) {
	m := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("proposal: the trigger is not a JSON object: %w", err)
	}
	delete(m, "status")
	spec, ok := m["spec"].(map[string]any)
	if !ok {
		return nil, errors.New("proposal: the trigger's spec is not an object")
	}
	session, ok := spec["session"].(map[string]any)
	if !ok {
		session = map[string]any{}
		spec["session"] = session
	}
	fn(spec, session)
	return json.Marshal(m)
}

// Changed is the manifest a change, a pause or a resume applies to the
// stored trigger doc, of the agent named agent: the fields p names
// changed, a pause suspending it and a resume clearing that, and every
// other field kept. A routine stays a routine, written by its own
// Manifest as the SDK's change of one writes it; any other trigger has
// the fields named written into its document. A routine is not changed to
// events no routine expresses, which a new proposal offers. A trigger that
// fires at one time or at a list of times (spec 090) is no routine the
// SDK's Fields express, whatever its name and labels, so its document is
// written as any other trigger's.
func Changed(agent string, stored *v1.Trigger, p runner.Proposal) (json.RawMessage, error) {
	doc, err := json.Marshal(stored)
	if err != nil {
		return nil, err
	}
	rt := routine.Trigger{Name: stored.Metadata.Name, DisplayName: stored.Metadata.DisplayName, Labels: stored.Metadata.Labels,
		Schedule: stored.Spec.Schedule, TimeZone: stored.Spec.TimeZone, Message: stored.Spec.Session.Message, Title: stored.Spec.Session.Title, Suspend: stored.Spec.Suspend}
	if stored.Spec.On != nil {
		rt.Product = stored.Spec.On.Product
	}
	if f, ok := routine.Read(rt, agent); ok && stored.Spec.At == nil && stored.Spec.Times == nil {
		return changedRoutine(agent, stored.Metadata.Name, doc, f, p)
	}
	return changedTrigger(doc, stored, p)
}

// changedRoutine writes p over the fields f of the routine named name.
func changedRoutine(agent, name string, doc json.RawMessage, f routine.Fields, p runner.Proposal) (json.RawMessage, error) {
	if p.Name != "" {
		f.Title = p.Name
	}
	if p.Ask != "" {
		f.Instruction = p.Ask
	}
	switch {
	case p.Schedule != "":
		sched, err := schedule(p.Schedule, p.TimeZone)
		if err != nil {
			return nil, err
		}
		f.Starts, f.Schedule = routine.OnSchedule, sched
	case p.On != nil:
		starts := Starts(*p.On)
		if starts == "" {
			return nil, errors.New("on: a routine starts on a schedule, from a link or from an email, and this filter is none of them; propose a new trigger for it")
		}
		f.Starts, f.Schedule = starts, nil
	}
	switch p.Verb() {
	case runner.TriggerPause:
		f.Paused = true
	case runner.TriggerResume:
		f.Paused = false
	}
	f, err := routine.New(f)
	if err != nil {
		return nil, err
	}
	body, err := routine.Manifest(agent, name, doc, f)
	if err != nil || p.Notify == "" && p.PerWeekUSDMicro == nil {
		return body, err
	}
	return edit(body, func(spec, session map[string]any) {
		writeNotify(session, p.Notify)
		writeBudget(spec, p.PerWeekUSDMicro)
	})
}

// changedTrigger writes p into a trigger that is no routine: its name as
// its displayName and its sessions' title, its ask as its message, its
// schedule or its filter, and its suspension.
func changedTrigger(doc json.RawMessage, stored *v1.Trigger, p runner.Proposal) (json.RawMessage, error) {
	title := ""
	if p.Name != "" {
		t, err := routine.CheckTitle(p.Name)
		if err != nil {
			return nil, err
		}
		title = t
	}
	ask := ""
	if p.Ask != "" {
		a, err := routine.CheckInstruction(p.Ask, routine.FromLink)
		if err != nil {
			return nil, err
		}
		ask = a
	}
	if p.Schedule != "" {
		if _, err := schedule(p.Schedule, p.TimeZone); err != nil {
			return nil, err
		}
	}
	events := (stored.Spec.On != nil || p.On != nil) && p.Schedule == ""
	return editWithMeta(doc, title, func(spec, session map[string]any) {
		if title != "" {
			session["title"] = escape(title)
		}
		if ask != "" {
			session["message"] = escape(ask)
			if events {
				session["message"] = routine.EventMessage(ask)
			}
		}
		// A schedule or a filter replaces what the trigger fired on, a
		// time or a list of times included, so it sets one of them.
		switch {
		case p.Schedule != "":
			spec["schedule"], spec["timeZone"] = strings.TrimSpace(p.Schedule), p.TimeZone
			delete(spec, "on")
			delete(spec, "at")
			delete(spec, "times")
		case p.On != nil:
			spec["on"] = *p.On
			delete(spec, "schedule")
			delete(spec, "at")
			delete(spec, "times")
		}
		switch p.Verb() {
		case runner.TriggerPause:
			spec["suspend"] = true
		case runner.TriggerResume:
			delete(spec, "suspend")
		}
		writeNotify(session, p.Notify)
		writeBudget(spec, p.PerWeekUSDMicro)
	})
}

// editWithMeta is edit with the metadata's displayName set to title, when
// title is not empty.
func editWithMeta(doc json.RawMessage, title string, fn func(spec, session map[string]any)) (json.RawMessage, error) {
	body, err := edit(doc, fn)
	if err != nil || title == "" {
		return body, err
	}
	m := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	meta, ok := m["metadata"].(map[string]any)
	if !ok {
		return nil, errors.New("proposal: the trigger's metadata is not an object")
	}
	meta["displayName"] = title
	return json.Marshal(m)
}
