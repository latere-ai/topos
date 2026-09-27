// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// Mode is how a session decides its calls (spec 012).
type Mode string

// Modes.
const (
	ModePlan        Mode = "plan"
	ModeConfirm     Mode = "confirm"
	ModeProgressive Mode = "progressive"
)

// Verdict is the decision on one call, ordered from most to least
// permissive.
type Verdict string

// Verdicts.
const (
	VerdictAllow Verdict = "allow"
	VerdictFlag  Verdict = "flag"
	VerdictAsk   Verdict = "ask"
	VerdictBlock Verdict = "block"
)

var verdictOrder = []Verdict{VerdictAllow, VerdictFlag, VerdictAsk, VerdictBlock}

// Stricter returns the less permissive of two verdicts.
func Stricter(a, b Verdict) Verdict {
	if slices.Index(verdictOrder, b) > slices.Index(verdictOrder, a) {
		return b
	}
	return a
}

// Thresholds are the progressive mode's score cut-offs.
type Thresholds struct {
	FlagAt  float64 `json:"flag_at"`
	AskAt   float64 `json:"ask_at"`
	BlockAt float64 `json:"block_at"`
}

// DefaultThresholds are spec 012's.
var DefaultThresholds = Thresholds{FlagAt: 0.3, AskAt: 0.5, BlockAt: 0.9}

// Policy is a session's permission policy: the mode, the organization's
// lists and the thresholds, from the authorizer's decision at session
// start or the agent's manifest.
type Policy struct {
	Mode          Mode
	AlwaysAllow   []string
	AlwaysConfirm []string
	Thresholds    Thresholds
	// Egress are the hosts the agent's machine may reach, for the
	// external-effect feature.
	Egress []string
}

// RiskSource is the source of the rule-feature score.
const RiskSource = "rules/1"

// networkPrograms raise a host bash call's score.
var networkPrograms = regexp.MustCompile(`(^|[\s;&|(])(curl|wget|ssh|scp|rsync|git\s+push|rm\s+-[a-zA-Z]*r)`)

// Score is the rule-feature risk of one call (spec 012).
func Score(name string, props tools.Properties, input json.RawMessage, machineKind string, egress []string) session.Risk {
	r := session.Risk{Source: RiskSource}
	host := machineKind != machine.KindCella
	switch {
	case props.Effect == tools.EffectNone || props.Effect == tools.EffectRead:
		r.Score, r.Features = 0, []string{"effect:" + string(props.Effect)}
	case name == "memory_sync":
		r.Score, r.Features = 0.1, []string{"memory_sync"}
	case props.Effect == tools.EffectWrite && !host:
		r.Score, r.Features = 0.1, []string{"effect:write", "machine:cella"}
	case name == "bash" && host:
		r.Score, r.Features = 0.5, []string{"bash", "machine:host"}
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(input, &in) == nil && networkPrograms.MatchString(in.Command) {
			r.Score, r.Features = 0.7, append(r.Features, "command:network_or_delete")
		}
	case props.Effect == tools.EffectWrite:
		r.Score, r.Features = 0.3, []string{"effect:write", "machine:host"}
	case props.Effect == tools.EffectExternal:
		r.Score, r.Features = 0.6, []string{"effect:external"}
		if h := fetchHost(input); h != "" && slices.Contains(egress, h) {
			r.Score, r.Features = 0.4, []string{"effect:external", "egress:named"}
		}
	default:
		r.Score, r.Features = 0.6, []string{"effect:unknown"}
	}
	return r
}

func fetchHost(input json.RawMessage) string {
	var in struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	rest, ok := strings.CutPrefix(in.URL, "https://")
	if !ok {
		rest, _ = strings.CutPrefix(in.URL, "http://")
	}
	host, _, _ := strings.Cut(rest, "/")
	host, _, _ = strings.Cut(host, ":")
	return strings.ToLower(host)
}

// Decision is a verdict with its reason, as agent.tool_use records it.
type Decision struct {
	Verdict Verdict
	Reason  string
}

// Decide applies the lists and the mode to one scored call. remembered
// are the session's own allow patterns, from confirmations answered with
// remember.
func (p Policy) Decide(name string, props tools.Properties, input json.RawMessage, risk session.Risk, machineKind string, remembered []string) Decision {
	mode := p.Mode
	if mode == "" {
		mode = ModeConfirm
	}
	readOnly := props.Effect == tools.EffectNone || props.Effect == tools.EffectRead
	subject := patternSubject(name, input)
	if matchAny(p.AlwaysConfirm, name, subject) {
		if mode == ModePlan {
			return Decision{VerdictBlock, "Plan mode: only read-only tools run."}
		}
		return Decision{VerdictAsk, "on the organization's always-confirm list"}
	}
	switch mode {
	case ModePlan:
		if readOnly {
			return Decision{VerdictAllow, "read-only"}
		}
		return Decision{VerdictBlock, "Plan mode: only read-only tools run."}
	case ModeProgressive:
		t := p.Thresholds
		if t == (Thresholds{}) {
			t = DefaultThresholds
		}
		switch {
		case risk.Score < t.FlagAt:
			return Decision{VerdictAllow, "below the flag threshold"}
		case risk.Score < t.AskAt:
			return Decision{VerdictFlag, "between the flag and ask thresholds"}
		case risk.Score < t.BlockAt:
			return Decision{VerdictAsk, "between the ask and block thresholds"}
		}
		return Decision{VerdictBlock, "above the block threshold"}
	}
	switch {
	case readOnly:
		return Decision{VerdictAllow, "read-only"}
	case matchAny(p.AlwaysAllow, name, subject):
		return Decision{VerdictAllow, "on the always-allow list"}
	case matchAny(remembered, name, subject):
		return Decision{VerdictAllow, "allowed earlier in this session"}
	case machineKind == machine.KindCella && props.Effect == tools.EffectWrite:
		return Decision{VerdictAllow, "stays inside the sandbox"}
	}
	return Decision{VerdictAsk, "needs a confirmation"}
}

// patternSubject is what a pattern's glob matches for a call: the
// command of bash, the path of a file tool, domain:<host> for web_fetch.
func patternSubject(name string, input json.RawMessage) string {
	var in struct {
		Command string `json:"command"`
		Path    string `json:"path"`
	}
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	switch name {
	case "bash":
		return in.Command
	case "web_fetch":
		if h := fetchHost(input); h != "" {
			return "domain:" + h
		}
	}
	return in.Path
}

// matchAny reports whether a call matches one of the patterns:
// "<tool>" or "<tool>(<glob>)".
func matchAny(patterns []string, name, subject string) bool {
	pathLike := name != "bash" && name != "web_fetch"
	for _, p := range patterns {
		tool, glob, hasGlob := strings.Cut(p, "(")
		if tool != name {
			continue
		}
		if !hasGlob {
			return true
		}
		if globRegexp(strings.TrimSuffix(glob, ")"), pathLike).MatchString(subject) {
			return true
		}
	}
	return false
}

// globRegexp compiles a pattern glob. For a path "*" stays within one
// segment and "**" crosses segments; for a command or a domain "*"
// matches any characters.
func globRegexp(glob string, pathLike bool) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		switch {
		case strings.HasPrefix(glob[i:], "**"):
			b.WriteString(".*")
			i++
		case glob[i] == '*' && pathLike:
			b.WriteString("[^/]*")
		case glob[i] == '*':
			b.WriteString(".*")
		case glob[i] == '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(glob[i : i+1]))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}
