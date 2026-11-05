// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"fmt"

	"latere.ai/x/pkg/sanitize"
)

// lineFault says why s is not one clean line of text a person reads, ""
// when it is: invalid UTF-8, or a control character, a line or paragraph
// separator or a bidirectional control, which the fault names.
func lineFault(s string) string {
	if sanitize.IsLine(s) {
		return ""
	}
	for _, r := range s {
		if !sanitize.IsLine(string(r)) {
			return fmt.Sprintf("holds the control character %U", r)
		}
	}
	return "is not UTF-8"
}
