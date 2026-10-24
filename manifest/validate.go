// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/manifest/trigger"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
)

// The ranges of spec 003's tables.
const (
	MaxConcurrentLimit = 32
	MinCompactAt       = 0.5
	MaxCompactAt       = 0.95
	MaxDescription     = 1024
	// MaxDisplayName bounds metadata.displayName, in characters.
	MaxDisplayName    = 200
	maxNameLength     = 63
	maxToolNameLength = 64
)

var (
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	toolName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	// hostName is a DNS name, optionally with a leading *. wildcard.
	hostName = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	// quantity is a Kubernetes quantity: a decimal with an optional SI
	// or binary suffix.
	quantity = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(m|k|M|G|T|P|E|Ki|Mi|Gi|Ti|Pi|Ei)?$`)
	// timeZone is the shape of an IANA zone name; the zone itself is
	// looked up where the trigger fires, so every build resolves the
	// same manifest the same way whatever time zone data it carries.
	timeZone = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9_+-]+)*$`)
	// headerName is an HTTP header field name.
	headerName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
)

// validator checks decoded, defaulted objects and collects every
// problem. Secret findings are kept apart: they carry their own code.
type validator struct {
	doc      int
	problems []Problem
	secrets  []Problem
	builtins []string
	// now is the resolver's clock, which a one-shot trigger's time is
	// held to.
	now func() time.Time
}

func (v *validator) add(path, detail string) {
	v.problems = append(v.problems, Problem{Doc: v.doc, Path: path, Detail: detail})
}

// text runs the input check over a free-text field.
func (v *validator) text(path, s string) {
	if kinds := secretKinds(s); len(kinds) > 0 {
		v.secrets = append(v.secrets, Problem{Doc: v.doc, Path: path, Detail: "holds " + strings.Join(kinds, " and ")})
	}
}

func (v *validator) object(o *object) {
	v.meta(*o.meta())
	switch {
	case o.agent != nil:
		v.agent("spec", o.agent.Spec)
	case o.trig != nil:
		// The hold over every trigger (spec 086) and the read of an
		// owner's runs (spec 088) are routes of the trigger's own path, so
		// no trigger takes their names.
		switch o.trig.Metadata.Name {
		case trigger.HoldName:
			v.add("metadata.name", trigger.HoldName+" is the name of the route that holds every trigger; choose another")
		case trigger.RunsName:
			v.add("metadata.name", trigger.RunsName+" is the name of the route that reads every trigger's runs; choose another")
		}
		v.trigger(o.trig.Spec)
	case o.store != nil:
		v.memoryStore(o.store.Spec)
	default:
		v.connection(o.conn.Spec)
	}
}

func (v *validator) meta(m v1.ObjectMeta) {
	v.name("metadata.name", m.Name)
	v.displayName("metadata.displayName", m.DisplayName)
	for _, set := range []struct {
		field string
		m     map[string]string
	}{{"metadata.labels", m.Labels}, {"metadata.annotations", m.Annotations}} {
		for k := range set.m {
			if strings.HasPrefix(k, v1.ReservedPrefix) {
				v.add(set.field+"["+k+"]", "keys under "+v1.ReservedPrefix+" are the core's own")
			}
		}
	}
}

// name checks a required DNS label.
func (v *validator) name(at, s string) {
	switch {
	case s == "":
		v.add(at, "required")
	case len(s) > maxNameLength || !dnsLabel.MatchString(s):
		v.add(at, "not a DNS label: lowercase letters, digits and hyphens, at most 63 characters")
	}
}

// displayName checks an optional display name: text a person reads on
// one line, so not blank, at most MaxDisplayName characters, and free of
// control characters, line and paragraph separators, and the
// bidirectional controls that would reorder how it renders. Every other
// character, joiners and emoji included, is accepted.
func (v *validator) displayName(at, s string) {
	if s == "" {
		return
	}
	switch n := utf8.RuneCountInString(s); {
	case strings.TrimSpace(s) == "":
		v.add(at, "blank; leave it out to show the name")
	case n > MaxDisplayName:
		v.add(at, fmt.Sprintf("%d characters, at most %d", n, MaxDisplayName))
	case strings.ContainsFunc(s, unprintable):
		v.add(at, "holds a control character, a line break or a bidirectional control")
	}
	v.text(at, s)
}

