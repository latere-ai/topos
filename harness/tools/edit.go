// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"strings"
)

//go:embed descriptions/edit.md
var editDescription string

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
	return newBuiltin(NameEdit, editDescription, editSchema, Properties{Effect: EffectWrite}, runEdit)
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
		return b.result(ctx, c, OutcomeError, "old_string is empty; give the exact text to replace, or use write to create a file.", nil)
	}
	if in.OldString == in.NewString {
		return b.result(ctx, c, OutcomeError, "old_string and new_string are the same; there is nothing to change.", nil)
	}
	f, err := current(ctx, c, p)
	if err != nil {
		return b.fail(ctx, c, p, err)
	}
	if !f.exists {
		return b.result(ctx, c, OutcomeError, p+" does not exist; use write to create it.", nil)
	}
	if f.refusal != "" {
		return b.result(ctx, c, OutcomeError, f.refusal, nil)
	}
	if bytes.IndexByte(f.content[:min(len(f.content), binarySniff)], 0) >= 0 {
		return b.result(ctx, c, OutcomeError, p+" is a binary file; edit changes text files.", nil)
	}
	content := string(f.content)
	n := strings.Count(content, in.OldString)
	switch {
	case n == 0:
		return b.result(ctx, c, OutcomeError, fmt.Sprintf("old_string does not occur in %s. Read the file again and copy the text exactly, whitespace and indentation included.", p), nil)
	case n > 1 && !in.ReplaceAll:
		return b.result(ctx, c, OutcomeError, fmt.Sprintf("old_string occurs %d times in %s. Add surrounding lines to make it unique, or set replace_all to replace every occurrence.", n, p), nil)
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
	text := fmt.Sprintf("Edited %s: replaced 1 occurrence at line %d.", p, line)
	if n > 1 {
		text = fmt.Sprintf("Edited %s: replaced %d occurrences, the first at line %d.", p, n, line)
	}
	return b.result(ctx, c, OutcomeOK, text, meta)
}
