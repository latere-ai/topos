// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"

	"github.com/goccy/go-yaml"

	"latere.ai/x/topos/harness/parsing"
	"latere.ai/x/topos/harness/tools"
)

// readResultMeta is the ReadResultMeta schema (specs 008 and 080): what
// the tool.result of a read call records beside its text, which a client
// reads to show that a PDF was read through the parsing service, how many
// pages it has and which the result shows.
var readResultMeta = yaml.MapSlice{
	{Key: "type", Value: "object"},
	{Key: "description", Value: fmt.Sprintf("The meta of a %s call's tool.result: the file read and its SHA-256 at the read. "+
		"On an installation with a parsing service, a PDF a person attached, or one the working directory held at the end of an earlier turn, is read as text through it, its pages as Markdown, and parse names the parse. "+
		"The parsing service's model reads are priced and carried as cost_usd_micro on the tool.result itself, beside meta, which the session's spent_cost_usd_micro counts; a read of a PDF already parsed carries none. "+
		"A PDF the service did not read is outcome %s, the text the binary answer and a sentence that says why.", tools.NameRead, tools.OutcomeError)},
	{Key: "properties", Value: yaml.MapSlice{
		{Key: "path", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "description", Value: "The file's path on the session's machine."}}},
		{Key: "sha256", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "description", Value: "The hex SHA-256 of the file's bytes at the read."}}},
		{Key: "parse", Value: yaml.MapSlice{
			{Key: "type", Value: "object"},
			{Key: "description", Value: "The parse of a PDF the parsing service read; absent for any other file."},
			{Key: "required", Value: []string{"id", "pages", "shown"}},
			{Key: "properties", Value: yaml.MapSlice{
				{Key: "id", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "description", Value: "The parsing service's id of the parse."}}},
				{Key: "pages", Value: yaml.MapSlice{{Key: "type", Value: "integer"}, {Key: "description", Value: fmt.Sprintf("How many pages the parse read: the PDF's pages, at most %d.", parsing.MaxPages)}}},
				{Key: "more", Value: yaml.MapSlice{{Key: "type", Value: "boolean"}, {Key: "description", Value: fmt.Sprintf("True for a PDF of %d pages or more, of which the first %d were read.", parsing.MaxPages, parsing.MaxPages)}}},
				{Key: "shown", Value: yaml.MapSlice{{Key: "type", Value: "string"}, {Key: "description", Value: "The pages the result shows, such as 1-5 or 1-3,7."}}},
				{Key: "unpriced", Value: yaml.MapSlice{{Key: "type", Value: "boolean"}, {Key: "description", Value: "True when the parse's model reads could not be priced, so the tool.result carries no cost although the reads were charged."}}},
			}},
		}},
	}},
}
