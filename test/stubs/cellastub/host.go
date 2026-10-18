// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellastub

import (
	"errors"
	"net/http"
	"slices"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/pkg/httpjson"
)

// closeGoingAway is the close a stream ends with when its worker went
// away, with no exit from the command.
const closeGoingAway = 1001

// hostEnv is an environment a person's computer serves through a Cella
// worker (Cella specs 081 and 082): what the host reports of itself, its
// phase, whether its worker answers, and whether it asks its owner
// before a sandbox runs.
type hostEnv struct {
	host      v1.HostInfo
	isolation string
	phase     string
	// gone is a worker that does not answer: every operation on the
	// environment's sandboxes answers driver_unavailable.
	gone bool
	// hold makes each create wait for the host's answer.
	hold bool
}

// admits is the code, status and detail Cella refuses a create on the
// environment with, "" when it takes it: a computer that is not Ready
// answers driver_unavailable, and a workspace.host outside the host's
// grants workspace_not_granted, as Cella's resolve answers them.
func (he *hostEnv) admits(obj v1.Sandbox) (string, int, string) {
	switch {
	case he.phase != v1.EnvironmentReady || he.gone:
		return "driver_unavailable", http.StatusServiceUnavailable, "this environment has no data plane that can take a sandbox"
	case obj.Spec.Workspace.Host == "":
		return "", 0, ""
	}
	if _, ok := he.host.Grant(obj.Spec.Workspace.Host); !ok {
		return "workspace_not_granted", http.StatusUnprocessableEntity, "the host grants no folder holding " + obj.Spec.Workspace.Host
	}
	return "", 0, ""
}

// AddHostEnvironment makes an environment a person's computer serves:
// a read of it answers Ready with host in status.hosts and isolation; a
// sandbox on it takes no image of the stub's, binds its workspace.host in
// place as its workspace when the host grants it, names its scratch
// directories in status.paths, and its file routes serve those too, as
// Cella's local driver's do.
func (s *Server) AddHostEnvironment(name string, host v1.HostInfo, isolation string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.envs[name] = true
	s.hosts[name] = &hostEnv{host: host, isolation: isolation, phase: v1.EnvironmentReady}
}

// SetEnvironmentPhase sets a host environment's phase; a create on one
// that is not Ready answers driver_unavailable.
func (s *Server) SetEnvironmentPhase(name, phase string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if he := s.hosts[name]; he != nil {
		he.phase = phase
	}
}

// Disconnect makes a host environment's worker stop answering, as a
// computer that sleeps does: every operation on its sandboxes answers
// driver_unavailable, and each exec session open on one ends with no
// exit, its command killed. Reconnect undoes it.
func (s *Server) Disconnect(name string) {
	s.mu.Lock()
	var open []*session
	if he := s.hosts[name]; he != nil {
		he.gone = true
		for id, sb := range s.sandboxes {
			if sb.obj.Spec.Environment == name {
				open = append(open, s.sessions[id]...)
				delete(s.sessions, id)
			}
		}
	}
	s.mu.Unlock()
	for _, se := range open {
		s.record(se.drop())
	}
}

// Reconnect makes a host environment's worker answer again.
func (s *Server) Reconnect(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if he := s.hosts[name]; he != nil {
		he.gone = false
	}
}

// HoldCreates sets whether a create on a host environment waits for the
// host's answer: the sandbox reads Pending with its HostApproval
// condition Unknown until Admit, RefuseCreate or TimeoutHost answers it.
func (s *Server) HoldCreates(name string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if he := s.hosts[name]; he != nil {
		he.hold = on
	}
}

// Admit answers a held sandbox as the host's owner allowing it: it runs,
// and HostApproval is True.
func (s *Server) Admit(ref string) bool {
	return s.answer(ref, "Running", "", v1.ConditionTrue, v1.ReasonHostApproved, "")
}

// RefuseCreate answers a held sandbox as the host's owner refusing it with
// text: it fails with reason HostRefused, and HostApproval is False with
// the host's own words.
func (s *Server) RefuseCreate(ref, text string) bool {
	return s.answer(ref, "Failed", v1.ReasonHostRefused, v1.ConditionFalse, v1.ReasonHostRefused, text)
}

// TimeoutHost answers a held sandbox as a host that did not answer in
// time: it fails with reason HostTimeout.
func (s *Server) TimeoutHost(ref string) bool {
	return s.answer(ref, "Failed", v1.ReasonHostTimeout, v1.ConditionFalse, v1.ReasonHostTimeout, "The host did not answer in time.")
}

func (s *Server) answer(ref, phase, reason, status, condReason, message string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb := s.find(ref)
	if sb == nil {
		return false
	}
	now := time.Now().UTC()
	sb.obj.Status.Phase, sb.obj.Status.Reason = phase, reason
	if phase == "Running" {
		sb.obj.Status.StartedAt = now
	}
	sb.obj.Status.Conditions = slices.DeleteFunc(sb.obj.Status.Conditions, func(c v1.Condition) bool { return c.Type == v1.ConditionHostApproval })
	sb.obj.Status.Conditions = append(sb.obj.Status.Conditions, v1.Condition{Type: v1.ConditionHostApproval, Status: status, Reason: condReason, Message: message, Since: now})
	return true
}

// Created is the manifest a sandbox's create carried, as the stub
// received it.
func (s *Server) Created(ref string) (v1.Sandbox, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb := s.find(ref)
	if sb == nil {
		return v1.Sandbox{}, false
	}
	return sb.created, true
}

// environment answers a read of an environment: a host environment with
// its phase, isolation and host, any other known one Ready with nothing
// more, and an unknown one not_found.
func (s *Server) environment(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := r.PathValue("ref")
	if !s.envs[name] {
		refuse(w, http.StatusNotFound, "not_found", "no environment named "+name)
		return
	}
	env := v1.Environment{APIVersion: v1.APIVersion, Kind: v1.KindEnvironment, Metadata: v1.Metadata{Name: name},
		Status: v1.EnvironmentStatus{ID: name, Owner: Owner, Phase: v1.EnvironmentReady}}
	if he := s.hosts[name]; he != nil {
		env.Spec.Mode = v1.EnvironmentWorker
		env.Status.Phase, env.Status.Isolation, env.Status.Driver = he.phase, he.isolation, "local"
		if !he.gone {
			env.Status.Workers = 1
			env.Status.Hosts = []v1.HostStatus{{Worker: "wrk_stub", Host: he.host}}
		}
	}
	httpjson.Write(w, http.StatusOK, env)
}

// drop ends the session as a worker that went away does: the stream
// closes with no exit from the command, which is killed.
func (se *session) drop() error {
	if !se.ended.CompareAndSwap(false, true) {
		return nil
	}
	return errors.Join(se.c.closeWith(closeGoingAway, "the worker went away"), kill(se.cmd))
}
