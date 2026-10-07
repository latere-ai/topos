// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/hostmatch"

	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// MaxNetworkHosts is the most hosts a network's hosts name (spec 052).
const MaxNetworkHosts = 512

// MaxInitiatorInstructions is the longest initiator's instructions an
// allow of session.create carries, in bytes of UTF-8 (spec 053).
const MaxInitiatorInstructions = 8 << 10

// Network is a session's network as an allow of session.create or
// session.send names it (spec 052): a mode, open, allowlist or none; the
// host patterns of an allowlist, each an exact name or one leading "*.";
// and whether a first contact with a host outside the network asks the
// person attending the session, which only an allowlist does.
type Network struct {
	Mode  string   `json:"mode"`
	Hosts []string `json:"hosts,omitempty"`
	Ask   bool     `json:"ask,omitempty"`
}

// Thresholds are the progressive permission mode's score cut-offs
// (spec 012), each a risk score between 0 and 1.
type Thresholds struct {
	FlagAt  float64 `json:"flag_at"`
	AskAt   float64 `json:"ask_at"`
	BlockAt float64 `json:"block_at"`
}

// Owner is the person or organization an agent belongs to at the
// installation's identity provider, as the authorizer names it (spec
// 018).
type Owner struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// The owner types an Owner names.
const (
	OwnerUser         = "user"
	OwnerOrganization = "organization"
)

// Limits are what an allow of session.create granted, an allow of
// agent.create or agent.update named, or an allow of session.create,
// session.update or session.send routed, decoded from the answer's
// limits object. The session takes the lowest of each figure
// against the agent's and the request's own (spec 005), and merges the
// lists and thresholds with the agent's approvals so neither loosens the
// other (spec 012). A member the answer left out is its zero here: no
// list, no thresholds, no ceiling, no model.
type Limits struct {
	// AlwaysConfirm and AlwaysAllow are the organization's permission
	// patterns (spec 012).
	AlwaysConfirm []string
	AlwaysAllow   []string
	// Thresholds lower the agent's cut-offs, each to the lower of the
	// two; nil keeps the agent's.
	Thresholds *Thresholds
	// BudgetUSDMicro is a ceiling on the session's spend in micro-USD;
	// nil is no ceiling, and a ceiling of zero allows no model request.
	BudgetUSDMicro *int64
	// TurnTimeout and MaxAge are ceilings; zero is none.
	TurnTimeout time.Duration
	MaxAge      time.Duration
	// Scope is the session's starting scope, one grant per entry in the
	// grammar of an agent's permissions (spec 018).
	Scope []json.RawMessage
	// Retention is how long the session is kept after it ends; zero keeps
	// it until it is deleted.
	Retention time.Duration
	// Owner is, on an allow of an agent's apply, the owner of an agent
	// that gets its identity; nil is the applier as a person.
	Owner *Owner
	// Model is the name of the model the session runs in place of the
	// one asked (spec 038): its agent's on an allow of session.create,
	// the one the change names on session.update, and the one the
	// session stands on at session.send. Empty runs the one asked.
	Model string
	// Reasoning is the reasoning level the session's next turn runs at,
	// read where Model is (spec 049): nil keeps the level the session
	// has, one of manifest/v1's Efforts sets it, and a pointer to ""
	// returns the session to its agent's own.
	Reasoning *string
	// Network is the session's network, read where Model is on an allow
	// of session.create and session.send (spec 052), its hosts lowercased
	// and sorted; nil keeps the agent's at a create and the session's at a
	// send.
	Network *Network
	// Instructions are the initiator's standing instructions, read on an
	// allow of session.create alone (spec 053); empty is none.
	Instructions string
	// Repositories are what an allow of session.create attaches beside
	// the request's repositories, each a repository resource marked
	// attached, optionally naming the app it is the source of (spec 058).
	Repositories []session.Resource
	// Context is the titled text an allow of session.create attaches,
	// which the model reads for the session's life (spec 058).
	Context []session.ContextPart
}

// WireRepository is one repository an allow of session.create attaches
// (spec 058): a repository as a request names it, and the app at the
// installation's app host it is the source of, nil for none.
type WireRepository struct {
	URL string               `json:"url"`
	Ref string               `json:"ref,omitempty"`
	App *session.ResourceApp `json:"app,omitempty"`
}

