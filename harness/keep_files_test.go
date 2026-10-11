// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// linked are the destinations refs links without showing them, in order.
func linked(refs []ref) []string {
	var out []string
	for _, r := range refs {
		if !r.Image {
			out = append(out, r.Dest)
		}
	}
	return out
}

// sparse makes a file of size bytes in dir without writing them.
func sparse(t *testing.T, dir, name string, size int64) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(p, size); err != nil {
		t.Fatal(err)
	}
}

// countingLog is a Log that stores nothing and counts the blobs it was
// handed whole. put replaces the read of a blob when set.
type countingLog struct {
	stored int
	put    func(r io.Reader) error
}

func (*countingLog) Append(context.Context, []session.Event) ([]session.Event, error) {
	return nil, nil
}

func (*countingLog) Blob(context.Context, session.Digest) (io.ReadCloser, error) {
	return nil, session.ErrNotFound
}

func (l *countingLog) PutBlob(_ context.Context, r io.Reader) (session.Digest, error) {
	if l.put != nil {
		if err := l.put(r); err != nil {
			return "", err
		}
	} else if _, err := io.Copy(io.Discard, r); err != nil {
		return "", fmt.Errorf("store: read blob: %w", err)
	}
	l.stored++
	return session.DigestOf(nil), nil
}

// oneFile is a machine whose working directory holds one file: stat
// reports size, and a read yields body, whatever size said.
type oneFile struct {
	machine.Machine
	size  int64
	body  func() io.Reader
	close error
	// read counts the bytes a read of the file handed out.
	read int64
}

func (*oneFile) Info() machine.Info { return machine.Info{Kind: machine.KindCella, Workdir: "/work"} }

func (m *oneFile) Stat(_ context.Context, p string) (machine.FileInfo, error) {
	return machine.FileInfo{Path: p, Size: m.size, Mode: 0o644}, nil
}

func (m *oneFile) ReadFile(context.Context, string) (io.ReadCloser, error) {
	return &countedBody{m: m, r: m.body()}, nil
}

type countedBody struct {
	m *oneFile
	r io.Reader
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.m.read += int64(n)
	return n, err
}

func (b *countedBody) Close() error { return b.m.close }