// unprintable is a character a one-line display name refuses.
func unprintable(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp, unicode.Bidi_Control)
}

// ref checks a reference: a name, or an id with the kind's prefix, and
// for an agent an optional @<version>.
func (v *validator) ref(at, s, prefix string) {
	if s == "" {
		v.add(at, "required")
		return
	}
	if strings.HasPrefix(s, prefix) {
		id, ver, pinned := strings.Cut(s, "@")
		if session.CheckID(prefix, id) != nil {
			v.add(at, "not a "+prefix+"<ulid> id")
			return
		}
		if pinned {
			n, err := strconv.Atoi(ver)
			if prefix != session.PrefixAgent || err != nil || n < 1 {
				v.add(at, "a version is @<n> from 1, and only an agent has versions")
			}
		}
		return
	}
	if len(s) > maxNameLength || !dnsLabel.MatchString(s) {
		v.add(at, "not a name or a "+prefix+"<ulid> id")
	}
}

func (v *validator) oneOf(at, s string, allowed ...string) {
	if !slices.Contains(allowed, s) {
		v.add(at, "not one of "+strings.Join(allowed, ", "))
	}
}

func (v *validator) duration(at, s string) {
	if s == "" {
		return
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		v.add(at, "not a positive Go duration, for example 90m")
	}
}

func (v *validator) decimal(at, s string) {
	if s == "" {
		return
	}
	if _, err := models.ParsePrice(s); err != nil {
		v.add(at, "not a non-negative decimal string with at most 6 fraction digits")
	}
}

func (v *validator) hosts(at string, hosts []string) {
	for i, h := range hosts {
		if len(h) > 253 || !hostName.MatchString(h) {
			v.add(indexed(at, i), "not a host name")
		}
	}
	v.unique(at, hosts)
}

func (v *validator) unique(at string, names []string) {
	seen := map[string]bool{}
	for i, n := range names {
		if n != "" && seen[n] {
			v.add(indexed(at, i), "repeats an earlier entry")
		}
		seen[n] = true
	}
}

func (v *validator) httpURL(at, s string) {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		v.add(at, "not an http or https URL")
	}
}

func (v *validator) agent(at string, s v1.AgentSpec) {
	v.model(at+".model", s.Model)
	v.text(at+".instructions", s.Instructions)
	v.tools(at+".tools", s.Tools)
	for i, p := range s.Permissions {
		if p.Action == "" {
			v.add(indexed(at+".permissions", i)+".action", "required")
		}
		if p.Resource == "" {
			v.add(indexed(at+".permissions", i)+".resource", "required")
		}
	}
	v.approvals(at+".approvals", s.Approvals)
	for i, h := range s.Hooks {
		hp := indexed(at+".hooks", i)
		v.oneOf(hp+".event", h.Event, v1.HookPreToolUse, v1.HookPostToolUse, v1.HookTurnStart, v1.HookTurnEnd, v1.HookPreCompact)
		if h.Command == "" {
			v.add(hp+".command", "required")
		}
		v.text(hp+".command", h.Command)
		if h.Matcher != "" {
			v.pattern(hp+".matcher", h.Matcher)
		}
		v.duration(hp+".timeout", h.Timeout)
	}
	v.subagents(at+".subagents", s.Subagents)
	if d := *s.Threads.MaxDepth; d < 1 || d > harness.MaxDepthLimit {
		v.add(at+".threads.maxDepth", fmt.Sprintf("%d is outside 1 to %d", d, harness.MaxDepthLimit))
	}
	if c := *s.Threads.MaxConcurrent; c < 1 || c > MaxConcurrentLimit {
		v.add(at+".threads.maxConcurrent", fmt.Sprintf("%d is outside 1 to %d", c, MaxConcurrentLimit))
	}
	if a := s.Advisor; a != nil {
		v.model(at+".advisor.model", a.Model)
		v.text(at+".advisor.instructions", a.Instructions)
	}
	if t := s.Title; t != nil && t.Model != nil && strings.TrimSpace(*t.Model) == "" {
		v.add(at+".title.model", "empty: name a model, or leave model out to ask for the session's own")
	}
	for i, sk := range s.Skills {
		sp := indexed(at+".skills", i)
		switch {
		case sk.Git == "" && sk.Path == "":
			v.add(sp, "set path, or git with an optional ref and path")
		case sk.Git == "" && sk.Ref != "":
			v.add(sp+".ref", "a ref needs git")
		}
	}
	v.mcpServers(at+".mcpServers", s.MCPServers)
	names := make([]string, 0, len(s.MemoryStores))
	for i, m := range s.MemoryStores {
		mp := indexed(at+".memoryStores", i)
		v.ref(mp+".name", m.Name, session.PrefixMemory)
		v.oneOf(mp+".access", m.Access, v1.AccessReadWrite, v1.AccessReadOnly)
		names = append(names, m.Name)
	}
	v.unique(at+".memoryStores", names)
	for i, c := range s.Connections {
		v.name(indexed(at+".connections", i), c)
	}
	v.unique(at+".connections", s.Connections)
	v.repositories(at+".repositories", s.Repositories)
	v.machine(at+".machine", s.Machine)
	v.decimal(at+".budget.maxCost", s.Budget.MaxCost)
	v.duration(at+".limits.turnTimeout", s.Limits.TurnTimeout)
	v.duration(at+".limits.maxAge", s.Limits.MaxAge)
	if c := *s.Context.CompactAt; c < MinCompactAt || c > MaxCompactAt {
		v.add(at+".context.compactAt", fmt.Sprintf("%g is outside %g to %g", c, MinCompactAt, MaxCompactAt))
	}
}

