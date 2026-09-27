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
	"unicode/utf8"

	"latere.ai/x/topos/harness"
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
	maxNameLength      = 63
	maxToolNameLength  = 64
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
		v.trigger(o.trig.Spec)
	case o.store != nil:
		v.memoryStore(o.store.Spec)
	default:
		v.connection(o.conn.Spec)
	}
}

func (v *validator) meta(m v1.ObjectMeta) {
	v.name("metadata.name", m.Name)
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
	v.oneOf(at+".identity", s.Identity, v1.IdentityPerson, v1.IdentityAgent)
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
	if m.Effort != "" {
		v.oneOf(at+".effort", m.Effort, v1.EffortMinimal, v1.EffortLow, v1.EffortMedium, v1.EffortHigh)
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
		switch {
		case t.Client && builtin:
			v.add(tp+".name", "a client tool may not take a built-in's name")
		case t.Client:
			if t.Description == "" {
				v.add(tp+".description", "a client tool needs a description")
			}
			if len(t.InputSchema) == 0 {
				v.add(tp+".inputSchema", "a client tool needs an input schema")
			}
		case !builtin:
			v.add(tp+".name", "not a built-in tool; declare a client tool with client: true")
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

func (v *validator) machine(at string, m v1.Machine) {
	v.oneOf(at+".kind", m.Kind, v1.MachineHost, v1.MachineCella)
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
	if s.Schedule == "" {
		v.add("spec.schedule", "required")
	} else if !validSchedule(s.Schedule) {
		v.add("spec.schedule", "not a five-field cron expression or @hourly, @daily, @weekly")
	}
	if s.TimeZone != "UTC" && !timeZone.MatchString(s.TimeZone) {
		v.add("spec.timeZone", "not an IANA time zone name")
	}
	ss := s.Session
	if ss.Message == "" {
		v.add("spec.session.message", "required")
	}
	v.text("spec.session.message", ss.Message)
	if m := ss.Machine; m != nil && m.Kind != "" {
		v.oneOf("spec.session.machine.kind", m.Kind, v1.MachineHost, v1.MachineCella)
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
			if r.URL == "" {
				v.add(rp+".url", "required")
			}
			if r.MemoryStore != "" || r.Access != "" {
				v.add(rp, "memoryStore and access are for a memory store")
			}
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
	v.duration("spec.maxAge", s.MaxAge)
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

// cronFields are the bounds of the five cron fields: minute, hour, day
// of month, month, day of week (0 and 7 are Sunday).
var cronFields = [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}

// validSchedule checks a cron expression: five fields of *, numbers,
// ranges and lists, each with an optional /step, or one of the three
// macros.
func validSchedule(s string) bool {
	switch s {
	case "@hourly", "@daily", "@weekly":
		return true
	}
	fields := strings.Fields(s)
	if len(fields) != len(cronFields) {
		return false
	}
	for i, f := range fields {
		for item := range strings.SplitSeq(f, ",") {
			if !cronItem(item, cronFields[i][0], cronFields[i][1]) {
				return false
			}
		}
	}
	return true
}

func cronItem(item string, lo, hi int) bool {
	base, step, stepped := strings.Cut(item, "/")
	if stepped {
		n, err := strconv.Atoi(step)
		if err != nil || n < 1 {
			return false
		}
	}
	if base == "*" {
		return true
	}
	from, to, ranged := strings.Cut(base, "-")
	a, err := strconv.Atoi(from)
	if err != nil || a < lo || a > hi {
		return false
	}
	if !ranged {
		return true
	}
	b, err := strconv.Atoi(to)
	return err == nil && b >= a && b <= hi
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
