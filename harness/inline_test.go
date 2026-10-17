// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
)

// countedFiles is a folder of a files store in memory that counts the
// reads of each ref.
type countedFiles struct {
	*memFiles
	mu    sync.Mutex
	opens map[string]int
}

func (f *countedFiles) Open(ctx context.Context, ref string) (io.ReadCloser, int64, error) {
	f.mu.Lock()
	f.opens[ref]++
	f.mu.Unlock()
	return f.memFiles.Open(ctx, ref)
}

func (f *countedFiles) opened(ref string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens[ref]
}

// withoutMachine is a harness configuration whose files are files, on a
// machine opened on demand that fails the test if it opens, with a tool
// named note that acts on nothing, so a turn of two requests opens no
// machine.
func withoutMachine(t *testing.T, files Files) func(*Config) {
	return func(c *Config) {
		c.Files = files
		c.Machine = machine.Defer(t.Context(), machine.KindCella, func(context.Context) (machine.Machine, error) {
			t.Error("the machine opened")
			return nil, errors.New("no machine")
		})
		if err := c.Tools.AddBuiltin(&fakeTool{name: "note", props: tools.Properties{Parallel: true, Effect: tools.EffectNone}}); err != nil {
			t.Fatal(err)
		}
	}
}

// attach appends a person's message that carries the files and a
// running status, as a client and the runner do before a turn.
func (e *env) attach(ctx context.Context, text string, files ...session.Attachment) {
	e.t.Helper()
	msg, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: e.s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: text}}, Attachments: files}, t0)
	if err != nil {
		e.t.Fatal(err)
	}
	e.appendEvents(ctx, msg)
	e.running(ctx)
}

// inFolder is the record of a file of the session's folder at ref,
// marked inline, which files holds with the bytes data.
func inFolder(files *countedFiles, ref, media string, data []byte) session.Attachment {
	files.files[ref], files.media[ref] = data, media
	return session.Attachment{Name: ref[strings.LastIndex(ref, "/")+1:], MediaType: media, Size: int64(len(data)), Blob: session.DigestOf(data), Path: ref, Ref: ref, Inline: true}
}

// textOf is the text block of a request's messages that holds the text
// of the file at path, "" for none.
func textOf(req *ir.Request, path string) string {
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			if b.Type == ir.BlockText && strings.HasPrefix(b.Text, "The text of "+path+",") {
				return b.Text
			}
		}
	}
	return ""
}

// TestAShortTextFileIsReadInTheRequest: a file a person's message marks
// inline is in every request of the turn, after the line that names it,
// fenced, read from the session's folder or its blobs once for the
// harness's life, and no machine opens for it; a file without the mark, one that cannot be read, and
// one whose bytes are not its record's stay named by the line alone, the
// turn runs, and the next turn tries the unread file again (spec 079).
func TestAShortTextFileIsReadInTheRequest(t *testing.T) {
	files := &countedFiles{memFiles: newMemFiles(), opens: map[string]int{}}
	e := setup(t, withoutMachine(t, files))
	ctx := t.Context()
	csv := inFolder(files, "attachments/m1/sales.csv", "text/csv", []byte("month,total\n7,```8\n"))
	plain := inFolder(files, "attachments/m1/plain.txt", "text/plain", []byte("not marked\n"))
	plain.Inline = false
	altered := inFolder(files, "attachments/m1/altered.txt", "text/plain", []byte("first\n"))
	files.files[altered.Ref] = []byte("other\n")
	missing := session.Attachment{Name: "missing.txt", MediaType: "text/plain", Size: 3, Blob: session.DigestOf([]byte("abc")), Path: "attachments/m1/missing.txt", Ref: "attachments/m1/missing.txt", Inline: true}
	notes := []byte("Remember the tide.\n")
	d, err := e.store.PutBlob(ctx, e.s.ID, bytes.NewReader(notes))
	if err != nil {
		t.Fatal(err)
	}
	blob := session.Attachment{Name: "notes.md", MediaType: "text/markdown", Size: int64(len(notes)), Blob: d, Path: "attachments/m1/notes.md", Inline: true}
	big := session.Attachment{Name: "big.txt", MediaType: "text/plain", Size: session.InlineTextBytes + 1, Blob: session.DigestOf([]byte("big")), Path: "attachments/m1/big.txt", Inline: true}

	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_1", "note", `{"text":"x"}`)), reply(ir.StopEndTurn, text("Done.")))
	e.attach(ctx, "Sum the sales.", csv, plain, altered, missing, blob, big)
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("the turn ended %+v", out)
	}
	reqs := e.stub.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	wantCSV := "The text of attachments/m1/sales.csv, so you need not read the file to see it:\n````\nmonth,total\n7,```8\n````"
	wantNotes := "The text of attachments/m1/notes.md, so you need not read the file to see it:\n```\nRemember the tide.\n```"
	for i, r := range reqs {
		if got := textOf(r.Request, csv.Path); got != wantCSV {
			t.Fatalf("request %d carries %q for the folder's file", i, got)
		}
		if got := textOf(r.Request, blob.Path); got != wantNotes {
			t.Fatalf("request %d carries %q for the blob's file", i, got)
		}
		for _, a := range []session.Attachment{plain, altered, missing, big} {
			if got := textOf(r.Request, a.Path); got != "" {
				t.Fatalf("request %d carries the text of %s: %q", i, a.Path, got)
			}
		}
		blocks := r.Request.Messages[0].Blocks
		at := slices.IndexFunc(blocks, func(b ir.Block) bool { return strings.HasPrefix(b.Text, "Attached files, in the working directory:") })
		if at < 0 || at+2 >= len(blocks) || blocks[at+1].Text != wantCSV || blocks[at+2].Text != wantNotes {
			t.Fatalf("request %d: the texts do not follow the line that names the files: %+v", i, blocks)
		}
	}
	if n := files.opened(csv.Ref); n != 1 {
		t.Fatalf("the folder's file was read %d times in a turn of two requests", n)
	}
	if n := files.opened(missing.Ref); n != 1 {
		t.Fatalf("a file that cannot be read was tried %d times in one turn", n)
	}
	if n := files.opened(plain.Ref); n != 0 {
		t.Fatalf("a file without the mark was read %d times", n)
	}

	e.stub.Script(model, reply(ir.StopEndTurn, text("Again.")))
	e.attach(ctx, "And now?")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("the second turn ended %+v", out)
	}
	if got := textOf(e.stub.Requests()[2].Request, csv.Path); got != wantCSV {
		t.Fatalf("the next turn carries %q", got)
	}
	if n := files.opened(csv.Ref); n != 1 {
		t.Fatalf("the harness read the file again at the next turn: %d reads", n)
	}
	if n := files.opened(missing.Ref); n != 2 {
		t.Fatalf("the next turn tried the unread file %d times in all", n)
	}
}

