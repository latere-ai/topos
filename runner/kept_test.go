// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/session"
)

// pngOf is a PNG of w by h.
func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestFilesKeptBeforeTheTurnCloses: a drive whose answer names the chart
// its own step's command draws, on a machine opened on demand by that
// command, appends files.kept after the command's result and before the
// turn's closing status, and the chart is a blob of the session (spec
// 055).
func TestFilesKeptBeforeTheTurnCloses(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	var opens atomic.Int32
	f.onDemand(f.work, &opens)
	chart := pngOf(t, 120, 80)
	src := filepath.Join(t.TempDir(), "drawn.png")
	if err := os.WriteFile(src, chart, 0o644); err != nil {
		t.Fatal(err)
	}
	f.stub.Script(model,
		reply(ir.Block{Type: ir.BlockText, Text: "Sales dip in March.\n\n![Monthly sales](out/chart.png)"},
			toolUse("toolu_1", "bash", `{"command":"mkdir -p out && cp '`+src+`' out/chart.png"}`)),
		reply(ir.Block{Type: ir.BlockText, Text: "Done."}),
	)
	f.message(ctx, "Chart the sales.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	evs, err := f.store.Events(ctx, f.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	at := slices.IndexFunc(evs, func(e session.Event) bool { return e.Type == session.TypeFilesKept })
	result := slices.IndexFunc(evs, func(e session.Event) bool { return e.Type == session.TypeToolResult })
	closing := len(evs) - 1
	if at < 0 || result < 0 || at < result || evs[closing].Type != session.TypeSessionStatus || at > closing {
		t.Fatalf("files.kept at %d, the result at %d, the closing status at %d", at, result, closing)
	}
	var p session.FilesKept
	if err := evs[at].Decode(&p); err != nil {
		t.Fatal(err)
	}
	if len(p.Files) != 1 || p.Files[0].Path != "out/chart.png" || p.Files[0].Resolved != filepath.ToSlash(filepath.Join(f.work, "out", "chart.png")) ||
		p.Files[0].Blob != session.DigestOf(chart) || p.Files[0].Width != 120 || p.Files[0].Height != 80 || len(p.Skipped) != 0 {
		t.Fatalf("files.kept %+v", p)
	}
	rc, err := f.store.Blob(ctx, f.s.ID, p.Files[0].Blob)
	if err != nil {
		t.Fatalf("the chart is no blob of the session: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestKeepingOpensNoMachine: an answer that names an image in a turn
// that opened no machine opens none to keep it: its reference is
// skipped no_machine, nothing is stored, and the machine's open count
// stays zero (spec 055).
func TestKeepingOpensNoMachine(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	write(t, filepath.Join(f.work, "chart.png"), string(pngOf(t, 10, 10)))
	var opens atomic.Int32
	f.onDemandWith(f.work, &opens, demand{noTools: true})
	f.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: "As before: ![Sales](chart.png)"}))
	f.message(ctx, "Show the chart again.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 0 || f.count(ctx, session.TypeSessionMachine) != 0 {
		t.Fatalf("keeping an image opened %d machines", opens.Load())
	}
	kept := f.events(ctx, session.TypeFilesKept)
	var p session.FilesKept
	if len(kept) != 1 || kept[0].Decode(&p) != nil || len(p.Files) != 0 ||
		!slices.Equal(p.Skipped, []session.SkippedFile{{Path: "chart.png", Reason: session.KeptNoMachine}}) {
		t.Fatalf("files.kept %d: %+v", len(kept), p)
	}
}
