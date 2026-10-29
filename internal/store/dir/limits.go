// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package dir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"latere.ai/x/topos/manifest"
)

// limitsMarker is the file under objects/ that records that every agent
// version names spec.subagentLimits, so Open renames spec.threads once.
const limitsMarker = ".subagent-limits"

// renameSubagentLimits renames spec.threads to spec.subagentLimits in
// every agent version a release before it wrote (spec 013), once for the
// directory: each version's document and bundle, and its digest, which
// becomes the renamed spec's. A version is replaced as PutVersion writes
// one, and the marker is written after the last, so a crash part way
// renames the rest at the next Open and a version already renamed is
// left as it is.
func (s *Store) renameSubagentLimits() error {
	marker := filepath.Join(s.root, limitsMarker)
	if _, err := os.Stat(marker); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("dir: stat %s: %w", limitsMarker, err)
	}
	ids := make([]string, 0, len(s.agents))
	for id := range s.agents {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		for n := 1; n <= s.agents[id].Latest; n++ {
			v, err := s.readVersion(id, n)
			if err != nil {
				return err
			}
			doc, err := manifest.RenameSubagentLimits(v.Doc)
			if err != nil {
				return fmt.Errorf("dir: rename spec.threads in agent %s version %d: %w", id, n, err)
			}
			bundle, err := manifest.RenameSubagentLimits(v.Bundle)
			if err != nil {
				return fmt.Errorf("dir: rename spec.threads in agent %s version %d: %w", id, n, err)
			}
			if !doc.Changed && !bundle.Changed {
				continue
			}
			f := versionFile{AgentID: id, Version: n, Digest: doc.Digest, Doc: string(doc.Stream), Bundle: string(bundle.Stream), CreatedBy: v.CreatedBy, CreatedAt: v.CreatedAt}
			if err := write(s.versionPath(id, n), f); err != nil {
				return err
			}
		}
	}
	if err := writeFile(marker, nil, 0o600); err != nil {
		return fmt.Errorf("dir: write %s: %w", limitsMarker, err)
	}
	return nil
}
