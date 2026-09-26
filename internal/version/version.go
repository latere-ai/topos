// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package version reports the build identity of the running binary. The
// values are set at link time by the release pipeline and default to
// development markers so a local build is never mistaken for a release.
package version

import "fmt"

// Set by -ldflags "-X latere.ai/x/topos/internal/version.Version=..." at
// build time.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String renders the identity of the named binary in the form printed by
// `-version` and reported by the /version endpoint.
func String(binary string) string {
	return fmt.Sprintf("%s %s (%s, %s)", binary, Version, Commit, Date)
}
