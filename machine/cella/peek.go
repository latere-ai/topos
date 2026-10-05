// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"context"
	"fmt"
	"io"
	"path"

	"latere.ai/x/cella/client"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// Peek reads the workspace of a session's sandbox while it runs, for a
// reader outside the session's drive, such as the API serving the
// session's files (spec 044). It finds the sandbox by name and never
// creates, starts or wakes it: a sandbox that does not exist, or is not
// Running, is machine.ErrNotRunning. Only the workspace is read, through
// Cella's file routes, so the helper, the spill directory and every
// other path of the sandbox stay out of reach. Of the options it reads
// URL, Token, HTTPClient and Session.
func Peek(ctx context.Context, o Options) (machine.FileReader, error) {
	if o.URL == "" {
		return nil, fmt.Errorf("%w: TOPOS_CELLA_URL is not set", machine.ErrNotRunning)
	}
	if err := session.CheckID(session.PrefixSession, o.Session); err != nil {
		return nil, fmt.Errorf("machine: the session id %q: %w", o.Session, err)
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = httpClient()
	}
	c, err := client.New(client.Config{URL: o.URL, Token: o.Token, HTTPClient: hc, UserAgent: "topos"})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	name := SandboxName(o.Session)
	sb, _, err := c.GetSandbox(ctx, name)
	switch {
	case client.CodeOf(err) == "not_found":
		return nil, fmt.Errorf("%w: the session has no sandbox %s", machine.ErrNotRunning, name)
	case err != nil:
		return nil, refused("find the sandbox "+name, err)
	case sb.Status.Phase != "Running":
		return nil, fmt.Errorf("%w: the sandbox %s is %s", machine.ErrNotRunning, name, sb.Status.Phase)
	}
	ws := sb.Spec.Workspace.Path
	if ws == "" {
		return nil, fmt.Errorf("%w: the sandbox %s names no workspace", ErrUnavailable, name)
	}
	return &peek{c: c, name: name, id: sb.Status.ID, workspace: ws, workdir: cmpOr(o.Workdir, sb.Spec.Workdir, ws)}, nil
}

// peek is a running sandbox's workspace, read through the file routes.
type peek struct {
	c                            *client.Client
	name, id, workspace, workdir string
}

// resolve makes p absolute against the working directory and refuses a
// path outside the workspace or one the deny-list names.
func (p *peek) resolve(name string) (string, error) {
	a := name
	if !path.IsAbs(a) {
		a = path.Join(p.workdir, a)
	}
	a = path.Clean(a)
	switch {
	case !under(a, p.workspace):
		return "", fmt.Errorf("%w: %s", machine.ErrOutside, a)
	case deny.Path(a):
		return "", fmt.Errorf("%w: %s", machine.ErrDenied, a)
	}
	return a, nil
}

// gone tells a sandbox that stopped or went away between the find and a
// read from a path that is missing: Cella answers a file route of a
// sandbox that is not running phase_conflict, and one of a deleted
// sandbox not_found.
func (p *peek) gone(ctx context.Context, err error) error {
	switch client.CodeOf(err) {
	case "phase_conflict":
		return fmt.Errorf("%w: the sandbox %s stopped: %w", machine.ErrNotRunning, p.name, err)
	case "not_found":
		if _, _, gerr := p.c.GetSandbox(ctx, p.id); client.CodeOf(gerr) == "not_found" {
			return fmt.Errorf("%w: the sandbox %s is gone", machine.ErrNotRunning, p.name)
		}
	}
	return nil
}

func (p *peek) Stat(ctx context.Context, name string) (machine.FileInfo, error) {
	a, err := p.resolve(name)
	if err != nil {
		return machine.FileInfo{}, err
	}
	e, _, err := p.c.FileStat(ctx, p.id, a)
	if err != nil {
		if g := p.gone(ctx, err); g != nil {
			return machine.FileInfo{}, g
		}
		return machine.FileInfo{}, routeErr("stat", a, err)
	}
	return routeInfo(e)
}

func (p *peek) ReadFile(ctx context.Context, name string) (io.ReadCloser, error) {
	a, err := p.resolve(name)
	if err != nil {
		return nil, err
	}
	rc, err := p.c.FileGet(ctx, p.id, a)
	if err != nil {
		if g := p.gone(ctx, err); g != nil {
			return nil, g
		}
		return nil, routeErr("open", a, err)
	}
	return rc, nil
}