// TestAFileReadInlineIsTheRecords: readInline takes the bytes its record
// names and nothing else: a record past the bound, a folder's file on a
// harness that reaches no files store, and a file that is not UTF-8 are
// errors (spec 079).
func TestAFileReadInlineIsTheRecords(t *testing.T) {
	ctx := t.Context()
	latin := []byte("caf\xe9")
	blobs := blobLog{session.DigestOf(latin): latin}
	for name, c := range map[string]struct {
		a     session.Attachment
		files Files
	}{
		"past the bound": {a: session.Attachment{Size: session.InlineTextBytes + 1}},
		"no files store": {a: session.Attachment{Size: 1, Ref: "attachments/x"}},
		"not UTF-8":      {a: session.Attachment{Size: int64(len(latin)), Blob: session.DigestOf(latin)}},
		"no such blob":   {a: session.Attachment{Size: 1, Blob: session.DigestOf([]byte("x"))}},
	} {
		if _, err := readInline(ctx, blobs, c.files, c.a); err == nil {
			t.Errorf("%s: read", name)
		}
	}
	if _, err := readInline(ctx, blobs, nil, session.Attachment{Size: 1, Ref: "attachments/x"}); !errors.Is(err, errNoFiles) {
		t.Fatalf("a folder's file with no files store: %v", err)
	}
	var c *texts
	c.put("sha256:x", "x")
	if _, ok := c.get("sha256:x"); ok {
		t.Fatal("a harness without texts kept one")
	}
	c = &texts{}
	c.put("sha256:a", strings.Repeat("a", maxCachedText))
	c.put("sha256:b", "b")
	if _, ok := c.get("sha256:b"); ok {
		t.Fatal("the texts kept more than their bound")
	}
}

// TestAReplayCarriesAFilesText: a replay builds a request that carried a
// file's text again with that text, read where the file is kept, and its
// hash matches the one recorded (spec 079).
func TestAReplayCarriesAFilesText(t *testing.T) {
	files := &countedFiles{memFiles: newMemFiles(), opens: map[string]int{}}
	e := setup(t, withoutMachine(t, files))
	ctx := t.Context()
	csv := inFolder(files, "attachments/m1/sales.csv", "text/csv", []byte("month,total\n7,8\n"))
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_1", "note", `{"text":"x"}`)), reply(ir.StopEndTurn, text("Done.")))
	e.attach(ctx, "Sum the sales.", csv)
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("the turn ended %+v", out)
	}
	if textOf(e.stub.Requests()[0].Request, csv.Path) == "" {
		t.Fatal("the request carried no text")
	}
	log, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	steps := e.replay(ctx, e.h, log)
	if got := outcomes(steps); len(got) != 2 || got[0] != models.ReplayMatch || got[1] != models.ReplayMatch {
		t.Fatalf("the replay %+v", steps)
	}
}