// zeros reads as zero bytes without end.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// TestAnAnswersLinkedFilesAreKept: an answer that links a PDF by a
// Markdown link, an archive by an a element, a file by a reference-style
// link and two by the sandbox: prefix, beside a call, appends files.kept
// after the call's result and before the next request, mapping each
// destination as the message wrote it to a blob of the session with the
// type the configured rule gives its name and leading bytes, its size in
// bytes and no width or height; the turn's status follows (spec 064).
func TestAnAnswersLinkedFilesAreKept(t *testing.T) {
	report := []byte("%PDF-1.7\n" + strings.Repeat("report ", 200))
	papers, notes, data := []byte("PK\x03\x04papers"), []byte("plain notes"), []byte("a,b\n1,2\n")
	m, work := workMachine(t, map[string][]byte{"report.pdf": report, "out/papers.zip": papers, "notes.txt": notes, "data.csv": data})
	heads := map[string][]byte{}
	e := setup(t, func(c *Config) {
		c.Machine = m
		c.MediaType = func(name string, head []byte) string {
			heads[name] = slices.Clone(head)
			return "x-test/" + name
		}
	})
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, text("The papers are ready.\n\n[Download the report](report.pdf)\n\n<a href=\"out/papers.zip\">All papers</a>\n\n"+
			"[The notes][n], [the data](sandbox:"+work+"/data.csv) and [again](sandbox:data.csv).\n\n[n]: <notes.txt> \"Notes\""),
			call("toolu_a", "echo", `{"text":"a"}`)),
		reply(ir.StopEndTurn, text("That is all.")),
	)
	e.send(ctx, "Fetch the papers.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	msgs := e.events(ctx, session.TypeAgentMessage)
	kept := keptOf(t, evs)
	p, ok := kept[msgs[0].ID]
	if len(kept) != 1 || !ok {
		t.Fatalf("%d files.kept, want the first answer's alone", len(kept))
	}
	file := func(dest, rel string, body []byte) session.KeptFile {
		return session.KeptFile{Path: dest, Resolved: work + "/" + rel, Blob: session.DigestOf(body), MediaType: "x-test/" + filepath.Base(rel), Size: int64(len(body))}
	}
	want := []session.KeptFile{
		file("report.pdf", "report.pdf", report),
		file("out/papers.zip", "out/papers.zip", papers),
		file("notes.txt", "notes.txt", notes),
		file("sandbox:"+work+"/data.csv", "data.csv", data),
		file("sandbox:data.csv", "data.csv", data),
	}
	if !slices.Equal(p.Files, want) || len(p.Skipped) != 0 {
		t.Fatalf("files.kept %+v", p)
	}
	if !bytes.Equal(heads["report.pdf"], report[:tools.SniffBytes]) || !bytes.Equal(heads["papers.zip"], papers) {
		t.Fatalf("the rule read %d bytes of the report and %q of the archive", len(heads["report.pdf"]), heads["papers.zip"])
	}
	for _, f := range p.Files {
		rc, err := e.store.Blob(ctx, e.s.ID, f.Blob)
		if err != nil {
			t.Fatalf("%s is not a blob of the session: %v", f.Path, err)
		}
		if err := rc.Close(); err != nil {
			t.Fatal(err)
		}
	}
	at := slices.IndexFunc(evs, func(e session.Event) bool { return e.Type == session.TypeFilesKept })
	// The entry of a linked file holds the seven members of an image's,
	// its width and height 0.
	var raw struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(evs[at].Payload, &raw); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range raw.Files[0] {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"blob", "height", "media_type", "path", "resolved", "size", "width"}) || raw.Files[0]["width"] != 0.0 || raw.Files[0]["height"] != 0.0 {
		t.Fatalf("a linked file's entry is %v", raw.Files[0])
	}
	got := types(evs[slices.IndexFunc(evs, func(e session.Event) bool { return e.ID == msgs[0].ID }):])
	wantOrder := []session.Type{session.TypeAgentMessage, session.TypeAgentToolUse, session.TypeToolResult, session.TypeFilesKept,
		session.TypeModelRequest, session.TypeAgentMessage, session.TypeSessionStatus}
	if !slices.Equal(got, wantOrder) {
		t.Fatalf("the step's events are %v, want %v", got, wantOrder)
	}
	if evs[at].Turn != msgs[0].Turn || evs[at].Step != msgs[0].Step {
		t.Fatalf("files.kept is turn %d step %d, its answer turn %d step %d", evs[at].Turn, evs[at].Step, msgs[0].Turn, msgs[0].Step)
	}

	// With no rule configured a linked file is typed as bytes of no known
	// type.
	bare, _, err := KeepFiles(ctx, m, e.log, answerEvent(t, "[The report](report.pdf)"), nil)
	if err != nil || len(bare.Files) != 1 || bare.Files[0].MediaType != "application/octet-stream" {
		t.Fatalf("with no rule: %+v, %v", bare, err)
	}
}

