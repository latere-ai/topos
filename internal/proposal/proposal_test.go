// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package proposal

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"latere.ai/x/sdk-go/agents/routine"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/manifest/trigger"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/runner"
)

// fakeTriggers is a server's reach that records each question and
// answers as its fields say.
type fakeTriggers struct {
	list    []runner.ListedTrigger
	refuse  *runner.TriggerRefusal
	fail    error
	checked []runner.Proposal
	applied []runner.Proposal
	// wakes and canceled are the sets and cancels asked, and pending the
	// wake a cancel finds, "" for none.
	wakes    []runner.Wake
	canceled []string
	pending  string
}

func (f *fakeTriggers) ListTriggers(context.Context) ([]runner.ListedTrigger, error) {
	if f.refuse != nil {
		return nil, f.refuse
	}
	return f.list, f.fail
}

func (f *fakeTriggers) CheckProposal(_ context.Context, p runner.Proposal) error {
	f.checked = append(f.checked, p)
	if f.refuse != nil {
		return f.refuse
	}
	return f.fail
}

func (f *fakeTriggers) ApplyProposal(_ context.Context, p runner.Proposal) (runner.ListedTrigger, error) {
	f.applied = append(f.applied, p)
	if f.refuse != nil {
		return runner.ListedTrigger{}, f.refuse
	}
	if f.fail != nil {
		return runner.ListedTrigger{}, f.fail
	}
	next := time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC)
	return runner.ListedTrigger{ID: "trg_1", Name: routine.Name("ses_1/" + p.ToolUseID), Title: cmp.Or(p.Name, "Weekly sign-ups"), NextFireAt: &next}, nil
}

func (f *fakeTriggers) SetWake(_ context.Context, w runner.Wake) (runner.ListedTrigger, error) {
	f.wakes = append(f.wakes, w)
	if f.refuse != nil {
		return runner.ListedTrigger{}, f.refuse
	}
	if f.fail != nil {
		return runner.ListedTrigger{}, f.fail
	}
	return runner.ListedTrigger{ID: "trg_w", Name: "wake-1", Title: w.Title, At: &w.At}, nil
}

func (f *fakeTriggers) CancelWake(_ context.Context, id string) (string, error) {
	f.canceled = append(f.canceled, id)
	if f.refuse != nil {
		return "", f.refuse
	}
	return f.pending, f.fail
}

// weekly is a proposal every routine rule takes.
const weekly = `{"action":"propose","name":"Weekly sign-ups","ask":"Count last week's sign-ups.","why":"Every Monday at 9:00, Berlin time, I count last week's sign-ups.","schedule":"0 9 * * 1","timeZone":"Europe/Berlin"}`