func (v *validator) model(at string, m v1.AgentModel) {
	if strings.TrimSpace(m.Name) == "" {
		v.add(at+".name", "required")
	}
	if m.Family != "" {
		v.oneOf(at+".family", m.Family, models.FamilyAnthropic, models.FamilyOpenAI, models.FamilyOther)
	}
	if m.Dialect != "" {
		v.oneOf(at+".dialect", m.Dialect, "anthropic-messages", "openai-responses", "openai-chat")
	}
	if m.BaseURL != "" {
		v.httpURL(at+".baseURL", m.BaseURL)
	}
	if m.Credential != "" {
		v.ref(at+".credential", m.Credential, session.PrefixCredential)
		v.text(at+".credential", m.Credential)
	}
	// The level is named reasoning, or effort as before the rename; a
	// model that names both names one level.
	if m.Reasoning != "" {
		v.oneOf(at+".reasoning", m.Reasoning, v1.Efforts...)
	}
	if m.Effort != "" {
		v.oneOf(at+".effort", m.Effort, v1.Efforts...)
	}
	if m.Reasoning != "" && m.Effort != "" && m.Reasoning != m.Effort {
		v.add(at+".reasoning", fmt.Sprintf("%q, and effort names %q: name the level once, as reasoning", m.Reasoning, m.Effort))
	}
	if m.InputWindow < 0 {
		v.add(at+".inputWindow", "negative")
	}
	if m.MaxOutputTokens < 0 {
		v.add(at+".maxOutputTokens", "negative")
	}
	if p := m.Pricing; p != nil {
		v.decimal(at+".pricing.input", p.Input)
		v.decimal(at+".pricing.output", p.Output)
		v.decimal(at+".pricing.cacheRead", p.CacheRead)
		v.decimal(at+".pricing.cacheWrite", p.CacheWrite)
	}
}

// reservedFor names the tool each name that is no built-in's is kept
// for.
var reservedFor = map[string]string{
	harness.ToolQuestion: "harness's question tool", session.ToolPublish: "hosted runner's publish tool",
	session.ToolTrigger:           "hosted runner's trigger tool",
	session.ToolWake:              "hosted runner's wake tool",
	session.ToolNotify:            "harness's notify tool",
	harness.ToolRequestConnection: "harness's request_connection tool",
}

