// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package cella

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/url"
	"path"
	"strings"

	"latere.ai/x/topos/sandbox"
)

// Cella exposes only tar-based file transfer (files/export and files/import),
// not single-file get/put. The single-file Provider methods are implemented on
// top of it: export with a path filter, then read the one entry; import a
// one-entry tar. Paths are workspace-relative and slash-separated (the export
// src_dir and import dest both default to the sandbox workdir, /workspace, so
// the relative path goes in the tar/`paths` rather than being absolutised here).

// exportReq is the POST /v1/sandboxes/{id}/files/export body. SrcDir is left
// empty so the server defaults it to the workspace root; Paths are relative to
// it (the arguments to `tar -C <root> -cf - <paths...>`).
type exportReq struct {
	Paths []string `json:"paths"`
}

// export POSTs files/export and returns the tar response. The caller owns
// closing the body.
func (p *Provider) export(ctx context.Context, id string, paths []string) (io.ReadCloser, error) {
	raw, err := json.Marshal(exportReq{Paths: paths})
	if err != nil {
		return nil, fmt.Errorf("cella: marshal export request: %w", err)
	}
	resp, err := p.send(ctx, "POST", "/v1/sandboxes/"+url.PathEscape(id)+"/files/export",
		bytes.NewReader(raw), "application/json")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close() //nolint:errcheck
		return nil, mapError(resp)
	}
	return resp.Body, nil
}

// ReadFile reads a single file by exporting just that path and returning the
// one entry from the tar. A path the sandbox does not have yields
// [sandbox.ErrNotFound].
func (p *Provider) ReadFile(ctx context.Context, id, filePath string) ([]byte, error) {
	want := normalizeTarName(filePath)
	body, err := p.export(ctx, id, []string{filePath})
	if err != nil {
		return nil, err
	}
	defer body.Close() //nolint:errcheck

	tr := tar.NewReader(body)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("cella: read file %q: tar: %w", filePath, err)
		}
		if hdr.Typeflag != tar.TypeReg || normalizeTarName(hdr.Name) != want {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("cella: read file %q: %w", filePath, err)
		}
		return data, nil
	}
	return nil, sandbox.ErrNotFound
}

// ListFiles lists the immediate children of a directory by exporting that
// subtree and reducing the tar headers to one entry per first path segment.
// (The export tars recursively; this collapses descendants back to the
// directory child that contains them.)
func (p *Provider) ListFiles(ctx context.Context, id, dir string) ([]sandbox.FileInfo, error) {
	prefix := normalizeTarName(dir) // "" for the workspace root
	body, err := p.export(ctx, id, []string{dir})
	if err != nil {
		return nil, err
	}
	defer body.Close() //nolint:errcheck

	// Preserve first-seen order so output is deterministic. own tracks whether
	// we have seen a child's own tar header (vs only inferring it from a
	// descendant), so an explicit header can refine an inferred directory.
	order := []string{}
	seen := map[string]sandbox.FileInfo{}
	own := map[string]bool{}

	tr := tar.NewReader(body)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("cella: list files %q: tar: %w", dir, err)
		}
		rel, ok := relativeTo(normalizeTarName(hdr.Name), prefix)
		if !ok || rel == "" {
			continue // the directory entry itself, or outside the subtree
		}

		child := rel
		isDir := hdr.Typeflag == tar.TypeDir
		isOwn := true
		if before, _, found := strings.Cut(rel, "/"); found {
			// A descendant: the immediate child is the directory it lives under.
			child, isDir, isOwn = before, true, false
		}

		if _, exists := seen[child]; !exists {
			order = append(order, child)
		} else if !isOwn || own[child] {
			continue // nothing better to record than what we already have
		}

		fi := sandbox.FileInfo{Name: child, IsDir: isDir}
		if isOwn && !isDir {
			fi.Size = hdr.Size
		}
		if isOwn {
			fi.Mode = uint32(hdr.Mode) & 0o777
		}
		seen[child] = fi
		own[child] = isOwn
	}

	out := make([]sandbox.FileInfo, 0, len(order))
	for _, name := range order {
		out = append(out, seen[name])
	}
	return out, nil
}

// WriteFile writes a single file by importing a one-entry tar. Parent
// directories are created by the server-side extract.
func (p *Provider) WriteFile(ctx context.Context, id, filePath string, data []byte) error {
	name := normalizeTarName(filePath)
	if name == "" {
		return fmt.Errorf("cella: write file: empty path")
	}

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(data)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		return fmt.Errorf("cella: write file %q: tar header: %w", filePath, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("cella: write file %q: tar body: %w", filePath, err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("cella: write file %q: tar close: %w", filePath, err)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("tarball", "workspace.tar")
	if err != nil {
		return fmt.Errorf("cella: write file %q: form: %w", filePath, err)
	}
	if _, err := part.Write(tarBuf.Bytes()); err != nil {
		return fmt.Errorf("cella: write file %q: form write: %w", filePath, err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("cella: write file %q: form close: %w", filePath, err)
	}

	resp, err := p.send(ctx, "POST", "/v1/sandboxes/"+url.PathEscape(id)+"/files/import", &body, mw.FormDataContentType())
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mapError(resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// normalizeTarName canonicalises a tar entry name or a caller path to a clean,
// slash-separated, relative form with no leading "./" or trailing "/". The
// workspace root ("." or "") normalises to "".
func normalizeTarName(name string) string {
	name = strings.TrimPrefix(name, "./")
	name = strings.TrimSuffix(name, "/")
	if name == "" || name == "." {
		return ""
	}
	cleaned := path.Clean(name)
	if cleaned == "." {
		return ""
	}
	return cleaned
}

// relativeTo returns name relative to prefix (both already normalised), and
// whether name is within the prefix subtree. For the root prefix ("") it
// returns name unchanged.
func relativeTo(name, prefix string) (string, bool) {
	if prefix == "" {
		return name, true
	}
	if name == prefix {
		return "", true // the directory entry itself
	}
	if rest, ok := strings.CutPrefix(name, prefix+"/"); ok {
		return rest, true
	}
	return "", false
}
