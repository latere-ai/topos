// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"time"

	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// ErrNotFound is a Lookup's answer for an object that does not exist or
// that the caller may not see; the resolver reports it as
// unknown_reference. Any other error stops Resolve as it is.
var ErrNotFound = errors.New("manifest: no such object")

// Lookup answers for the stored objects: the ones a manifest references
// and the current versions of the ones it declares. The server answers
// from its store, the topos command from its local state. Every object
// it returns carries its status: id, version and digest.
type Lookup interface {
	// Agent returns an agent by name or agent_<ulid>, its latest
	// version, or by agent_<ulid>@<n>, that version.
	Agent(ctx context.Context, ref string) (*v1.Agent, error)
	Trigger(ctx context.Context, name string) (*v1.Trigger, error)
	// MemoryStore returns a memory store by name or mem_<ulid>.
	MemoryStore(ctx context.Context, ref string) (*v1.MemoryStore, error)
	Connection(ctx context.Context, name string) (*v1.Connection, error)
	// Credential returns the cred_<ulid> id of a credential named by
	// name or id. Credentials are not manifests and hold no spec here.
	Credential(ctx context.Context, ref string) (string, error)
}

// Options are what one Resolve runs under.
type Options struct {
	// Lookup answers for the stored objects; nil knows none, so every
	// reference must name a document of the same file.
	Lookup Lookup
	// Files is the directory the manifest came from, for
	// instructionsFile; nil refuses instructionsFile.
	Files fs.FS
	// NewID mints the id of an object the Lookup does not hold; nil is
	// session.NewID.
	NewID func(prefix string) string
	// Now stamps status.createdAt of a new version; nil is time.Now.
	Now func() time.Time
}

// Resolved is one resolved document.
type Resolved struct {
	Kind string
	Name string
	// Doc is the document's index in the file, from 0.
	Doc int
	// Spec is the resolved spec in canonical JSON: fields in declaration
	// order, fixed defaults written out, references pinned.
	Spec json.RawMessage
	// Digest is the sha256 of Spec, as status.digest.
	Digest string
	// One of these is set, by Kind, with its status written.
	Agent       *v1.Agent
	Trigger     *v1.Trigger
	MemoryStore *v1.MemoryStore
	Connection  *v1.Connection
	// Pinned are, for an Agent, the agents its subagents reference, and
	// theirs in turn down to its threads.maxDepth, by pinned reference
	// agent_<ulid>@<n>. They are what AgentConfig builds the subagents
	// from.
	Pinned map[string]*v1.Agent
}

// Status is the status the resolver wrote.
func (r Resolved) Status() v1.Status {
	switch {
	case r.Agent != nil:
		return r.Agent.Status
	case r.Trigger != nil:
		return r.Trigger.Status
	case r.MemoryStore != nil:
		return r.MemoryStore.Status
	case r.Connection != nil:
		return r.Connection.Status
	}
	return v1.Status{}
}

