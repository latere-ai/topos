// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"latere.ai/x/cella/client"
	"latere.ai/x/pkg/otel"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/cella"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// WorkspaceOptions configure the reads of hosted sessions' working
// directories that the API serves (spec 044).
type WorkspaceOptions struct {
	// CellaURL is TOPOS_CELLA_URL; empty reads no sandbox.
	CellaURL string
	// CellaToken is the installation's bearer, TOPOS_CELLA_TOKEN_FILE's,
	// presented when the server mints no Cella token for the session;
	// nil presents none.
	CellaToken client.TokenSource
	// Mint answers the session id's own credential for an audience and a
	// workload, as a drive's credentials are minted, so the read presents
	// the token Cella's authorizer already binds to the session's sandbox;
	// nil mints none.
	Mint func(ctx context.Context, id, audience, workload string) (runner.Credential, error)
	// DataDir is TOPOS_DATA_DIR when TOPOS_HOST_SESSIONS is on: each host
	// session works in a directory of its own under it. Empty reads no
	// host session.
	DataDir string
	// Clock is the clock a minted credential's expiry is read against;
	// time.Now when nil.
	Clock func() time.Time
}

// Workspaces reads the working directory of a hosted session as it is
// now: a Cella session's sandbox while it runs, through cella.Peek with
// the credential its drive would present, and a host session's
// directory on this server's disk. Neither starts nor creates a
// machine; one that does not run is machine.ErrNotRunning.
func Workspaces(o WorkspaceOptions) func(ctx context.Context, s session.Session) (machine.FileReader, error) {
	if o.Clock == nil {
		o.Clock = time.Now
	}
	return func(ctx context.Context, s session.Session) (machine.FileReader, error) {
		if s.Machine.Kind == session.MachineHost {
			return o.host(s.ID)
		}
		return o.cella(ctx, s.ID)
	}
}

// minted is one session's Cella token as a drive's token source reads
// it. It answers no other audience: a read holds no lease, and a Lux key
// minted without one would replace the key the session's drive holds.
type minted struct {
	id   string
	mint func(ctx context.Context, id, audience, workload string) (runner.Credential, error)
}

func (m minted) Credential(ctx context.Context, audience, workload string) (runner.Credential, error) {
	if audience != AudienceCella {
		return runner.Credential{}, fmt.Errorf("hosted: a read of a session's files mints no %s credential", audience)
	}
	return m.mint(ctx, m.id, audience, workload)
}

// cella peeks at the session's sandbox, presenting the session's own
// Cella token when the installation mints one and its bearer otherwise,
// the rule a drive follows.
func (o WorkspaceOptions) cella(ctx context.Context, id string) (machine.FileReader, error) {
	if o.CellaURL == "" {
		return nil, fmt.Errorf("%w: this server reaches no Cella", machine.ErrNotRunning)
	}
	if o.Mint != nil {
		// The source is never lost: it lives for this one read.
		ctx = runner.WithTokens(ctx, runner.NewTokenSource(minted{id: id, mint: o.Mint}, nil, o.Clock))
	}
	token, err := cellaToken(ctx, o.CellaToken)
	if err != nil {
		return nil, err
	}
	return cella.Peek(ctx, cella.Options{URL: o.CellaURL, Token: token, Session: id, HTTPClient: otel.HTTPClient()})
}

// host reads a host session's working directory. A session that never
// ran on this host has none.
func (o WorkspaceOptions) host(id string) (machine.FileReader, error) {
	if o.DataDir == "" {
		return nil, fmt.Errorf("%w: this server runs no host sessions", machine.ErrNotRunning)
	}
	work, _, _, err := hostDirs(o.DataDir, id)
	if err != nil {
		return nil, err
	}
	// The paths a session's tools name are under the working directory
	// with its symlinks evaluated, as the machine opened it.
	real, err := filepath.EvalSymlinks(work)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("%w: the session has no working directory on this host", machine.ErrNotRunning)
	case err != nil:
		return nil, fmt.Errorf("host sessions: the working directory of %s: %w", id, err)
	}
	return hostFiles{dir: real, deny: machine.DenyList{DataDir: o.DataDir}}, nil
}

// hostFiles is a host session's working directory, read through an
// os.Root so a symlink cannot lead a read out of it.
type hostFiles struct {
	dir  string
	deny machine.DenyList
}

// resolve is p relative to the working directory, and p absolute.
func (h hostFiles) resolve(p string) (rel, abs string, err error) {
	abs = filepath.FromSlash(p)
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(h.dir, abs)
	}
	abs = filepath.Clean(abs)
	rel, err = filepath.Rel(h.dir, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", abs, fmt.Errorf("%w: %s", machine.ErrOutside, abs)
	}
	if h.deny.Path(abs) {
		return "", abs, fmt.Errorf("%w: %s", machine.ErrDenied, abs)
	}
	return rel, abs, nil
}

// escaped is an os.Root refusal of a path that leaves the root, through
// a symlink or otherwise, as machine.ErrOutside.
func escaped(err error, abs string) error {
	var pe *os.PathError
	if errors.As(err, &pe) && strings.Contains(pe.Err.Error(), "escapes") {
		return fmt.Errorf("%w: %s", machine.ErrOutside, abs)
	}
	return err
}

func (h hostFiles) Stat(_ context.Context, p string) (machine.FileInfo, error) {
	rel, abs, err := h.resolve(p)
	if err != nil {
		return machine.FileInfo{}, err
	}
	root, err := os.OpenRoot(h.dir)
	if err != nil {
		return machine.FileInfo{}, err
	}
	fi, serr := root.Stat(rel)
	if err := errors.Join(escaped(serr, abs), root.Close()); err != nil {
		return machine.FileInfo{}, err
	}
	return machine.FileInfo{Path: abs, Size: fi.Size(), Mode: fi.Mode(), ModTime: fi.ModTime(), IsDir: fi.IsDir()}, nil
}

func (h hostFiles) ReadFile(_ context.Context, p string) (io.ReadCloser, error) {
	rel, abs, err := h.resolve(p)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(h.dir)
	if err != nil {
		return nil, err
	}
	// An open file outlives the root it was opened through.
	f, oerr := root.Open(rel)
	if err := errors.Join(escaped(oerr, abs), root.Close()); err != nil {
		if f != nil {
			err = errors.Join(err, f.Close())
		}
		return nil, err
	}
	return f, nil
}
