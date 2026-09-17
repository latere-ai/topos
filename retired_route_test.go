// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package topos

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// retiredCellaRoute is the token route Cella removed under decision D2 of
// 2026-09-13 (leaf id-03, shipped in sandbox v0.11.0). It is assembled from
// two halves so this file, which names the rule, is not itself a violation of
// it.
var retiredCellaRoute = "/v1/tokens" + "/exchange"

// TestNoSourceNamesTheRetiredCellaRoute holds the correction of 2026-09-17.
//
// Cella issues no credential in exchange for an upstream token any more: a
// caller presents the actor token its own issuer minted for the audience
// sandboxd. This repository never called the route, but its SDK comment and
// two documents described it as the way a bearer is obtained, which sent a
// reader to a door that is gone. topos declares identity role none, so no
// family gate reads this tree for retired mechanisms; this test is what keeps
// the sentence from coming back.
//
// An archive records what was once true and is not read, the same rule the
// family's gate applies to its own document scans. Neither is .claude, which
// holds worktrees: a full copy of the tree at whatever commit somebody is
// working from, including copies that predate this correction.
func TestNoSourceNamesTheRetiredCellaRoute(t *testing.T) {
	read := map[string]bool{".go": true, ".md": true, ".yaml": true, ".yml": true}
	var found []string
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != "." && (name == ".git" || name == ".claude" || name == ".archive" || name == "node_modules" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !read[filepath.Ext(name)] {
			return nil
		}
		body, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(body), "\n") {
			if strings.Contains(line, retiredCellaRoute) {
				found = append(found, filepath.ToSlash(p)+":"+strconv.Itoa(i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("these lines name %s, a Cella route removed under D2 on 2026-09-13:\n  %s\n\n"+
			"a caller presents the short-lived actor token its own issuer mints for the "+
			"audience sandboxd; Cella mints nothing in return. See the amendment of "+
			"2026-09-17 in specs/010-sandbox-cella.md.",
			retiredCellaRoute, strings.Join(found, "\n  "))
	}
}