// Resolve reads the documents of one file, YAML or a JSON stream, and
// resolves each in five stages, each whole before the next: decode,
// the strict field check, defaulting, validation, and reference
// resolution. Every problem of a stage is collected, with its field
// path, into one *Error. The documents come back in dependency order:
// each after every document of the file it references, otherwise in
// file order. The same bytes, options and Lookup answers resolve to the
// same result.
func Resolve(ctx context.Context, docs []byte, o Options) ([]Resolved, error) {
	objs, n, invalid, err := decodeAll(docs)
	if err != nil {
		return nil, err
	}
	var secrets []Problem
	builtins := Builtins()
	for _, ob := range objs {
		d := &defaulter{doc: ob.doc, files: o.Files}
		d.object(ob)
		v := &validator{doc: ob.doc, builtins: builtins}
		v.object(ob)
		invalid = append(invalid, d.problems...)
		invalid = append(invalid, beside(v.problems, invalid)...)
		secrets = append(secrets, v.secrets...)
	}
	invalid = append(invalid, duplicates(objs)...)
	if len(invalid) > 0 {
		return nil, newError(CodeInvalidManifest, n, invalid)
	}
	if len(secrets) > 0 {
		return nil, newError(CodeHoldsSecret, n, secrets)
	}
	order, cyclic := sortByReference(objs)
	if len(cyclic) > 0 {
		return nil, newError(CodeInvalidManifest, n, cyclic)
	}
	r := &resolver{o: o, batch: map[string]*object{}, pinned: map[string]*v1.Agent{}}
	out := make([]Resolved, 0, n)
	for _, ob := range order {
		res, err := r.object(ctx, ob)
		if err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	if len(r.unknown) > 0 {
		return nil, newError(CodeUnknownReference, n, r.unknown)
	}
	for i := range out {
		if out[i].Agent != nil {
			pinned, err := r.pinnedOf(ctx, out[i].Agent.Spec)
			if err != nil {
				return nil, err
			}
			out[i].Pinned = pinned
		}
	}
	return out, nil
}

// decodeAll parses and decodes every document and returns the objects,
// the number of documents, and the problems of the strict field check.
// A document of another apiVersion refuses the whole file with
// unsupported_version before anything else is reported.
func decodeAll(body []byte) ([]*object, int, []Problem, error) {
	trees, err := parse(body)
	if err != nil {
		return nil, 0, nil, err
	}
	n := len(trees)
	if n == 0 {
		return nil, 0, nil, newError(CodeInvalidManifest, 1, []Problem{{Detail: "the file holds no document"}})
	}
	var version, invalid []Problem
	var objs []*object
	for i, tree := range trees {
		kind, vp, ip := envelope(i, tree)
		version, invalid = append(version, vp...), append(invalid, ip...)
		m, ok := tree.(map[string]any)
		if !ok || kind == "" || len(vp) > 0 {
			continue
		}
		ob, problems := decodeObject(i, kind, m, false)
		invalid = append(invalid, problems...)
		objs = append(objs, ob)
	}
	if len(version) > 0 {
		return nil, 0, nil, newError(CodeUnsupportedVersion, n, version)
	}
	return objs, n, invalid, nil
}

// beside drops the problems at or under a path an earlier problem of
// the same document already covers: a field that did not decode is left
// zero, and validation would report it again as missing.
func beside(problems, earlier []Problem) []Problem {
	var out []Problem
	for _, p := range problems {
		covered := slices.ContainsFunc(earlier, func(e Problem) bool {
			return e.Doc == p.Doc && e.Path != "" && (p.Path == e.Path || strings.HasPrefix(p.Path, e.Path+".") || strings.HasPrefix(p.Path, e.Path+"["))
		})
		if !covered {
			out = append(out, p)
		}
	}
	return out
}

// duplicates refuses two documents of one kind with one name.
func duplicates(objs []*object) []Problem {
	var out []Problem
	seen := map[string]int{}
	for _, ob := range objs {
		key := ob.kind + "/" + ob.name
		if first, ok := seen[key]; ok {
			out = append(out, Problem{Doc: ob.doc, Path: "metadata.name", Detail: fmt.Sprintf("document %d already declares this %s", first+1, ob.kind)})
			continue
		}
		seen[key] = ob.doc
	}
	return out
}

// edge is one reference by name from a document to another kind.
type edge struct{ kind, name string }

// references lists the names an object references, by kind. A reference
// by id never names a document of the file.
func references(ob *object) []edge {
	var out []edge
	add := func(kind, ref string) {
		if ref != "" && !strings.Contains(ref, "_") {
			out = append(out, edge{kind, ref})
		}
	}
	switch {
	case ob.agent != nil:
		var walk func(s v1.AgentSpec)
		walk = func(s v1.AgentSpec) {
			for _, sub := range s.Subagents {
				add(v1.KindAgent, sub.Agent)
				if sub.Spec != nil {
					walk(*sub.Spec)
				}
			}
			for _, m := range s.MemoryStores {
				add(v1.KindMemoryStore, m.Name)
			}
			for _, c := range s.Connections {
				add(v1.KindConnection, c)
			}
			for _, m := range s.MCPServers {
				add(v1.KindConnection, m.Connection)
			}
		}
		walk(ob.agent.Spec)
	case ob.trig != nil:
		add(v1.KindAgent, ob.trig.Spec.Agent)
		for _, r := range ob.trig.Spec.Session.Resources {
			add(v1.KindMemoryStore, r.MemoryStore)
		}
	}
	return out
}

// sortByReference orders the documents so each follows the documents
// of the file it references, ties in file order. References pin ids and
// versions, so a cycle among a file's documents has no order to resolve
// in and is refused.
func sortByReference(objs []*object) ([]*object, []Problem) {
	byKey := map[edge]*object{}
	for _, ob := range objs {
		byKey[edge{ob.kind, ob.name}] = ob
	}
	deps := map[*object][]*object{}
	for _, ob := range objs {
		for _, e := range references(ob) {
			if dep, ok := byKey[e]; ok && !slices.Contains(deps[ob], dep) {
				deps[ob] = append(deps[ob], dep)
			}
		}
	}
	done := map[*object]bool{}
	var order []*object
	for len(order) < len(objs) {
		progressed := false
		for _, ob := range objs {
			if done[ob] || slices.ContainsFunc(deps[ob], func(d *object) bool { return !done[d] }) {
				continue
			}
			done[ob] = true
			order = append(order, ob)
			progressed = true
			break
		}
		if !progressed {
			var out []Problem
			for _, ob := range objs {
				if !done[ob] {
					out = append(out, Problem{Doc: ob.doc, Path: "metadata.name", Detail: "in a cycle of references among this file's documents"})
				}
			}
			return nil, out
		}
	}
	return order, nil
}

// resolver pins references and writes statuses, one document at a time
// in dependency order.
type resolver struct {
	o Options
	// batch holds the documents resolved so far, by kind/name.
	batch map[string]*object
	// pinned holds every agent resolved or looked up, by its pinned
	// reference.
	pinned  map[string]*v1.Agent
	unknown []Problem
	doc     int
}

func (r *resolver) object(ctx context.Context, ob *object) (Resolved, error) {
	r.doc = ob.doc
	var err error
	switch {
	case ob.agent != nil:
		err = r.agentSpec(ctx, "spec", &ob.agent.Spec)
	case ob.trig != nil:
		err = r.trigger(ctx, &ob.trig.Spec)
	case ob.conn != nil:
		if c := ob.conn.Spec.Credential; c != "" {
			ob.conn.Spec.Credential, err = r.credential(ctx, "spec.credential", c)
		}
	}
	if err != nil {
		return Resolved{}, err
	}
	spec, err := session.Marshal(ob.spec())
	if err != nil {
		return Resolved{}, fmt.Errorf("manifest: render the spec of %s %s: %w", ob.kind, ob.name, err)
	}
	digest := string(session.DigestOf(spec))
	st, err := r.status(ctx, ob, digest)
	if err != nil {
		return Resolved{}, err
	}
	*ob.status() = st
	r.batch[ob.kind+"/"+ob.name] = ob
	res := Resolved{Kind: ob.kind, Name: ob.name, Doc: ob.doc, Spec: spec, Digest: digest,
		Agent: ob.agent, Trigger: ob.trig, MemoryStore: ob.store, Connection: ob.conn}
	if ob.agent != nil {
		r.pinned[pin(st)] = ob.agent
	}
	return res, nil
}

// pin is an agent's pinned reference.
func pin(st v1.Status) string { return st.ID + "@" + strconv.Itoa(st.Version) }

// status is the object's status against its stored latest version: the
// same digest keeps that version, a different one is the next version
// under the same id, and an object the Lookup does not hold is version
// 1 under a new id.
func (r *resolver) status(ctx context.Context, ob *object, digest string) (v1.Status, error) {
	now := time.Now
	if r.o.Now != nil {
		now = r.o.Now
	}
	existing, err := r.stored(ctx, ob.kind, ob.name)
	if err != nil {
		return v1.Status{}, err
	}
	if existing == nil {
		id := ""
		if prefix := idPrefix(ob.kind); prefix != "" {
			id = r.newID(prefix)
		}
		return v1.Status{ID: id, Version: 1, Digest: digest, CreatedAt: now().UTC()}, nil
	}
	if existing.Digest == digest {
		return *existing, nil
	}
	return v1.Status{ID: existing.ID, Version: existing.Version + 1, Digest: digest, CreatedAt: now().UTC(), Identity: existing.Identity, Owner: existing.Owner}, nil
}

func (r *resolver) newID(prefix string) string {
	if r.o.NewID != nil {
		return r.o.NewID(prefix)
	}
	return session.NewID(prefix)
}

// idPrefix is a kind's id prefix (spec 004); a Connection has no id.
func idPrefix(kind string) string {
	switch kind {
	case v1.KindAgent:
		return session.PrefixAgent
	case v1.KindTrigger:
		return session.PrefixTrigger
	case v1.KindMemoryStore:
		return session.PrefixMemory
	}
	return ""
}

// stored is the status of the stored object of a kind and name, or nil.
func (r *resolver) stored(ctx context.Context, kind, name string) (*v1.Status, error) {
	if r.o.Lookup == nil {
		return nil, nil
	}
	var st *v1.Status
	var err error
	switch kind {
	case v1.KindAgent:
		var a *v1.Agent
		if a, err = r.o.Lookup.Agent(ctx, name); a != nil {
			st = &a.Status
		}
	case v1.KindTrigger:
		var t *v1.Trigger
		if t, err = r.o.Lookup.Trigger(ctx, name); t != nil {
			st = &t.Status
		}
	case v1.KindMemoryStore:
		var m *v1.MemoryStore
		if m, err = r.o.Lookup.MemoryStore(ctx, name); m != nil {
			st = &m.Status
		}
	default:
		var c *v1.Connection
		if c, err = r.o.Lookup.Connection(ctx, name); c != nil {
			st = &c.Status
		}
	}
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("manifest: look up %s %s: %w", kind, name, err)
	}
	return st, nil
}

