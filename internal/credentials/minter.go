// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package credentials is toposd's minter of session credentials (spec
// 018): the hosted-agent token a session's runner or sandbox presents to
// one core, asked of the identity provider for the session's agent, and
// the session's two Lux keys, whose values toposd generates and whose
// hashes the installation's authorizer registers with Lux. It answers
// only for a lease: in process for the drives of serve's own runners,
// and through the runner protocol's token route for a remote runner,
// which checks the lease's generation first.
package credentials

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"latere.ai/x/topos/internal/identity"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// RenewBefore is how much life a held Lux key must have left to be
// answered as it is; one with less is renewed first.
const RenewBefore = 10 * time.Minute

// Tokens mints hosted-agent tokens; *identity.Client is the one toposd
// runs.
type Tokens interface {
	Mint(ctx context.Context, subject, audience string, s identity.Session) (identity.Token, error)
}

// Keys registers a session's Lux key for a workload by the SHA-256 of
// its value and answers when it expires; *KeyRoutes is the one toposd
// runs.
type Keys interface {
	Put(ctx context.Context, session, workload, hash string) (time.Time, error)
}

// Options configure a Minter.
type Options struct {
	// Tokens is the identity provider; nil mints no token.
	Tokens Tokens
	// Keys is the authorizer's session key routes; nil mints no Lux
	// key.
	Keys Keys
	// Sessions and Agents find a session's agent and its identity.
	Sessions session.Store
	Agents   store.Agents
	Now      func() time.Time
}

// Minter answers a session's credentials.
type Minter struct {
	o      Options
	leases atomic.Int64

	mu   sync.Mutex
	keys map[[2]string]*key
}

// key is a generated Lux key: its value, the hash the authorizer holds,
// the lease it was generated for, and when it expires.
type key struct {
	value, hash, lease string
	expires            time.Time
}

// New builds the minter. With neither Tokens nor Keys it mints nothing,
// and every answer is runner.ErrNotMinted.
func New(o Options) (*Minter, error) {
	if o.Tokens != nil && (o.Sessions == nil || o.Agents == nil) {
		return nil, errors.New("credentials: minting tokens needs the session and agent stores")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Minter{o: o, keys: map[[2]string]*key{}}, nil
}

// Credential answers the session id's credential for audience and
// workload, to the holder of the lease named lease: the session's Lux
// key for the audience runner.AudienceLux, and a hosted-agent token for
// any other. The caller has checked that the lease is held.
func (m *Minter) Credential(ctx context.Context, id, lease, audience, workload string) (runner.Credential, error) {
	switch {
	case workload != runner.WorkloadSession && workload != runner.WorkloadSandbox:
		return runner.Credential{}, fmt.Errorf("credentials: the workload %q is neither session nor sandbox", workload)
	case audience == "":
		return runner.Credential{}, errors.New("credentials: no audience")
	case audience == runner.AudienceLux:
		return m.key(ctx, id, lease, workload)
	case m.o.Tokens == nil:
		return runner.Credential{}, runner.ErrNotMinted
	}
	subject, err := m.subject(ctx, id)
	if err != nil {
		return runner.Credential{}, err
	}
	tok, err := m.o.Tokens.Mint(ctx, subject, audience, identity.Session{ID: id, Workload: workload})
	if err != nil {
		return runner.Credential{}, err
	}
	return runner.Credential{Value: tok.Value, ExpiresAt: tok.ExpiresAt}, nil
}

// subject is the identity of the session's agent: the latest version's
// status.identity, which every version since the identity's creation
// carries.
func (m *Minter) subject(ctx context.Context, id string) (string, error) {
	s, err := m.o.Sessions.Get(ctx, id)
	if err != nil {
		return "", err
	}
	a, err := m.o.Agents.Agent(ctx, s.Agent.ID)
	if err != nil {
		return "", err
	}
	v, err := m.o.Agents.Version(ctx, a.ID, a.Latest)
	if err != nil {
		return "", err
	}
	doc, err := store.DecodeAgent(v.Doc)
	if err != nil {
		return "", err
	}
	if doc.Status.Identity == "" {
		return "", runner.ErrNoIdentity
	}
	return doc.Status.Identity, nil
}

// key is the session's Lux key for the workload. A key held for this
// lease with RenewBefore left is answered as it is; one with less is
// renewed by registering its hash again; a lease with no key of its own
// gets a new value, whose hash replaces the key another lease held, so a
// runner that lost the session loses its key.
func (m *Minter) key(ctx context.Context, id, lease, workload string) (runner.Credential, error) {
	if m.o.Keys == nil {
		return runner.Credential{}, runner.ErrNotMinted
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.o.Now()
	for k, held := range m.keys {
		// An expired key is of no use to anyone; a later request
		// generates a new one.
		if !now.Before(held.expires) {
			delete(m.keys, k)
		}
	}
	slot := [2]string{id, workload}
	k := m.keys[slot]
	if k != nil && k.lease == lease && k.expires.Sub(now) >= RenewBefore {
		return runner.Credential{Value: k.value, ExpiresAt: k.expires}, nil
	}
	next := key{lease: lease}
	if k != nil && k.lease == lease {
		next.value, next.hash = k.value, k.hash
	} else {
		v, err := NewKeyValue()
		if err != nil {
			return runner.Credential{}, err
		}
		next.value, next.hash = v, HashKeyValue(v)
	}
	exp, err := m.o.Keys.Put(ctx, id, workload, next.hash)
	if err != nil {
		return runner.Credential{}, err
	}
	next.expires = exp
	m.keys[slot] = &next
	return runner.Credential{Value: next.value, ExpiresAt: exp}, nil
}

// Local is the credentials of a drive of serve's own runners that holds
// lease on the session id: each lease is a lease of its own to the
// minter, and a lost one is answered runner.ErrLeaseLost.
func (m *Minter) Local(id string, lease session.Lease) runner.Credentials {
	return &local{m: m, id: id, lease: lease, tag: "local:" + strconv.FormatInt(m.leases.Add(1), 10)}
}

type local struct {
	m     *Minter
	id    string
	lease session.Lease
	tag   string
}

func (l *local) Credential(ctx context.Context, audience, workload string) (runner.Credential, error) {
	select {
	case <-l.lease.Lost():
		return runner.Credential{}, runner.ErrLeaseLost
	default:
	}
	return l.m.Credential(ctx, l.id, l.tag, audience, workload)
}

// keyAlphabet is [A-Za-z0-9_-], 64 letters, so one random byte's low six
// bits pick one without bias; it is the alphabet Lux mints its own
// values in.
const keyAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"

// NewKeyValue is a fresh Lux key value: lux_ and 40 letters of
// keyAlphabet from crypto/rand, 240 bits.
func NewKeyValue() (string, error) {
	var b [40]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("credentials: a key value: %w", err)
	}
	out := make([]byte, 0, 4+len(b))
	out = append(out, "lux_"...)
	for _, c := range b {
		out = append(out, keyAlphabet[c&63])
	}
	return string(out), nil
}

// HashKeyValue is the SHA-256 of a key's value in lower-case hex, the
// only form of it the authorizer receives.
func HashKeyValue(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}
