// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package wordcount counts the words of a text.
package wordcount

import "strings"

// Count returns how many times each word occurs in text. Words are
// separated by white space, compared without case, and stripped of the
// punctuation at either end.
func Count(text string) map[string]int {
	counts := map[string]int{}
	for _, w := range strings.Split(text, " ") {
		w = strings.ToLower(strings.Trim(w, ".,;:!?\"'"))
		if w == "" {
			continue
		}
		counts[w]++
	}
	return counts
}
