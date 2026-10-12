// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/machine/host"
	"latere.ai/x/topos/session"
)

// memFiles is a session's folder of a files store in memory: create-only
// writes, each written whole, and a refusal a test injects.
type memFiles struct {
	mu    sync.Mutex
	files map[string][]byte
	media map[string]string
	fail  error
	// openFail fails every read.
	openFail error
	puts     int
}

func newMemFiles() *memFiles {
	return &memFiles{files: map[string][]byte{}, media: map[string]string{}}
}

func (f *memFiles) Put(_ context.Context, ref, mediaType string, size int64, r io.Reader) (session.Digest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	if f.fail != nil {
		return "", f.fail
	}
	if _, taken := f.files[ref]; taken {
		return "", ErrFileExists
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	if int64(len(b)) != size {
		return "", ErrFileSizeChanged
	}
	f.files[ref], f.media[ref] = b, mediaType
	return session.DigestOf(b), nil
}

func (f *memFiles) Open(_ context.Context, ref string) (io.ReadCloser, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.openFail != nil {
		return nil, 0, f.openFail
	}
	b, ok := f.files[ref]
	if !ok {
		return nil, 0, ErrFileNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func (f *memFiles) CopyFolder(context.Context, session.Folder, string) error {
	return errors.New("memFiles copies nothing")
}

// TestKeptFilesGoToTheFilesStore: with a files store, an answer's image
// and linked files are written at results/<turn>/<path> and named by ref
// beside their digest, none of them a blob; a linked file is held to no
// byte bound of the blob store's, so two past MaxKeptFileBytes are kept;
// a path the turn took already is kept by its ref when the file is the
// same, and skipped unavailable when it changed; a write refused for its
// size is too_large and one refused otherwise unavailable (spec 068).
func TestKeptFilesGoToTheFilesStore(t *testing.T) {
	chart := encoded(t, "png", 4, 4)
	big := bytes.Repeat([]byte("a"), MaxKeptFileBytes+1)
	bigger := bytes.Repeat([]byte("b"), MaxKeptFileBytes+2)
	csv := []byte("a,b\n1,2\n")
	m, work := workMachine(t, map[string][]byte{"chart.png": chart, "out/big.bin": big, "out/bigger.bin": bigger, "data.csv": csv})
	files := newMemFiles()
	e := setup(t, func(c *Config) { c.Machine = m })
	ctx := t.Context()
	msg := answerEvent(t, "![Chart](chart.png) [Big](out/big.bin) [Bigger](out/bigger.bin) [Data](data.csv) [Again](sandbox:"+work+"/data.csv)")
	msg.Turn = 3
	p, ok, err := KeepFiles(ctx, m, e.log, files, msg, func(name string, _ []byte) string { return "x-test/" + name })
	if err != nil || !ok {
		t.Fatalf("KeepFiles: %t, %v", ok, err)
	}
	file := func(dest, rel string, body []byte) session.KeptFile {
		return session.KeptFile{Path: dest, Resolved: work + "/" + rel, Blob: session.DigestOf(body), MediaType: "x-test/" + filepath.Base(rel), Size: int64(len(body)), Ref: "results/3/" + rel}
	}
	image := session.KeptFile{Path: "chart.png", Resolved: work + "/chart.png", Blob: session.DigestOf(chart), MediaType: "image/png", Size: int64(len(chart)), Width: 4, Height: 4, Ref: "results/3/chart.png"}
	want := []session.KeptFile{image, file("out/big.bin", "out/big.bin", big), file("out/bigger.bin", "out/bigger.bin", bigger), file("data.csv", "data.csv", csv), file("sandbox:"+work+"/data.csv", "data.csv", csv)}
	if !slices.Equal(p.Files, want) || len(p.Skipped) != 0 {
		t.Fatalf("files.kept %+v", p)
	}
	for _, f := range want {
		if !bytes.Equal(files.files[f.Ref], map[string][]byte{"results/3/chart.png": chart, "results/3/out/big.bin": big, "results/3/out/bigger.bin": bigger, "results/3/data.csv": csv}[f.Ref]) {
			t.Fatalf("the store holds %d bytes at %s", len(files.files[f.Ref]), f.Ref)
		}
		if _, err := e.store.Blob(ctx, e.s.ID, f.Blob); !errors.Is(err, session.ErrNotFound) {
			t.Fatalf("%s is a blob of the session too: %v", f.Path, err)
		}
	}
	if files.media["results/3/out/big.bin"] != "x-test/big.bin" || files.media["results/3/chart.png"] != "image/png" {
		t.Fatalf("the media types written %v", files.media)
	}

	// The keep sent again finds its own files and keeps them by ref.
	again, _, err := KeepFiles(ctx, m, e.log, files, msg, func(name string, _ []byte) string { return "x-test/" + name })
	if err != nil || !slices.Equal(again.Files, want) {
		t.Fatalf("the keep sent again: %+v, %v", again, err)
	}
	// A file that changed after an earlier answer of the turn kept it is
	// not written over.
	if err := os.WriteFile(filepath.Join(work, "data.csv"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, _, err := KeepFiles(ctx, m, e.log, files, msg, nil)
	if err != nil || !slices.Contains(changed.Skipped, session.SkippedFile{Path: "data.csv", Reason: session.KeptUnavailable}) {
		t.Fatalf("a changed file at a taken path: %+v, %v", changed, err)
	}
	if !bytes.Equal(files.files["results/3/data.csv"], csv) {
		t.Fatal("a changed file was written over the one kept")
	}

	later := answerEvent(t, "![Chart](chart.png) [Data](data.csv)")
	later.Turn = 4
	for fail, reason := range map[error]string{
		&FileError{Op: "put", Status: StatusObjectTooLarge, Code: CodeObjectTooLarge}: session.KeptTooLarge,
		&FileError{Op: "put", Status: 403, Code: "forbidden"}:                         session.KeptUnavailable,
	} {
		files.fail = fail
		p, _, err := KeepFiles(ctx, m, e.log, files, later, nil)
		want := []session.SkippedFile{{Path: "chart.png", Reason: reason}, {Path: "data.csv", Reason: reason}}
		if err != nil || len(p.Files) != 0 || !slices.Equal(p.Skipped, want) {
			t.Fatalf("a write refused %v: %+v, %v", fail, p, err)
		}
	}
	files.fail = nil
	if keepFor(0) != keepTimeout || keepFor(10*keepRate) != keepTimeout+10*time.Second {
		t.Fatalf("a file's bound: %s, %s", keepFor(0), keepFor(10*keepRate))
	}
}

// TestATurnKeepsIntoItsFilesStore: a turn whose harness has a files store
// keeps its answer's files there, its files.kept naming them by ref, and
// a message's file with a ref is delivered into the machine from there
// (spec 068).
func TestATurnKeepsIntoItsFilesStore(t *testing.T) {
	report := []byte("%PDF-1.7 report")
	m, _ := workMachine(t, map[string][]byte{"report.pdf": report})
	files := newMemFiles()
	notes := []byte("my notes")
	files.files["attachments/evt_1/notes.txt"] = notes
	e := setup(t, func(c *Config) { c.Machine = m; c.Files = files })
	ctx := t.Context()
	msg, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: session.Sender{Subject: "usr_1", Kind: session.SenderPerson},
		Content:     []lux.Block{{Type: ir.BlockText, Text: "Report on my notes."}},
		Attachments: []session.Attachment{{Name: "notes.txt", MediaType: "text/plain", Size: int64(len(notes)), Blob: session.DigestOf(notes), Path: "attachments/evt_1/notes.txt", Ref: "attachments/evt_1/notes.txt"}},
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.appendEvents(ctx, msg)
	e.stub.Script(model, reply(ir.StopEndTurn, text("[The report](report.pdf)")))
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	got, err := os.ReadFile(filepath.Join(m.Info().Workdir, "attachments", "evt_1", "notes.txt"))
	if err != nil || !bytes.Equal(got, notes) {
		t.Fatalf("the delivered file %q, %v", got, err)
	}
	evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range keptOf(t, evs) {
		if len(p.Files) != 1 || p.Files[0].Ref != "results/1/report.pdf" || !bytes.Equal(files.files["results/1/report.pdf"], report) {
			t.Fatalf("files.kept %+v", p)
		}
		return
	}
	t.Fatal("no files.kept")
}

// TestAnAttachmentWithARefIsDeliveredFromTheFolder: a file whose record
// names a ref is written into the machine from the files store; on a
// runner that reaches none, or one whose store lost it, it stays pending
// with attachment_unavailable (spec 068).
func TestAnAttachmentWithARefIsDeliveredFromTheFolder(t *testing.T) {
	work := t.TempDir()
	m, err := host.Open(host.Options{Workdir: work, SpillDir: filepath.Join(t.TempDir(), "spill")})
	if err != nil {
		t.Fatal(err)
	}
	notes := []byte("my notes")
	files := newMemFiles()
	files.files["attachments/evt_1/notes.txt"] = notes
	msg, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Content: []lux.Block{}, Attachments: []session.Attachment{
		{Name: "notes.txt", Blob: session.DigestOf(notes), Path: "attachments/evt_1/notes.txt", Ref: "attachments/evt_1/notes.txt"},
		{Name: "lost.txt", Blob: session.DigestOf([]byte("lost")), Path: "attachments/evt_1/lost.txt", Ref: "attachments/evt_1/lost.txt"},
	}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	for name, store := range map[string]Files{"a store": files, "no store": nil} {
		evs, failed, err := DeliverAttachments(t.Context(), m, blobLog{}, store, []session.Event{msg}, nil, t0)
		if err != nil {
			t.Fatal(err)
		}
		wantFailed := []string{"attachments/evt_1/lost.txt"}
		if store == nil {
			wantFailed = []string{"attachments/evt_1/notes.txt", "attachments/evt_1/lost.txt"}
		}
		if !slices.Equal(failed, wantFailed) || evs[len(evs)-1].Type != session.TypeSessionError {
			t.Fatalf("%s: failed %v, events %v", name, failed, types(evs))
		}
	}
	if got, err := os.ReadFile(filepath.Join(work, "attachments", "evt_1", "notes.txt")); err != nil || !bytes.Equal(got, notes) {
		t.Fatalf("the delivered file %q, %v", got, err)
	}
}

// TestAKeepIntoTheFilesStoreSaysWhyNot: with a files store, a link to a
// directory is not_a_file, to nothing not_found, and a taken path whose
// file cannot be read back to compare is unavailable; a refusal names its
// call, status and code.
func TestAKeepIntoTheFilesStoreSaysWhyNot(t *testing.T) {
	m, _ := workMachine(t, map[string][]byte{"dir/x": []byte("x"), "data.csv": []byte("a,b\n"), "chart.png": encoded(t, "png", 2, 2)})
	files := newMemFiles()
	e := setup(t, func(c *Config) { c.Machine = m })
	msg := answerEvent(t, "[Dir](dir) [Missing](missing.txt) [Data](data.csv) ![Chart](chart.png)")
	msg.Turn = 1
	files.files["results/1/data.csv"] = []byte("other")
	files.files["results/1/chart.png"] = []byte("other")
	files.openFail = errors.New("the store is away")
	p, _, err := KeepFiles(t.Context(), m, e.log, files, msg, nil)
	want := []session.SkippedFile{{Path: "dir", Reason: session.KeptNotAFile}, {Path: "missing.txt", Reason: session.KeptNotFound},
		{Path: "data.csv", Reason: session.KeptUnavailable}, {Path: "chart.png", Reason: session.KeptUnavailable}}
	if err != nil || !slices.Equal(p.Skipped, want) {
		t.Fatalf("files.kept %+v, %v", p, err)
	}
	if e := (&FileError{Op: "put", Status: 403, Code: "forbidden", Detail: "no"}).Error(); e != "harness: the files store's put answered 403 forbidden: no" {
		t.Fatalf("the refusal reads %q", e)
	}
}
