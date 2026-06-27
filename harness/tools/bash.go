// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/latere-ai/topos/models"
	"github.com/latere-ai/topos/sandbox"
)

// BashTool implements the builtin "bash" tool: runs a shell command inside the
// agent's Cella sandbox (or local sandbox in dev mode) and returns combined
// stdout/stderr as the tool result.
type BashTool struct{}

var _ Tool = (*BashTool)(nil)

// bashInput is the expected JSON input shape for the bash tool.
type bashInput struct {
	Command string `json:"command"`
}

// Name returns the tool identifier.
func (b *BashTool) Name() string { return "bash" }

// Def returns the canonical tool definition.
func (b *BashTool) Def() models.ToolDef {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {
				"type": "string",
				"description": "The shell command to run."
			}
		},
		"required": ["command"]
	}`)
	return models.ToolDef{
		Name:        "bash",
		Description: "Run a shell command in the agent sandbox and return its output. Stdout and stderr are combined.",
		InputSchema: schema,
	}
}

// Invoke executes the bash command in the sandbox.
// A non-zero exit code is returned as a ToolResult with IsError=true.
func (b *BashTool) Invoke(ctx context.Context, input json.RawMessage, sb sandbox.SandboxProvider, sandboxID string) (models.ToolResult, error) {
	var inp bashInput
	if err := json.Unmarshal(input, &inp); err != nil {
		return models.ToolResult{
			IsError: true,
			Content: fmt.Sprintf("bash: invalid input JSON: %v", err),
		}, nil
	}
	if strings.TrimSpace(inp.Command) == "" {
		return models.ToolResult{
			IsError: true,
			Content: "bash: command is empty",
		}, nil
	}

	res, err := sb.Exec(ctx, sandboxID, sandbox.ExecOptions{
		Argv: []string{"sh", "-lc", inp.Command},
	})
	if err != nil {
		return models.ToolResult{
			IsError: true,
			Content: fmt.Sprintf("bash: exec error: %v", err),
		}, nil
	}

	content := string(res.Stdout)
	if res.ExitCode != 0 {
		return models.ToolResult{
			IsError: true,
			Content: fmt.Sprintf("bash: exit %d\n%s", res.ExitCode, content),
		}, nil
	}
	return models.ToolResult{Content: content}, nil
}

// Builtins returns a Registry pre-loaded with all built-in tools.
func Builtins() *Registry {
	r := NewRegistry()
	r.Register(&BashTool{})
	return r
}