// TestAFileShownAndLinkedIsKeptOnceAsTheImage: a destination an answer
// both links and shows as an image is one image entry at its first
// mention, whichever comes first, under the image's rules: a file that is
// no image is skipped not_an_image and is not kept as a file (spec 064).
func TestAFileShownAndLinkedIsKeptOnceAsTheImage(t *testing.T) {
	chart, data := encoded(t, "png", 40, 30), []byte("a,b\n1,2\n")
	m, work := workMachine(t, map[string][]byte{"chart.png": chart, "data.csv": data, "notes.txt": []byte("notes")})
	e := setup(t, func(c *Config) { c.Machine = m })
	msg := answerEvent(t, "[![Sales](chart.png)](chart.png) and [the chart again](chart.png).",
		"[The data](data.csv), [the notes](notes.txt), and the data as a picture: ![Data](data.csv)")
	p, ok, err := KeepFiles(t.Context(), m, e.log, msg, nil)
	if err != nil || !ok {
		t.Fatalf("KeepFiles: %t, %v", ok, err)
	}
	want := []session.KeptFile{
		{Path: "chart.png", Resolved: work + "/chart.png", Blob: session.DigestOf(chart), MediaType: "image/png", Size: int64(len(chart)), Width: 40, Height: 30},
		{Path: "notes.txt", Resolved: work + "/notes.txt", Blob: session.DigestOf([]byte("notes")), MediaType: "application/octet-stream", Size: 5},
	}
	if !slices.Equal(p.Files, want) || !slices.Equal(p.Skipped, []session.SkippedFile{{Path: "data.csv", Reason: session.KeptNotAnImage}}) {
		t.Fatalf("files.kept %+v", p)
	}
}

// TestLinksAreReadAsCommonMarkReadsThem: an inline link, a
// reference-style link and the href of an a element are read once per
// destination, as the message wrote them; a link in a code span or a
// fenced block and an autolink are not; and a destination names a file of
// the machine when it has no scheme, the file: scheme or the sandbox:
// prefix before a path with no scheme of its own (spec 064).
func TestLinksAreReadAsCommonMarkReadsThem(t *testing.T) {
	src := strings.Join([]string{
		"A `[code](code.pdf)` span and [a report](report.pdf \"Title\").",
		"```\n[fenced](fenced.pdf)\n```",
		"[web](https://example.com/w.pdf) [mail](mailto:ada@example.com) <https://example.com/auto> [net](//example.com/n.pdf) [anchor](#top)",
		"[a](a.pdf) [again](a.pdf) [b](<my report.pdf>) [c][data] [d](x\\(1\\).pdf) [e](e.pdf?v=2#p3) [f](f%20g.pdf)",
		"<a href=\" spaced.zip \">zip</a> and <a name=\"x\">no address</a>",
		"<div>\n<a href=\"block.zip\">zip</a>\n</div>",
		"[s1](sandbox:/work/s.pdf) [s2](sandbox:rel.pdf) [s3](SANDBOX:up%20to.pdf?x#y) [s4](sandbox:https://example.com/x) [s5](sandbox:) [s6](file:///work/f.pdf)",
		"[data]: data.csv",
	}, "\n\n")
	refs := linked(fileRefs(src))
	want := []string{"report.pdf", "https://example.com/w.pdf", "mailto:ada@example.com", "//example.com/n.pdf", "#top",
		"a.pdf", "my report.pdf", "data.csv", "x(1).pdf", "e.pdf?v=2#p3", "f%20g.pdf", "spaced.zip", "block.zip",
		"sandbox:/work/s.pdf", "sandbox:rel.pdf", "SANDBOX:up%20to.pdf?x#y", "sandbox:https://example.com/x", "sandbox:", "file:///work/f.pdf"}
	if !slices.Equal(refs, want) {
		t.Fatalf("links %q, want %q", refs, want)
	}
	var local []string
	for _, r := range refs {
		if p, ok := localPath(r); ok {
			local = append(local, p)
		}
	}
	wantLocal := []string{"report.pdf", "a.pdf", "my report.pdf", "data.csv", "x(1).pdf", "e.pdf", "f g.pdf", "spaced.zip", "block.zip",
		"/work/s.pdf", "rel.pdf", "up to.pdf", "/work/f.pdf"}
	if !slices.Equal(local, wantLocal) {
		t.Fatalf("local paths %q, want %q", local, wantLocal)
	}
}

