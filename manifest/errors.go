// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"errors"
	"fmt"
	"strings"
)

// The error codes of spec 003.
const (
	CodeInvalidManifest    = "invalid_manifest"
	CodeHoldsSecret        = "manifest_holds_secret"
	CodeUnknownReference   = "unknown_reference"
	CodeUnsupportedVersion = "unsupported_version"
)

// messages are the one user sentence of each code.
var messages = map[string]string{
	CodeInvalidManifest:    "The manifest is not valid.",
	CodeHoldsSecret:        "The manifest holds what looks like a secret; name a credential instead.",
	CodeUnknownReference:   "The manifest references an object that does not exist.",
	CodeUnsupportedVersion: "The manifest's apiVersion is not topos.latere.ai/v1.",
}

// Problem is one finding: the field it is at and what is wrong. Doc is
// the document's index in the file, from 0. Detail never quotes a string
// value of the manifest, so a secret pasted into any field does not
// reach an error.
type Problem struct {
	Doc    int
	Path   string
	Detail string
}

// Error is a refused manifest: one code, the code's user sentence, and
// every problem the stage that refused found.
type Error struct {
	Code     string
	Message  string
	Problems []Problem
	// docs is how many documents the file held; the rendered detail
	// names a problem's document only when there is more than one.
	docs int
}

// Detail renders the problems for a developer, one per line: the
// document when the file holds several, the field path, and the problem.
func (e *Error) Detail() string {
	lines := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		var b strings.Builder
		if e.docs > 1 {
			fmt.Fprintf(&b, "document %d: ", p.Doc+1)
		}
		if p.Path != "" {
			b.WriteString(p.Path + ": ")
		}
		b.WriteString(p.Detail)
		lines = append(lines, b.String())
	}
	return strings.Join(lines, "\n")
}

func (e *Error) Error() string {
	d := e.Detail()
	if d == "" {
		return e.Code + ": " + e.Message
	}
	return e.Code + ": " + e.Message + "\n" + d
}

// newError is a refusal with the code's sentence.
func newError(code string, docs int, problems []Problem) *Error {
	return &Error{Code: code, Message: messages[code], Problems: problems, docs: docs}
}

// Code returns the manifest code of err, or "" when err is not a
// refused manifest.
func Code(err error) string {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code
	}
	return ""
}
