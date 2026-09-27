// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"math"
	"regexp"
	"slices"
	"strings"
)

// tokenFormats are the well-known credential formats of spec 018's
// input check. Each begins at the start of the text or after a
// character that cannot be part of the token, so a word that merely
// contains a prefix does not match.
var tokenFormats = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"a token of the form sk-ant-", regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])sk-ant-[A-Za-z0-9_-]{16,}`)},
	{"a token of the form sk-", regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])sk-[A-Za-z0-9_-]{16,}`)},
	{"a token of the form ghp_", regexp.MustCompile(`(?:^|[^A-Za-z0-9_])ghp_[A-Za-z0-9]{16,}`)},
	{"a token of the form github_pat_", regexp.MustCompile(`(?:^|[^A-Za-z0-9_])github_pat_[A-Za-z0-9_]{16,}`)},
	{"a token of the form gho_", regexp.MustCompile(`(?:^|[^A-Za-z0-9_])gho_[A-Za-z0-9]{16,}`)},
	{"a token of the form xoxb-", regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])xoxb-[A-Za-z0-9-]{10,}`)},
	{"a token of the form xoxp-", regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])xoxp-[A-Za-z0-9-]{10,}`)},
	{"a token of the form lux_", regexp.MustCompile(`(?:^|[^A-Za-z0-9_])lux_[A-Za-z0-9_-]{16,}`)},
	{"an AWS access key id", regexp.MustCompile(`(?:^|[^A-Za-z0-9])AKIA[0-9A-Z]{16}(?:[^A-Za-z0-9]|$)`)},
	{"a PEM private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"a JSON web token", regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])eyJ[A-Za-z0-9_-]{5,}\.eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}`)},
}

// The high-entropy rule of spec 018: a run of 24 or more characters of
// a base64 alphabet at 4.0 bits per character or more, or of hex at 3.0
// or more.
const (
	entropyMinLength = 24
	entropyBase64    = 4.0
	entropyHex       = 3.0
)

// base64Char reports whether c belongs to the standard or the URL-safe
// base64 alphabet.
func base64Char(c rune) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.ContainsRune("+/=_-", c)
}

// secretKinds runs the input check over one text and returns what it
// found, by kind and never by value, each kind once.
func secretKinds(text string) []string {
	var kinds []string
	for _, f := range tokenFormats {
		if f.re.MatchString(text) {
			kinds = append(kinds, f.kind)
		}
	}
	for tok := range strings.FieldsFuncSeq(text, func(c rune) bool { return !base64Char(c) }) {
		if kind := entropyKind(tok); kind != "" && !slices.Contains(kinds, kind) {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// entropyKind classifies one run of base64 characters. A run of hex
// digits takes the hex bound. A run that is not hex must mix letters
// and digits, as every generated key does, so a long identifier such as
// a CamelCase test name does not count as a secret.
func entropyKind(tok string) string {
	if len(tok) < entropyMinLength {
		return ""
	}
	if strings.Trim(tok, "0123456789abcdef") == "" || strings.Trim(tok, "0123456789ABCDEF") == "" {
		if shannon(tok) >= entropyHex {
			return "a high-entropy hex string"
		}
		return ""
	}
	if !strings.ContainsAny(tok, "0123456789") || strings.Trim(tok, "0123456789+/=_-") == "" {
		return ""
	}
	if shannon(tok) >= entropyBase64 {
		return "a high-entropy string"
	}
	return ""
}

// shannon is the entropy of s in bits per character.
func shannon(s string) float64 {
	counts := map[rune]int{}
	n := 0
	for _, c := range s {
		counts[c]++
		n++
	}
	var h float64
	for _, k := range counts {
		p := float64(k) / float64(n)
		h -= p * math.Log2(p)
	}
	return h
}
