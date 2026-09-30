// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"io/fs"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	v1 "latere.ai/x/topos/manifest/v1"
)

// The fixed defaults of spec 003's tables. A default that comes from
// elsewhere (the catalog, TOPOS_MODELS_URL, Cella, the installation, the
// agent a trigger runs) is applied where the value is used and is not
// written into the resolved spec, so the digest depends on the manifest
// and this build's fixed defaults alone.
const (
	DefaultImage         = "base"
	DefaultTurnTimeout   = "2h"
	DefaultMaxAge        = "168h"
	DefaultCompactAt     = 0.8
	DefaultTimeZone      = "UTC"
	DefaultTriggerMaxAge = "1h"
	DefaultInjectHeader  = "Authorization"
	DefaultInjectFormat  = "Bearer {value}"
)

// Builtins are the names of the built-in tools, the tools of an agent
// whose manifest lists none.
func Builtins() []string {
	all := tools.Builtins()
	names := make([]string, 0, len(all))
	for _, t := range all {
		names = append(names, t.Definition().Name)
	}
	return names
}

// defaulter fills the fixed defaults in place and inlines
// instructionsFile, recording what it cannot read.
type defaulter struct {
	doc      int
	files    fs.FS
	problems []Problem
}

func (d *defaulter) add(path, detail string) {
	d.problems = append(d.problems, Problem{Doc: d.doc, Path: path, Detail: detail})
}

func (d *defaulter) object(o *object) {
	switch {
	case o.agent != nil:
		d.agent("spec", &o.agent.Spec)
	case o.trig != nil:
		triggerDefaults(&o.trig.Spec)
	case o.conn != nil:
		connection(&o.conn.Spec)
	}
}

func (d *defaulter) agent(path string, s *v1.AgentSpec) {
	if s.InstructionsFile != "" {
		d.instructionsFile(path, s)
	}
	if s.Tools == nil {
		for _, n := range Builtins() {
			s.Tools = append(s.Tools, v1.Tool{Name: n})
		}
	}
	if s.Approvals.Mode == "" {
		s.Approvals.Mode = v1.ModeConfirm
	}
	t := &s.Approvals.Thresholds
	setFloat(&t.FlagAt, harness.DefaultThresholds.FlagAt)
	setFloat(&t.AskAt, harness.DefaultThresholds.AskAt)
	setFloat(&t.BlockAt, harness.DefaultThresholds.BlockAt)
	setInt(&s.Threads.MaxDepth, harness.DefaultMaxDepth)
	setInt(&s.Threads.MaxConcurrent, harness.DefaultMaxConcurrent)
	if s.Machine.Kind == "" {
		s.Machine.Kind = v1.MachineHost
	}
	if s.Machine.Kind == v1.MachineCella && s.Machine.Image == "" {
		s.Machine.Image = DefaultImage
	}
	if s.Limits.TurnTimeout == "" {
		s.Limits.TurnTimeout = DefaultTurnTimeout
	}
	if s.Limits.MaxAge == "" {
		s.Limits.MaxAge = DefaultMaxAge
	}
	setFloat(&s.Context.CompactAt, DefaultCompactAt)
	for i := range s.Subagents {
		if sub := s.Subagents[i].Spec; sub != nil {
			d.agent(indexed(path+".subagents", i)+".spec", sub)
		}
	}
}

// instructionsFile reads the file into Instructions. A spec that also
// sets instructions is refused by validation, which sees both.
func (d *defaulter) instructionsFile(path string, s *v1.AgentSpec) {
	at := path + ".instructionsFile"
	switch {
	case s.Instructions != "":
		d.add(at, "set instructions or instructionsFile, not both")
		return
	case d.files == nil:
		d.add(at, "this caller reads no files; inline the instructions")
		return
	case !fs.ValidPath(s.InstructionsFile):
		d.add(at, "not a relative path inside the manifest's directory")
		return
	}
	b, err := fs.ReadFile(d.files, s.InstructionsFile)
	if err != nil {
		d.add(at, "the file cannot be read")
		return
	}
	s.Instructions, s.InstructionsFile = string(b), ""
}

// triggerDefaults writes a trigger's fixed defaults. The fields spec 022
// added take theirs where the trigger fires (package trigger), so a
// trigger that sets none of them keeps its digest; endOnIdle's default
// follows the policy, false under continue, whose sessions take the
// next firing's message.
func triggerDefaults(s *v1.TriggerSpec) {
	if s.TimeZone == "" {
		s.TimeZone = DefaultTimeZone
	}
	setBool(&s.Session.EndOnIdle, s.Session.Policy != v1.PolicyContinue)
	setBool(&s.SkipIfActive, true)
	if s.MaxAge == "" {
		s.MaxAge = DefaultTriggerMaxAge
	}
}

func connection(s *v1.ConnectionSpec) {
	if s.Inject.Header == "" {
		s.Inject.Header = DefaultInjectHeader
	}
	if s.Inject.Format == "" {
		s.Inject.Format = DefaultInjectFormat
	}
}

func setFloat(p **float64, v float64) {
	if *p == nil {
		*p = &v
	}
}

func setInt(p **int, v int) {
	if *p == nil {
		*p = &v
	}
}

func setBool(p **bool, v bool) {
	if *p == nil {
		*p = &v
	}
}
