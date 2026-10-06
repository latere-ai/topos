// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"latere.ai/x/cella/client"
	v1 "latere.ai/x/cella/manifest/v1"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// MaxEgressRecords is how many of the gateway's newest connection records
// Refused reads, Cella's ceiling on one page: a command that made more
// connections than this has its earliest refusals unread.
const MaxEgressRecords = 200

// decisionDenied is the decision of a connection record whose host the
// gateway refused before any dial.
const decisionDenied = "denied"

// egress is the sandbox's egress for a session's network (spec 052): open
// and none name no host, as Cella refuses allowedHosts outside allowlist,
// and an allowlist joins the hosts of the session's named secrets to its
// own.
func (m *Machine) egress(ctx context.Context, mode string, hosts []string) (v1.Egress, error) {
	switch mode {
	case "", session.NetworkAllowlist:
	case session.NetworkOpen:
		return v1.Egress{Mode: v1.EgressOpen}, nil
	case session.NetworkNone:
		return v1.Egress{Mode: v1.EgressNone}, nil
	default:
		return v1.Egress{}, fmt.Errorf("%w: the network's mode %q is not open, allowlist or none", ErrUnavailable, mode)
	}
	secret, err := m.secretHosts(ctx)
	if err != nil {
		return v1.Egress{}, err
	}
	return v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: session.JoinHosts(hosts, secret)}, nil
}

// secretHosts are the hosts the session's named secrets are for, read
// from Cella the first time they are asked.
func (m *Machine) secretHosts(ctx context.Context) ([]string, error) {
	m.mu.Lock()
	read, hosts := m.secretsRead, m.secretHostList
	m.mu.Unlock()
	if read {
		return hosts, nil
	}
	var out []string
	for _, s := range m.o.Secrets {
		sec, _, err := m.c.GetSecret(ctx, s.Name)
		if err != nil {
			return nil, refused("read the secret "+s.Name, err)
		}
		out = append(out, sec.Spec.Scope.Hosts...)
	}
	m.mu.Lock()
	m.secretHostList, m.secretsRead = out, true
	m.mu.Unlock()
	return out, nil
}

// sameEgress reports whether two egresses admit the same hosts.
func sameEgress(a, b v1.Egress) bool {
	return a.Mode == b.Mode && slices.Equal(session.JoinHosts(a.AllowedHosts), session.JoinHosts(b.AllowedHosts)) &&
		slices.Equal(session.JoinHosts(a.DeniedHosts), session.JoinHosts(b.DeniedHosts))
}

// reconcile gives a sandbox found by name the session's network when it
// runs another, and answers the sandbox as it is then.
func (m *Machine) reconcile(ctx context.Context, sb v1.Sandbox) (v1.Sandbox, error) {
	m.mu.Lock()
	mode, hosts := m.o.EgressMode, slices.Clone(m.o.Egress)
	m.mu.Unlock()
	want, err := m.egress(ctx, mode, hosts)
	if err != nil {
		return v1.Sandbox{}, err
	}
	if sameEgress(sb.Spec.Network.Egress, want) {
		m.mu.Lock()
		m.asked = &want
		m.mu.Unlock()
		return sb, nil
	}
	return m.update(ctx, sb, want)
}

// update applies the sandbox's whole spec with its egress replaced, as
// the sandbox's owner, whom Cella spec 003 lets widen a narrow field and
// any caller narrow it: an apply that omitted a field would set it to its
// default, which Cella refuses as a change of an immutable field.
func (m *Machine) update(ctx context.Context, sb v1.Sandbox, want v1.Egress) (v1.Sandbox, error) {
	body := v1.Sandbox{
		APIVersion: v1.APIVersion, Kind: v1.KindSandbox,
		Metadata: v1.Metadata{Name: sb.Metadata.Name, Labels: sb.Metadata.Labels, Annotations: sb.Metadata.Annotations},
		Spec:     sb.Spec,
	}
	body.Spec.Network.Egress = want
	b, err := json.Marshal(body)
	if err != nil {
		return v1.Sandbox{}, fmt.Errorf("machine: encode the sandbox %s: %w", m.name, err)
	}
	applied, _, err := m.c.ApplySandbox(ctx, m.name, client.JSON(b))
	if err != nil {
		return v1.Sandbox{}, refused("change the network of the sandbox "+m.name, err)
	}
	m.mu.Lock()
	m.asked = &want
	m.info.Egress = string(applied.Spec.Network.Egress.Mode)
	m.mu.Unlock()
	return applied, nil
}

// ApplyNetwork gives the running sandbox the session's network: a host
// the person allowed, or the network an authorizer's send named (spec
// 052). A network this machine already asked for sends nothing. The
// network is kept for a sandbox created again after one was lost. A
// refusal leaves the sandbox's egress as it was.
func (m *Machine) ApplyNetwork(ctx context.Context, n machine.Network) error {
	if _, err := m.usable(); err != nil {
		return err
	}
	want, err := m.egress(ctx, n.Mode, n.Hosts)
	if err != nil {
		return err
	}
	m.mu.Lock()
	asked := m.asked
	m.mu.Unlock()
	if asked != nil && sameEgress(*asked, want) {
		return nil
	}
	err = m.call(ctx, false, func(id string) error {
		sb, _, err := m.c.GetSandbox(ctx, id)
		if err != nil {
			return refused("read the sandbox "+m.name, err)
		}
		_, err = m.update(ctx, sb, want)
		return err
	})
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.o.EgressMode, m.o.Egress = n.Mode, slices.Clone(n.Hosts)
	m.mu.Unlock()
	return nil
}

// Refused are the hosts the gateway refused for the sandbox since a time,
// each once with the first connection to it, oldest first. The gateway's
// clock decides the records' times, the runner's since: a skew between
// the two moves the window by as much.
func (m *Machine) Refused(ctx context.Context, since time.Time) ([]machine.Connection, error) {
	id, err := m.usable()
	if err != nil {
		return nil, err
	}
	recs, _, err := m.c.EgressRecords(ctx, id, MaxEgressRecords)
	if err != nil {
		return nil, refused("read the egress records of the sandbox "+m.name, err)
	}
	var out []machine.Connection
	seen := map[string]bool{}
	for _, r := range slices.Backward(recs) {
		host := session.JoinHosts([]string{r.Host})
		if r.Decision != decisionDenied || r.At.Before(since) || len(host) == 0 || seen[host[0]] {
			continue
		}
		seen[host[0]] = true
		out = append(out, machine.Connection{Host: host[0], Port: r.Port, At: r.At})
	}
	return out, nil
}

var _ machine.Networked = (*Machine)(nil)
