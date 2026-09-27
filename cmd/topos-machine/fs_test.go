// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fsOp runs one file operation with the given input frames.
func fsOp(t *testing.T, input []frame, args ...string) response {
	t.Helper()
	var in bytes.Buffer
	w := &frameWriter{w: &in}
	for _, f := range input {
		if err := w.frame(f.kind, f.payload); err != nil {
			t.Fatal(err)
		}
	}
	var stdout bytes.Buffer
	if code := run(t.Context(), append([]string{"fs"}, args...), &in, &stdout, io.Discard); code != 0 {
		t.Fatalf("%q: exit %d", args, code)
	}
	return decode(t, &stdout)
}

func contents(body string) []frame {
	return []frame{{frameInput, []byte(body)}, {frameEOF, nil}}
}

func TestFSWrite(t *testing.T) {
	root := tempDir(t)
	p := filepath.Join(root, "a", "b.txt")
	if res := fsOp(t, contents("hello"), "write", root, p, "0"); !res.Done {
		t.Fatalf("write = %+v", res)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "hello" {
		t.Fatalf("file = %q %v", b, err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm()&0o600 != 0o600 {
		t.Errorf("a new file's mode is %v %v", fi.Mode(), err)
	}
	script := filepath.Join(root, "run.sh")
	if res := fsOp(t, contents("#!/bin/sh\n"), "write", root, script, "755"); !res.Done {
		t.Fatalf("write = %+v", res)
	}
	if res := fsOp(t, contents("#!/bin/sh\necho\n"), "write", root, "run.sh", "0"); !res.Done {
		t.Fatalf("a relative write = %+v", res)
	}
	if fi, err := os.Stat(script); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("a write with mode 0 keeps the file's mode: %v %v", fi.Mode(), err)
	}
	writeFile(t, filepath.Join(root, "file"), "x")
	for name, c := range map[string]struct {
		input []frame
		args  []string
		kinds string
	}{
		"bad mode":       {contents(""), []string{root, p, "9"}, kindOther},
		"mode past 0777": {contents(""), []string{root, p, "1777"}, kindOther},
		"the root":       {contents(""), []string{root, root, "0"}, kindIsDir},
		// The platform decides which: Linux answers ENOTDIR, macOS EEXIST.
		"below a file":    {contents(""), []string{root, filepath.Join(root, "file", "x"), "0"}, kindNotDir + "," + kindExist},
		"outside":         {contents(""), []string{root, "/elsewhere", "0"}, kindOutside},
		"denied":          {contents(""), []string{root, filepath.Join(root, ".env"), "0"}, kindDenied},
		"killed":          {[]frame{{frameInput, []byte("part")}, {frameKill, nil}}, []string{root, p, "0"}, kindOther},
		"input cut short": {[]frame{{frameInput, []byte("part")}}, []string{root, p, "0"}, kindOther},
	} {
		res := fsOp(t, c.input, append([]string{"write"}, c.args...)...)
		if res.Error == nil || !slices.Contains(strings.Split(c.kinds, ","), res.Error.Kind) {
			t.Errorf("%s: %+v, want the kind %s", name, res.Error, c.kinds)
		}
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "hello" {
		t.Errorf("a failed write changed the file: %q %v", b, err)
	}
	left, err := filepath.Glob(filepath.Join(root, "a", "*.tmp"))
	if err != nil || len(left) != 0 {
		t.Errorf("temporary files left: %v %v", left, err)
	}
}

func TestFSStatListRemoveRename(t *testing.T) {
	root := tempDir(t)
	writeFile(t, filepath.Join(root, "b.txt"), "bb")
	writeFile(t, filepath.Join(root, "a.txt"), "a")
	writeFile(t, filepath.Join(root, ".env"), "SECRET=1")
	writeFile(t, filepath.Join(root, "full", "x"), "x")
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := tempDir(t)
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	res := fsOp(t, nil, "stat", root, filepath.Join(root, "b.txt"))
	if len(res.Entries) != 1 || res.Entries[0].Path != filepath.Join(root, "b.txt") || res.Entries[0].Size != 2 || res.Entries[0].IsDir {
		t.Errorf("stat = %+v", res)
	}
	res = fsOp(t, nil, "stat", root, root)
	if len(res.Entries) != 1 || !res.Entries[0].IsDir || !fs.FileMode(res.Entries[0].Mode).IsDir() {
		t.Errorf("stat of the root = %+v", res)
	}
	res = fsOp(t, nil, "list", root, root)
	var names []string
	for _, e := range res.Entries {
		names = append(names, filepath.Base(e.Path))
	}
	if strings.Join(names, ",") != "a.txt,b.txt,empty,full,link" {
		t.Errorf("list = %q: sorted, without .env", names)
	}
	if res := fsOp(t, nil, "list", root, filepath.Join(root, "empty")); !res.Done || len(res.Entries) != 0 {
		t.Errorf("an empty directory = %+v", res)
	}

	if res := fsOp(t, nil, "rename", root, "a.txt", "c.txt"); !res.Done {
		t.Errorf("rename = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "c.txt")); err != nil {
		t.Errorf("the renamed file: %v", err)
	}
	if res := fsOp(t, nil, "remove", root, filepath.Join(root, "c.txt")); !res.Done {
		t.Errorf("remove = %+v", res)
	}
	if res := fsOp(t, nil, "remove", root, filepath.Join(root, "empty")); !res.Done {
		t.Errorf("remove an empty directory = %+v", res)
	}

	for name, c := range map[string]struct {
		args []string
		kind string
	}{
		"stat missing":        {[]string{"stat", root, "gone"}, kindNotExist},
		"stat outside":        {[]string{"stat", root, "/"}, kindOutside},
		"stat denied":         {[]string{"stat", root, ".env"}, kindDenied},
		"stat through a link": {[]string{"stat", root, "link/x"}, kindOutside},
		"list a file":         {[]string{"list", root, "b.txt"}, kindNotDir},
		"list missing":        {[]string{"list", root, "gone"}, kindNotExist},
		"remove the root":     {[]string{"remove", root, root}, kindOther},
		"remove missing":      {[]string{"remove", root, "gone"}, kindNotExist},
		"remove non-empty":    {[]string{"remove", root, "full"}, kindExist},
		"rename missing":      {[]string{"rename", root, "gone", "other"}, kindNotExist},
		"rename out":          {[]string{"rename", root, "b.txt", "/elsewhere"}, kindOther},
		"rename to denied":    {[]string{"rename", root, "b.txt", "key.pem"}, kindPermission},
	} {
		res := fsOp(t, nil, c.args...)
		if res.Error == nil || res.Error.Kind != c.kind {
			t.Errorf("%s: %+v, want the kind %s", name, res.Error, c.kind)
		}
	}
}

// readFrames runs the read mode and returns its frames.
func readFrames(t *testing.T, ctx context.Context, root, p string) []frame {
	t.Helper()
	var stdout bytes.Buffer
	if code := run(ctx, []string{"fs", "read", root, p}, nil, &stdout, io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var out []frame
	for stdout.Len() > 0 {
		kind, payload, err := readFrame(&stdout)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, frame{kind, payload})
	}
	return out
}

func TestFSRead(t *testing.T) {
	root := tempDir(t)
	body := strings.Repeat("0123456789", chunk/5)
	writeFile(t, filepath.Join(root, "big.txt"), body)
	frames := readFrames(t, t.Context(), root, filepath.Join(root, "big.txt"))
	if len(frames) < 3 || frames[0].kind != frameStart || frames[len(frames)-1].kind != frameExit {
		t.Fatalf("frames = %d, first %c", len(frames), frames[0].kind)
	}
	var got strings.Builder
	for _, f := range frames[1 : len(frames)-1] {
		if f.kind != frameOutput {
			t.Fatalf("frame %c", f.kind)
		}
		got.Write(f.payload)
	}
	if got.String() != body {
		t.Errorf("read %d bytes, want %d", got.Len(), len(body))
	}
	for name, c := range map[string]struct {
		p    string
		kind string
	}{
		"missing":     {filepath.Join(root, "gone"), kindNotExist},
		"a directory": {root, kindIsDir},
		"outside":     {"/", kindOutside},
	} {
		frames := readFrames(t, t.Context(), root, c.p)
		var e errorBody
		if len(frames) != 1 || frames[0].kind != frameFail || json.Unmarshal(frames[0].payload, &e) != nil || e.Kind != c.kind {
			t.Errorf("%s: frames %v, want one failure of kind %s", name, frames, c.kind)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	frames = readFrames(t, ctx, root, filepath.Join(root, "big.txt"))
	if frames[len(frames)-1].kind != frameFail {
		t.Errorf("a canceled read ends with %c", frames[len(frames)-1].kind)
	}
	if code := run(t.Context(), []string{"fs", "read", root, filepath.Join(root, "big.txt")}, nil, &limitedWriter{}, io.Discard); code != 3 {
		t.Errorf("a start that cannot be sent: exit %d", code)
	}
	if code := run(t.Context(), []string{"fs", "read", root, filepath.Join(root, "big.txt")}, nil, &limitedWriter{n: 1}, io.Discard); code != 3 {
		t.Errorf("bytes that cannot be sent: exit %d", code)
	}
}
