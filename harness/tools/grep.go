// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"errors"
	"io/fs"
	"strings"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/prompts"
)

const grepSchema = `{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "minLength": 1, "description": "An RE2 regular expression."},
    "path": {"type": "string", "description": "The directory or file to search; default the working directory."},
    "glob": {"type": "string", "description": "Only files whose path matches this glob, such as *.go or src/**/*.ts."},
    "output_mode": {"type": "string", "enum": ["files_with_matches", "content", "count"], "description": "files_with_matches (default) lists the files, content shows the matching lines, count counts them per file."},
    "context": {"type": "integer", "minimum": 0, "description": "Lines of context around each match in content mode."},
    "case_insensitive": {"type": "boolean", "description": "Match regardless of case."},
    "multiline": {"type": "boolean", "description": "Let . match newlines and a match span lines."},
    "head_limit": {"type": "integer", "minimum": 1, "description": "At most this many output lines; default 250."}
  },
  "required": ["pattern"],
  "additionalProperties": false
}`

func grepTool() Tool {
	return newBuiltin(NameGrep, prompts.Text(prompts.ToolGrep), grepSchema, Properties{Parallel: true, Effect: EffectRead}, runGrep)
}

type grepInput struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path"`
	Glob            string `json:"glob"`
	OutputMode      string `json:"output_mode"`
	Context         int    `json:"context"`
	CaseInsensitive bool   `json:"case_insensitive"`
	Multiline       bool   `json:"multiline"`
	HeadLimit       int    `json:"head_limit"`
}

func runGrep(ctx context.Context, b *builtin, c Call) (Result, error) {
	var in grepInput
	if r := b.decode(c, &in); r != nil {
		return *r, nil
	}
	limit := in.HeadLimit
	if limit <= 0 {
		limit = machine.DefaultHeadLimit
	}
	q := machine.SearchRequest{
		Kind:            machine.SearchGrep,
		Pattern:         in.Pattern,
		Path:            searchPath(c.Machine, in.Path),
		Glob:            in.Glob,
		OutputMode:      in.OutputMode,
		Context:         max(in.Context, 0),
		CaseInsensitive: in.CaseInsensitive,
		Multiline:       in.Multiline,
		HeadLimit:       limit,
	}
	res, err := c.Machine.Search(ctx, q)
	if err != nil {
		return searchFailed(ctx, b, c, q.Path, err)
	}
	if len(res.Lines) == 0 {
		return b.result(ctx, c, OutcomeOK, prompts.Text(prompts.GrepNoMatches), nil)
	}
	text := strings.Join(res.Lines, "\n") + "\n"
	if res.Truncated {
		text += prompts.Render(prompts.GrepMore, prompts.Data{"Limit": limit}) + "\n"
	}
	return b.result(ctx, c, OutcomeOK, text, nil)
}

// searchPath resolves a search's start: the working directory when the
// input names none.
func searchPath(m machine.Machine, p string) string {
	if p == "" {
		return m.Info().Workdir
	}
	return resolve(m, p)
}

// searchFailed answers a failed search. A machine error on the path is
// said as the file tools say it; anything else is the search's own
// complaint, a bad pattern or mode.
func searchFailed(ctx context.Context, b *builtin, c Call, p string, err error) (Result, error) {
	if errors.Is(err, machine.ErrReleased) || errors.Is(err, machine.ErrOutside) || errors.Is(err, machine.ErrDenied) ||
		errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return b.fail(ctx, c, p, err)
	}
	msg := strings.TrimPrefix(err.Error(), "machine: ")
	return b.result(ctx, c, OutcomeError, prompts.Render(prompts.SearchFailed, prompts.Data{"Error": msg}), nil)
}
