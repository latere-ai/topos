// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package topos_test

import (
	"os"
	"strings"
	"testing"
)

func TestOpenSourceRepositoryContract(t *testing.T) {
	t.Parallel()

	files := map[string][]string{
		"README.md": {
			"actions/workflows/ci.yml/badge.svg",
			"pkg.go.dev/badge/latere.ai/x/topos.svg",
			"img.shields.io/github/v/release/latere-ai/topos",
			"img.shields.io/github/license/latere-ai/topos",
			"go get latere.ai/x/topos@latest",
			"CONTRIBUTING.md",
			"SECURITY.md",
		},
		"CONTRIBUTING.md": {
			"make all",
			"regression test for every bug fix",
		},
		"SECURITY.md": {
			"security/advisories/new",
			"Do not open a public issue",
		},
		".github/ISSUE_TEMPLATE/bug_report.yml": {
			"Topos version",
			"Go version",
			"Minimal reproduction",
		},
		".github/ISSUE_TEMPLATE/feature_request.yml": {
			"Problem",
			"Proposed API or behavior",
			"Compatibility impact",
		},
		".github/ISSUE_TEMPLATE/config.yml": {
			"blank_issues_enabled: false",
			"security/advisories/new",
		},
	}

	for path, required := range files {
		t.Run(path, func(t *testing.T) {
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read repository entry point: %v", err)
			}
			for _, text := range required {
				if !strings.Contains(string(body), text) {
					t.Errorf("missing %q", text)
				}
			}
		})
	}
}
