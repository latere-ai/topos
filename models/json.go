// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package models

import (
	"encoding/json/jsontext"
	"encoding/json/v2"

	"latere.ai/x/pkg/llmdialect/ir"
)

// FromProvider reads JSON this system does not produce: model output,
// provider responses and tool-call arguments. Duplicate names, invalid
// UTF-8 and any letter case in a name are accepted.
var FromProvider = json.JoinOptions(
	jsontext.AllowDuplicateNames(true),
	jsontext.AllowInvalidUTF8(true),
	json.MatchCaseInsensitiveNames(true),
)

// ToProvider writes JSON a model provider reads: keys sorted so a request
// repeats byte for byte, nil lists and maps as null, and raw tool-call
// arguments and text that came from a model passed through as they are.
var ToProvider = json.JoinOptions(
	json.Deterministic(true),
	json.FormatNilSliceAsNull(true),
	json.FormatNilMapAsNull(true),
	jsontext.AllowDuplicateNames(true),
	jsontext.AllowInvalidUTF8(true),
	json.WithMarshalers(json.MarshalToFunc(func(enc *jsontext.Encoder, _ ir.Loss) error {
		// A loss report has no exported fields and is written as an empty object.
		if err := enc.WriteToken(jsontext.BeginObject); err != nil {
			return err
		}
		return enc.WriteToken(jsontext.EndObject)
	})),
)
