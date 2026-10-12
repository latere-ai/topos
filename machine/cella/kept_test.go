// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"slices"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/cellastub"
)

// blobLog is a harness.Log that keeps blobs in memory and appends
// nothing.
type blobLog map[session.Digest][]byte

func (blobLog) Append(context.Context, []session.Event) ([]session.Event, error) { return nil, nil }

func (l blobLog) PutBlob(_ context.Context, r io.Reader) (session.Digest, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	d := session.DigestOf(b)
	l[d] = b
	return d, nil
}

func (l blobLog) Blob(_ context.Context, d session.Digest) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(l[d])), nil
}

// TestKeepReadsTheWorkspace: the image an answer names on a Cella
// machine is read through the sandbox's file routes, never the helper,
// and a path of the sandbox outside the working directory, such as the
// spill directory, is refused before anything is read (spec 055).
func TestKeepReadsTheWorkspace(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, 48, 24))); err != nil {
		t.Fatal(err)
	}
	chart := b.Bytes()
	if err := f.m.WriteFile(ctx, f.ws()+"/out/chart.png", bytes.NewReader(chart), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.m.WriteFile(ctx, f.m.SpillDir()+"/leak.png", bytes.NewReader(chart), 0o644); err != nil {
		t.Fatal(err)
	}
	files, execs := f.stub.Count(cellastub.OpFiles), f.stub.Count(cellastub.OpExec)
	msg, err := session.NewEvent(session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{
		{Type: ir.BlockText, Text: "![Chart](out/chart.png) ![Leak](" + f.m.SpillDir() + "/leak.png)"},
	}}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	log := blobLog{}
	p, ok, err := harness.KeepFiles(ctx, f.m, log, nil, msg, nil)
	if err != nil || !ok {
		t.Fatalf("KeepFiles: %t, %v", ok, err)
	}
	if len(p.Files) != 1 || p.Files[0].Resolved != f.ws()+"/out/chart.png" || p.Files[0].Width != 48 || !bytes.Equal(log[p.Files[0].Blob], chart) {
		t.Fatalf("kept %+v", p.Files)
	}
	if !slices.Equal(p.Skipped, []session.SkippedFile{{Path: f.m.SpillDir() + "/leak.png", Reason: session.KeptOutsideWorkdir}}) {
		t.Fatalf("skipped %+v", p.Skipped)
	}
	if got := f.stub.Count(cellastub.OpFiles) - files; got < 2 {
		t.Fatalf("%d file route requests for the stat and the read", got)
	}
	if got := f.stub.Count(cellastub.OpExec) - execs; got != 0 {
		t.Fatalf("the keep ran %d helper commands", got)
	}
}

// TestKeepReadsALinkedFileFromTheWorkspace: a file an answer links on a
// Cella machine, by a path and by the sandbox: prefix, is read through
// the sandbox's file routes, never the helper; a directory is no file,
// and a path of the sandbox outside the working directory is refused
// before anything is read (spec 064).
func TestKeepReadsALinkedFileFromTheWorkspace(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	report := []byte("%PDF-1.7\nthe report")
	if err := f.m.WriteFile(ctx, f.ws()+"/out/report.pdf", bytes.NewReader(report), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.m.WriteFile(ctx, f.m.SpillDir()+"/leak.pdf", bytes.NewReader(report), 0o644); err != nil {
		t.Fatal(err)
	}
	files, execs := f.stub.Count(cellastub.OpFiles), f.stub.Count(cellastub.OpExec)
	msg, err := session.NewEvent(session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{
		{Type: ir.BlockText, Text: "[Report](out/report.pdf) [Again](sandbox:" + f.ws() + "/out/report.pdf) [Folder](out) [Leak](sandbox:" + f.m.SpillDir() + "/leak.pdf)"},
	}}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	log := blobLog{}
	p, ok, err := harness.KeepFiles(ctx, f.m, log, nil, msg, func(name string, head []byte) string { return name + " " + string(head[:4]) })
	if err != nil || !ok {
		t.Fatalf("KeepFiles: %t, %v", ok, err)
	}
	want := []session.KeptFile{
		{Path: "out/report.pdf", Resolved: f.ws() + "/out/report.pdf", Blob: session.DigestOf(report), MediaType: "report.pdf %PDF", Size: int64(len(report))},
		{Path: "sandbox:" + f.ws() + "/out/report.pdf", Resolved: f.ws() + "/out/report.pdf", Blob: session.DigestOf(report), MediaType: "report.pdf %PDF", Size: int64(len(report))},
	}
	if !slices.Equal(p.Files, want) || !bytes.Equal(log[session.DigestOf(report)], report) {
		t.Fatalf("kept %+v", p.Files)
	}
	if !slices.Equal(p.Skipped, []session.SkippedFile{
		{Path: "out", Reason: session.KeptNotAFile},
		{Path: "sandbox:" + f.m.SpillDir() + "/leak.pdf", Reason: session.KeptOutsideWorkdir},
	}) {
		t.Fatalf("skipped %+v", p.Skipped)
	}
	if got := f.stub.Count(cellastub.OpFiles) - files; got < 5 {
		t.Fatalf("%d file route requests for three stats and two reads", got)
	}
	if got := f.stub.Count(cellastub.OpExec) - execs; got != 0 {
		t.Fatalf("the keep ran %d helper commands", got)
	}
}
