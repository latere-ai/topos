// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellastub

import (
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/client"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/pkg/hostmatch"
	"latere.ai/x/pkg/httpjson"
)

// The operations of a sandbox's apply and of its egress records, which a
// Failure is injected on.
const (
	OpApply  = "apply"
	OpEgress = "egress"
)

// RefuseHost makes the egress gateway refuse a command's connection to
// host on port, as Cella's does for a host outside a sandbox's egress: a
// command whose text names the host, run in a sandbox whose egress does
// not admit it, adds a denied record to the sandbox's egress records,
// stamped when the command starts. A sandbox whose egress admits the host,
// as one widened to it does, records nothing.
func (s *Server) RefuseHost(host string, port int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refusing[strings.ToLower(host)] = port
}

// AddEgressRecord adds one connection record to a sandbox's, as the
// gateway reports it; false when there is no such sandbox.
func (s *Server) AddEgressRecord(ref string, rec client.EgressRecord) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb := s.find(ref)
	if sb == nil {
		return false
	}
	sb.records = append(sb.records, rec)
	return true
}

// noteRefusals records the connections the gateway refuses for a command
// the sandbox ref runs.
func (s *Server) noteRefusals(ref string, command []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb := s.find(ref)
	if sb == nil {
		return
	}
	text := strings.ToLower(strings.Join(command, " "))
	now := time.Now().UTC()
	for _, host := range slices.Sorted(maps.Keys(s.refusing)) {
		if !strings.Contains(text, host) || s.admits(sb, host) {
			continue
		}
		sb.records = append(sb.records, client.EgressRecord{
			Principal: sb.obj.Status.ID, At: now, Door: "proxy", Host: host, Port: s.refusing[host], Decision: "denied", Reason: "not_allowed",
		})
	}
}

// admits reports whether the sandbox's egress admits host: every host
// under open, none under none, and under allowlist the allowed hosts and
// the hosts of the secrets it mounts. The caller holds mu.
func (s *Server) admits(sb *sandbox, host string) bool {
	e := sb.obj.Spec.Network.Egress
	lower := func(h string) string { return strings.ToLower(strings.TrimSpace(h)) }
	switch e.Mode {
	case v1.EgressOpen:
		return !hostmatch.New(e.DeniedHosts, lower).Matches(host)
	case v1.EgressNone:
		return false
	}
	hosts := slices.Clone(e.AllowedHosts)
	for _, m := range sb.obj.Spec.Secrets {
		hosts = append(hosts, s.secrets[m.Name].Spec.Scope.Hosts...)
	}
	return hostmatch.New(hosts, lower).Matches(host)
}

// apply serves PUT /v1/sandboxes/{ref}: a create when the name is free,
// and an update of the sandbox's labels, annotations and egress when it
// is held, as Cella's apply is. The update keeps every field Cella holds
// immutable, and one that changes them is refused immutable_field, so a
// caller that sends less than the whole spec hears it as Cella answers it.
// The authorizer is asked sandbox.update about the stored sandbox.
func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	obj, ok := decodeSandbox(w, r)
	if !ok {
		return
	}
	name := r.PathValue("ref")
	if obj.Metadata.Name == "" {
		obj.Metadata.Name = name
	}
	if obj.Metadata.Name != name {
		refuse(w, http.StatusBadRequest, "invalid_field", "the path names "+name+" and the body names "+obj.Metadata.Name)
		return
	}
	s.mu.Lock()
	sb := s.find(name)
	s.mu.Unlock()
	if sb == nil {
		s.createSandbox(w, obj)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if obj.APIVersion != v1.APIVersion || obj.Kind != v1.KindSandbox {
		refuse(w, http.StatusBadRequest, "invalid_field", "the manifest is not a "+v1.APIVersion+" Sandbox")
		return
	}
	have, want := sb.obj.Spec, obj.Spec
	var changed []string
	for _, f := range []struct {
		path string
		same bool
	}{
		{"spec.environment", want.Environment == have.Environment},
		{"spec.image", want.Image == have.Image},
		{"spec.workdir", want.Workdir == have.Workdir},
		{"spec.workspace.path", want.Workspace.Path == have.Workspace.Path},
		{"spec.env", maps.Equal(want.Env, have.Env)},
		{"spec.secrets", slices.Equal(want.Secrets, have.Secrets)},
	} {
		if !f.same {
			changed = append(changed, f.path)
		}
	}
	if len(changed) > 0 {
		refuse(w, http.StatusBadRequest, "immutable_field", "the update changes "+strings.Join(changed, ", "))
		return
	}
	res := Resource{Kind: authorizer.KindSandbox, ID: sb.obj.Status.ID, Name: sb.obj.Metadata.Name, Owner: sb.obj.Status.Owner, Labels: sb.obj.Metadata.Labels}
	if reason := s.refused(authorizer.ActionSandboxUpdate, res); reason != "" {
		refuse(w, http.StatusForbidden, "forbidden", authorizer.ActionSandboxUpdate+": "+reason)
		return
	}
	sb.obj.Metadata.Labels, sb.obj.Metadata.Annotations = obj.Metadata.Labels, obj.Metadata.Annotations
	sb.obj.Spec.Network = want.Network
	sb.obj.Spec.Lifecycle = want.Lifecycle
	sb.obj.Spec.Resources = want.Resources
	httpjson.Write(w, http.StatusOK, sb.obj)
}

// egressRecords serves GET /v1/sandboxes/{ref}/egress: the sandbox's
// connection records, newest first, at most limit of them.
func (s *Server) egressRecords(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			refuse(w, http.StatusBadRequest, "invalid_field", "limit is not a positive count")
			return
		}
		limit = n
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sb := s.item(w, r)
	if sb == nil {
		return
	}
	items := slices.Clone(sb.records)
	slices.Reverse(items)
	if len(items) > limit {
		items = items[:limit]
	}
	if items == nil {
		items = []client.EgressRecord{}
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"items": items})
}