// TestKeptFileRefusals: a link to a path outside the working directory,
// with the sandbox: prefix or without, a symbolic link out of it, a
// missing file, a directory, a file past the bytes one file is kept at
// and a file the credential deny-list names are each skipped with their
// reason, and nothing of them is stored (spec 064).
func TestKeptFileRefusals(t *testing.T) {
	m, work := workMachine(t, map[string][]byte{"dir/x": []byte("x"), ".env": []byte("KEY=secret")})
	outside := []byte("%PDF-1.7 outside")
	if err := os.WriteFile(filepath.Join(filepath.Dir(work), "secret.pdf"), outside, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(filepath.Dir(work), "secret.pdf"), filepath.Join(work, "link.pdf")); err != nil {
		t.Fatal(err)
	}
	sparse(t, work, "big.bin", MaxKeptFileBytes+1)
	log := &countingLog{}
	msg := answerEvent(t, "[a](../secret.pdf) [b](link.pdf) [c](missing.pdf) [d](dir) [e](big.bin)",
		"[f](sandbox:/etc/hosts) [g](sandbox:../secret.pdf) <a href=\"/etc/hosts\">h</a> [i](.env)")
	p, ok, err := KeepFiles(t.Context(), m, log, msg, nil)
	if err != nil || !ok {
		t.Fatalf("KeepFiles: %t, %v", ok, err)
	}
	want := []session.SkippedFile{
		{Path: "../secret.pdf", Reason: session.KeptOutsideWorkdir}, {Path: "link.pdf", Reason: session.KeptOutsideWorkdir},
		{Path: "missing.pdf", Reason: session.KeptNotFound}, {Path: "dir", Reason: session.KeptNotAFile},
		{Path: "big.bin", Reason: session.KeptTooLarge},
		{Path: "sandbox:/etc/hosts", Reason: session.KeptOutsideWorkdir}, {Path: "sandbox:../secret.pdf", Reason: session.KeptOutsideWorkdir},
		{Path: "/etc/hosts", Reason: session.KeptOutsideWorkdir}, {Path: ".env", Reason: session.KeptUnavailable},
	}
	if len(p.Files) != 0 || !slices.Equal(p.Skipped, want) || p.Message != msg.ID {
		t.Fatalf("files.kept %+v", p)
	}
	if log.stored != 0 {
		t.Fatalf("%d refused files were stored", log.stored)
	}
}

