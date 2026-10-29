// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package dir

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"latere.ai/x/topos/manifest"
	"latere.ai/x/topos/session"
)

// limitsMarker is the file under sessions/ that records that every
// session's agent names spec.subagentLimits, so Open renames spec.threads
// once.
const limitsMarker = ".subagent-limits"

// renameSubagentLimits gives each session whose agent a release before
// spec.subagentLimits kept (spec 013) its agent renamed: the spec and the
// bundle with threads under subagentLimits, and each digest the renamed
// spec's, as new blobs, and the header naming them. The blobs before are
// left, and a session that runs no manifest is passed by.
func (s *Store) renameSubagentLimits(ctx context.Context) error {
	renamed := map[session.Digest]manifest.Renamed{}
	return s.rewriteOnce(limitsMarker, func(id string) error { return s.renameAgentOf(ctx, id, renamed) })
}

// renameAgentOf renames one session's agent under its lock; renamed holds
// the bundles already renamed, by their digest.
func (s *Store) renameAgentOf(ctx context.Context, id string, renamed map[session.Digest]manifest.Renamed) error {
	hdr, err := s.header(id)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || hdr.Agent.Bundle == "" {
		return err
	}
	return s.lockedRewrite(id, "rename spec.threads in the agent of", func() error {
		// The header is read again under the lock, so no write lands
		// between the read and the replace.
		hdr, err := s.header(id)
		if err != nil {
			return err
		}
		r, ok := renamed[hdr.Agent.Bundle]
		if !ok {
			rc, err := s.Blob(ctx, id, hdr.Agent.Bundle)
			if err != nil {
				return err
			}
			b, err := io.ReadAll(rc)
			if err := errors.Join(err, rc.Close()); err != nil {
				return fmt.Errorf("dir: read the agent of %s: %w", id, err)
			}
			if r, err = manifest.RenameSubagentLimits(b); err != nil {
				return fmt.Errorf("dir: rename spec.threads in the agent of %s: %w", id, err)
			}
			renamed[hdr.Agent.Bundle] = r
		}
		if !r.Changed {
			return nil
		}
		spec, err := s.PutBlob(ctx, id, bytes.NewReader(r.Spec))
		if err != nil {
			return err
		}
		bundle, err := s.PutBlob(ctx, id, bytes.NewReader(r.Stream))
		if err != nil {
			return err
		}
		hdr.Agent.Digest, hdr.Agent.Bundle = spec, bundle
		return s.writeHeader(id, hdr)
	})
}

// header reads a session's header as its file holds it.
func (s *Store) header(id string) (session.Session, error) {
	b, err := os.ReadFile(s.headerPath(id))
	if err != nil {
		return session.Session{}, fmt.Errorf("dir: read session %s: %w", id, err)
	}
	var hdr session.Session
	if err := json.Unmarshal(b, &hdr); err != nil {
		return session.Session{}, fmt.Errorf("%w: session.json of %s: %w", session.ErrCorrupt, id, err)
	}
	return hdr, nil
}