func (r *resolver) unknownAt(path string) {
	r.unknown = append(r.unknown, Problem{Doc: r.doc, Path: path, Detail: "names no object the caller may see"})
}

// fromBatch is a document of the file already resolved, when a
// reference is a name.
func (r *resolver) fromBatch(kind, ref string) *object {
	if strings.Contains(ref, "_") {
		return nil
	}
	return r.batch[kind+"/"+ref]
}

func (r *resolver) agentSpec(ctx context.Context, at string, s *v1.AgentSpec) error {
	var err error
	if c := s.Model.Credential; c != "" {
		if s.Model.Credential, err = r.credential(ctx, at+".model.credential", c); err != nil {
			return err
		}
	}
	if a := s.Advisor; a != nil && a.Model.Credential != "" {
		if a.Model.Credential, err = r.credential(ctx, at+".advisor.model.credential", a.Model.Credential); err != nil {
			return err
		}
	}
	for i := range s.Subagents {
		sub := &s.Subagents[i]
		sp := indexed(at+".subagents", i)
		if sub.Spec != nil {
			if err := r.agentSpec(ctx, sp+".spec", sub.Spec); err != nil {
				return err
			}
			continue
		}
		if sub.Agent, err = r.agent(ctx, sp+".agent", sub.Agent, true); err != nil {
			return err
		}
	}
	for i := range s.MemoryStores {
		m := &s.MemoryStores[i]
		if m.Name, err = r.memoryStore(ctx, indexed(at+".memoryStores", i)+".name", m.Name); err != nil {
			return err
		}
	}
	for i, c := range s.Connections {
		if err := r.connection(ctx, indexed(at+".connections", i), c); err != nil {
			return err
		}
	}
	for i, m := range s.MCPServers {
		if m.Connection != "" {
			if err := r.connection(ctx, indexed(at+".mcpServers", i)+".connection", m.Connection); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *resolver) trigger(ctx context.Context, s *v1.TriggerSpec) error {
	var err error
	if s.Agent, err = r.agent(ctx, "spec.agent", s.Agent, false); err != nil {
		return err
	}
	for i := range s.Session.Resources {
		res := &s.Session.Resources[i]
		if res.Type != v1.ResourceMemoryStore {
			continue
		}
		if res.MemoryStore, err = r.memoryStore(ctx, indexed("spec.session.resources", i)+".memoryStore", res.MemoryStore); err != nil {
			return err
		}
	}
	return nil
}

// agent resolves an agent reference. withVersion pins the version too,
// as a subagent is pinned; without it, a reference that named no
// version keeps none, so a trigger runs the agent's latest.
func (r *resolver) agent(ctx context.Context, at, ref string, withVersion bool) (string, error) {
	if ob := r.fromBatch(v1.KindAgent, ref); ob != nil {
		return r.agentPin(ob.agent.Status, withVersion), nil
	}
	if r.o.Lookup == nil {
		r.unknownAt(at)
		return ref, nil
	}
	a, err := r.o.Lookup.Agent(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		r.unknownAt(at)
		return ref, nil
	}
	if err != nil {
		return "", fmt.Errorf("manifest: look up the agent at %s: %w", at, err)
	}
	r.pinned[pin(a.Status)] = a
	if strings.Contains(ref, "@") {
		withVersion = true
	}
	return r.agentPin(a.Status, withVersion), nil
}

func (r *resolver) agentPin(st v1.Status, withVersion bool) string {
	if withVersion {
		return pin(st)
	}
	return st.ID
}

func (r *resolver) memoryStore(ctx context.Context, at, ref string) (string, error) {
	if ob := r.fromBatch(v1.KindMemoryStore, ref); ob != nil {
		return ob.store.Status.ID, nil
	}
	if r.o.Lookup == nil {
		r.unknownAt(at)
		return ref, nil
	}
	m, err := r.o.Lookup.MemoryStore(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		r.unknownAt(at)
		return ref, nil
	}
	if err != nil {
		return "", fmt.Errorf("manifest: look up the memory store at %s: %w", at, err)
	}
	return m.Status.ID, nil
}

// connection checks that a Connection exists. Connections have no id,
// so the reference stays a name.
func (r *resolver) connection(ctx context.Context, at, name string) error {
	if r.fromBatch(v1.KindConnection, name) != nil {
		return nil
	}
	if r.o.Lookup == nil {
		r.unknownAt(at)
		return nil
	}
	_, err := r.o.Lookup.Connection(ctx, name)
	if errors.Is(err, ErrNotFound) {
		r.unknownAt(at)
		return nil
	}
	if err != nil {
		return fmt.Errorf("manifest: look up the connection at %s: %w", at, err)
	}
	return nil
}

func (r *resolver) credential(ctx context.Context, at, ref string) (string, error) {
	if r.o.Lookup == nil {
		r.unknownAt(at)
		return ref, nil
	}
	id, err := r.o.Lookup.Credential(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		r.unknownAt(at)
		return ref, nil
	}
	if err != nil {
		return "", fmt.Errorf("manifest: look up the credential at %s: %w", at, err)
	}
	return id, nil
}

// pinnedOf collects the agents an agent's subagents pin, and theirs in
// turn, down to the agent's maxDepth: the levels its threads can spawn.
// A stored agent that references back up the chain is legal, so the
// walk is bounded by depth, not by what it has seen.
func (r *resolver) pinnedOf(ctx context.Context, root v1.AgentSpec) (map[string]*v1.Agent, error) {
	out := map[string]*v1.Agent{}
	limit := *root.Threads.MaxDepth
	var walk func(s v1.AgentSpec, level int) error
	walk = func(s v1.AgentSpec, level int) error {
		if level > limit {
			return nil
		}
		for _, sub := range s.Subagents {
			spec := sub.Spec
			if spec == nil {
				a, err := r.pinnedAgent(ctx, sub.Agent)
				if err != nil {
					return err
				}
				out[sub.Agent] = a
				spec = &a.Spec
			}
			if err := walk(*spec, level+1); err != nil {
				return err
			}
		}
		return nil
	}
	return out, walk(root, 1)
}

// pinnedAgent returns the agent a pinned reference names: one this
// Resolve saw, or the stored version.
func (r *resolver) pinnedAgent(ctx context.Context, ref string) (*v1.Agent, error) {
	if a, ok := r.pinned[ref]; ok {
		return a, nil
	}
	if r.o.Lookup == nil {
		return nil, fmt.Errorf("manifest: %s is pinned but no Lookup holds it", ref)
	}
	a, err := r.o.Lookup.Agent(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("manifest: look up the pinned agent %s: %w", ref, err)
	}
	r.pinned[ref] = a
	return a, nil
}
