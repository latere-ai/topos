// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"latere.ai/x/pkg/relpath"
)

// ErrConfined is returned by a Confine-wrapped Provider when a path argument
// escapes the workspace root or is denied by the non-overridable secret
// deny-list. It is the sentinel a caller errors.Is to distinguish a policy
// refusal from a backend failure.
var ErrConfined = errors.New("sandbox: path outside workspace root or denied by secret policy")

// secretBasenameGlobs are filename patterns a confined Provider never reads,
// writes, or lists, so a session driving a real machine cannot exfiltrate
// credentials in place. The list is
// non-overridable: there is no option to disable it. It mirrors the lift/drop
// deny-list so the same secrets stay unreadable whether copied or driven in place.
var secretBasenameGlobs = []string{
	".env", ".env.*", "*.pem", "id_rsa", "id_rsa.*", "id_dsa", "id_ecdsa",
	"id_ed25519", ".netrc",
}

// secretDirSegments deny any path that traverses one of these directories.
var secretDirSegments = map[string]bool{".ssh": true}

// secretExactPaths deny specific workspace-relative paths.
var secretExactPaths = map[string]bool{".aws/credentials": true}

// isSecretPath reports whether relPath (slash-separated, workspace-relative) is
// excluded by the non-overridable secret deny-list.
func isSecretPath(relPath string) bool {
	relPath = path.Clean(filepath.ToSlash(relPath))
	if secretExactPaths[relPath] {
		return true
	}
	segs := strings.Split(relPath, "/")
	for _, s := range segs {
		if secretDirSegments[s] {
			return true
		}
	}
	base := segs[len(segs)-1]
	for _, g := range secretBasenameGlobs {
		if ok, _ := path.Match(g, base); ok {
			return true
		}
	}
	return false
}

// PathResolver is implemented by providers that can resolve filesystem aliases.
// The returned path must name the same target and be usable by that provider.
// Confine validates both names and passes the resolved name to the operation.
// Resolution must stay within the provider's filesystem; providers remain
// responsible for guarding the eventual access against concurrent path changes.
type PathResolver interface {
	ResolvePath(context.Context, string, string) (string, error)
}

// confined checks requested and, when available, resolved paths against the
// workspace root and secret deny-list.
type confined struct {
	inner Provider
	root  string
}

// Confine checks file paths and explicit exec working directories against root
// and the secret deny-list. Providers implementing PathResolver also have their
// resolved targets checked; other providers receive lexical checks only.
// root is the session's workspace root
// ("." when empty, i.e. confine relative paths against upward escape only).
// Create, Destroy, and HealthCheck carry no path and pass straight through.
func Confine(inner Provider, root string) Provider {
	if root == "" {
		root = "."
	}
	return &confined{inner: inner, root: filepath.Clean(root)}
}

// allow validates a single path argument. It returns ErrConfined when the path is
// absolute-outside-root, escapes root via "..", or is deny-listed.
func (c *confined) allow(p string) error {
	rel := filepath.Clean(p)
	if filepath.IsAbs(rel) {
		r, err := filepath.Rel(c.root, rel)
		if err != nil {
			return fmt.Errorf("%w: absolute path %q outside root %q", ErrConfined, p, c.root)
		}
		if _, err := relpath.Clean(filepath.ToSlash(r)); err != nil {
			return fmt.Errorf("%w: absolute path %q outside root %q", ErrConfined, p, c.root)
		}
		rel = r
	} else if _, err := relpath.Clean(filepath.ToSlash(rel)); err != nil {
		return fmt.Errorf("%w: path %q escapes root %q", ErrConfined, p, c.root)
	}
	if isSecretPath(rel) {
		return fmt.Errorf("%w: secret path %q", ErrConfined, p)
	}
	return nil
}

func (c *confined) checkedPath(ctx context.Context, id, p string) (string, error) {
	if err := c.allow(p); err != nil {
		return "", err
	}
	if resolver, ok := c.inner.(PathResolver); ok {
		resolved, err := resolver.ResolvePath(ctx, id, p)
		if err != nil {
			return "", err
		}
		if err := c.allow(resolved); err != nil {
			return "", err
		}
		return resolved, nil
	}
	return p, nil
}

func (c *confined) Create(ctx context.Context, opts CreateOptions) (Sandbox, error) {
	return c.inner.Create(ctx, opts)
}

func (c *confined) Destroy(ctx context.Context, id string) error {
	return c.inner.Destroy(ctx, id)
}

func (c *confined) Exec(ctx context.Context, id string, opts ExecOptions) (ExecResult, error) {
	if opts.Cwd != "" {
		cwd, err := c.checkedPath(ctx, id, opts.Cwd)
		if err != nil {
			return ExecResult{}, err
		}
		opts.Cwd = cwd
	}
	return c.inner.Exec(ctx, id, opts)
}

func (c *confined) StreamExec(ctx context.Context, id string, opts ExecOptions) (ExecStream, error) {
	if opts.Cwd != "" {
		cwd, err := c.checkedPath(ctx, id, opts.Cwd)
		if err != nil {
			return nil, err
		}
		opts.Cwd = cwd
	}
	return c.inner.StreamExec(ctx, id, opts)
}

func (c *confined) ReadFile(ctx context.Context, id, p string) ([]byte, error) {
	resolved, err := c.checkedPath(ctx, id, p)
	if err != nil {
		return nil, err
	}
	return c.inner.ReadFile(ctx, id, resolved)
}

func (c *confined) WriteFile(ctx context.Context, id, p string, data []byte) error {
	resolved, err := c.checkedPath(ctx, id, p)
	if err != nil {
		return err
	}
	return c.inner.WriteFile(ctx, id, resolved, data)
}

func (c *confined) ListFiles(ctx context.Context, id, p string) ([]FileInfo, error) {
	resolved, err := c.checkedPath(ctx, id, p)
	if err != nil {
		return nil, err
	}
	return c.inner.ListFiles(ctx, id, resolved)
}

func (c *confined) HealthCheck(ctx context.Context, id string) error {
	return c.inner.HealthCheck(ctx, id)
}
