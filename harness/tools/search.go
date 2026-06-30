// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package tools

import (
	"context"
	"encoding/json"
	"strings"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/sandbox"
)

// This file adds the search half of the Claude-Code core surface: grep (search
// file contents) and glob (find files by name pattern). They prefer ripgrep
// (fast, .gitignore-aware) and fall back to POSIX grep/find when rg is absent, so
// they work in any sandbox image and in tests. Both run via sandbox.Exec with an
// argv (no shell), so the pattern and path cannot inject shell. A "no matches"
// result is normal output, not an error.

// maxSearchBytes caps search output so a broad pattern cannot flood the context.
const maxSearchBytes = 128 * 1024

// GrepTool searches file contents and returns matching "path:line:text" rows.
type GrepTool struct{}

var _ Tool = (*GrepTool)(nil)

type grepInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
	Glob    string `json:"glob"`
}

// Name returns the tool identifier.
func (t *GrepTool) Name() string { return "grep" }

// Def returns the canonical tool definition.
func (t *GrepTool) Def() models.ToolDef {
	return models.ToolDef{
		Name:        "grep",
		Description: "Search file contents in the agent sandbox for a pattern (regular expression). Returns matching lines as path:line:text. Optionally restrict to files matching a glob.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"pattern": {"type": "string", "description": "The pattern to search for (regular expression)."},
				"path": {"type": "string", "description": "Directory or file to search (default: current directory)."},
				"glob": {"type": "string", "description": "Only search files matching this glob, e.g. *.go (optional)."}
			},
			"required": ["pattern"]
		}`),
	}
}

// Invoke runs ripgrep (falling back to grep) over the sandbox files.
func (t *GrepTool) Invoke(ctx context.Context, input json.RawMessage, sb sandbox.Provider, sandboxID string) (models.ToolResult, error) {
	var in grepInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult("grep: invalid input JSON: %v", err), nil
	}
	if strings.TrimSpace(in.Pattern) == "" {
		return errResult("grep: pattern is empty"), nil
	}
	path := in.Path
	if path == "" {
		path = "."
	}

	rg := []string{"rg", "--line-number", "--no-heading", "--color=never"}
	if in.Glob != "" {
		rg = append(rg, "--glob", in.Glob)
	}
	rg = append(rg, "--", in.Pattern, path)

	grep := []string{"grep", "-rn"}
	if in.Glob != "" {
		grep = append(grep, "--include", in.Glob)
	}
	grep = append(grep, "-e", in.Pattern, path)

	return searchExec(ctx, sb, sandboxID, "grep", rg, grep, "no matches found")
}

// GlobTool lists files whose path matches a glob pattern.
type GlobTool struct{}

var _ Tool = (*GlobTool)(nil)

type globInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

// Name returns the tool identifier.
func (t *GlobTool) Name() string { return "glob" }

// Def returns the canonical tool definition.
func (t *GlobTool) Def() models.ToolDef {
	return models.ToolDef{
		Name:        "glob",
		Description: "Find files in the agent sandbox whose name matches a glob pattern (e.g. **/*.go). Returns matching file paths.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"pattern": {"type": "string", "description": "Glob pattern, e.g. *.go or **/*.ts."},
				"path": {"type": "string", "description": "Directory to search under (default: current directory)."}
			},
			"required": ["pattern"]
		}`),
	}
}

// Invoke lists files matching the glob via ripgrep (falling back to find).
func (t *GlobTool) Invoke(ctx context.Context, input json.RawMessage, sb sandbox.Provider, sandboxID string) (models.ToolResult, error) {
	var in globInput
	if err := json.Unmarshal(input, &in); err != nil {
		return errResult("glob: invalid input JSON: %v", err), nil
	}
	if strings.TrimSpace(in.Pattern) == "" {
		return errResult("glob: pattern is empty"), nil
	}
	path := in.Path
	if path == "" {
		path = "."
	}
	rg := []string{"rg", "--files", "--glob", in.Pattern, path}
	// find -name matches the basename only; strip any leading **/ so a glob like
	// **/*.go still finds files by their name component.
	name := in.Pattern
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	find := []string{"find", path, "-type", "f", "-name", name}
	return searchExec(ctx, sb, sandboxID, "glob", rg, find, "no files matched")
}

// searchExec runs primary (ripgrep) and, if that binary is unavailable, falls
// back to the POSIX command. Exit code 1 means "no matches" (a normal result for
// grep/rg), not a failure; exit >=2 is a real error. Output is capped.
func searchExec(ctx context.Context, sb sandbox.Provider, sandboxID, tool string, primary, fallback []string, emptyMsg string) (models.ToolResult, error) {
	res, err := sb.Exec(ctx, sandboxID, sandbox.ExecOptions{Argv: primary})
	if err != nil || res.ExitCode == 127 {
		// ripgrep not installed in this sandbox: use the POSIX fallback.
		res, err = sb.Exec(ctx, sandboxID, sandbox.ExecOptions{Argv: fallback})
	}
	if err != nil {
		return errResult("%s: exec error: %v", tool, err), nil
	}
	out := string(res.Stdout)
	if res.ExitCode >= 2 {
		return errResult("%s: %s", tool, strings.TrimSpace(out+" "+string(res.Stderr))), nil
	}
	if strings.TrimSpace(out) == "" {
		return models.ToolResult{Content: emptyMsg}, nil
	}
	if len(out) > maxSearchBytes {
		out = out[:maxSearchBytes] + "\n... (results truncated)"
	}
	return models.ToolResult{Content: out}, nil
}