func (v *validator) tools(at string, list []v1.Tool) {
	names := make([]string, 0, len(list))
	for i, t := range list {
		tp := indexed(at, i)
		names = append(names, t.Name)
		switch {
		case t.Name == "":
			v.add(tp+".name", "required")
			continue
		case len(t.Name) > maxToolNameLength || !toolName.MatchString(t.Name):
			v.add(tp+".name", "not a tool name: letters, digits, _ and -, at most 64 characters")
			continue
		}
		if t.OutputLimit < 0 {
			v.add(tp+".outputLimit", "negative")
		}
		builtin := slices.Contains(v.builtins, t.Name)
		// The question tool is the harness's own (spec 039), and the publish
		// and trigger tools the hosted runner's (specs 043 and 086): an agent
		// names each among its tools, and no client tool may take their
		// names.
		harnessTool := t.Name == harness.ToolQuestion || t.Name == session.ToolPublish || t.Name == session.ToolTrigger || t.Name == session.ToolWake || t.Name == session.ToolNotify
		switch {
		case t.Client && builtin:
			v.add(tp+".name", "a client tool may not take a built-in's name")
		// request_connection is offered by the harness while a session has a
		// service the person may be asked to connect, and no agent names it
		// (spec 069); a client tool of the name would take its place.
		case t.Client && (harnessTool || t.Name == harness.ToolRequestConnection):
			v.add(tp+".name", "the name "+t.Name+" is reserved for the "+reservedFor[t.Name]+"; rename the client tool")
		case harnessTool && (t.Description != "" || len(t.InputSchema) > 0 || t.OutputLimit != 0):
			v.add(tp, "the "+t.Name+" tool takes only its name")
		case harnessTool:
		case t.Client:
			if t.Description == "" {
				v.add(tp+".description", "a client tool needs a description")
			}
			if len(t.InputSchema) == 0 {
				v.add(tp+".inputSchema", "a client tool needs an input schema")
			}
		case !builtin:
			v.add(tp+".name", "not a built-in tool, web_search, history, question, publish, trigger, wake or notify; declare a client tool with client: true")
		case t.Description != "" || len(t.InputSchema) > 0:
			v.add(tp, "a built-in takes only name and outputLimit")
		}
	}
	v.unique(at, names)
}

func (v *validator) approvals(at string, a v1.Approvals) {
	v.oneOf(at+".mode", a.Mode, v1.ModePlan, v1.ModeConfirm, v1.ModeProgressive)
	for i, p := range a.AlwaysAllow {
		v.pattern(indexed(at+".alwaysAllow", i), p)
	}
	for i, p := range a.AlwaysConfirm {
		v.pattern(indexed(at+".alwaysConfirm", i), p)
	}
	t := a.Thresholds
	vals := []float64{*t.FlagAt, *t.AskAt, *t.BlockAt}
	for i, f := range vals {
		name := []string{"flagAt", "askAt", "blockAt"}[i]
		if f < 0 || f > 1 {
			v.add(at+".thresholds."+name, fmt.Sprintf("%g is outside 0 to 1", f))
		}
	}
	if vals[0] >= vals[1] || vals[1] >= vals[2] {
		v.add(at+".thresholds", "flagAt, askAt and blockAt must increase")
	}
}

// pattern checks a permission pattern: <tool> or <tool>(<glob>).
func (v *validator) pattern(at, p string) {
	tool, glob, hasGlob := strings.Cut(p, "(")
	if !toolName.MatchString(tool) || hasGlob && (!strings.HasSuffix(glob, ")") || strings.Contains(strings.TrimSuffix(glob, ")"), ")")) {
		v.add(at, "not a pattern of the form <tool> or <tool>(<glob>)")
	}
}

func (v *validator) subagents(at string, subs []v1.Subagent) {
	names := make([]string, 0, len(subs))
	for i, s := range subs {
		sp := indexed(at, i)
		v.name(sp+".name", s.Name)
		names = append(names, s.Name)
		switch {
		case s.Agent != "" && s.Spec != nil:
			v.add(sp, "set agent or spec, not both")
		case s.Agent != "":
			v.ref(sp+".agent", s.Agent, session.PrefixAgent)
		case s.Spec != nil:
			v.agent(sp+".spec", *s.Spec)
		default:
			v.add(sp, "set agent, a reference, or spec, an inline agent")
		}
	}
	v.unique(at, names)
}