// with is weekly with one field replaced.
func with(t *testing.T, key string, v any) json.RawMessage {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(weekly), &m); err != nil {
		t.Fatal(err)
	}
	if v == nil {
		delete(m, key)
	} else {
		m[key] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAProposalIsCheckedBeforeAnyoneIsAsked: a proposal the routine's
// rules refuse is answered invalid_input by Check, naming what is wrong,
// and one they take passes, as a list does.
func TestAProposalIsCheckedBeforeAnyoneIsAsked(t *testing.T) {
	tool := New(&fakeTriggers{}, nil)
	cases := map[string]struct {
		input json.RawMessage
		want  string
	}{
		"no action":            {json.RawMessage(`{}`), "action"},
		"another action":       {json.RawMessage(`{"action":"archive","id":"trg_1"}`), "action"},
		"an unknown field":     {json.RawMessage(`{"action":"list","when":"now"}`), "unknown field"},
		"an unknown on field":  {with(t, "on", map[string]any{"product": "mail", "verbs": []string{"received"}, "every": true}), "unknown field"},
		"schedule and on":      {with(t, "on", map[string]any{"product": "mail", "verbs": []string{"received"}}), "not both"},
		"an on of no verb":     {json.RawMessage(`{"action":"propose","name":"n","ask":"a","why":"w","on":{"product":"mail","verbs":[]}}`), "on.verbs"},
		"a change of no id":    {json.RawMessage(`{"action":"change","name":"n","why":"w"}`), "trigger's id"},
		"a change of nothing":  {json.RawMessage(`{"action":"change","id":"trg_01JAAAAAAAAAAAAAAAAAAAAAAA","why":"w"}`), "names what it changes"},
		"a change of no why":   {json.RawMessage(`{"action":"change","id":"trg_01JAAAAAAAAAAAAAAAAAAAAAAA","name":"n"}`), "why is required"},
		"a change to both":     {json.RawMessage(`{"action":"change","id":"trg_01JAAAAAAAAAAAAAAAAAAAAAAA","why":"w","schedule":"0 9 * * 1","timeZone":"UTC","on":{"product":"mail","verbs":["received"]}}`), "not both"},
		"a change's bad name":  {json.RawMessage(`{"action":"change","id":"trg_01JAAAAAAAAAAAAAAAAAAAAAAA","why":"w","name":"a\nb"}`), "not one short line"},
		"a change's schedule":  {json.RawMessage(`{"action":"change","id":"trg_01JAAAAAAAAAAAAAAAAAAAAAAA","why":"w","schedule":"*/5 * * * *","timeZone":"UTC"}`), "not one a routine runs on"},
		"a pause of no why":    {json.RawMessage(`{"action":"pause","id":"trg_01JAAAAAAAAAAAAAAAAAAAAAAA"}`), "why is required"},
		"a delete of no id":    {json.RawMessage(`{"action":"delete"}`), "trigger's id"},
		"no action at all":     {json.RawMessage(`{"id":"trg_01JAAAAAAAAAAAAAAAAAAAAAAA"}`), "action is required"},
		"no why":               {with(t, "why", nil), "why is required"},
		"a long why":           {with(t, "why", strings.Repeat("x", trigger.MaxPurposeLength+1)), "why is longer"},
		"no time zone":         {with(t, "timeZone", nil), "timeZone is required"},
		"an unknown time zone": {with(t, "timeZone", "Mars/Olympus"), "no time zone"},
		"every five minutes":   {with(t, "schedule", "*/5 * * * *"), "not one a routine runs on"},
		"weekdays as a range":  {with(t, "schedule", "0 9 * * 1-5"), "not one a routine runs on"},
		"no name":              {with(t, "name", " "), "no title"},
		"a long name":          {with(t, "name", strings.Repeat("x", routine.MaxTitle+1)), "not one short line"},
		"no ask":               {with(t, "ask", ""), "no instruction"},
	}
	for name, c := range cases {
		r := tool.Check(tools.Call{ID: "toolu_1", Input: c.input})
		if r == nil || r.Outcome != tools.OutcomeInvalidInput || !strings.Contains(r.Content[0].Text, c.want) {
			t.Errorf("%s: %+v, want invalid_input naming %q", name, r, c.want)
		}
	}
	for _, in := range []string{weekly, `{"action":"list"}`,
		`{"action":"propose","name":"Pushes","ask":"Review it.","why":"On every push.","on":{"product":"github","verbs":["push"],"resources":["acme/app"],"match":[{"path":"payload.ref","in":["refs/heads/main"]}]}}`,
		`{"action":"change","id":"trg_01JAAAAAAAAAAAAAAAAAAAAAAA","why":"Earlier.","schedule":"0 8 * * 1","timeZone":"UTC"}`,
		`{"action":"change","id":"trg_01JAAAAAAAAAAAAAAAAAAAAAAA","why":"Mail.","ask":"Save it.","on":{"product":"mail","verbs":["received"]}}`,
		`{"action":"pause","id":"trg_01JAAAAAAAAAAAAAAAAAAAAAAA","why":"Away."}`,
		`{"action":"resume","id":"trg_01JAAAAAAAAAAAAAAAAAAAAAAA"}`,
		`{"action":"delete","id":"trg_01JAAAAAAAAAAAAAAAAAAAAAAA"}`} {
		if r := tool.Check(tools.Call{ID: "toolu_1", Input: json.RawMessage(in)}); r != nil {
			t.Errorf("%s was refused: %+v", in, r)
		}
	}
}

// TestEveryProposalWaitsForItsPerson: Confirm holds every proposal and no
// list, and Probe answers a proposal the server refuses with its reason,
// asks nothing of a list, and leaves a proposal it could not ask about to
// be decided.
func TestEveryProposalWaitsForItsPerson(t *testing.T) {
	f := &fakeTriggers{}
	tool := New(f, nil)
	if r := tool.Confirm(tools.Call{Input: json.RawMessage(weekly)}); r != confirmPropose {
		t.Fatalf("a proposal is held %q", r)
	}
	if r := tool.Confirm(tools.Call{Input: json.RawMessage(`{"action":"list"}`)}); r != "" {
		t.Fatalf("a list is held %q", r)
	}
	if r := tool.Probe(t.Context(), tools.Call{ID: "toolu_1", Input: json.RawMessage(weekly)}); r != nil {
		t.Fatalf("a proposal the server takes answered %+v", r)
	}
	if len(f.checked) != 1 || f.checked[0].ToolUseID != "toolu_1" || f.checked[0].Schedule != "0 9 * * 1" || f.checked[0].TimeZone != "Europe/Berlin" {
		t.Fatalf("the server was asked %+v", f.checked)
	}
	if r := tool.Probe(t.Context(), tools.Call{ID: "toolu_l", Input: json.RawMessage(`{"action":"list"}`)}); r != nil || len(f.checked) != 1 {
		t.Fatalf("a list was probed: %+v", r)
	}
	f.fail = errors.New("the server is away")
	if r := tool.Probe(t.Context(), tools.Call{ID: "toolu_2", Input: json.RawMessage(weekly)}); r != nil {
		t.Fatalf("a proposal the server could not be asked about answered %+v", r)
	}
	f.fail, f.refuse = nil, &runner.TriggerRefusal{Code: "proposals_full", Message: "the person holds as many as they may"}
	r := tool.Probe(t.Context(), tools.Call{ID: "toolu_3", Input: json.RawMessage(weekly)})
	if r == nil || r.Outcome != tools.OutcomeInvalidInput || !strings.Contains(r.Content[0].Text, "proposals_full") || !strings.Contains(r.Content[0].Text, "were not asked") {
		t.Fatalf("a refused proposal answered %+v", r)
	}
}

// TestAnAllowedProposalIsApplied: once the person allowed it, a proposal
// is applied and its result names the routine and its next run; a
// refusal then is the call's result, and a server's failure an error.
func TestAnAllowedProposalIsApplied(t *testing.T) {
	f := &fakeTriggers{}
	tool := New(f, nil)
	res, err := tool.Run(t.Context(), tools.Call{ID: "toolu_1", Input: json.RawMessage(weekly)})
	if err != nil || res.Outcome != tools.OutcomeOK || !strings.Contains(res.Content[0].Text, `"Weekly sign-ups" is theirs now`) || !strings.Contains(res.Content[0].Text, "2026-10-26T08:00:00Z") {
		t.Fatalf("the allowed proposal answered %+v, %v", res, err)
	}
	if len(f.applied) != 1 || f.applied[0] != (runner.Proposal{ToolUseID: "toolu_1", Action: ActionPropose, Name: "Weekly sign-ups", Ask: "Count last week's sign-ups.",
		Why: "Every Monday at 9:00, Berlin time, I count last week's sign-ups.", Schedule: "0 9 * * 1", TimeZone: "Europe/Berlin"}) {
		t.Fatalf("applied %+v", f.applied)
	}
	f.refuse = &runner.TriggerRefusal{Code: "forbidden", Message: "no allow"}
	if res, err := tool.Run(t.Context(), tools.Call{ID: "toolu_2", Input: json.RawMessage(weekly)}); err != nil || res.Outcome != tools.OutcomeError || !strings.Contains(res.Content[0].Text, "was not made (forbidden: no allow)") {
		t.Fatalf("a refused apply answered %+v, %v", res, err)
	}
	f.refuse, f.fail = nil, errors.New("the server is away")
	if _, err := tool.Run(t.Context(), tools.Call{ID: "toolu_3", Input: json.RawMessage(weekly)}); err == nil {
		t.Fatal("a server's failure was answered as a result")
	}
}

// TestAListNamesWhatThePersonHolds: a list names each trigger's title,
// schedule, next run and whether a conversation offered it, and says so
// when there is none.
func TestAListNamesWhatThePersonHolds(t *testing.T) {
	next := time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC)
	f := &fakeTriggers{list: []runner.ListedTrigger{
		{ID: "trg_1", Name: "routine-a", Title: "Weekly sign-ups", Schedule: "0 9 * * 1", TimeZone: "Europe/Berlin", Ask: "Count.", NextFireAt: &next, Origin: &v1.TriggerOrigin{Kind: v1.OriginProposal}},
		{ID: "trg_2", Name: "on-mail", Product: "mail", Suspended: true},
	}}
	res, err := New(f, nil).Run(t.Context(), tools.Call{ID: "toolu_l", Input: json.RawMessage(`{"action":"list"}`)})
	if err != nil {
		t.Fatal(err)
	}
	want := "The person holds these triggers of you, newest first:\n" +
		"- trg_1 Weekly sign-ups (routine-a): 0 9 * * 1 in Europe/Berlin, next at 2026-10-26T08:00:00Z, offered in a conversation. Asks: Count.\n" +
		"- trg_2 on-mail (on-mail): when mail sends an event, paused\n"
	if res.Outcome != tools.OutcomeOK || res.Content[0].Text != want {
		t.Fatalf("the list answered %q, want %q", res.Content[0].Text, want)
	}
	f.list = nil
	if res, err := New(f, nil).Run(t.Context(), tools.Call{ID: "toolu_l", Input: json.RawMessage(`{"action":"list"}`)}); err != nil || res.Content[0].Text != "The person holds no triggers of you." {
		t.Fatalf("an empty list answered %+v, %v", res, err)
	}
}

