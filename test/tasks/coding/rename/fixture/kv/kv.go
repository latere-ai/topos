// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package kv reads settings from a map of keys to values.
package kv

import "strings"

// Fetch returns the value of key in m, with surrounding white space
// removed, and whether m holds key.
func Fetch(m map[string]string, key string) (string, bool) {
	v, ok := m[key]
	return strings.TrimSpace(v), ok
}

// FetchOr returns the value of key in m, or def when m does not hold key.
func FetchOr(m map[string]string, key, def string) string {
	if v, ok := Fetch(m, key); ok {
		return v
	}
	return def
}
