// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/prompt"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/session"
)

// maxInstructionBytes bounds one instruction file as the harness renders
// it; the machine applies the same bound when it records the file.
const maxInstructionBytes = 64 << 10

// BlobReader reads a session blob.
type BlobReader interface {
	Blob(ctx context.Context, d session.Digest) (io.ReadCloser, error)
}

// systemBlocks renders a request's system prompt (spec 010, parts 2 to
// 7): the harness prompt, the agent's instructions, then the fold's
// system parts in order.
func systemBlocks(ctx context.Context, harnessPrompt, agentInstructions string, parts []session.Part, blobs BlobReader) ([]lux.Block, error) {
	blocks := []lux.Block{{Type: ir.BlockText, Text: harnessPrompt}}
	if s := strings.TrimSpace(agentInstructions); s != "" {
		blocks = append(blocks, lux.Block{Type: ir.BlockText, Text: s})
	}
	for _, p := range parts {
		text, err := renderPart(ctx, p, blobs)
		if err != nil {
			return nil, err
		}
		if text != "" {
			blocks = append(blocks, lux.Block{Type: ir.BlockText, Text: text})
		}
	}
	return blocks, nil
}

// renderPart is the text of spec 011 for one system part.
func renderPart(ctx context.Context, p session.Part, blobs BlobReader) (string, error) {
	switch p.Kind {
	case session.PartContext:
		return p.Context, nil
	case session.PartInstructions:
		in := p.Instructions
		rc, err := blobs.Blob(ctx, in.Blob)
		if err != nil {
			return "", fmt.Errorf("harness: read the instructions of %s: %w", in.Path, err)
		}
		b, rerr := io.ReadAll(io.LimitReader(rc, maxInstructionBytes+1))
		if err := rc.Close(); err != nil && rerr == nil {
			rerr = err
		}
		if rerr != nil {
			return "", fmt.Errorf("harness: read the instructions of %s: %w", in.Path, rerr)
		}
		body := string(b)
		if len(b) > maxInstructionBytes {
			body = string(b[:maxInstructionBytes]) + "\n[the file is longer than 64 KiB and was cut here]"
		}
		return fmt.Sprintf("<instructions path=%q>\n%s\n</instructions>", in.Path, strings.TrimRight(body, "\n")), nil
	case session.PartSkills:
		var b strings.Builder
		b.WriteString("<skills>\n")
		for _, s := range p.Skills {
			fmt.Fprintf(&b, "- name: %s\n  description: %s\n  path: %s\n", s.Name, s.Description, s.Path)
		}
		b.WriteString("</skills>")
		return b.String(), nil
	case session.PartMemory:
		m := p.Memory
		access := "read-write"
		if m.Access == "read_only" {
			access = "read-only"
		}
		line := fmt.Sprintf("Memory store %s (%s) is at %s", m.Name, access, m.Path)
		if m.Description != "" {
			line += ": " + m.Description
		}
		return line, nil
	}
	return "", nil
}

// luxTools are the registry's definitions on the Lux wire.
func luxTools(defs []tools.Definition) []lux.Tool {
	out := make([]lux.Tool, len(defs))
	for i, d := range defs {
		schema := d.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out[i] = lux.Tool{Name: d.Name, Description: d.Description, InputSchema: schema}
	}
	return out
}

// toolsHash is the sha256 of the tool definitions as sent.
func toolsHash(defs []lux.Tool) (string, error) {
	b, err := session.Marshal(defs)
	if err != nil {
		return "", fmt.Errorf("harness: encode the tool definitions: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// placeBreakpoints marks the cache breakpoints of spec 010 that the IR
// can carry: the last system block, and the last block of the final user
// message and of the user message before the latest assistant message.
// The prefix a provider caches runs tools, then system, then messages,
// so the system breakpoint covers the tool definitions too.
func placeBreakpoints(system []lux.Block, messages []lux.Message) {
	if n := len(system); n > 0 {
		system[n-1].CacheHint = true
	}
	marked := 0
	for i := len(messages) - 1; i >= 0 && marked < 2; i-- {
		m := &messages[i]
		if m.Role != ir.RoleUser || len(m.Blocks) == 0 {
			continue
		}
		m.Blocks[len(m.Blocks)-1].CacheHint = true
		marked++
	}
}

// requestParts are what one step's request is built from.
type requestParts struct {
	Model     string
	System    []lux.Block
	Messages  []lux.Message
	Tools     []lux.Tool
	MaxTokens int64
	Effort    string
	CacheKey  string
}

// buildRequest turns the parts into the IR through the Lux codec: the Lux
// dialect is the IR on the wire, so the log's Lux JSON and the request
// agree by construction.
func buildRequest(p requestParts) (ir.Request, error) {
	msgs := make([]lux.Message, len(p.Messages))
	for i, m := range p.Messages {
		msgs[i] = lux.Message{Role: m.Role, Blocks: append([]lux.Block(nil), m.Blocks...)}
	}
	system := append([]lux.Block(nil), p.System...)
	placeBreakpoints(system, msgs)
	limit := p.MaxTokens
	req := lux.Request{Model: p.Model, System: system, Messages: msgs, Tools: p.Tools, MaxTokens: &limit, Stream: true, CacheKey: p.CacheKey}
	if p.Effort != "" {
		req.Reasoning = &lux.Reasoning{Effort: ir.Effort(p.Effort)}
	}
	b, err := json.Marshal(req)
	if err != nil {
		return ir.Request{}, fmt.Errorf("harness: encode the request: %w", err)
	}
	r, err := (&lux.Frontend{}).DecodeRequest(b)
	if err != nil {
		return ir.Request{}, fmt.Errorf("harness: decode the request: %w", err)
	}
	return *r, nil
}

// harnessPrompt renders the harness prompt of a version.
func harnessPrompt(version int, o prompt.Options) (string, error) {
	if version == 0 {
		version = prompt.Current
	}
	return prompt.Render(version, o)
}
