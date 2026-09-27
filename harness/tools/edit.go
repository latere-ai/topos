// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"context"
	"strings"

	"latere.ai/x/topos/prompts"
)

const editSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "minLength": 1, "description": "The file to edit: absolute, or relative to the working directory."},
    "old_string": {"type": "string", "minLength": 1, "description": "The exact text to replace, whitespace included."},
    "new_string": {"type": "string", "description": "The text to put in its place."},
    "replace_all": {"type": "boolean", "description": "Replace every occurrence instead of exactly one; default false."}
  },
  "required": ["path", "old_string", "new_string"],
  "additionalProperties": false
}`

func editTool() Tool {
	return newBuiltin(NameEdit, prompts.Text(prompts.ToolEdit), editSchema, Properties{Effect: EffectWrite}, runEdit)
}

type editInput struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func runEdit(ctx context.Context, b *builtin, c Call) (Result, error) {
	var in editInput
	if r := b.decode(c, &in); r != nil {
		return *r, nil
	}
	p := resolve(c.Machine, in.Path)
	if in.OldString == "" {
		return b.result(ctx, c, OutcomeError, prompts.Text(prompts.EditEmptyOld), nil)
	}
	if in.OldString == in.NewString {
		return b.result(ctx, c, OutcomeError, prompts.Text(prompts.EditSame), nil)
	}
	f, err := current(ctx, c, p)
	if err != nil {
		return b.fail(ctx, c, p, err)
	}
	if !f.exists {
		return b.result(ctx, c, OutcomeError, prompts.Render(prompts.EditMissing, prompts.Data{"Path": p}), nil)
	}
	if f.refusal != "" {
		return b.result(ctx, c, OutcomeError, f.refusal, nil)
	}
	if bytes.IndexByte(f.content[:min(len(f.content), binarySniff)], 0) >= 0 {
		return b.result(ctx, c, OutcomeError, prompts.Render(prompts.EditBinary, prompts.Data{"Path": p}), nil)
	}
	content := string(f.content)
	n := strings.Count(content, in.OldString)
	switch {
	case n == 0:
		return b.result(ctx, c, OutcomeError, prompts.Render(prompts.EditNotFound, prompts.Data{"Path": p}), nil)
	case n > 1 && !in.ReplaceAll:
		return b.result(ctx, c, OutcomeError, prompts.Render(prompts.EditAmbiguous, prompts.Data{"Count": n, "Path": p}), nil)
	}
	line := strings.Count(content[:strings.Index(content, in.OldString)], "\n") + 1
	var updated string
	if in.ReplaceAll {
		updated = strings.ReplaceAll(content, in.OldString, in.NewString)
	} else {
		updated = strings.Replace(content, in.OldString, in.NewString, 1)
	}
	if err := c.Machine.WriteFile(ctx, p, strings.NewReader(updated), 0); err != nil {
		return b.fail(ctx, c, p, err)
	}
	meta := &Meta{Path: p, SHA256: digest([]byte(updated))}
	text := prompts.Render(prompts.EditDoneOne, prompts.Data{"Path": p, "Line": line})
	if n > 1 {
		text = prompts.Render(prompts.EditDoneMany, prompts.Data{"Path": p, "Count": n, "Line": line})
	}
	return b.result(ctx, c, OutcomeOK, text, meta)
}
