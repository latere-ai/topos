// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package cella

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"

	"latere.ai/x/topos/sandbox"
)

// workspaceRoot is where a sandbox's workspace is mounted: the control plane's
// default, which this provider never overrides. The control plane refuses a
// file path outside it.
const workspaceRoot = "/workspace"

// resolvePath maps a path from the interface onto the control plane's rule,
// which takes only absolute paths at or below the workspace. A relative path,
// the empty path and "." are resolved under the workspace, as the local
// provider resolves them under its directory; an absolute path is sent as
// given and the control plane refuses one outside the workspace.
func resolvePath(p string) string {
	if path.IsAbs(p) {
		return path.Clean(p)
	}
	return path.Join(workspaceRoot, p)
}

// ReadFile reads one file from the one-file content route. A path the sandbox
// does not have yields [sandbox.ErrNotFound].
func (p *Provider) ReadFile(ctx context.Context, id, filePath string) ([]byte, error) {
	body, err := p.core.FileGet(ctx, id, resolvePath(filePath))
	if err != nil {
		return nil, mapError(err)
	}
	defer body.Close() //nolint:errcheck
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("cella: read file %q: %w", filePath, err)
	}
	return data, nil
}

// WriteFile writes one file. The control plane creates the missing parent
// directories and stages the body, so a write that fails part way leaves the
// previous file whole.
func (p *Provider) WriteFile(ctx context.Context, id, filePath string, data []byte) error {
	if filePath == "" {
		return errors.New("cella: write file: empty path")
	}
	if err := p.core.FilePut(ctx, id, resolvePath(filePath), "", bytes.NewReader(data)); err != nil {
		return mapError(err)
	}
	return nil
}

// ListFiles lists the immediate entries of a directory, sorted by name. A
// directory the sandbox does not have yields [sandbox.ErrNotFound].
func (p *Provider) ListFiles(ctx context.Context, id, dir string) ([]sandbox.FileInfo, error) {
	entries, _, err := p.core.FileList(ctx, id, resolvePath(dir))
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]sandbox.FileInfo, 0, len(entries))
	for _, e := range entries {
		mode, err := strconv.ParseUint(e.Mode, 8, 32)
		if err != nil {
			return nil, fmt.Errorf("cella: list files %q: entry %q has mode %q, not octal", dir, e.Name, e.Mode)
		}
		fi := sandbox.FileInfo{Name: e.Name, Mode: uint32(mode) & 0o777, IsDir: e.IsDir}
		if !e.IsDir {
			fi.Size = e.Size
		}
		out = append(out, fi)
	}
	return out, nil
}
