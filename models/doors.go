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
