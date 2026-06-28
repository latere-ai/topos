// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

// Package loop implements the Topos native agentic turn driver.
//
// The loop takes a model, a sandbox, a tool registry, a hook bus, and an
// initial prompt, then runs the following until done or cancelled:
//
//  1. Build a Request from the system prompt + transcript + tool defs.
//  2. Call model.Stream and drain events accumulating text, tool calls, usage.
//  3. If StopReason == tool_use: for each tool call, run it through the hook
//     bus three-phase path (PreToolUse → permission → execute → PostToolUse /
//     PostToolUseFailure), append ToolResults as a tool-role Message.
//  4. Stop on end_turn, context cancellation, or max-iterations cap.
//
// The loop emits lifecycle events to the bus: SessionStart, UserPromptSubmit,
// Stop, SessionEnd (always via defer).
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"latere.ai/x/topos/harness/hooks"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/sandbox"
)

const (
	// MaxIterations is the turn-cap safety net. A well-behaved model stops
	// earlier; this prevents runaway in tests and against broken models.
	MaxIterations = 16
)

// Config holds the dependencies for a single agentic run.
type Config struct {
	// Model is the chat-completion backend.
	Model models.Model
	// Sandbox is the execution backend.
	Sandbox sandbox.Provider
	// SandboxID is the pre-provisioned sandbox instance.
	SandboxID string
	// Tools is the tool registry. Tools.Defs() are injected into each
	// model request; tool calls are routed to Tools.Get(name).Invoke.
	Tools *tools.Registry
	// Bus is the hook event dispatcher. All lifecycle events are emitted here.
	Bus *hooks.Bus
	// SessionID is the opaque identifier for this run (for event payloads).
	SessionID string
	// AgentID identifies which agent is being run.
	AgentID string
	// SystemPrompt is the agent's static system instruction.
	SystemPrompt string
	// UserPrompt is the initial user message that starts the run. For a
	// resumed run it may be empty (the run continues from InitialTranscript
	// with no new user input).
	UserPrompt string
	// InitialTranscript, when non-empty, seeds the conversation — used to
	// resume a crashed session from its replayed event log. UserPrompt, if
	// also set, is appended after it.
	InitialTranscript []models.Message
	// Logger is used for structured logging. Nil → slog.Default().
	Logger *slog.Logger
	// MaxTokens caps model response size (0 = provider default).
	MaxTokens int
}

// Result is the summary of a completed agentic run.
type Result struct {
	// Transcript is the full conversation history (user + assistant + tool).
	Transcript []models.Message
	// FinalText is the last assistant text turn (may be empty if the run
	// ended on a tool call).
	FinalText string
	// StopReason is the final stop reason from the model.
	StopReason models.StopReason
	// TotalUsage is the accumulated token usage across all turns.
	TotalUsage models.Usage
	// ToolCallCount is the total number of tool calls executed.
	ToolCallCount int
}