// TestKeptFilesAreBounded: a message linking more files than MaxKeptFiles
// keeps the first ones and skips the rest limit, counted apart from the
// images it shows, which are all kept; a linked file past the bytes the
// message's linked files are kept at together is skipped limit while a
// smaller one after it is kept; and a turn with no open machine skips its
// links no_machine (spec 064).
func TestKeptFilesAreBounded(t *testing.T) {
	files := map[string][]byte{}
	var refs []string
	for i := range tools.MaxImages {
		name := fmt.Sprintf("i%02d.png", i)
		files[name] = encoded(t, "png", i+1, 1)
		refs = append(refs, "!["+name+"]("+name+")")
	}
	for i := range MaxKeptFiles + 2 {
		name := fmt.Sprintf("f%02d.txt", i)
		files[name] = fmt.Appendf(nil, "file %d", i)
		refs = append(refs, "["+name+"]("+name+")")
	}
	m, work := workMachine(t, files)
	e := setup(t, func(c *Config) { c.Machine = m })
	p, ok, err := KeepFiles(t.Context(), m, e.log, answerEvent(t, strings.Join(refs, " ")), nil)
	if err != nil || !ok {
		t.Fatalf("KeepFiles: %t, %v", ok, err)
	}
	if len(p.Files) != tools.MaxImages+MaxKeptFiles {
		t.Fatalf("kept %d files, want %d images and %d linked files", len(p.Files), tools.MaxImages, MaxKeptFiles)
	}
	last := fmt.Sprintf("f%02d.txt", MaxKeptFiles-1)
	if got := p.Files[len(p.Files)-1]; got.Path != last || got.Width != 0 {
		t.Fatalf("the last file kept is %+v, want %s", got, last)
	}
	if want := []session.SkippedFile{
		{Path: fmt.Sprintf("f%02d.txt", MaxKeptFiles), Reason: session.KeptLimit},
		{Path: fmt.Sprintf("f%02d.txt", MaxKeptFiles+1), Reason: session.KeptLimit},
	}; !slices.Equal(p.Skipped, want) {
		t.Fatalf("skipped %+v", p.Skipped)
	}

	// Two files of the bytes one file is kept at fill the message's
	// total: the third is past it, and an empty file still fits.
	for _, name := range []string{"a.bin", "b.bin", "c.bin"} {
		sparse(t, work, name, MaxKeptFileBytes)
	}
	sparse(t, work, "empty.bin", 0)
	if MaxKeptFilesBytes != 2*MaxKeptFileBytes {
		t.Fatalf("the test fills the total with two files; the bounds are %d and %d", MaxKeptFileBytes, MaxKeptFilesBytes)
	}
	log := &countingLog{}
	p, _, err = KeepFiles(t.Context(), m, log, answerEvent(t, "[a](a.bin) [b](b.bin) [c](c.bin) [f](f00.txt) [e](empty.bin)"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, f := range p.Files {
		kept = append(kept, fmt.Sprintf("%s %d", f.Path, f.Size))
	}
	if want := []string{fmt.Sprintf("a.bin %d", MaxKeptFileBytes), fmt.Sprintf("b.bin %d", MaxKeptFileBytes), "empty.bin 0"}; !slices.Equal(kept, want) || log.stored != 3 {
		t.Fatalf("kept %q, stored %d", kept, log.stored)
	}
	if want := []session.SkippedFile{{Path: "c.bin", Reason: session.KeptLimit}, {Path: "f00.txt", Reason: session.KeptLimit}}; !slices.Equal(p.Skipped, want) {
		t.Fatalf("skipped %+v", p.Skipped)
	}

	p, _, err = KeepFiles(t.Context(), nil, e.log, answerEvent(t, "[a](f00.txt) [w](https://example.com/w.pdf) ![i](i00.png)"), nil)
	if err != nil || len(p.Files) != 0 || !slices.Equal(p.Skipped, []session.SkippedFile{{Path: "f00.txt", Reason: session.KeptNoMachine}, {Path: "i00.png", Reason: session.KeptNoMachine}}) {
		t.Fatalf("with no machine: %+v, %v", p, err)
	}
}

// TestALinkedFileIsHandedToTheStoreAsItIsRead: the harness reads a
// linked file from the machine only as the store reads it, holding none
// of it ahead, so a file is in memory once, in the store (spec 064).
func TestALinkedFileIsHandedToTheStoreAsItIsRead(t *testing.T) {
	const size = 4 << 20
	m := &oneFile{size: size, body: func() io.Reader { return io.LimitReader(zeros{}, size) }}
	log := &countingLog{}
	log.put = func(r io.Reader) error {
		var one [1]byte
		if _, err := io.ReadFull(r, one[:]); err != nil {
			return err
		}
		if m.read != 1 {
			t.Errorf("the store read 1 byte and the machine had handed out %d", m.read)
		}
		_, err := io.Copy(io.Discard, r)
		return err
	}
	f, reason := keepLinked(t.Context(), m, log, "/work/data.bin", MaxKeptFilesBytes, func(name string, head []byte) string {
		return fmt.Sprintf("%s %d", name, len(head))
	})
	if reason != "" || f.Size != size || f.Resolved != "/work/data.bin" || f.MediaType != fmt.Sprintf("data.bin %d", tools.SniffBytes) || m.read != size {
		t.Fatalf("kept %+v, %q, after %d bytes", f, reason, m.read)
	}
}

// TestAFileThatGrewOrFailedIsNotStored: a file that grew past the bytes
// one file is kept at since its stat is too_large, one that grew past
// what its message may still keep is limit, and a read that fails, a
// close that fails and a store that fails are unavailable; a read that
// reports the file gone is not_found. Nothing of any of them is stored
// (spec 064).
func TestAFileThatGrewOrFailedIsNotStored(t *testing.T) {
	typeOf := func(string, []byte) string { return "x-test/any" }
	broken := errors.New("the stream broke")
	for _, c := range []struct {
		name   string
		m      *oneFile
		room   int64
		put    func(io.Reader) error
		reason string
	}{
		{name: "grew past one file's bytes", m: &oneFile{size: 10, body: func() io.Reader { return io.LimitReader(zeros{}, MaxKeptFileBytes+1) }}, room: MaxKeptFilesBytes, reason: session.KeptTooLarge},
		{name: "grew past the message's bytes", m: &oneFile{size: 10, body: func() io.Reader { return io.LimitReader(zeros{}, 101) }}, room: 100, reason: session.KeptLimit},
		{name: "a read that fails", m: &oneFile{size: 10, body: func() io.Reader { return io.MultiReader(strings.NewReader("half"), iotestErr{broken}) }}, room: MaxKeptFilesBytes, reason: session.KeptUnavailable},
		{name: "a read that finds the file gone", m: &oneFile{size: 10, body: func() io.Reader { return iotestErr{&fs.PathError{Op: "read", Path: "/work/x", Err: fs.ErrNotExist}} }}, room: MaxKeptFilesBytes, reason: session.KeptNotFound},
		{name: "a close that fails", m: &oneFile{size: 4, body: func() io.Reader { return strings.NewReader("data") }, close: broken}, room: MaxKeptFilesBytes, reason: session.KeptUnavailable},
		{name: "a store that fails", m: &oneFile{size: 4, body: func() io.Reader { return strings.NewReader("data") }}, room: MaxKeptFilesBytes,
			put: func(io.Reader) error { return broken }, reason: session.KeptUnavailable},
	} {
		log := &countingLog{put: c.put}
		f, reason := keepLinked(t.Context(), c.m, log, "/work/x", c.room, typeOf)
		if reason != c.reason || f != (session.KeptFile{}) {
			t.Errorf("%s: %+v, %q, want %q", c.name, f, reason, c.reason)
		}
		if c.name != "a close that fails" && log.stored != 0 {
			t.Errorf("%s: the file was stored", c.name)
		}
	}
	// A file at exactly what its message may still keep is kept.
	m := &oneFile{size: 100, body: func() io.Reader { return io.LimitReader(zeros{}, 100) }}
	if f, reason := keepLinked(t.Context(), m, &countingLog{}, "/work/x", 100, typeOf); reason != "" || f.Size != 100 {
		t.Fatalf("a file at the bound: %+v, %q", f, reason)
	}
}

// iotestErr is a reader that fails with err.
type iotestErr struct{ err error }

func (r iotestErr) Read([]byte) (int, error) { return 0, r.err }

// TestTheKeepIsBoundedByItsBytes: the bound on one message's reads and
// stores is the time its most bytes take at keepRate, so it follows the
// bounds of images and linked files when they change.
func TestTheKeepIsBoundedByItsBytes(t *testing.T) {
	most := int64(tools.MaxImages*tools.ReadMaxImage + MaxKeptFilesBytes)
	if got := int64(keepTimeout.Seconds()) * keepRate; got != most {
		t.Fatalf("keepTimeout is %s, which at %d bytes a second reads %d of a message's %d bytes", keepTimeout, keepRate, got, most)
	}
	if MaxKeptFileBytes > MaxKeptFilesBytes {
		t.Fatalf("one file's bound %d is past a message's %d", MaxKeptFileBytes, MaxKeptFilesBytes)
	}
}
