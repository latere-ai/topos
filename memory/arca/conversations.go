// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arca

import (
	"context"
	"errors"
	"io/fs"
	"strings"

	"latere.ai/x/topos/memory"
	"latere.ai/x/topos/session"
)

// conversations lists a folder of conversations (spec 094) through the
// machine's reach: one file per conversation, its version the checksum a
// search caches its text by.
func (m *Machine) conversations(ctx context.Context, f session.MemoryFolder) ([]listed, bool, error) {
	files, more, err := m.chats.ListConversations(ctx, f.Name)
	if err != nil {
		return nil, false, reached(ctx, err, "the folder's conversations could not be listed")
	}
	out := make([]listed, 0, min(len(files), memory.MaxListed))
	for _, c := range files {
		if c.Name == "" || strings.Contains(c.Name, "/") || !cleanRel(c.Name) {
			continue
		}
		if len(out) == memory.MaxListed {
			return out, true, nil
		}
		out = append(out, listed{rel: c.Name, size: c.Size, checksum: c.Version, modified: c.Modified})
	}
	return out, more, nil
}

// conversation reads the file rel of a folder of conversations through
// the machine's reach and keeps its version for the path. A file no
// conversation of the folder answers is not there.
func (m *Machine) conversation(ctx context.Context, f session.MemoryFolder, rel, p string) (memory.ConversationFile, []byte, error) {
	if strings.Contains(rel, "/") {
		m.keep(p, "")
		return memory.ConversationFile{}, nil, notExist("read", p)
	}
	file, data, err := m.chats.ReadConversation(ctx, f.Name, rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		m.keep(p, "")
		return memory.ConversationFile{}, nil, notExist("read", p)
	case err != nil:
		return memory.ConversationFile{}, nil, reached(ctx, err, "the conversation could not be read")
	case len(data) > memory.MaxFileBytes:
		return memory.ConversationFile{}, nil, &memory.TooLarge{Path: p, Size: int64(len(data))}
	}
	m.keep(p, file.Version)
	return file, data, nil
}

// reached is a failure of the reach to conversations as the file tools
// say it: the call's own context ending as that context's error, an error
// the memory package names as it is, and any other as Unreachable.
func reached(ctx context.Context, err error, detail string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var unreachable *memory.Unreachable
	var refusal *memory.Refusal
	if errors.As(err, &unreachable) || errors.As(err, &refusal) {
		return err
	}
	return &memory.Unreachable{Detail: detail, Err: err}
}
