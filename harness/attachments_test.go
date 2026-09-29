// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/host"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// png is the smallest bytes the read tool takes for a PNG image.
var png = []byte("\x89PNG\r\n\x1a\nimage")

// imageMessage appends a message that carries a text and an image.
func (e *env) imageMessage(ctx context.Context) {
	e.t.Helper()
	msg, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: e.s.Initiator, Content: []lux.Block{
		{Type: ir.BlockText, Text: "What is in the picture?"},
		{Type: ir.BlockImage, Image: &lux.Image{MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(png)}},
	}}, t0)
	if err != nil {
		e.t.Fatal(err)
	}
	e.appendEvents(ctx, msg)
	e.running(ctx)
}

// TestAnImageReachesOnlyAModelThatTakesImages: a message's image is an
// image block of the request when the model's figures say it takes
// images, and a note that the model cannot see it otherwise; the turn
// runs either way.
func TestAnImageReachesOnlyAModelThatTakesImages(t *testing.T) {
	for _, images := range []bool{true, false} {
		e := setup(t, func(c *Config) { c.Entry.Supports.Images = images })
		ctx := t.Context()
		e.stub.Script(model, reply(ir.StopEndTurn, text("A picture.")))
		e.imageMessage(ctx)
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("images %v: outcome %+v", images, out)
		}
		reqs := e.stub.Requests()
		if len(reqs) != 1 {
			t.Fatalf("images %v: %d requests", images, len(reqs))
		}
		blocks := reqs[0].Request.Messages[0].Blocks
		var sawImage, sawNote bool
		for _, b := range blocks {
			sawImage = sawImage || b.Type == ir.BlockImage && b.Image != nil && b.Image.Data == base64.StdEncoding.EncodeToString(png)
			sawNote = sawNote || b.Type == ir.BlockText && b.Text == prompts.Text(prompts.TranscriptImageUnseen)
		}
		if sawImage != images || sawNote == images {
			t.Fatalf("images %v: the request carried an image %v and the note %v: %+v", images, sawImage, sawNote, blocks)
		}
	}
}

// blobLog is a BlobReader over a map, which fails a digest it lacks.
type blobLog map[session.Digest][]byte

func (b blobLog) Blob(_ context.Context, d session.Digest) (io.ReadCloser, error) {
	body, ok := b[d]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

// TestDeliverAttachments: the files of a log's messages are written at
// their paths once for each machine, a file that cannot be read is a
// session.error and stays pending, a skipped path is not tried, and in a
// repository the directory is excluded from git once.
func TestDeliverAttachments(t *testing.T) {
	ctx := t.Context()
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".git", "info", "exclude"), []byte("# local"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := host.Open(host.Options{Workdir: work, SpillDir: filepath.Join(t.TempDir(), "spill")})
	if err != nil {
		t.Fatal(err)
	}
	notes, csv := []byte("Remember the milk.\n"), []byte("month,total\n")
	blobs := blobLog{session.DigestOf(notes): notes, session.DigestOf(csv): csv}
	message := func(as ...session.Attachment) session.Event {
		e, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Content: []lux.Block{}, Attachments: as}, t0)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	log := []session.Event{
		message(session.Attachment{Name: "notes.txt", MediaType: "text/plain", Size: int64(len(notes)), Blob: session.DigestOf(notes), Path: "attachments/notes.txt"}),
		message(session.Attachment{Name: "gone.bin", Blob: session.DigestOf([]byte("gone")), Path: "attachments/gone.bin"},
			session.Attachment{Name: "sales.csv", MediaType: "text/csv", Size: int64(len(csv)), Blob: session.DigestOf(csv), Path: "attachments/sales.csv"}),
	}
	evs, failed, err := DeliverAttachments(ctx, m, blobs, log, nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].Type != session.TypeAttachmentsDelivered || evs[1].Type != session.TypeSessionError || !slices.Equal(failed, []string{"attachments/gone.bin"}) {
		t.Fatalf("events %+v, failed %v", evs, failed)
	}
	var d session.AttachmentsDelivered
	var se session.SessionError
	if evs[0].Decode(&d) != nil || !slices.Equal(d.Paths, []string{"attachments/notes.txt", "attachments/sales.csv"}) || evs[1].Decode(&se) != nil || se.Code != CodeAttachmentUnavailable || !se.Retryable {
		t.Fatalf("delivered %+v, error %+v", d, se)
	}
	for p, want := range map[string][]byte{"attachments/notes.txt": notes, "attachments/sales.csv": csv, ".git/info/exclude": []byte("# local\n/attachments/\n")} {
		if got, err := os.ReadFile(filepath.Join(work, p)); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s holds %q, %v; want %q", p, got, err, want)
		}
	}
	log = append(log, evs...)
	if pending := session.PendingAttachments(log); len(pending) != 1 || pending[0].Path != "attachments/gone.bin" {
		t.Fatalf("pending after the delivery: %+v", pending)
	}
	if evs, _, err := DeliverAttachments(ctx, m, blobs, log, map[string]bool{"attachments/gone.bin": true}, t0); err != nil || len(evs) != 0 {
		t.Fatalf("a skipped path was tried: %+v, %v", evs, err)
	}
	// A later machine is given every file again, and the exclude line is
	// not added twice.
	attached, err := session.NewEvent(session.TypeSessionMachine, session.SessionMachine{Machine: session.AttachedMachine{Kind: machine.KindHost}, Reason: "handoff"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	log = append(log, attached)
	if pending := session.PendingAttachments(log); len(pending) != 3 {
		t.Fatalf("a new machine has %d files pending, want 3", len(pending))
	}
	delete(blobs, session.DigestOf(csv))
	blobs[session.DigestOf([]byte("gone"))] = []byte("gone")
	if _, _, err := DeliverAttachments(ctx, m, blobs, log, nil, t0); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(work, ".git", "info", "exclude")); err != nil || strings.Count(string(got), "/attachments/") != 1 {
		t.Fatalf("exclude %q, %v", got, err)
	}
}

// TestAttachmentsOutsideARepositoryAreNotExcluded: a working directory
// that is no repository gets its files and no .git.
func TestAttachmentsOutsideARepositoryAreNotExcluded(t *testing.T) {
	work := t.TempDir()
	m, err := host.Open(host.Options{Workdir: work, SpillDir: filepath.Join(t.TempDir(), "spill")})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("x")
	msg, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Content: []lux.Block{}, Attachments: []session.Attachment{{Name: "x", Blob: session.DigestOf(body), Path: "attachments/x"}}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	evs, _, err := DeliverAttachments(t.Context(), m, blobLog{session.DigestOf(body): body}, []session.Event{msg}, nil, t0)
	if err != nil || len(evs) != 1 {
		t.Fatalf("%+v, %v", evs, err)
	}
	if _, err := os.Stat(filepath.Join(work, ".git")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a .git appeared: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "attachments", "x")); err != nil {
		t.Fatal(err)
	}
}
