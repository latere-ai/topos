// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/machine"
)

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestReadNumbersLines(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	read := builtinTool(t, NameRead)
	body := "one\ntwo\nthree\nfour\nfive\n"
	put(t, filepath.Join(f.work, "a.txt"), body)

	res := run(ctx, t, read, f.h, State{}, `{"path":"a.txt"}`)
	want := "     1\tone\n     2\ttwo\n     3\tthree\n     4\tfour\n     5\tfive\n"
	if res.Outcome != OutcomeOK || text(res) != want {
		t.Fatalf("read %s %q", res.Outcome, text(res))
	}
	if res.Meta == nil || res.Meta.Path != filepath.Join(f.work, "a.txt") || res.Meta.SHA256 != sum(body) {
		t.Fatalf("meta %+v", res.Meta)
	}

	res = run(ctx, t, read, f.h, State{}, mustInput(t, map[string]any{"path": filepath.Join(f.work, "a.txt"), "offset": 2, "limit": 2}))
	want = "     2\ttwo\n     3\tthree\n[lines 2 to 3 of 5; read on with offset 4]\n"
	if text(res) != want || res.Meta.SHA256 != sum(body) {
		t.Fatalf("offset and limit %q, meta %+v", text(res), res.Meta)
	}

	res = run(ctx, t, read, f.h, State{}, `{"path":"a.txt","offset":4}`)
	if text(res) != "     4\tfour\n     5\tfive\n" {
		t.Fatalf("to the end %q", text(res))
	}

	res = run(ctx, t, read, f.h, State{}, `{"path":"a.txt","offset":9}`)
	if res.Outcome != OutcomeError || text(res) != filepath.Join(f.work, "a.txt")+" has 5 lines; offset 9 is past its end." || res.Meta == nil {
		t.Fatalf("past the end %s %q", res.Outcome, text(res))
	}

	put(t, filepath.Join(f.work, "b.txt"), "no newline\r\nat the end")
	res = run(ctx, t, read, f.h, State{}, `{"path":"b.txt"}`)
	if text(res) != "     1\tno newline\r\n     2\tat the end\n" {
		t.Fatalf("a last line without a newline %q", text(res))
	}

	put(t, filepath.Join(f.work, "empty.txt"), "")
	res = run(ctx, t, read, f.h, State{}, `{"path":"empty.txt"}`)
	if res.Outcome != OutcomeOK || text(res) != filepath.Join(f.work, "empty.txt")+" is empty." || res.Meta.SHA256 != sum("") {
		t.Fatalf("empty %q %+v", text(res), res.Meta)
	}
}

func TestReadCutsLongLines(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	read := builtinTool(t, NameRead)
	cut := fmt.Sprintf(" [... line cut at %d characters]", ReadMaxLineChars)
	lines := []string{
		strings.Repeat("x", 2500),
		strings.Repeat("é", 3000),
		strings.Repeat("y", 70000),
		strings.Repeat("z", ReadMaxLineChars),
		"after",
	}
	body := strings.Join(lines, "\n") + "\n"
	put(t, filepath.Join(f.work, "long.txt"), body)
	res := run(ctx, t, read, f.h, State{}, `{"path":"long.txt"}`)
	want := "     1\t" + strings.Repeat("x", 2000) + cut + "\n" +
		"     2\t" + strings.Repeat("é", 2000) + cut + "\n" +
		"     3\t" + strings.Repeat("y", 2000) + cut + "\n" +
		"     4\t" + strings.Repeat("z", 2000) + "\n" +
		"     5\tafter\n"
	if text(res) != want {
		t.Fatalf("long lines: got %d bytes, want %d", len(text(res)), len(want))
	}
	if res.Meta.SHA256 != sum(body) {
		t.Fatal("the hash is not of the whole file")
	}
}

func TestReadImages(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	read := builtinTool(t, NameRead)
	for name, head := range map[string]string{
		"a.png":  "\x89PNG\r\n\x1a\n",
		"a.jpg":  "\xff\xd8\xff\xe0",
		"a.gif":  "GIF89a",
		"b.gif":  "GIF87a",
		"a.webp": "RIFF\x00\x00\x00\x00WEBPVP8 ",
	} {
		body := head + "\x00\x01\x02 image data"
		put(t, filepath.Join(f.work, name), body)
		res := run(ctx, t, read, f.h, State{}, mustInput(t, map[string]string{"path": name}))
		if res.Outcome != OutcomeOK || len(res.Content) != 1 || res.Content[0].Type != ir.BlockImage || res.Content[0].Image == nil {
			t.Fatalf("%s: %+v", name, res)
		}
		img := res.Content[0].Image
		wantType := map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}[filepath.Ext(name)]
		if img.MediaType != wantType || img.Data != base64.StdEncoding.EncodeToString([]byte(body)) {
			t.Fatalf("%s: media %s", name, img.MediaType)
		}
		if res.Meta == nil || res.Meta.SHA256 != sum(body) || res.Meta.Path != filepath.Join(f.work, name) {
			t.Fatalf("%s: meta %+v", name, res.Meta)
		}
	}
	big := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", ReadMaxImage)
	put(t, filepath.Join(f.work, "big.png"), big)
	res := run(ctx, t, read, f.h, State{}, `{"path":"big.png"}`)
	if res.Outcome != OutcomeError || !strings.Contains(text(res), "over the 5 MiB limit") {
		t.Fatalf("a big image %s %q", res.Outcome, text(res))
	}
}

