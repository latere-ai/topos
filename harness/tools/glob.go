// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"latere.ai/x/topos/machine"
)

//go:embed descriptions/glob.md
var globDescription string

const globSchema = `{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "minLength": 1, "description": "A glob such as **/*.go; ** matches any number of directories."},
    "path": {"type": "string", "description": "The directory to search; default the working directory."}
  },
  "required": ["pattern"],
  "additionalProperties": false
}`

func globTool() Tool {
	return newBuiltin(NameGlob, globDescription, globSchema, Properties{Parallel: true, Effect: EffectRead}, runGlob)
}

type globInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

func runGlob(ctx context.Context, b *builtin, c Call) (Result, error) {
	var in globInput
	if r := b.decode(c, &in); r != nil {
		return *r, nil
	}
	q := machine.SearchRequest{Kind: machine.SearchGlob, Pattern: in.Pattern, Path: searchPath(c.Machine, in.Path)}
	res, err := c.Machine.Search(ctx, q)
	if err != nil {
		return searchFailed(ctx, b, c, q.Path, err)
	}
	if len(res.Lines) == 0 {
		return b.result(ctx, c, OutcomeOK, "No files match.", nil)
	}
	text := strings.Join(res.Lines, "\n") + "\n"
	if res.Truncated {
		text += fmt.Sprintf("[... more than %d paths match; narrow the pattern or the path]\n", machine.GlobLimit)
	}
	return b.result(ctx, c, OutcomeOK, text, nil)
}