// WireLimits is the limits object as an answer carries it, so an
// authorizer renders its answer through the type toposd decodes. Every
// member is optional and a member left nil is left out.
type WireLimits struct {
	AlwaysConfirm  []string              `json:"always_confirm,omitempty"`
	AlwaysAllow    []string              `json:"always_allow,omitempty"`
	Thresholds     *Thresholds           `json:"thresholds,omitempty"`
	BudgetUSDMicro *int64                `json:"budget_usd_micro,omitempty"`
	TurnTimeout    string                `json:"turn_timeout,omitempty"`
	MaxAge         string                `json:"max_age,omitempty"`
	Scope          []json.RawMessage     `json:"scope,omitempty"`
	Retention      string                `json:"retention,omitempty"`
	Owner          *Owner                `json:"owner,omitempty"`
	Model          string                `json:"model,omitempty"`
	Reasoning      *string               `json:"reasoning,omitempty"`
	Network        *Network              `json:"network,omitempty"`
	Instructions   string                `json:"instructions,omitempty"`
	Repositories   []WireRepository      `json:"repositories,omitempty"`
	Context        []session.ContextPart `json:"context,omitempty"`
}

// DecodeLimits reads a decision's limits object. A decision with none is
// the zero Limits, and a member toposd does not know is ignored. A known
// member that does not decode is an error, and toposd refuses the request
// as authorizer_unavailable: a ceiling it cannot read is one it cannot
// apply.
func DecodeLimits(d authz.Decision) (Limits, error) {
	var w WireLimits
	if err := d.DecodeLimits(&w); err != nil {
		return Limits{}, fmt.Errorf("limits: %w", err)
	}
	l := Limits{AlwaysConfirm: w.AlwaysConfirm, AlwaysAllow: w.AlwaysAllow, Scope: w.Scope}
	if t := w.Thresholds; t != nil {
		if t.FlagAt < 0 || t.FlagAt > t.AskAt || t.AskAt > t.BlockAt || t.BlockAt > 1 {
			return Limits{}, fmt.Errorf("limits.thresholds %+v are not 0 <= flag_at <= ask_at <= block_at <= 1", *t)
		}
		l.Thresholds = t
	}
	if b := w.BudgetUSDMicro; b != nil {
		if *b < 0 {
			return Limits{}, fmt.Errorf("limits.budget_usd_micro is %d, below zero", *b)
		}
		l.BudgetUSDMicro = b
	}
	for _, f := range []struct {
		name string
		src  string
		dst  *time.Duration
	}{
		{"turn_timeout", w.TurnTimeout, &l.TurnTimeout},
		{"max_age", w.MaxAge, &l.MaxAge},
		{"retention", w.Retention, &l.Retention},
	} {
		if f.src == "" {
			continue
		}
		v, err := time.ParseDuration(f.src)
		if err != nil {
			return Limits{}, fmt.Errorf("limits.%s: %w", f.name, err)
		}
		if v <= 0 {
			return Limits{}, fmt.Errorf("limits.%s is %s, not a positive duration", f.name, f.src)
		}
		*f.dst = v
	}
	for i, g := range w.Scope {
		if t := bytes.TrimSpace(g); len(t) == 0 || t[0] != '{' {
			return Limits{}, fmt.Errorf("limits.scope[%d] is not a grant object", i)
		}
	}
	if o := w.Owner; o != nil {
		if (o.Type != OwnerUser && o.Type != OwnerOrganization) || o.ID == "" {
			return Limits{}, fmt.Errorf("limits.owner %+v is not a user or an organization with an id", *o)
		}
		l.Owner = o
	}
	// A name is sent to the gateway as it is written, so one with space
	// around it names no model.
	if strings.TrimSpace(w.Model) != w.Model {
		return Limits{}, fmt.Errorf("limits.model is %q, not a model's name", w.Model)
	}
	l.Model = w.Model
	if r := w.Reasoning; r != nil {
		if *r != "" && !slices.Contains(v1.Efforts, *r) {
			return Limits{}, fmt.Errorf("limits.reasoning is %q, not one of %s or empty for the agent's own", *r, strings.Join(v1.Efforts, ", "))
		}
		l.Reasoning = r
	}
	if n := w.Network; n != nil {
		checked, err := checkNetwork(*n)
		if err != nil {
			return Limits{}, err
		}
		l.Network = &checked
	}
	if len(w.Instructions) > MaxInitiatorInstructions {
		return Limits{}, fmt.Errorf("limits.instructions is %d bytes, more than %d", len(w.Instructions), MaxInitiatorInstructions)
	}
	// encoding/json replaces invalid UTF-8 and a lone surrogate escape as
	// it decodes, so the text is held to UTF-8 as the answer wrote it.
	var raw struct {
		Instructions json.RawMessage `json:"instructions"`
		Context      []struct {
			Title json.RawMessage `json:"title"`
			Text  json.RawMessage `json:"text"`
		} `json:"context"`
	}
	if err := d.DecodeLimits(&raw); err != nil {
		return Limits{}, fmt.Errorf("limits: %w", err)
	}
	if !validString(raw.Instructions) {
		return Limits{}, errors.New("limits.instructions is not valid UTF-8")
	}
	l.Instructions = w.Instructions
	repos, err := attachedRepositories(w.Repositories)
	if err != nil {
		return Limits{}, err
	}
	l.Repositories = repos
	for i, p := range raw.Context {
		if !validString(p.Title) || !validString(p.Text) {
			return Limits{}, fmt.Errorf("limits.context[%d] is not valid UTF-8", i)
		}
	}
	if err := session.CheckContext(w.Context); err != nil {
		return Limits{}, fmt.Errorf("limits.%w", err)
	}
	l.Context = w.Context
	return l, nil
}