// Run executes the agentic loop with the given config and returns a Result.
// It returns an error only for unrecoverable infrastructure failures (model
// stream error, context cancellation); tool errors are captured in ToolResult.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	startedAt := time.Now().UTC()

	// Emit SessionStart.
	cfg.Bus.Dispatch(hooks.EventSessionStart, &hooks.SessionStartPayload{
		Version:   "1",
		SessionID: cfg.SessionID,
		AgentID:   cfg.AgentID,
		StartedAt: startedAt,
	})

	// Emit UserPromptSubmit only when there is new user input. A resumed run
	// that continues without a new prompt must not record a phantom prompt.
	if cfg.UserPrompt != "" {
		cfg.Bus.Dispatch(hooks.EventUserPromptSubmit, &hooks.UserPromptSubmitPayload{
			Version:     "1",
			SessionID:   cfg.SessionID,
			Prompt:      cfg.UserPrompt,
			SubmittedAt: startedAt,
		})
	}

	result := &Result{}

	// Ensure SessionEnd fires exactly once, even on error.
	defer func() {
		cfg.Bus.Dispatch(hooks.EventSessionEnd, &hooks.SessionEndPayload{
			Version:     "1",
			SessionID:   cfg.SessionID,
			EndedAt:     time.Now().UTC(),
			FinalReason: result.StopReason,
		})
	}()

	// Build the tool path for permission resolution.
	tp := hooks.NewToolPath(cfg.Bus, nil /* no deny-rules in MVP trusted sandbox */)

	// Initialise the transcript: seed from a resumed transcript if provided,
	// then append the new user prompt (if any).
	var transcript []models.Message
	transcript = append(transcript, cfg.InitialTranscript...)
	if cfg.UserPrompt != "" {
		transcript = append(transcript, models.Message{Role: "user", Content: cfg.UserPrompt})
	}

	maxTokens := cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	var finalStopReason models.StopReason

	// toolsDisabled is set to true for the remainder of this session once we
	// detect that the model does not support tool calling. All subsequent turns
	// are issued without tools (chat-only fallback).
	var toolsDisabled bool

	for iter := range MaxIterations {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		var toolDefs []models.ToolDef
		if !toolsDisabled {
			toolDefs = cfg.Tools.Defs()
		}

		req := models.Request{
			System:    cfg.SystemPrompt,
			Messages:  transcript,
			Tools:     toolDefs,
			MaxTokens: maxTokens,
		}

		stream, err := cfg.Model.Stream(ctx, req)
		if err != nil {
			// If the model rejected the request because it does not support
			// tools, disable tools for this session and retry once without them.
			if errors.Is(err, models.ErrToolsUnsupported) && len(req.Tools) > 0 {
				toolsDisabled = true
				logger.Warn("loop: model does not support tools; continuing in chat-only mode",
					"agent_id", cfg.AgentID,
					"session_id", cfg.SessionID,
				)
				req.Tools = nil
				stream, err = cfg.Model.Stream(ctx, req)
			}
			if err != nil {
				return nil, fmt.Errorf("loop: model stream (iter %d): %w", iter, err)
			}
		}

		// Drain the stream.
		var (
			assistantText strings.Builder
			toolCalls     []*models.ToolCall
			turnUsage     models.Usage
			stopReason    models.StopReason
		)

		for {
			if ctx.Err() != nil {
				_ = stream.Close()
				return nil, ctx.Err()
			}

			ev, recvErr := stream.Recv()
			if recvErr != nil {
				if errors.Is(recvErr, io.EOF) {
					break
				}
				_ = stream.Close()
				return nil, fmt.Errorf("loop: stream recv (iter %d): %w", iter, recvErr)
			}

			switch ev.Kind {
			case models.KindTextDelta:
				assistantText.WriteString(ev.TextDelta)
			case models.KindToolCallDone:
				if ev.ToolCall != nil {
					tc := *ev.ToolCall
					toolCalls = append(toolCalls, &tc)
				}
			case models.KindUsage:
				if ev.Usage != nil {
					turnUsage.InputTokens += ev.Usage.InputTokens
					turnUsage.OutputTokens += ev.Usage.OutputTokens
					turnUsage.CacheReadTokens += ev.Usage.CacheReadTokens
					turnUsage.CacheWriteTokens += ev.Usage.CacheWriteTokens
				}
			case models.KindDone:
				stopReason = ev.StopReason
			}
		}
		_ = stream.Close()

		// Accumulate usage.
		result.TotalUsage.InputTokens += turnUsage.InputTokens
		result.TotalUsage.OutputTokens += turnUsage.OutputTokens
		result.TotalUsage.CacheReadTokens += turnUsage.CacheReadTokens
		result.TotalUsage.CacheWriteTokens += turnUsage.CacheWriteTokens

		finalStopReason = stopReason

		// Build the assistant message for the transcript.
		assistMsg := models.Message{
			Role:      "assistant",
			Content:   assistantText.String(),
			ToolCalls: make([]models.ToolCall, 0, len(toolCalls)),
		}
		for _, tc := range toolCalls {
			assistMsg.ToolCalls = append(assistMsg.ToolCalls, *tc)
		}
		transcript = append(transcript, assistMsg)

		if assistantText.Len() > 0 {
			result.FinalText = assistantText.String()
		}

		// Stop if no tool calls requested.
		if stopReason != models.StopToolUse || len(toolCalls) == 0 {
			logger.Info("loop: turn completed",
				"iter", iter,
				"stop_reason", stopReason,
				"tool_calls", len(toolCalls),
			)
			break
		}

		// Execute each tool call through the hook bus three-phase path.
		toolResults := make([]models.ToolResult, 0, len(toolCalls))
		for _, tc := range toolCalls {
			tr, execErr := executeToolCall(ctx, cfg, tp, tc, logger)
			tr.CallID = tc.ID
			toolResults = append(toolResults, tr)
			if execErr != nil {
				logger.Warn("loop: tool execution error",
					"tool", tc.Name,
					"error", execErr,
				)
			}
			result.ToolCallCount++
		}

		// Append results as a single tool-role message.
		transcript = append(transcript, models.Message{
			Role:        "tool",
			ToolResults: toolResults,
		})
	}

	result.Transcript = transcript
	result.StopReason = finalStopReason

	// Emit Stop.
	cfg.Bus.Dispatch(hooks.EventStop, &hooks.StopPayload{
		Version:       "1",
		SessionID:     cfg.SessionID,
		StopReason:    finalStopReason,
		ToolCallCount: result.ToolCallCount,
	})

	return result, nil
}

