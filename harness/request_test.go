// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/session"
)

type blobs map[session.Digest][]byte

func (b blobs) Blob(ctx context.Context, d session.Digest) (io.ReadCloser, error) {
	v, ok := b[d]
	if !ok {
		return nil, session.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(v)), nil
}

func TestSystemBlocksRenderEveryPart(t *testing.T) {
	small := []byte("Run make check before a commit.\n")
	big := bytes.Repeat([]byte("x"), maxInstructionBytes+10)
	store := blobs{session.DigestOf(small): small, session.DigestOf(big): big}
	parts := []session.Part{
		{Kind: session.PartContext, Context: "<context>\nWorking directory: /work\n</context>"},
		{Kind: session.PartInstructions, Instructions: &session.Instructions{Path: "/work/AGENTS.md", Blob: session.DigestOf(small)}},
		{Kind: session.PartInstructions, Instructions: &session.Instructions{Path: "/work/big/AGENTS.md", Blob: session.DigestOf(big)}},
		{Kind: session.PartSkills, Skills: []session.Skill{{Name: "release", Description: "Cut a release.", Path: "/work/.agents/skills/release/SKILL.md"}}},
		{Kind: session.PartMemory, Memory: &session.MemoryAttached{Name: "notes", Access: "read_write", Path: "/mnt/notes", Description: "what we learned"}},
		{Kind: session.PartMemory, Memory: &session.MemoryAttached{Name: "team", Access: "read_only", Path: "/mnt/team"}},
		{Kind: "future"},
	}
	got, err := systemBlocks(t.Context(), "HARNESS", "  Be terse.  ", parts, store)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, b := range got {
		texts = append(texts, b.Text)
	}
	want := []string{
		"HARNESS",
		"Be terse.",
		"<context>\nWorking directory: /work\n</context>",
		"<instructions path=\"/work/AGENTS.md\">\nRun make check before a commit.\n</instructions>",
	}
	for i, w := range want {
		if texts[i] != w {
			t.Fatalf("block %d = %q, want %q", i, texts[i], w)
		}
	}
	if !strings.Contains(texts[4], "longer than 64 KiB and was cut here") {
		t.Fatal("a long instruction file was not cut")
	}
	if texts[5] != "<skills>\n- name: release\n  description: Cut a release.\n  path: /work/.agents/skills/release/SKILL.md\n</skills>" {
		t.Fatalf("skills %q", texts[5])
	}
	if texts[6] != "Memory store notes (read-write) is at /mnt/notes: what we learned" || texts[7] != "Memory store team (read-only) is at /mnt/team" {
		t.Fatalf("memory notes %q %q", texts[6], texts[7])
	}
	if len(texts) != 8 {
		t.Fatalf("%d blocks; an unknown part kind renders nothing", len(texts))
	}
	missing := []session.Part{{Kind: session.PartInstructions, Instructions: &session.Instructions{Path: "/x", Blob: session.DigestOf([]byte("absent"))}}}
	if _, err := systemBlocks(t.Context(), "H", "", missing, store); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("a missing instruction blob: %v", err)
	}
}

func TestBreakpointsRollWithTheConversation(t *testing.T) {
	sys := []lux.Block{{Type: ir.BlockText, Text: "a"}, {Type: ir.BlockText, Text: "b"}}
	msgs := []lux.Message{
		{Role: ir.RoleUser, Blocks: []lux.Block{{Type: ir.BlockText, Text: "1"}}},
		{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockText, Text: "2"}}},
		{Role: ir.RoleUser, Blocks: []lux.Block{{Type: ir.BlockText, Text: "3"}}},
		{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockText, Text: "4"}}},
		{Role: ir.RoleUser, Blocks: []lux.Block{{Type: ir.BlockText, Text: "5a"}, {Type: ir.BlockText, Text: "5b"}}},
	}
	placeBreakpoints(sys, msgs)
	if sys[0].CacheHint || !sys[1].CacheHint {
		t.Fatal("the system breakpoint is not on the last system block")
	}
	if msgs[0].Blocks[0].CacheHint || !msgs[2].Blocks[0].CacheHint || msgs[4].Blocks[0].CacheHint || !msgs[4].Blocks[1].CacheHint {
		t.Fatal("the rolling breakpoints are not on the last two user messages' last blocks")
	}
	req, err := buildRequest(requestParts{Model: "m", System: []lux.Block{{Type: ir.BlockText, Text: "s"}}, Messages: msgs[:1], MaxTokens: 10, Effort: "high", CacheKey: "ses_1"})
	if err != nil {
		t.Fatal(err)
	}
	if req.Reasoning == nil || req.Reasoning.Effort != "high" || *req.MaxTokens != 10 || !req.Stream || req.CacheKey != "ses_1" {
		t.Fatalf("request %+v", req)
	}
	if msgs[0].Blocks[0].CacheHint && req.Messages[0].Blocks[0].CacheHint != true {
		t.Fatal("the request lost a breakpoint")
	}
	defs := luxTools(nil)
	if len(defs) != 0 {
		t.Fatal("tools from nothing")
	}
}