func (v *validator) mcpServers(at string, servers []v1.MCPServer) {
	names := make([]string, 0, len(servers))
	for i, s := range servers {
		sp := indexed(at, i)
		names = append(names, s.Name)
		if s.Name == "" || len(s.Name) > maxToolNameLength || !toolName.MatchString(s.Name) {
			v.add(sp+".name", "required: letters, digits, _ and -, at most 64 characters")
		}
		switch {
		case (s.Command == "") == (s.URL == ""):
			v.add(sp, "set exactly one of command, for stdio, and url, for streamable HTTP")
		case s.URL != "":
			v.httpURL(sp+".url", s.URL)
			if len(s.Args) > 0 || len(s.Env) > 0 {
				v.add(sp, "args and env are for a stdio server")
			}
			if s.Connection != "" {
				v.name(sp+".connection", s.Connection)
			}
		case s.Connection != "":
			v.add(sp+".connection", "a connection is for an HTTP server")
		}
		for j, a := range s.Args {
			v.text(indexed(sp+".args", j), a)
		}
		for _, k := range sortedStrings(s.Env) {
			v.text(sp+".env["+k+"]", s.Env[k])
		}
	}
	v.unique(at, names)
}

// repositories checks an agent's repositories as the API checks a
// session's, since a session that names none takes them (spec 019): at
// most session.MaxRepositories, each an https URL naming its host with no
// credential, and a ref git reads as a name.
func (v *validator) repositories(at string, repos []v1.Repository) {
	if len(repos) > session.MaxRepositories {
		v.add(at, fmt.Sprintf("%d repositories, at most %d", len(repos), session.MaxRepositories))
	}
	for i, r := range repos {
		rp := indexed(at, i)
		if r.URL == "" {
			v.add(rp+".url", "required")
			continue
		}
		v.text(rp+".url", r.URL)
		if err := session.CheckRepository(session.Resource{URL: r.URL, Ref: r.Ref}, "https"); err != nil {
			v.add(rp, err.Error())
		}
	}
}

func (v *validator) machine(at string, m v1.Machine) {
	if m.Kind == session.MachineDevice {
		// A person's own computer is named per session at its create, never
		// by an agent, whose sessions any person may start (spec 080).
		v.add(at+".kind", "device is named per session when it is created, never in an agent's manifest; an agent's machine is host or cella")
	} else {
		v.oneOf(at+".kind", m.Kind, v1.MachineHost, v1.MachineCella)
	}
	if m.Kind == v1.MachineHost {
		if m.Image != "" || m.Environment != "" || m.Resources != (v1.Resources{}) {
			v.add(at, "image, environment and resources are for a cella machine")
		}
	}
	if m.Kind == v1.MachineCella && (len(m.Roots) > 0 || len(m.ReadPaths) > 0) {
		v.add(at, "roots and readPaths are for the host")
	}
	for _, q := range []struct{ name, v string }{{"cpu", m.Resources.CPU}, {"memory", m.Resources.Memory}, {"disk", m.Resources.Disk}} {
		if q.v != "" && !quantity.MatchString(q.v) {
			v.add(at+".resources."+q.name, "not a Kubernetes quantity, for example 500m or 4Gi")
		}
	}
	if m.EgressMode != "" {
		v.oneOf(at+".egressMode", m.EgressMode, v1.EgressOpen, v1.EgressAllowlist, v1.EgressNone)
	}
	// Cella refuses allowedHosts outside allowlist, so hosts with another
	// mode would name hosts the machine never reaches.
	if len(m.Egress) > 0 && m.Mode() != v1.EgressAllowlist {
		v.add(at+".egress", "set only with egressMode allowlist; egressMode is "+m.EgressMode)
	}
	v.hosts(at+".egress", m.Egress)
	for _, list := range []struct {
		name  string
		paths []string
	}{{"roots", m.Roots}, {"readPaths", m.ReadPaths}} {
		for i, p := range list.paths {
			if !path.IsAbs(p) || path.Clean(p) != p {
				v.add(indexed(at+"."+list.name, i), "not a clean absolute path")
			}
		}
	}
}