// executeToolCall runs one tool call through the hook bus three-phase path and
// returns the ToolResult.
func executeToolCall(
	ctx context.Context,
	cfg Config,
	tp *hooks.ToolPath,
	tc *models.ToolCall,
	logger *slog.Logger,
) (models.ToolResult, error) {
	logger.Info("loop: tool call", "tool", tc.Name, "id", tc.ID)

	// Input normalisation: ensure valid JSON.
	rawInput := tc.Input
	if len(rawInput) == 0 || string(rawInput) == "null" {
		rawInput = json.RawMessage("{}")
	}

	// Phase 1+2: permission via hook bus.
	phase := tp.Resolve(cfg.SessionID, models.ToolCall{ID: tc.ID, Name: tc.Name, Input: rawInput})
	if !phase.Allowed {
		logger.Warn("loop: tool denied",
			"tool", tc.Name,
			"denied_by", phase.DeniedBy,
			"reason", phase.Reason,
		)
		cfg.Bus.Dispatch(hooks.EventPostToolUseFailure, &hooks.PostToolUseFailurePayload{
			Version:   "1",
			SessionID: cfg.SessionID,
			ToolCall:  *tc,
			Error:     fmt.Sprintf("denied by %s: %s", phase.DeniedBy, phase.Reason),
		})
		return models.ToolResult{
			IsError: true,
			Content: fmt.Sprintf("tool %q denied: %s", tc.Name, phase.Reason),
		}, nil
	}

	// Phase 3: look up tool in registry.
	tool := cfg.Tools.Get(tc.Name)
	if tool == nil {
		errMsg := fmt.Sprintf("tool %q not found in registry", tc.Name)
		cfg.Bus.Dispatch(hooks.EventPostToolUseFailure, &hooks.PostToolUseFailurePayload{
			Version:   "1",
			SessionID: cfg.SessionID,
			ToolCall:  *tc,
			Error:     errMsg,
		})
		return models.ToolResult{
			IsError: true,
			Content: errMsg,
		}, nil
	}

	// Execute. The caller stamps tr.CallID = tc.ID on every return path, so it
	// is not set here.
	tr, invokeErr := tool.Invoke(ctx, phase.ModifiedInput, cfg.Sandbox, cfg.SandboxID)

	if invokeErr != nil || tr.IsError {
		errStr := ""
		if invokeErr != nil {
			errStr = invokeErr.Error()
		} else {
			errStr = tr.Content
		}
		cfg.Bus.Dispatch(hooks.EventPostToolUseFailure, &hooks.PostToolUseFailurePayload{
			Version:   "1",
			SessionID: cfg.SessionID,
			ToolCall:  *tc,
			Error:     errStr,
		})
		if invokeErr != nil {
			return models.ToolResult{
				IsError: true,
				Content: fmt.Sprintf("tool %q invoke error: %v", tc.Name, invokeErr),
			}, invokeErr
		}
		return tr, nil
	}

	cfg.Bus.Dispatch(hooks.EventPostToolUse, &hooks.PostToolUsePayload{
		Version:   "1",
		SessionID: cfg.SessionID,
		ToolCall:  *tc,
		Result:    tr,
	})

	return tr, nil
}