// TestToolTextsReadTheirBounds: the tool's description states the bounds
// its schema and checks hold a call to, and its schema holds them.
func TestToolTextsReadTheirBounds(t *testing.T) {
	in, out := models.Price(3_000_000), models.Price(15_000_000)
	d := New(nil, &models.Pricing{Input: &in, Output: &out}).Definition()
	text := d.Description
	if d.Name != Name || text != Description(&models.Pricing{Input: &in, Output: &out}) {
		t.Fatalf("the definition is %+v", d)
	}
	for _, bound := range []string{
		"at most " + strconv.Itoa(routine.MaxTitle) + " characters",
		"at most " + strconv.Itoa(trigger.MaxPurposeLength) + " characters",
		"at most " + strconv.Itoa(trigger.MaxListed) + " triggers",
		// The allowance is estimated from the model's price, and is the
		// ceiling the server enforces (spec 087).
		"$3 per million input tokens and $15 per million output tokens",
		"It is the ceiling the server enforces",
		"more than " + strconv.Itoa(trigger.SpendWindowDays) + " days old",
	} {
		if !strings.Contains(text, bound) {
			t.Errorf("the description does not say %q", bound)
		}
	}
	// A model the catalog prices at nothing is estimated from a run's
	// size alone.
	if free := New(nil, nil).Definition().Description; strings.Contains(free, "per million") || !strings.Contains(free, "does not state") {
		t.Errorf("an unpriced model's description: %s", free)
	}
	if half := Description(&models.Pricing{Input: &in}); strings.Contains(half, "per million") {
		t.Errorf("a model priced for input alone: %s", half)
	}
	var s struct {
		Properties map[string]struct {
			MaxLength int `json:"maxLength"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(d.InputSchema, &s); err != nil {
		t.Fatal(err)
	}
	if s.Properties["name"].MaxLength != routine.MaxTitle || s.Properties["why"].MaxLength != trigger.MaxPurposeLength {
		t.Fatalf("the schema bounds are %+v", s.Properties)
	}
}

// TestOnlyAListAndAPauseRunWithoutThePerson is Topos spec 086's fifth
// criterion at the tool: Confirm holds a propose, a change, a resume and a
// delete for the person and neither a list nor a pause; Probe asks the
// server about each held one and never about a pause; and each action's
// result says what was done, or why it was not.
func TestOnlyAListAndAPauseRunWithoutThePerson(t *testing.T) {
	const id = "trg_01JAAAAAAAAAAAAAAAAAAAAAAA"
	calls := map[string]string{
		ActionList:    `{"action":"list"}`,
		ActionPropose: weekly,
		ActionChange:  `{"action":"change","id":"` + id + `","why":"Earlier.","schedule":"0 8 * * 1","timeZone":"UTC"}`,
		ActionPause:   `{"action":"pause","id":"` + id + `","why":"Away."}`,
		ActionResume:  `{"action":"resume","id":"` + id + `"}`,
		ActionDelete:  `{"action":"delete","id":"` + id + `"}`,
	}
	held := map[string]string{ActionPropose: confirmPropose, ActionChange: confirmChange, ActionResume: confirmResume, ActionDelete: confirmDelete}
	f := &fakeTriggers{}
	tool := New(f, nil)
	for action, in := range calls {
		if got := tool.Confirm(tools.Call{Input: json.RawMessage(in)}); got != held[action] {
			t.Errorf("a %s is held %q, want %q", action, got, held[action])
		}
		f.checked = nil
		if r := tool.Probe(t.Context(), tools.Call{ID: "toolu_" + action, Input: json.RawMessage(in)}); r != nil {
			t.Errorf("a %s the server takes was answered %+v", action, r)
		}
		if asked := len(f.checked) == 1; asked != (held[action] != "") || asked && (f.checked[0].Verb() != action || f.checked[0].ToolUseID != "toolu_"+action) {
			t.Errorf("a %s asked the server %+v", action, f.checked)
		}
	}
	results := map[string]string{
		ActionChange: `The person said yes. "Weekly sign-ups" is changed, and it runs next at 2026-10-26T08:00:00Z.`,
		ActionPause:  `"Weekly sign-ups" is paused and runs nothing until the person resumes it.`,
		ActionResume: `The person said yes. "Weekly sign-ups" runs again, next at 2026-10-26T08:00:00Z.`,
		ActionDelete: `The person said yes. "Weekly sign-ups" is deleted, and the conversations it started keep going.`,
	}
	for action, want := range results {
		f.applied = nil
		res, err := tool.Run(t.Context(), tools.Call{ID: "toolu_" + action, Input: json.RawMessage(calls[action])})
		if err != nil || res.Outcome != tools.OutcomeOK || !strings.HasPrefix(res.Content[0].Text, want) {
			t.Errorf("a %s answered %+v, %v", action, res, err)
		}
		if len(f.applied) != 1 || f.applied[0].Verb() != action || f.applied[0].ID != id {
			t.Errorf("a %s applied %+v", action, f.applied)
		}
	}
	f.refuse = &runner.TriggerRefusal{Code: "not_found", Message: "no such trigger"}
	if res, _ := tool.Run(t.Context(), tools.Call{ID: "toolu_p", Input: json.RawMessage(calls[ActionPause])}); res.Outcome != tools.OutcomeError ||
		res.Content[0].Text != "The pause was not done (not_found: no such trigger). Tell them in one sentence; do not try it again in another way." {
		t.Errorf("a refused pause answered %+v", res)
	}
	if res, _ := tool.Run(t.Context(), tools.Call{ID: "toolu_d", Input: json.RawMessage(calls[ActionDelete])}); !strings.HasPrefix(res.Content[0].Text, "The person said yes, but the delete was not done (not_found") {
		t.Errorf("a refused delete answered %+v", res)
	}
	if r := tool.Probe(t.Context(), tools.Call{ID: "toolu_c", Input: json.RawMessage(calls[ActionChange])}); r == nil || r.Outcome != tools.OutcomeInvalidInput ||
		!strings.HasPrefix(r.Content[0].Text, "This change cannot be done, so the person was not asked and nothing changed (not_found: no such trigger).") {
		t.Errorf("a refused change was probed as %+v", r)
	}
}

// TestAProposalIsWrittenInTheRoutinesShape: a schedule and a whole
// product's arrivals of mail or of a link are written as the SDK writes a
// routine, under the routine's name; a filter no routine expresses is the
// same manifest under the event- name with the filter as its on and the
// event after the ask; and a change keeps a routine a routine.
func TestAProposalIsWrittenInTheRoutinesShape(t *testing.T) {
	read := func(body json.RawMessage) v1.Trigger {
		t.Helper()
		var tr v1.Trigger
		if err := json.Unmarshal(body, &tr); err != nil {
			t.Fatal(err)
		}
		return tr
	}
	sched := runner.Proposal{Name: "Weekly", Ask: "Count {{them}}.", Why: "Mondays.", Schedule: "0 9 * * 1", TimeZone: "UTC"}
	if n := TriggerName("ses_1", "toolu_1", sched); n != routine.Name("ses_1/toolu_1") {
		t.Fatalf("a schedule's name is %s", n)
	}
	body, err := Manifest("chat", routine.Name("ses_1/toolu_1"), sched)
	if err != nil {
		t.Fatal(err)
	}
	want, err := routine.Manifest("chat", routine.Name("ses_1/toolu_1"), nil, routine.Fields{Title: "Weekly", Instruction: "Count {{them}}.", Starts: routine.OnSchedule, Schedule: routine.ParseSchedule("0 9 * * 1", "UTC")})
	if err != nil || string(body) != string(want) {
		t.Fatalf("a schedule is written %s, want %s (%v)", body, want, err)
	}
	for product, starts := range map[string]routine.Start{"mail": routine.FromEmail, "webhook": routine.FromLink} {
		p := runner.Proposal{Name: "Arrivals", Ask: "File it.", Why: "On arrival.", On: &v1.TriggerOn{Product: product, Verbs: []string{"received"}}}
		body, err := Manifest("chat", TriggerName("ses_1", "toolu_2", p), p)
		if err != nil {
			t.Fatal(err)
		}
		want, err := routine.Manifest("chat", routine.Name("ses_1/toolu_2"), nil, routine.Fields{Title: "Arrivals", Instruction: "File it.", Starts: starts})
		if err != nil || string(body) != string(want) {
			t.Fatalf("%s is written %s, want %s (%v)", product, body, want, err)
		}
	}
	for _, on := range []v1.TriggerOn{
		{Product: "github", Verbs: []string{"push"}},
		{Product: "mail", Verbs: []string{"received"}, Resources: []string{"invoices@*"}},
		{Product: "webhook", Verbs: []string{"received", "retried"}},
	} {
		p := runner.Proposal{Name: "Pushes", Ask: "Review {{it}}.", Why: "On push.", On: &on}
		name := TriggerName("ses_1", "toolu_3", p)
		if name != routine.EventName(routine.OfferKey("ses_1", "toolu_3")) || !routine.IsEventName(name) || routine.IsName(name) {
			t.Fatalf("%+v is named %s", on, name)
		}
		body, err := Manifest("chat", name, p)
		if err != nil {
			t.Fatal(err)
		}
		tr := read(body)
		if tr.Spec.On == nil || tr.Spec.On.Product != on.Product || tr.Spec.Schedule != "" || tr.Spec.Session.Message != routine.EventMessage("Review {{it}}.") ||
			tr.Metadata.Labels[routine.Label] != "chat" || tr.Metadata.DisplayName != "Pushes" || tr.Spec.SkipIfActive == nil || *tr.Spec.SkipIfActive {
			t.Fatalf("%+v is written %s", on, body)
		}
	}
	// A change of a routine's time keeps it the SDK's routine, its ask and
	// a field the routine does not own.
	stored := read(want)
	stored.Spec.MaxActive = new(7)
	changed, err := Changed("chat", &stored, runner.Proposal{Action: ActionChange, ID: "trg_1", Why: "Later.", Schedule: "30 7 * * *", TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	if tr := read(changed); tr.Spec.Schedule != "30 7 * * *" || tr.Spec.On != nil || tr.Spec.MaxActive == nil || *tr.Spec.MaxActive != 7 || tr.Spec.Session.Message != `Count \{{them}}.` {
		t.Fatalf("the changed routine is %s", changed)
	}
	paused, err := Changed("chat", &stored, runner.Proposal{Action: ActionPause, ID: "trg_1", Why: "Away."})
	if err != nil || !read(paused).Spec.Suspend {
		t.Fatalf("the paused routine is %s, %v", paused, err)
	}
	// A trigger no routine is has the fields named written into it.
	own := v1.Trigger{APIVersion: v1.APIVersion, Kind: v1.KindTrigger, Metadata: v1.ObjectMeta{Name: "nightly"},
		Spec: v1.TriggerSpec{Agent: "agent_1", Schedule: "0 2 * * *", TimeZone: "UTC", Session: v1.TriggerSession{Message: "Build {{trigger.name}}.", Key: "k"}, Suspend: true}}
	changed, err = Changed("chat", &own, runner.Proposal{Action: ActionResume, ID: "trg_2"})
	if tr := read(changed); err != nil || tr.Spec.Suspend || tr.Spec.Session.Message != "Build {{trigger.name}}." || tr.Spec.Session.Key != "k" {
		t.Fatalf("the resumed trigger is %s, %v", changed, err)
	}
	changed, err = Changed("chat", &own, runner.Proposal{Action: ActionChange, ID: "trg_2", Why: "Pushes.", Name: "On push", On: &v1.TriggerOn{Product: "github", Verbs: []string{"push"}}, Ask: "Look."})
	if tr := read(changed); err != nil || tr.Spec.On == nil || tr.Spec.Schedule != "" || tr.Metadata.DisplayName != "On push" || tr.Spec.Session.Message != routine.EventMessage("Look.") || !tr.Spec.Suspend {
		t.Fatalf("the changed trigger is %s, %v", changed, err)
	}
}

// TestAChangeIsHeldToTheRulesOfAProposal: a change of a routine writes
// its name, its ask and a routine's events over it and refuses what a
// routine refuses; a change of any other trigger refuses a name, an ask
// or a schedule a routine's rules refuse; and a document that is no
// trigger is refused rather than written over.
func TestAChangeIsHeldToTheRulesOfAProposal(t *testing.T) {
	read := func(body json.RawMessage) v1.Trigger {
		t.Helper()
		var tr v1.Trigger
		if err := json.Unmarshal(body, &tr); err != nil {
			t.Fatal(err)
		}
		return tr
	}
	body, err := routine.Manifest("chat", routine.Name("k"), nil, routine.Fields{Title: "Weekly", Instruction: "Count.", Starts: routine.OnSchedule, Schedule: routine.ParseSchedule("0 9 * * 1", "UTC")})
	if err != nil {
		t.Fatal(err)
	}
	stored := read(body)
	stored.Spec.Suspend = true
	changed, err := Changed("chat", &stored, runner.Proposal{Action: ActionChange, ID: "trg_1", Why: "Mail.", Name: "Invoices", Ask: "Save it.", On: &v1.TriggerOn{Product: "mail", Verbs: []string{"received"}}})
	if tr := read(changed); err != nil || tr.Metadata.DisplayName != "Invoices" || tr.Spec.On == nil || tr.Spec.On.Product != "mail" || tr.Spec.Schedule != "" || !tr.Spec.Suspend {
		t.Fatalf("the routine changed to mail is %s, %v", changed, err)
	}
	resumed, err := Changed("chat", &stored, runner.Proposal{Action: ActionResume, ID: "trg_1"})
	if err != nil || read(resumed).Spec.Suspend {
		t.Fatalf("the resumed routine is %s, %v", resumed, err)
	}
	for name, p := range map[string]runner.Proposal{
		"a schedule no routine runs on": {Action: ActionChange, Why: "w", Schedule: "*/5 * * * *", TimeZone: "UTC"},
		"a name of two lines":           {Action: ActionChange, Why: "w", Name: "a\nb"},
	} {
		if _, err := Changed("chat", &stored, p); err == nil {
			t.Errorf("a routine's change to %s was written", name)
		}
	}
	own := v1.Trigger{APIVersion: v1.APIVersion, Kind: v1.KindTrigger, Metadata: v1.ObjectMeta{Name: "nightly"},
		Spec: v1.TriggerSpec{Agent: "agent_1", Schedule: "0 2 * * *", TimeZone: "UTC", Session: v1.TriggerSession{Message: "Build."}}}
	paused, err := Changed("chat", &own, runner.Proposal{Action: ActionPause, ID: "trg_2", Why: "Away."})
	if err != nil || !read(paused).Spec.Suspend {
		t.Fatalf("the paused trigger is %s, %v", paused, err)
	}
	moved, err := Changed("chat", &own, runner.Proposal{Action: ActionChange, ID: "trg_2", Why: "Later.", Schedule: "0 6 * * *", TimeZone: "Europe/Berlin"})
	if tr := read(moved); err != nil || tr.Spec.Schedule != "0 6 * * *" || tr.Spec.TimeZone != "Europe/Berlin" || tr.Spec.Session.Message != "Build." {
		t.Fatalf("the moved trigger is %s, %v", moved, err)
	}
	for name, p := range map[string]runner.Proposal{
		"a schedule no routine runs on": {Action: ActionChange, Why: "w", Schedule: "*/5 * * * *", TimeZone: "UTC"},
		"a name of two lines":           {Action: ActionChange, Why: "w", Name: "a\nb"},
		"an ask past the bound":         {Action: ActionChange, Why: "w", Ask: strings.Repeat("x", routine.MaxEventInstruction+1)},
	} {
		if _, err := Changed("chat", &own, p); err == nil {
			t.Errorf("a trigger's change to %s was written", name)
		}
	}
	for _, doc := range []string{`[]`, `{"spec":[]}`, `not json`} {
		if _, err := edit(json.RawMessage(doc), func(map[string]any, map[string]any) {}); err == nil {
			t.Errorf("%s was edited", doc)
		}
	}
	if _, err := editWithMeta(json.RawMessage(`{"metadata":[],"spec":{}}`), "Title", func(map[string]any, map[string]any) {}); err == nil {
		t.Error("a trigger whose metadata is no object was edited")
	}
	for name, p := range map[string]runner.Proposal{
		"a filter the resolver refuses": {Name: "n", Ask: "a", Why: "w", On: &v1.TriggerOn{Product: "GitHub", Verbs: []string{"push"}}},
		"an event's name of two lines":  {Name: "a\nb", Ask: "a", Why: "w", On: &v1.TriggerOn{Product: "github", Verbs: []string{"push"}}},
		"an event's ask past the bound": {Name: "n", Ask: strings.Repeat("x", routine.MaxEventInstruction+1), Why: "w", On: &v1.TriggerOn{Product: "github", Verbs: []string{"push"}}},
		"a schedule no routine runs on": {Name: "n", Ask: "a", Why: "w", Schedule: "*/5 * * * *", TimeZone: "UTC"},
	} {
		if _, err := Manifest("chat", "routine-x", p); err == nil {
			t.Errorf("%s was written", name)
		}
	}
	if Starts(v1.TriggerOn{Product: "github", Verbs: []string{"received"}}) != "" {
		t.Error("a product no routine starts from reads as a routine's")
	}
}

// TestTheToolsSmallAnswers: the tool reads, runs one call at a time,
// holds no call it cannot read, and answers a list the server refuses
// with the refusal and one it could not ask as an error.
func TestTheToolsSmallAnswers(t *testing.T) {
	f := &fakeTriggers{}
	tool := New(f, nil)
	if p := tool.Properties(); p.Effect != tools.EffectRead || p.Parallel {
		t.Fatalf("the properties are %+v", p)
	}
	if r := tool.Confirm(tools.Call{Input: json.RawMessage(`{"action":`)}); r != "" {
		t.Fatalf("a call that does not read is held %q", r)
	}
	if r := tool.Probe(t.Context(), tools.Call{Input: json.RawMessage(`{"action":`)}); r != nil {
		t.Fatalf("a call that does not read was probed: %+v", r)
	}
	if res, err := tool.Run(t.Context(), tools.Call{Input: json.RawMessage(`{"action":`)}); err != nil || res.Outcome != tools.OutcomeInvalidInput {
		t.Fatalf("a call that does not read ran: %+v, %v", res, err)
	}
	f.refuse = &runner.TriggerRefusal{Code: "forbidden", Message: "not yours"}
	if res, err := tool.Run(t.Context(), tools.Call{Input: json.RawMessage(`{"action":"list"}`)}); err != nil || res.Outcome != tools.OutcomeError || !strings.Contains(res.Content[0].Text, "forbidden") {
		t.Fatalf("a refused list answered %+v, %v", res, err)
	}
	f.refuse, f.fail = nil, errors.New("the server is away")
	if _, err := tool.Run(t.Context(), tools.Call{Input: json.RawMessage(`{"action":"list"}`)}); err == nil {
		t.Fatal("a list the server could not answer was answered as a result")
	}
}

// TestAProposalCarriesItsAllowance (spec 087): a propose's perWeekUsd is
// read as USD micros, carried to the server's questions and applied as
// spec.budget.perWeek; a change with it replaces the allowance and one
// without keeps it; an amount at or below zero, past six digits after the
// point or not a number is refused before anyone is asked.
func TestAProposalCarriesItsAllowance(t *testing.T) {
	f := &fakeTriggers{}
	tool := New(f, nil)
	call := tools.Call{ID: "toolu_b", Input: with(t, "perWeekUsd", json.Number("0.4"))}
	if r := tool.Check(call); r != nil {
		t.Fatalf("a proposal with an allowance was refused: %+v", r)
	}
	if r := tool.Probe(t.Context(), call); r != nil || len(f.checked) != 1 || f.checked[0].PerWeekUSDMicro == nil || *f.checked[0].PerWeekUSDMicro != 400000 {
		t.Fatalf("the question asked before the hold: %+v, %+v", r, f.checked)
	}
	if _, err := tool.Run(t.Context(), call); err != nil || len(f.applied) != 1 || *f.applied[0].PerWeekUSDMicro != 400000 {
		t.Fatalf("the allowed proposal: %v, %+v", err, f.applied)
	}
	for _, bad := range []json.Number{"0", "-1", "0.0000001", "1e3"} {
		r := tool.Check(tools.Call{ID: "toolu_x", Input: with(t, "perWeekUsd", bad)})
		if r == nil || r.Outcome != tools.OutcomeInvalidInput || !strings.Contains(r.Content[0].Text, "perWeekUsd") {
			t.Errorf("perWeekUsd %s: %+v", bad, r)
		}
	}
	if r := tool.Check(tools.Call{ID: "toolu_x", Input: with(t, "perWeekUsd", "five")}); r == nil || r.Outcome != tools.OutcomeInvalidInput {
		t.Errorf("a perWeekUsd that is no number: %+v", r)
	}
	change := tools.Call{ID: "toolu_c", Input: json.RawMessage(`{"action":"change","id":"trg_01JAAAAAAAAAAAAAAAAAAAAAAA","why":"It costs more now.","perWeekUsd":1.25}`)}
	if r := tool.Check(change); r != nil {
		t.Fatalf("a change of the allowance alone was refused: %+v", r)
	}

	read := func(body json.RawMessage) v1.Trigger {
		t.Helper()
		var tr v1.Trigger
		if err := json.Unmarshal(body, &tr); err != nil {
			t.Fatal(err)
		}
		return tr
	}
	p := runner.Proposal{Name: "Weekly", Ask: "Count.", Why: "Mondays, about $0.40 a week.", Schedule: "0 9 * * 1", TimeZone: "UTC", PerWeekUSDMicro: new(int64(400000))}
	body, err := Manifest("chat", routine.Name("ses_1/toolu_b"), p)
	if err != nil {
		t.Fatal(err)
	}
	stored := read(body)
	if b := stored.Spec.Budget; b == nil || b.PerWeek != 400000 {
		t.Fatalf("the proposal is written %s", body)
	}
	if _, ok := routine.Read(routine.Trigger{Name: stored.Metadata.Name, DisplayName: stored.Metadata.DisplayName, Labels: stored.Metadata.Labels,
		Schedule: stored.Spec.Schedule, TimeZone: stored.Spec.TimeZone, Message: stored.Spec.Session.Message, Title: stored.Spec.Session.Title}, "chat"); !ok {
		t.Fatal("a routine with an allowance no longer reads as a routine")
	}
	kept, err := Changed("chat", &stored, runner.Proposal{Action: ActionChange, ID: "trg_1", Why: "Later.", Schedule: "30 7 * * *", TimeZone: "UTC"})
	if err != nil || read(kept).Spec.Budget == nil || read(kept).Spec.Budget.PerWeek != 400000 {
		t.Fatalf("a change without perWeekUsd: %s, %v", kept, err)
	}
	raised, err := Changed("chat", &stored, runner.Proposal{Action: ActionChange, ID: "trg_1", Why: "More.", PerWeekUSDMicro: new(int64(1250000))})
	if err != nil || read(raised).Spec.Budget.PerWeek != 1250000 {
		t.Fatalf("a change of the allowance: %s, %v", raised, err)
	}
	own := v1.Trigger{APIVersion: v1.APIVersion, Kind: v1.KindTrigger, Metadata: v1.ObjectMeta{Name: "nightly"},
		Spec: v1.TriggerSpec{Agent: "agent_1", On: &v1.TriggerOn{Product: "github", Verbs: []string{"push"}}, Session: v1.TriggerSession{Message: "Build."}}}
	changed, err := Changed("chat", &own, runner.Proposal{Action: ActionChange, ID: "trg_2", Why: "Pushes cost more.", PerWeekUSDMicro: new(int64(2000000))})
	if err != nil || read(changed).Spec.Budget == nil || read(changed).Spec.Budget.PerWeek != 2000000 {
		t.Fatalf("an event trigger's allowance: %s, %v", changed, err)
	}
}

// TestTheListSaysEachAllowance (spec 087): list answers each trigger's
// allowance and its week's spend, and nothing for one without.
func TestTheListSaysEachAllowance(t *testing.T) {
	f := &fakeTriggers{list: []runner.ListedTrigger{
		{ID: "trg_1", Name: "routine-a", Title: "Weekly", Schedule: "0 9 * * 1", TimeZone: "UTC", PerWeekUSDMicro: new(int64(400000)), SpentUSDMicro: new(int64(120000))},
		{ID: "trg_2", Name: "routine-b", Title: "Daily", Schedule: "0 9 * * *", TimeZone: "UTC"},
	}}
	res, err := New(f, nil).Run(t.Context(), tools.Call{ID: "toolu_l", Input: json.RawMessage(`{"action":"list"}`)})
	if err != nil {
		t.Fatal(err)
	}
	want := "The person holds these triggers of you, newest first:\n" +
		"- trg_1 Weekly (routine-a): 0 9 * * 1 in UTC, at most $0.40 a week, $0.12 spent in the last 7 days\n" +
		"- trg_2 Daily (routine-b): 0 9 * * * in UTC\n"
	if res.Content[0].Text != want {
		t.Fatalf("the list answered %q, want %q", res.Content[0].Text, want)
	}
}

// TestAChangeKeepsTheModelItsTriggerNames (spec 089): the trigger tool's
// change, pause and resume of a routine, and its change of a trigger that
// is no routine, keep the spec.session.model the person chose, which no
// field of the tool names.
func TestAChangeKeepsTheModelItsTriggerNames(t *testing.T) {
	model := &v1.SessionModel{Name: "tier/thorough", Reasoning: v1.EffortHigh}
	body, err := Manifest("chat", routine.Name("ses_1/toolu_m"), runner.Proposal{Name: "Weekly", Ask: "Count.", Why: "Mondays.", Schedule: "0 9 * * 1", TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	var stored v1.Trigger
	if err := json.Unmarshal(body, &stored); err != nil {
		t.Fatal(err)
	}
	stored.Spec.Session.Model = model
	own := v1.Trigger{APIVersion: v1.APIVersion, Kind: v1.KindTrigger, Metadata: v1.ObjectMeta{Name: "nightly"},
		Spec: v1.TriggerSpec{Agent: "agent_1", On: &v1.TriggerOn{Product: "github", Verbs: []string{"push"}}, Session: v1.TriggerSession{Message: "Build.", Model: model}}}
	for _, c := range []struct {
		name    string
		trigger *v1.Trigger
		p       runner.Proposal
	}{
		{"a routine's time", &stored, runner.Proposal{Action: ActionChange, ID: "trg_1", Why: "Later.", Schedule: "30 7 * * *", TimeZone: "UTC"}},
		{"a routine's pause", &stored, runner.Proposal{Action: ActionPause, ID: "trg_1", Why: "Away."}},
		{"a routine's resume", &stored, runner.Proposal{Action: ActionResume, ID: "trg_1", Why: "Back."}},
		{"a trigger's ask", &own, runner.Proposal{Action: ActionChange, ID: "trg_2", Why: "Clearer.", Ask: "Build and test."}},
	} {
		changed, err := Changed("chat", c.trigger, c.p)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var got v1.Trigger
		if err := json.Unmarshal(changed, &got); err != nil {
			t.Fatal(err)
		}
		if m := got.Spec.Session.Model; m == nil || *m != *model {
			t.Errorf("%s writes the model %+v: %s", c.name, m, changed)
		}
	}
}

// TestAChangeKeepsTheTimesItsTriggerFiresAt (spec 090): the trigger
// tool's change of its ask, pause and resume keep the times a trigger
// fires at, a list's or a one-shot's, of a trigger in a routine's name
// and labels and of any other; a change to a schedule or to events
// replaces them, so the trigger fires on one of them.
func TestAChangeKeepsTheTimesItsTriggerFiresAt(t *testing.T) {
	times := []time.Time{time.Date(2026, 10, 26, 17, 40, 0, 0, time.UTC), time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)}
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	body, err := Manifest("chat", routine.Name("ses_1/toolu_t"), runner.Proposal{Name: "Fares", Ask: "Look at the fares.", Why: "On the days you named.", Schedule: "0 9 * * 1", TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	read := func(body json.RawMessage) v1.Trigger {
		t.Helper()
		var tr v1.Trigger
		if err := json.Unmarshal(body, &tr); err != nil {
			t.Fatal(err)
		}
		return tr
	}
	shaped := read(body)
	shaped.Spec.Schedule = ""
	plain := v1.Trigger{APIVersion: v1.APIVersion, Kind: v1.KindTrigger, Metadata: v1.ObjectMeta{Name: "fares"},
		Spec: v1.TriggerSpec{Agent: "agent_1", TimeZone: "UTC", Session: v1.TriggerSession{Message: "Look at the fares."}}}
	for _, base := range []struct {
		name string
		tr   v1.Trigger
	}{{"a routine's", shaped}, {"a trigger's", plain}} {
		for _, when := range []struct {
			name string
			set  func(*v1.TriggerSpec)
			kept func(v1.TriggerSpec) bool
		}{
			{"times", func(s *v1.TriggerSpec) { s.Times = times }, func(s v1.TriggerSpec) bool { return slices.EqualFunc(s.Times, times, time.Time.Equal) && s.At == nil }},
			{"at", func(s *v1.TriggerSpec) { s.At = &at }, func(s v1.TriggerSpec) bool { return s.At != nil && s.At.Equal(at) && s.Times == nil }},
		} {
			stored := base.tr
			when.set(&stored.Spec)
			for _, p := range []runner.Proposal{
				{Action: ActionChange, ID: "trg_1", Why: "Clearer.", Ask: "Look at the fares to Lisbon."},
				{Action: ActionPause, ID: "trg_1", Why: "Away."},
				{Action: ActionResume, ID: "trg_1", Why: "Back."},
			} {
				changed, err := Changed("chat", &stored, p)
				if err != nil {
					t.Fatalf("%s %s, %s: %v", base.name, when.name, p.Action, err)
				}
				if got := read(changed).Spec; !when.kept(got) || got.Schedule != "" {
					t.Errorf("%s %s, %s writes %s", base.name, when.name, p.Action, changed)
				}
			}
			changed, err := Changed("chat", &stored, runner.Proposal{Action: ActionChange, ID: "trg_1", Why: "Weekly now.", Schedule: "0 9 * * 1", TimeZone: "UTC"})
			if err != nil {
				t.Fatalf("%s %s to a schedule: %v", base.name, when.name, err)
			}
			if got := read(changed).Spec; got.Schedule != "0 9 * * 1" || got.Times != nil || got.At != nil {
				t.Errorf("%s %s to a schedule writes %s", base.name, when.name, changed)
			}
		}
	}
}