func (v *validator) trigger(s v1.TriggerSpec) {
	v.ref("spec.agent", s.Agent, session.PrefixAgent)
	set := 0
	for _, given := range []bool{s.Schedule != "", s.On != nil, s.At != nil} {
		if given {
			set++
		}
	}
	switch {
	case set == 0:
		v.add("spec.schedule", "required: set schedule, on for delivered events, or at for one time")
	case set > 1:
		v.add("spec", "set one of schedule, on and at")
	case s.Schedule != "":
		v.schedule("spec.schedule", s.Schedule)
	case s.On != nil:
		v.on("spec.on", *s.On)
	case !s.At.After(v.now()):
		v.add("spec.at", "is in the past; a one-shot trigger fires at a time to come")
	}
	if s.TimeZone != "UTC" && !timeZone.MatchString(s.TimeZone) {
		v.add("spec.timeZone", "not an IANA time zone name")
	}
	// A schedule fires with no event, so a path into one names nothing.
	scheduled := s.On == nil
	ss := s.Session
	if ss.Message == "" {
		v.add("spec.session.message", "required")
	}
	v.template("spec.session.message", ss.Message, scheduled)
	v.text("spec.session.message", ss.Message)
	v.template("spec.session.title", ss.Title, scheduled)
	v.template("spec.session.key", ss.Key, scheduled)
	if ss.Policy != "" {
		v.oneOf("spec.session.policy", ss.Policy, v1.PolicyNew, v1.PolicyContinue)
	}
	if ss.Notify != "" {
		v.oneOf("spec.session.notify", ss.Notify, v1.NotifyAlways, v1.NotifyJudged)
	}
	// A target is the session a firing continues, in place of the one its
	// key names (spec 086); the server holds it to an open session of the
	// trigger's agent the applier may send to.
	if ss.Target != "" {
		switch {
		case ss.Policy != v1.PolicyContinue:
			v.add("spec.session.target", "set only under policy continue, whose firings continue a session")
		case ss.Key != "":
			v.add("spec.session.target", "names the session every firing continues, so a key names none; set target or key")
		}
		if session.CheckID(session.PrefixSession, ss.Target) != nil {
			v.add("spec.session.target", "not a "+session.PrefixSession+"<ulid> id")
		}
	}
	if m := ss.Machine; m != nil && m.Kind != "" {
		v.oneOf("spec.session.machine.kind", m.Kind, v1.MachineHost, v1.MachineCella, session.MachineDevice)
		switch {
		case m.Kind == session.MachineDevice && m.Image != "":
			v.add("spec.session.machine.image", "a device runs the computer's own tools, and takes no image")
		case m.Kind == session.MachineDevice:
			// The session a firing starts works on the computer and in the
			// folder named here (spec 080).
			if err := session.CheckDevice(m.Environment, m.Workdir); err != nil {
				v.add("spec.session.machine", strings.TrimPrefix(err.Error(), session.ErrInvalidDevice.Error()+": "))
			}
		case m.Workdir != "":
			v.add("spec.session.machine.workdir", "set only on a device")
		}
	}
	for i, r := range ss.Resources {
		rp := indexed("spec.session.resources", i)
		switch r.Type {
		case v1.ResourceMemoryStore:
			v.ref(rp+".memoryStore", r.MemoryStore, session.PrefixMemory)
			v.oneOf(rp+".access", r.Access, v1.AccessReadWrite, v1.AccessReadOnly)
			if r.URL != "" || r.Ref != "" {
				v.add(rp, "url and ref are for a repository")
			}
		case v1.ResourceRepository:
			v.triggerRepository(rp, r, scheduled)
		default:
			v.oneOf(rp+".type", r.Type, v1.ResourceMemoryStore, v1.ResourceRepository)
		}
	}
	if b := ss.Budget; b != nil {
		v.decimal("spec.session.budget.maxCost", b.MaxCost)
	}
	if l := ss.Limits; l != nil {
		v.duration("spec.session.limits.turnTimeout", l.TurnTimeout)
		v.duration("spec.session.limits.maxAge", l.MaxAge)
	}
	if b := s.Budget; b != nil && b.PerWeek <= 0 {
		v.add("spec.budget.perWeek", fmt.Sprintf("%d is not above zero: an allowance is in USD micros, 5000000 for five dollars a week", b.PerWeek))
	}
	if n := s.MaxActive; n != nil && (*n < 1 || *n > trigger.MaxActiveCeiling) {
		v.add("spec.maxActive", fmt.Sprintf("%d is outside 1 to %d", *n, trigger.MaxActiveCeiling))
	}
	v.duration("spec.maxAge", s.MaxAge)
}