func TestReadRefuses(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	read := builtinTool(t, NameRead)
	put(t, filepath.Join(f.work, "bin"), "abc\x00def")
	put(t, filepath.Join(f.work, ".env"), "KEY=secret")
	put(t, filepath.Join(f.outside, "x.txt"), "outside")
	put(t, filepath.Join(f.work, "dir", "x.txt"), "x")
	for _, c := range []struct{ path, want string }{
		{"bin", filepath.Join(f.work, "bin") + " is a binary file; read shows text files and PNG, JPEG, GIF and WebP images. Inspect it with bash."},
		{"dir", filepath.Join(f.work, "dir") + " is a directory; use glob to list the files in it."},
		{"absent.txt", filepath.Join(f.work, "absent.txt") + " does not exist."},
		{filepath.Join(f.outside, "x.txt"), filepath.Join(f.outside, "x.txt") + " is outside the working directory."},
		{"../outside/x.txt", filepath.Join(f.outside, "x.txt") + " is outside the working directory."},
		{".env", filepath.Join(f.work, ".env") + " is on the credential deny-list; the tools do not open it."},
		{filepath.Join(f.home, ".ssh", "id_ed25519"), filepath.Join(f.home, ".ssh", "id_ed25519") + " is on the credential deny-list; the tools do not open it."},
	} {
		res := run(ctx, t, read, f.h, State{}, mustInput(t, map[string]string{"path": c.path}))
		if res.Outcome != OutcomeError || !res.IsError() || text(res) != c.want || res.Meta != nil {
			t.Fatalf("%s: %s %q, want %q", c.path, res.Outcome, text(res), c.want)
		}
	}
}

func TestReadMachineFailures(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	read := builtinTool(t, NameRead)
	p := filepath.Join(f.work, "a.txt")
	put(t, p, "a\nb\n")
	for name, c := range map[string]struct {
		m    machine.Machine
		want string
	}{
		"read":      {faulty{Machine: f.h, readErr: errors.New("the disk went away")}, p + ": the disk went away."},
		"stream":    {faulty{Machine: f.h, body: failingReader{}}, p + ": the disk went away."},
		"line":      {faulty{Machine: f.h, body: &errAfter{data: []byte(strings.Repeat("a\n", 5000)), err: errors.New("the disk went away")}}, p + ": the disk went away."},
		"image":     {faulty{Machine: f.h, body: &errAfter{data: []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x01", 9000)), err: errors.New("the disk went away")}}, p + ": the disk went away."},
		"stat":      {faulty{Machine: f.h, statErr: errors.New("permission denied by policy")}, p + ": permission denied by policy."},
		"forbidden": {faulty{Machine: f.h, statErr: fmt.Errorf("stat: %w", fs.ErrPermission)}, p + " is not accessible: permission denied."},
	} {
		res := run(ctx, t, read, c.m, State{}, `{"path":"a.txt"}`)
		if res.Outcome != OutcomeError || text(res) != c.want {
			t.Fatalf("%s: %s %q", name, res.Outcome, text(res))
		}
	}
	if _, err := read.Run(ctx, Call{Input: []byte(`{"path":"a.txt"}`), Machine: faulty{Machine: f.h, closeErr: errors.New("close failed")}}); err == nil || !strings.Contains(err.Error(), "close failed") {
		t.Fatalf("a failed close is a harness error: %v", err)
	}
	if err := f.h.Release(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := read.Run(ctx, Call{Input: []byte(`{"path":"a.txt"}`), Machine: f.h}); !errors.Is(err, machine.ErrReleased) {
		t.Fatalf("a released machine is a harness error: %v", err)
	}
}

// errAfter returns data, then err.
type errAfter struct {
	data []byte
	err  error
}

func (e *errAfter) Read(p []byte) (int, error) {
	if len(e.data) == 0 {
		return 0, e.err
	}
	n := copy(p, e.data)
	e.data = e.data[n:]
	return n, nil
}

func TestCutRunes(t *testing.T) {
	if got := cutRunes("héllo", 2); got != "hé" {
		t.Fatalf("cut %q", got)
	}
	if got := cutRunes("hé", 5); got != "hé" {
		t.Fatalf("a short string %q", got)
	}
	if got := cutRunes("", 0); got != "" {
		t.Fatalf("empty %q", got)
	}
}
