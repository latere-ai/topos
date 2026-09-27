// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package models

import (
	"net/url"
	"path"
	"slices"
	"strings"

	"latere.ai/x/pkg/llmdialect/ir"
)

// Doors are the per-family doors of a Lux installation, the base URLs
// its /.well-known/lux document names, keyed by Lux's dialect name. Nil
// is a base URL that is not a Lux root: one provider's API base, or a
// URL that already names a door.
type Doors map[string]string

// doorNames are the path segments Lux serves its doors under.
var doorNames = []string{"anthropic", "openai", "gemini", "lux"}

// NamesADoor reports whether base already ends in one of Lux's doors,
// such as https://lux.example/anthropic, which is used as it is.
func NamesADoor(base string) bool {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return false
	}
	return slices.Contains(doorNames, path.Base(u.Path))
}

// Door is the base URL a request of dialect d goes to: the door ds names
// for d's family, and base itself when ds names none. A request carries
// the family's own bytes, so it goes to the family's door and never to
// the /lux door, which translates.
func (ds Doors) Door(base string, d ir.Dialect) string {
	var name string
	switch d {
	case ir.DialectAnthropicMessages:
		name = "anthropic"
	case ir.DialectOpenAIResponses, ir.DialectOpenAIChat:
		name = "openai"
	}
	if door := ds[name]; door != "" {
		return strings.TrimRight(door, "/")
	}
	return base
}

// Root is the address a Lux installation published ds under: the URL
// every door shares, each door being it followed by the door's own name,
// as Lux builds them from its public URL. It is "" when ds is empty or
// its doors share no such root.
func (ds Doors) Root() string {
	root := ""
	for name, door := range ds {
		r, ok := strings.CutSuffix(strings.TrimRight(door, "/"), "/"+name)
		if !ok || r == "" || (root != "" && r != root) {
			return ""
		}
		root = r
	}
	return root
}

// Under is ds with each door moved from the root it was published under
// to base, the root it was discovered at, so a server that reaches Lux at
// another address than Lux publishes, such as its in-cluster Service,
// keeps every request on that address. Doors that share no root are
// returned as they are.
func (ds Doors) Under(base string) Doors {
	root := ds.Root()
	if root == "" {
		return ds
	}
	base = strings.TrimRight(base, "/")
	out := make(Doors, len(ds))
	for name := range ds {
		out[name] = base + "/" + name
	}
	return out
}
