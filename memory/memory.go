// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package memory names a session's memory folders as its file tools reach
// them (spec 093): the folders an allow names, each at /memory/<name>/,
// the bounds of a file and a listing, and the errors a memory machine
// answers, which the file tools say to the model. It dials nothing; the
// machine that reaches an Arca-compatible files API is memory/arca.
package memory

import (
	"path"
	"strings"
)

// Root is the directory the memory folders are served under.
const Root = "/memory"

// Kind is a memory machine's kind in its Info.
const Kind = "memory"

// The bounds of spec 093.
const (
	// MaxFileBytes is the most bytes of one file written or read through
	// a memory folder; an installation bounds lower with a refusal.
	MaxFileBytes = 1 << 20
	// MaxListed is the most files a listing, a glob or a grep reads in one
	// folder; past it, the result says the rest were not read.
	MaxListed = 2000
)

// Under reports whether p is /memory or a path under it: an absolute
// path, cleaned, so a path that climbs out of /memory is not. A relative
// path resolves against the working directory and is never memory.
func Under(p string) bool {
	if !path.IsAbs(p) {
		return false
	}
	c := path.Clean(p)
	return c == Root || strings.HasPrefix(c, Root+"/")
}

// Locate is a path under /memory as its folder's name and its path in
// the folder: both empty for /memory itself, the name alone for a
// folder. ok is false for a path not under /memory.
func Locate(p string) (name, rel string, ok bool) {
	if !Under(p) {
		return "", "", false
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(path.Clean(p), Root), "/")
	name, rel, _ = strings.Cut(rest, "/")
	return name, rel, true
}
