// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/sandbox"
)

// This file adds the first-class file tools that make an agent a reliable coding
// assistant rather than a bash-only shell: read_file, write_file, and edit_file.
// They mirror the core of Claude Code's tool surface (Read/Write/Edit) and run
// against the agent's sandbox via sandbox.Provider's ReadFile/WriteFile, so the
// model edits files through structured, individually-governable calls instead of
// fragile shell heredocs and sed. Each goes through the same hook-bus + trust-gate
// path as bash, so governance is per-tool.

// maxReadBytes caps a single read so a huge file cannot blow the model context or
// the transport. Callers page past it with offset/limit.
const maxReadBytes = 256 * 1024

// ReadFileTool reads a file from the sandbox and returns it with 1-based line
// numbers (cat -n style), which lets the model reference and edit exact lines.
type ReadFileTool struct{}

var _ Tool = (*ReadFileTool)(nil)

type readFileInput struct {
	Path string `json:"path"`
	// Offset is the 1-based line to start from (0/omitted = start of file).
	Offset int `json:"offset"`
	// Limit caps the number of lines returned (0/omitted = a sensible default).
	Limit int `json:"limit"`
}

// Name returns the tool identifier.
func (t *ReadFileTool) Name() string { return "read_file" }

// Def returns the canonical tool definition.
func (t *ReadFileTool) Def() models.ToolDef {
	return models.ToolDef{
		Name:        "read_file",
		Description: "Read a text file from the agent sandbox. Returns the contents with 1-based line numbers. Use offset/limit to page through large files.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Absolute or sandbox-relative file path."},
				"offset": {"type": "integer", "description": "1-based line to start from (optional)."},
				"limit": {"type": "integer", "description": "Maximum number of lines to return (optional)."}
			},
			"required": ["path"]
		}`),
	}
}

// defaultReadLines is the number of lines returned when no limit is given.
const defaultReadLines = 2000

// Invoke reads the requested file from the sandbox and returns numbered lines.
func (t *ReadFileTool) Invoke(ctx context.Context, input json.RawMessage, sb sandbox.Provider, sandboxID string) (models.ToolResult, error) {
	var in readFileInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult("read_file: invalid input JSON: %v", err), nil
	}
	if strings.TrimSpace(in.Path) == "" {
		return errResult("read_file: path is empty"), nil
	}
	data, err := sb.ReadFile(ctx, sandboxID, in.Path)
	if err != nil {
		return errResult("read_file: %v", err), nil
	}
	truncatedBytes := false
	if len(data) > maxReadBytes {
		data = data[:maxReadBytes]
		truncatedBytes = true
	}

	lines := strings.Split(string(data), "\n")
	start := 0
	if in.Offset > 1 {
		start = in.Offset - 1
	}
	start = min(start, len(lines))
	limit := in.Limit
	if limit <= 0 {
		limit = defaultReadLines
	}
	end := min(start+limit, len(lines))

	var b strings.Builder
	for i := start; i < end; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, lines[i])
	}
	if end < len(lines) {
		fmt.Fprintf(&b, "... (%d more lines; use offset=%d to continue)\n", len(lines)-end, end+1)
	}
	if truncatedBytes {
		b.WriteString("... (file truncated at read limit)\n")
	}
	return models.ToolResult{Content: b.String()}, nil
}

// WriteFileTool creates or overwrites a file in the sandbox.
type WriteFileTool struct{}

var _ Tool = (*WriteFileTool)(nil)

type writeFileInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Name returns the tool identifier.
func (t *WriteFileTool) Name() string { return "write_file" }

// Def returns the canonical tool definition.
func (t *WriteFileTool) Def() models.ToolDef {
	return models.ToolDef{
		Name:        "write_file",
		Description: "Create or overwrite a file in the agent sandbox with the given contents. Overwrites without warning; use edit_file for targeted changes.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Absolute or sandbox-relative file path."},
				"content": {"type": "string", "description": "The full file contents to write."}
			},
			"required": ["path", "content"]
		}`),
	}
}

// Invoke writes the given content to the file in the sandbox.
func (t *WriteFileTool) Invoke(ctx context.Context, input json.RawMessage, sb sandbox.Provider, sandboxID string) (models.ToolResult, error) {
	var in writeFileInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult("write_file: invalid input JSON: %v", err), nil
	}
	if strings.TrimSpace(in.Path) == "" {
		return errResult("write_file: path is empty"), nil
	}
	if err := sb.WriteFile(ctx, sandboxID, in.Path, []byte(in.Content)); err != nil {
		return errResult("write_file: %v", err), nil
	}
	return models.ToolResult{Content: fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path)}, nil
}

// EditFileTool applies an exact-string replacement to a file. The old_string must
// match exactly and, unless replace_all is set, must be unique, so an edit can
// never silently change the wrong occurrence. This is the model's reliable
// alternative to sed.
type EditFileTool struct{}

var _ Tool = (*EditFileTool)(nil)

type editFileInput struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// Name returns the tool identifier.
func (t *EditFileTool) Name() string { return "edit_file" }

// Def returns the canonical tool definition.
func (t *EditFileTool) Def() models.ToolDef {
	return models.ToolDef{
		Name:        "edit_file",
		Description: "Replace an exact string in a file in the agent sandbox. old_string must match exactly; unless replace_all is true it must be unique, or the edit is rejected.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Absolute or sandbox-relative file path."},
				"old_string": {"type": "string", "description": "The exact text to replace."},
				"new_string": {"type": "string", "description": "The replacement text."},
				"replace_all": {"type": "boolean", "description": "Replace every occurrence instead of requiring a unique match."}
			},
			"required": ["path", "old_string", "new_string"]
		}`),
	}
}

// Invoke applies the exact-string replacement to the file in the sandbox.
func (t *EditFileTool) Invoke(ctx context.Context, input json.RawMessage, sb sandbox.Provider, sandboxID string) (models.ToolResult, error) {
	var in editFileInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult("edit_file: invalid input JSON: %v", err), nil
	}
	if strings.TrimSpace(in.Path) == "" {
		return errResult("edit_file: path is empty"), nil
	}
	if in.OldString == in.NewString {
		return errResult("edit_file: old_string and new_string are identical"), nil
	}
	data, err := sb.ReadFile(ctx, sandboxID, in.Path)
	if err != nil {
		return errResult("edit_file: %v", err), nil
	}
	content := string(data)
	n := strings.Count(content, in.OldString)
	if n == 0 {
		return errResult("edit_file: old_string not found in %s", in.Path), nil
	}
	if n > 1 && !in.ReplaceAll {
		return errResult("edit_file: old_string is not unique in %s (%d matches); pass replace_all or add more context", in.Path, n), nil
	}
	var updated string
	if in.ReplaceAll {
		updated = strings.ReplaceAll(content, in.OldString, in.NewString)
	} else {
		updated = strings.Replace(content, in.OldString, in.NewString, 1)
	}
	if err := sb.WriteFile(ctx, sandboxID, in.Path, []byte(updated)); err != nil {
		return errResult("edit_file: %v", err), nil
	}
	return models.ToolResult{Content: fmt.Sprintf("edited %s (%d replacement(s))", in.Path, n)}, nil
}

// errResult builds an error ToolResult. A tool returns its failure to the model
// (IsError) rather than a Go error, so the loop continues and the model can
// recover, matching BashTool's contract.
func errResult(format string, args ...any) models.ToolResult {
	return models.ToolResult{IsError: true, Content: fmt.Sprintf(format, args...)}
}
