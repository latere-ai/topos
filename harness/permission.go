// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"cmp"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"latere.ai/x/pkg/verdict"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/prompts"
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

// Verdict is the decision on one call: the vocabulary every decision
// point shares, latere.ai/x/pkg/verdict, ordered from most to least
// permissive.
type Verdict = verdict.Verdict

// Verdicts.
const (
	VerdictAllow = verdict.Allow
	VerdictFlag  = verdict.Flag
	VerdictAsk   = verdict.Ask
	VerdictBlock = verdict.Block
)

// Stricter returns the less permissive of two verdicts. A verdict outside
// the four counts as block, so a malformed one can only narrow.
func Stricter(a, b Verdict) Verdict { return verdict.Least(a, b) }

// CodeSandboxUnavailable is spec 012's code for progressive asked for
// on a host with no operating-system sandbox.
const CodeSandboxUnavailable = "sandbox_unavailable"

// CheckSandbox refuses progressive on a machine that records
// machine.SandboxNone. progressive runs a call the score rates low
// without asking, which spec 012 allows only where an operating-system
// sandbox holds when the score is wrong; such a host runs in plan or
// confirm, where the person decides every write.
func CheckSandbox(mode Mode, info machine.Info) error {
	if mode != ModeProgressive || info.Sandbox != machine.SandboxNone {
		return nil
	}
	return fmt.Errorf("the progressive mode needs an operating-system sandbox, and this %s host has none; run in plan or confirm", info.OS)
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

// Merge is the policy of a session from the agent's, p, and the
// organization's lists and thresholds from the authorizer's limits, by
// spec 012's rule, so neither side loosens the other: always_confirm is
// the union, always_allow the intersection when both give one and the one
// given otherwise, and each threshold the lower. The organization sets no
// mode, so the mode is the agent's; nil thresholds keep the agent's.
func (p Policy) Merge(confirm, allow []string, thresholds *Thresholds) Policy {
	out := p
	out.Mode = cmp.Or(p.Mode, ModeConfirm)
	out.AlwaysConfirm = slices.Clone(p.AlwaysConfirm)
	for _, c := range confirm {
		if !slices.Contains(out.AlwaysConfirm, c) {
			out.AlwaysConfirm = append(out.AlwaysConfirm, c)
		}
	}
	switch {
	case len(p.AlwaysAllow) > 0 && len(allow) > 0:
		out.AlwaysAllow = nil
		for _, a := range p.AlwaysAllow {
			if slices.Contains(allow, a) {
				out.AlwaysAllow = append(out.AlwaysAllow, a)
			}
		}
	case len(allow) > 0:
		out.AlwaysAllow = slices.Clone(allow)
	default:
		out.AlwaysAllow = slices.Clone(p.AlwaysAllow)
	}
	if thresholds != nil {
		out.Thresholds = Thresholds{
			FlagAt: min(p.Thresholds.FlagAt, thresholds.FlagAt), AskAt: min(p.Thresholds.AskAt, thresholds.AskAt), BlockAt: min(p.Thresholds.BlockAt, thresholds.BlockAt),
		}
	}
	return out
}

// Session is the policy as the Session header records it.
func (p Policy) Session() session.Policy {
	t := p.Thresholds
	return session.Policy{
		Mode: string(cmp.Or(p.Mode, ModeConfirm)), AlwaysConfirm: slices.Clone(p.AlwaysConfirm), AlwaysAllow: slices.Clone(p.AlwaysAllow),
		Thresholds: session.Thresholds{FlagAt: t.FlagAt, AskAt: t.AskAt, BlockAt: t.BlockAt},
	}
}

// Under is p with the mode, the lists and the thresholds of a session's
// recorded policy in place of the agent's own; the egress stays the
// agent's machine's.
func (p Policy) Under(sp session.Policy) Policy {
	t := sp.Thresholds
	return Policy{
		Mode: Mode(sp.Mode), AlwaysAllow: slices.Clone(sp.AlwaysAllow), AlwaysConfirm: slices.Clone(sp.AlwaysConfirm),
		Thresholds: Thresholds{FlagAt: t.FlagAt, AskAt: t.AskAt, BlockAt: t.BlockAt}, Egress: p.Egress,
	}
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

// Decision is a verdict with its reason, as agent.tool_use records it. A
// blocked call's reason is also its result, so the model reads it and its
// text is a prompt.
type Decision struct {
	Verdict Verdict
	Reason  string
	// ReviewProbability is the probability, fixed before the call runs,
	// that a person sees it (spec 037): 1 for an ask and a forced flag, the
	// audit rate for an automatic verdict a decision service samples, 0
	// otherwise. The harness records 1 for a shown verdict a decider gave
	// no probability.
	ReviewProbability float64
	// Draw is the uniform draw a sampled review used; nil when none was
	// drawn.
	Draw *float64
	// Suggestion is a decision service's suggestion, when one was asked.
	Suggestion *session.Suggestion
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
			return Decision{Verdict: VerdictBlock, Reason: prompts.Text(prompts.CallPlanMode)}
		}
		return Decision{Verdict: VerdictAsk, Reason: "on the organization's always-confirm list"}
	}
	switch mode {
	case ModePlan:
		if readOnly {
			return Decision{Verdict: VerdictAllow, Reason: "read-only"}
		}
		return Decision{Verdict: VerdictBlock, Reason: prompts.Text(prompts.CallPlanMode)}
	case ModeProgressive:
		t := p.Thresholds
		if t == (Thresholds{}) {
			t = DefaultThresholds
		}
		switch {
		case risk.Score < t.FlagAt:
			return Decision{Verdict: VerdictAllow, Reason: "below the flag threshold"}
		case risk.Score < t.AskAt:
			return Decision{Verdict: VerdictFlag, Reason: "between the flag and ask thresholds"}
		case risk.Score < t.BlockAt:
			return Decision{Verdict: VerdictAsk, Reason: "between the ask and block thresholds"}
		}
		return Decision{Verdict: VerdictBlock, Reason: prompts.Text(prompts.CallAboveBlock)}
	}
	switch {
	case readOnly:
		return Decision{Verdict: VerdictAllow, Reason: "read-only"}
	case matchAny(p.AlwaysAllow, name, subject):
		return Decision{Verdict: VerdictAllow, Reason: "on the always-allow list"}
	case matchAny(remembered, name, subject):
		return Decision{Verdict: VerdictAllow, Reason: "allowed earlier in this session"}
	case machineKind == machine.KindCella && props.Effect == tools.EffectWrite:
		return Decision{Verdict: VerdictAllow, Reason: "stays inside the sandbox"}
	}
	return Decision{Verdict: VerdictAsk, Reason: "needs a confirmation"}
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
