// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// fromHelper reads what the helper in the sandbox answers: file names and
// messages it passes on from the sandbox's system, so invalid UTF-8 and
// repeated names are accepted.
var fromHelper = json.JoinOptions(
	jsontext.AllowDuplicateNames(true),
	jsontext.AllowInvalidUTF8(true),
)

// toHelper writes a request the helper reads. A path in it is a file name
// of the sandbox and need not be valid UTF-8.
var toHelper = jsontext.AllowInvalidUTF8(true)

// toCella writes a sandbox Cella's API reads: keys sorted, nil lists and
// maps as null, as the manifest has always been sent.
var toCella = json.JoinOptions(
	json.Deterministic(true),
	json.FormatNilSliceAsNull(true),
	json.FormatNilMapAsNull(true),
	jsontext.AllowInvalidUTF8(true),
)