// schedule checks a cron expression, and that it fires at all: a
// schedule of a day no calendar has, such as the 30th of February, is
// refused.
func (v *validator) schedule(at, s string) {
	sc, err := trigger.ParseSchedule(s)
	if err != nil {
		v.add(at, err.Error())
		return
	}
	if sc.Next(time.Unix(0, 0), time.UTC).IsZero() {
		v.add(at, "matches no day of the calendar, so it never fires")
	}
}

// on checks an event filter by trigger.CheckFilter's rules.
func (v *validator) on(at string, on v1.TriggerOn) {
	for _, p := range trigger.CheckFilter(on) {
		v.add(at+"."+p.Path, p.Detail)
	}
}

// template checks a template field: every {{ opens a placeholder of a
// known root, and a schedule's names no event.
func (v *validator) template(at, s string, scheduled bool) {
	if s == "" {
		return
	}
	t, err := trigger.Parse(s)
	switch {
	case err != nil:
		v.add(at, err.Error())
	case scheduled && t.Uses(trigger.RootEvent):
		v.add(at, "a schedule trigger fires with no event, so an event path names nothing; use trigger or firing")
	}
}

// triggerRepository checks a triggered session's repository. The url and
// the ref are templates; one that holds no placeholder is checked as the
// API checks a session's repository, and a rendered one where the
// firing starts its session.
func (v *validator) triggerRepository(at string, r v1.SessionResource, scheduled bool) {
	if r.MemoryStore != "" || r.Access != "" {
		v.add(at, "memoryStore and access are for a memory store")
	}
	if r.URL == "" {
		v.add(at+".url", "required")
		return
	}
	v.text(at+".url", r.URL)
	v.template(at+".url", r.URL, scheduled)
	v.template(at+".ref", r.Ref, scheduled)
	u, uerr := trigger.Parse(r.URL)
	ref, rerr := trigger.Parse(r.Ref)
	if uerr != nil || rerr != nil || !u.Literal() || !ref.Literal() {
		return
	}
	lit := session.Resource{URL: u.Render(nil), Ref: ref.Render(nil)}
	if err := session.CheckRepository(lit, "https"); err != nil {
		v.add(at, err.Error())
	}
}

func (v *validator) memoryStore(s v1.MemoryStoreSpec) {
	switch n := utf8.RuneCountInString(s.Description); {
	case strings.TrimSpace(s.Description) == "":
		v.add("spec.description", "required")
	case n > MaxDescription:
		v.add("spec.description", fmt.Sprintf("%d characters, at most %d", n, MaxDescription))
	}
}

func (v *validator) connection(s v1.ConnectionSpec) {
	if s.Service == "" {
		v.add("spec.service", "required")
	}
	v.oneOf("spec.mode", s.Mode, v1.ConnectionPerson, v1.ConnectionAgent)
	if len(s.Hosts) == 0 {
		v.add("spec.hosts", "required")
	}
	v.hosts("spec.hosts", s.Hosts)
	switch {
	case s.Mode == v1.ConnectionAgent && s.Credential == "":
		v.add("spec.credential", "required when mode is agent")
	case s.Mode == v1.ConnectionPerson && s.Credential != "":
		v.add("spec.credential", "refused when mode is person: the credential is the person's own")
	case s.Credential != "":
		v.ref("spec.credential", s.Credential, session.PrefixCredential)
	}
	v.text("spec.credential", s.Credential)
	if !headerName.MatchString(s.Inject.Header) {
		v.add("spec.inject.header", "not an HTTP header name")
	}
	if !strings.Contains(s.Inject.Format, "{value}") {
		v.add("spec.inject.format", "must contain {value}")
	}
}

func indexed(at string, i int) string { return fmt.Sprintf("%s[%d]", at, i) }

func sortedStrings(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