// attachedRepositories reads an allow's repositories (spec 058): at most
// session.MaxRepositories, each a repository a request could name over
// https, its app, when it names one, held to session.CheckApp, and no URL
// or app named twice. Each is a repository resource marked attached.
func attachedRepositories(ws []WireRepository) ([]session.Resource, error) {
	if len(ws) > session.MaxRepositories {
		return nil, fmt.Errorf("limits.repositories names %d repositories, more than %d", len(ws), session.MaxRepositories)
	}
	var out []session.Resource
	urls, slugs := map[string]bool{}, map[string]bool{}
	for i, w := range ws {
		r := session.Resource{Type: session.ResourceRepository, URL: w.URL, Ref: w.Ref, App: w.App, Attached: true}
		if err := session.CheckRepository(r, "https"); err != nil {
			return nil, fmt.Errorf("limits.repositories[%d]: %w", i, err)
		}
		if urls[w.URL] {
			return nil, fmt.Errorf("limits.repositories[%d] names %s a second time", i, w.URL)
		}
		urls[w.URL] = true
		if a := w.App; a != nil {
			if err := session.CheckApp(*a); err != nil {
				return nil, fmt.Errorf("limits.repositories[%d].app: %w", i, err)
			}
			if slugs[a.Slug] {
				return nil, fmt.Errorf("limits.repositories[%d] names the app %s a second time", i, a.Slug)
			}
			slugs[a.Slug] = true
		}
		out = append(out, r)
	}
	return out, nil
}

// validString reports whether a JSON string member, as raw bytes, is
// valid UTF-8 whose every \u escape is a character or a surrogate pair.
// An absent or null member is valid.
func validString(raw json.RawMessage) bool {
	if !utf8.Valid(raw) {
		return false
	}
	b := bytes.TrimSpace(raw)
	surrogate := func(i int) (rune, bool) {
		if i+6 > len(b) || b[i] != '\\' || b[i+1] != 'u' {
			return 0, false
		}
		v, err := strconv.ParseUint(string(b[i+2:i+6]), 16, 16)
		return rune(v), err == nil
	}
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' || i+1 >= len(b) {
			continue
		}
		if b[i+1] != 'u' {
			i++
			continue
		}
		r, ok := surrogate(i)
		switch {
		case !ok:
			return false
		case utf16.IsSurrogate(r):
			low, ok := surrogate(i + 6)
			if !ok || utf16.DecodeRune(r, low) == utf8.RuneError {
				return false
			}
			i += 11
		default:
			i += 5
		}
	}
	return true
}

// checkNetwork holds a network to Cella's rules (spec 052): one of the
// three modes; hosts and ask with an allowlist alone; at most
// MaxNetworkHosts hosts, each an exact name or one leading "*.", never an
// address, a port or a single label. The hosts it answers are lowercased,
// without a repeat, sorted, so two answers that name one network compare
// equal.
func checkNetwork(n Network) (Network, error) {
	if !slices.Contains(session.NetworkModes, n.Mode) {
		return Network{}, fmt.Errorf("limits.network.mode is %q, not one of %s", n.Mode, strings.Join(session.NetworkModes, ", "))
	}
	if n.Mode != session.NetworkAllowlist && (len(n.Hosts) > 0 || n.Ask) {
		return Network{}, fmt.Errorf("limits.network names hosts or ask with mode %s; only an allowlist takes them", n.Mode)
	}
	if len(n.Hosts) > MaxNetworkHosts {
		return Network{}, fmt.Errorf("limits.network.hosts names %d hosts, more than %d", len(n.Hosts), MaxNetworkHosts)
	}
	for i, h := range n.Hosts {
		if !hostmatch.ValidPattern(h) || isAddr(strings.TrimPrefix(h, "*.")) {
			return Network{}, fmt.Errorf("limits.network.hosts[%d] is %q, not a host name or a \"*.\" pattern", i, h)
		}
	}
	n.Hosts = session.JoinHosts(n.Hosts)
	return n, nil
}

// isAddr reports whether a host is an IP address, which a network names
// by no pattern: Cella's host rule admits names alone.
func isAddr(h string) bool {
	_, err := netip.ParseAddr(h)
	return err == nil
}
