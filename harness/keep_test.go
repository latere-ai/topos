// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	pngenc "image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/host"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// encoded is an image of w by h in format, png, jpeg or gif.
func encoded(t *testing.T, format string, w, h int) []byte {
	t.Helper()
	img := image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{color.White, color.Black})
	var b bytes.Buffer
	var err error
	switch format {
	case "png":
		err = pngenc.Encode(&b, img)
	case "jpeg":
		err = jpeg.Encode(&b, img, nil)
	case "gif":
		err = gif.Encode(&b, img, nil)
	default:
		t.Fatalf("no encoder for %s", format)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// pngHeader is the signature and IHDR chunk of a PNG of w by h, all a
// header read needs, whatever pixels would follow.
func pngHeader(w, h uint32) []byte {
	ihdr := binary.BigEndian.AppendUint32([]byte("IHDR"), w)
	ihdr = binary.BigEndian.AppendUint32(ihdr, h)
	ihdr = append(ihdr, 8, 0, 0, 0, 0)
	b := append([]byte("\x89PNG\r\n\x1a\n"), 0, 0, 0, 13)
	b = append(b, ihdr...)
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(ihdr))
}

// webp is a WebP whose first chunk is fourcc with body, padded past the
// bytes a header read takes.
func webp(fourcc string, body ...byte) []byte {
	b := append([]byte("RIFF\x00\x00\x00\x00WEBP"+fourcc), 0, 0, 0, 0)
	b = append(b, body...)
	return append(b, make([]byte, 16)...)
}

// workMachine is a host machine whose working directory holds files,
// each name's bytes.
func workMachine(t *testing.T, files map[string][]byte) (*host.Host, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(base, "work")
	for name, b := range files {
		p := filepath.Join(work, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	m, err := host.Open(host.Options{Workdir: work, SpillDir: filepath.Join(base, "spill"), Environ: []string{"PATH=" + os.Getenv("PATH")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Release(context.Background(), true); err != nil {
			t.Error(err)
		}
	})
	return m, m.Info().Workdir
}

// keptOf is the payload of each files.kept of the log, by the id of the
// message it names.
func keptOf(t *testing.T, evs []session.Event) map[string]session.FilesKept {
	t.Helper()
	out := map[string]session.FilesKept{}
	for _, e := range evs {
		if e.Type != session.TypeFilesKept {
			continue
		}
		var p session.FilesKept
		if err := e.Decode(&p); err != nil {
			t.Fatal(err)
		}
		if _, twice := out[p.Message]; twice {
			t.Fatalf("two files.kept name %s", p.Message)
		}
		out[p.Message] = p
	}
	return out
}

// types are the types of evs, in order.
func types(evs []session.Event) []session.Type {
	var out []session.Type
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

// answerEvent is an agent.message with the text blocks texts.
func answerEvent(t *testing.T, texts ...string) session.Event {
	t.Helper()
	var blocks []lux.Block
	for _, s := range texts {
		blocks = append(blocks, lux.Block{Type: ir.BlockText, Text: s})
	}
	e, err := session.NewEvent(session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: blocks}, StopReason: ir.StopEndTurn}, t0)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// TestAnAnswersImageIsKept: an answer that names a PNG by Markdown and a
// GIF by an img element, beside a call, appends files.kept after the
// call's result and before the next request, mapping each name as the
// message wrote it to a blob of the session with the type its bytes are
// and the size its header gives; the turn's status follows (spec 055).
func TestAnAnswersImageIsKept(t *testing.T) {
	chart, logo := encoded(t, "png", 1200, 800), encoded(t, "gif", 64, 32)
	m, work := workMachine(t, map[string][]byte{"chart.png": chart, "out/logo.gif": logo})
	e := setup(t, func(c *Config) { c.Machine = m })
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, text("March dips because two stores closed for a week.\n\n![Monthly sales](chart.png)\n\n<img src=\"./out/logo.gif\" alt=\"Logo\">"), call("toolu_a", "echo", `{"text":"a"}`)),
		reply(ir.StopEndTurn, text("That is all.")),
	)
	e.send(ctx, "Chart the sales.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	msgs := e.events(ctx, session.TypeAgentMessage)
	kept := keptOf(t, evs)
	if len(kept) != 1 {
		t.Fatalf("%d files.kept, want the first answer's alone", len(kept))
	}
	p, ok := kept[msgs[0].ID]
	if !ok {
		t.Fatal("no files.kept names the answer with the images")
	}
	want := []session.KeptFile{
		{Path: "chart.png", Resolved: work + "/chart.png", Blob: session.DigestOf(chart), MediaType: "image/png", Size: int64(len(chart)), Width: 1200, Height: 800},
		{Path: "./out/logo.gif", Resolved: work + "/out/logo.gif", Blob: session.DigestOf(logo), MediaType: "image/gif", Size: int64(len(logo)), Width: 64, Height: 32},
	}
	if !slices.Equal(p.Files, want) || len(p.Skipped) != 0 {
		t.Fatalf("files.kept %+v", p)
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
	got := types(evs[slices.IndexFunc(evs, func(e session.Event) bool { return e.ID == msgs[0].ID }):])
	wantOrder := []session.Type{session.TypeAgentMessage, session.TypeAgentToolUse, session.TypeToolResult, session.TypeFilesKept,
		session.TypeModelRequest, session.TypeAgentMessage, session.TypeSessionStatus}
	if !slices.Equal(got, wantOrder) {
		t.Fatalf("the step's events are %v, want %v", got, wantOrder)
	}
	if evs[at].Turn != msgs[0].Turn || evs[at].Step != msgs[0].Step || evs[at].Thread != "" {
		t.Fatalf("files.kept is turn %d step %d thread %q, its answer turn %d step %d", evs[at].Turn, evs[at].Step, evs[at].Thread, msgs[0].Turn, msgs[0].Step)
	}
	tr, err := session.Fold(evs, "")
	if err != nil || tr.Check() != nil {
		t.Fatalf("the log does not fold: %v", err)
	}
}

// TestAnImageDrawnInTheSameStepIsKept: an answer that names a chart its
// own step's command draws keeps it, since the keep runs once the step's
// calls have; an answer with no call keeps its images before the turn's
// status (spec 055).
func TestAnImageDrawnInTheSameStepIsKept(t *testing.T) {
	m, _ := workMachine(t, map[string][]byte{"notes.txt": []byte("plan")})
	chart := encoded(t, "png", 320, 200)
	e := setup(t, func(c *Config) {
		c.Machine = m
		c.Policy = Policy{AlwaysAllow: []string{"bash"}}
	})
	ctx := t.Context()
	e.write.run = func(c tools.Call) (tools.Result, error) {
		if err := c.Machine.WriteFile(ctx, "chart.png", bytes.NewReader(chart), 0o644); err != nil {
			return tools.Result{}, err
		}
		return tools.Text(tools.OutcomeOK, "drew chart.png"), nil
	}
	e.stub.Script(model,
		reply(ir.StopToolUse, text("Here is the chart.\n\n![Sales](chart.png)"), call("toolu_draw", "bash", `{"command":"python plot.py"}`)),
		reply(ir.StopEndTurn, text("Again: ![Sales](chart.png)")),
	)
	e.send(ctx, "Chart it.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	msgs := e.events(ctx, session.TypeAgentMessage)
	kept := keptOf(t, evs)
	for _, msg := range msgs {
		p := kept[msg.ID]
		if len(p.Files) != 1 || p.Files[0].Blob != session.DigestOf(chart) || p.Files[0].Width != 320 || len(p.Skipped) != 0 {
			t.Fatalf("the answer %s keeps %+v", msg.ID, p)
		}
	}
	if n := len(evs); evs[n-1].Type != session.TypeSessionStatus || evs[n-2].Type != session.TypeFilesKept {
		t.Fatalf("the turn ends %v", types(evs[n-3:]))
	}
}

// TestOnlyImageReferencesAreRead: a reference in a code span or a fenced
// block, a link that is no image, an image by a URL of another scheme and
// one that names no path keep nothing and record nothing; an image
// reference of every other form is read, once per destination, as the
// message wrote it (spec 055).
func TestOnlyImageReferencesAreRead(t *testing.T) {
	src := strings.Join([]string{
		"Here: `![code](code.png)` and [the report](report.pdf).",
		"```\n![fenced](fenced.png)\n```",
		"![web](https://example.com/w.png) ![inline](data:image/png;base64,AAAA) ![net](//example.com/n.png) ![anchor](#top)",
		"![a](a.png) ![again](a.png) ![b](<my chart.png> \"Title\") ![c][chart]",
		"<img alt=\"d\" src=\"d.png\"> ![e](e.png?v=2#x) ![f](file:///work/f.png) ![g](g%20h.png)",
		"<p>\n<img src=\"block.png\">\n</p>",
		"[chart]: c.png",
	}, "\n\n")
	refs := imageRefs(src)
	want := []string{"https://example.com/w.png", "data:image/png;base64,AAAA", "//example.com/n.png", "#top", "a.png", "my chart.png", "c.png", "d.png", "e.png?v=2#x", "file:///work/f.png", "g%20h.png", "block.png"}
	if !slices.Equal(refs, want) {
		t.Fatalf("references %q, want %q", refs, want)
	}
	var local []string
	for _, r := range refs {
		if p, ok := localPath(r); ok {
			local = append(local, p)
		}
	}
	if wantLocal := []string{"a.png", "my chart.png", "c.png", "d.png", "e.png", "/work/f.png", "g h.png", "block.png"}; !slices.Equal(local, wantLocal) {
		t.Fatalf("local paths %q, want %q", local, wantLocal)
	}
	if _, ok := localPath("file://example.com/x.png"); ok {
		t.Fatal("a file URL of another host is a path of the machine")
	}

	// A message whose references are none of the machine's appends
	// nothing and reads nothing: the fake machine fails any read.
	e := setup(t, nil)
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopEndTurn, text("See `![x](x.png)`, [y](y.png) and ![z](https://example.com/z.png).")))
	e.send(ctx, "Show me.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if got := e.events(ctx, session.TypeFilesKept); len(got) != 0 {
		t.Fatalf("files.kept for no local image: %s", got[0].Payload)
	}
}

// TestKeptImageRefusals: a path outside the working directory, a
// symbolic link out of it, a missing file, a directory, an SVG, text
// under a .png name, a file past the bytes an image is kept at, a header
// past the side and one past the pixels are each skipped with their
// reason, and nothing of them is stored; a refused reference counts
// toward the images one message reads (spec 055).
func TestKeptImageRefusals(t *testing.T) {
	outside := encoded(t, "png", 4, 4)
	big := append(pngHeader(10, 10), make([]byte, tools.ReadMaxImage)...)
	m, work := workMachine(t, map[string][]byte{
		"drawing.svg": []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"fake.png":    []byte("this is no image"),
		"big.png":     big,
		"wide.png":    pngHeader(MaxKeptImageSide+1, 10),
		"pixels.png":  pngHeader(7000, 7000),
		"dir/x":       []byte("x"),
	})
	if err := os.WriteFile(filepath.Join(filepath.Dir(work), "secret.png"), outside, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(filepath.Dir(work), "secret.png"), filepath.Join(work, "link.png")); err != nil {
		t.Fatal(err)
	}
	e := setup(t, func(c *Config) { c.Machine = m })
	msg := answerEvent(t, "![a](../secret.png) ![b](link.png) ![c](missing.png) ![d](dir) ![e](drawing.svg) ![f](fake.png)",
		"![g](big.png) ![h](wide.png)\n\n<img src=\"pixels.png\">")
	p, ok, err := KeepImages(t.Context(), m, e.log, msg)
	if err != nil || !ok {
		t.Fatalf("KeepImages: %t, %v", ok, err)
	}
	want := []session.SkippedFile{
		{Path: "../secret.png", Reason: session.KeptOutsideWorkdir}, {Path: "link.png", Reason: session.KeptOutsideWorkdir},
		{Path: "missing.png", Reason: session.KeptNotFound}, {Path: "dir", Reason: session.KeptNotAnImage},
		{Path: "drawing.svg", Reason: session.KeptNotAnImage}, {Path: "fake.png", Reason: session.KeptNotAnImage},
		{Path: "big.png", Reason: session.KeptTooLarge}, {Path: "wide.png", Reason: session.KeptTooLarge},
		{Path: "pixels.png", Reason: session.KeptLimit},
	}
	if len(p.Files) != 0 || !slices.Equal(p.Skipped, want) || p.Message != msg.ID {
		t.Fatalf("files.kept %+v", p)
	}
	msg = answerEvent(t, "<img src=\"pixels.png\"> ![](/etc/hosts)")
	if p, _, err = KeepImages(t.Context(), m, e.log, msg); err != nil {
		t.Fatal(err)
	}
	if want := []session.SkippedFile{{Path: "pixels.png", Reason: session.KeptTooLarge}, {Path: "/etc/hosts", Reason: session.KeptOutsideWorkdir}}; !slices.Equal(p.Skipped, want) {
		t.Fatalf("skipped %+v", p.Skipped)
	}
	if _, err := e.store.Blob(t.Context(), e.s.ID, session.DigestOf(outside)); err == nil {
		t.Fatal("the image the link leads to was stored")
	}
}

// TestKeptImagesAreBounded: a message naming more images than
// tools.MaxImages keeps the first ones and skips the rest limit, reading
// none of them; a turn with no open machine skips its references
// no_machine (spec 055).
func TestKeptImagesAreBounded(t *testing.T) {
	files := map[string][]byte{}
	var refs []string
	for i := range tools.MaxImages + 2 {
		name := string(rune('a'+i)) + ".png"
		files[name] = encoded(t, "png", i+1, 1)
		refs = append(refs, "!["+name+"]("+name+")")
	}
	m, _ := workMachine(t, files)
	e := setup(t, func(c *Config) { c.Machine = m })
	msg := answerEvent(t, strings.Join(refs, " "))
	p, ok, err := KeepImages(t.Context(), m, e.log, msg)
	if err != nil || !ok {
		t.Fatalf("KeepImages: %t, %v", ok, err)
	}
	if len(p.Files) != tools.MaxImages || len(p.Skipped) != 2 {
		t.Fatalf("kept %d, skipped %+v", len(p.Files), p.Skipped)
	}
	for i, f := range p.Files {
		if f.Width != i+1 {
			t.Fatalf("kept %d is %s", i, f.Path)
		}
	}
	for _, s := range p.Skipped {
		if s.Reason != session.KeptLimit {
			t.Fatalf("skipped %+v", s)
		}
	}
	p, _, err = KeepImages(t.Context(), nil, e.log, answerEvent(t, "![a](a.png) ![w](https://example.com/w.png)"))
	if err != nil || len(p.Files) != 0 || !slices.Equal(p.Skipped, []session.SkippedFile{{Path: "a.png", Reason: session.KeptNoMachine}}) {
		t.Fatalf("with no machine: %+v, %v", p, err)
	}
}

// TestImageSizesFromHeaders: the size of each kept format is read from
// its header, and a header that does not hold one is no image.
func TestImageSizesFromHeaders(t *testing.T) {
	for _, c := range []struct {
		name string
		data []byte
		w, h int
	}{
		{"png", encoded(t, "png", 31, 17), 31, 17},
		{"jpeg", encoded(t, "jpeg", 33, 9), 33, 9},
		{"gif", encoded(t, "gif", 5, 12), 5, 12},
		{"vp8x", webp("VP8X", 0x10, 0, 0, 0, 0x7f, 0x02, 0x00, 0xdf, 0x01, 0x00), 640, 480},
		{"vp8l", webp("VP8L", 0x2f, 0xff, 0xc4, 0xc7, 0x00), 1280, 800},
		{"vp8", webp("VP8 ", 0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0x00, 0x05, 0x20, 0x03), 1280, 800},
	} {
		media := tools.ImageType(c.data)
		w, h, err := imageSize(media, c.data)
		if err != nil || w != c.w || h != c.h {
			t.Errorf("%s (%s): %dx%d, %v; want %dx%d", c.name, media, w, h, err, c.w, c.h)
		}
	}
	for name, data := range map[string][]byte{
		"a png with no IHDR":   []byte("\x89PNG\r\n\x1a\nnothing"),
		"a short webp":         []byte("RIFF\x00\x00\x00\x00WEBPVP8X"),
		"an unknown chunk":     webp("ALPH", 1, 2, 3),
		"a vp8l without mark":  webp("VP8L", 0x00, 0, 0, 0, 0),
		"a vp8 without frame":  webp("VP8 ", 0, 0, 0, 0, 0, 0, 0, 0, 0, 0),
		"a gif named png":      append([]byte("\x89PNG\r\n\x1a\n"), encoded(t, "gif", 2, 2)...),
		"a jpeg with no frame": []byte("\xff\xd8\xff\xd9"),
	} {
		if w, h, err := imageSize(tools.ImageType(data), data); err == nil {
			t.Errorf("%s reads as %dx%d", name, w, h)
		}
	}
}

// TestAWaitingStepKeepsOnceItsCallsAreAnswered: a step whose call waits
// for a confirmation keeps nothing while it waits, and its images are
// kept in the claim that runs the confirmed call, once (spec 055).
func TestAWaitingStepKeepsOnceItsCallsAreAnswered(t *testing.T) {
	m, _ := workMachine(t, nil)
	chart := encoded(t, "png", 40, 30)
	e := setup(t, func(c *Config) { c.Machine = m })
	ctx := t.Context()
	e.write.run = func(c tools.Call) (tools.Result, error) {
		if err := c.Machine.WriteFile(ctx, "chart.png", bytes.NewReader(chart), 0o644); err != nil {
			return tools.Result{}, err
		}
		return tools.Text(tools.OutcomeOK, "drew"), nil
	}
	e.stub.Script(model,
		reply(ir.StopToolUse, text("![Sales](chart.png)"), call("toolu_draw", "bash", `{"command":"plot"}`)),
		luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("Done.")}, StopReason: ir.StopEndTurn}},
	)
	e.send(ctx, "Chart it.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v", out)
	}
	if got := e.events(ctx, session.TypeFilesKept); len(got) != 0 {
		t.Fatal("a step waiting on a confirmation kept its images")
	}
	conf, err := session.NewEvent(session.TypeUserToolConfirmation, session.UserToolConfirmation{Sender: e.s.Initiator, ToolUseID: "toolu_draw", Decision: session.DecisionAllow}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.appendEvents(ctx, conf)
	e.running(ctx)
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	got := e.events(ctx, session.TypeFilesKept)
	var p session.FilesKept
	if len(got) != 1 || got[0].Decode(&p) != nil || len(p.Files) != 1 || p.Files[0].Blob != session.DigestOf(chart) {
		t.Fatalf("files.kept after the confirmation: %d", len(got))
	}
}

// TestAnAnswerRedactedWhileItsStepRunsKeepsNothing: a person who redacts
// an answer while its step's calls run leaves no files.kept for it, and
// its image is never stored (spec 055).
func TestAnAnswerRedactedWhileItsStepRunsKeepsNothing(t *testing.T) {
	chart := encoded(t, "png", 8, 8)
	m, _ := workMachine(t, map[string][]byte{"chart.png": chart})
	e := setup(t, func(c *Config) {
		c.Machine = m
		c.Policy = Policy{AlwaysAllow: []string{"bash"}}
	})
	ctx := t.Context()
	e.write.run = func(tools.Call) (tools.Result, error) {
		msgs := e.events(ctx, session.TypeAgentMessage)
		if err := e.store.Redact(ctx, e.s.ID, msgs[len(msgs)-1].ID, e.s.Initiator, "the wrong chart"); err != nil {
			return tools.Result{}, err
		}
		return tools.Text(tools.OutcomeOK, "ran"), nil
	}
	e.stub.Script(model,
		reply(ir.StopToolUse, text("![Sales](chart.png)"), call("toolu_a", "bash", `{"command":"plot"}`)),
		reply(ir.StopEndTurn, text("Done.")),
	)
	e.send(ctx, "Chart it.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if got := e.events(ctx, session.TypeFilesKept); len(got) != 0 {
		t.Fatalf("a redacted answer kept its images: %s", got[0].Payload)
	}
	if _, err := e.store.Blob(ctx, e.s.ID, session.DigestOf(chart)); err == nil {
		t.Fatal("the redacted answer's image was stored")
	}
}

// TestAResumedStepKeepsOnlyItsOwnAnswerOnce: a resume whose step's
// answer was redacted keeps nothing, never an earlier answer in its
// place; an answer whose files.kept a person redacted is not kept again;
// and a resume that opened no machine records nothing, so no reference
// is marked no_machine for a step that ran on one (spec 055).
func TestAResumedStepKeepsOnlyItsOwnAnswerOnce(t *testing.T) {
	chart := encoded(t, "png", 8, 8)
	m, _ := workMachine(t, map[string][]byte{"chart.png": chart})
	for _, c := range []struct {
		name    string
		prepare func(e *env, earlier, waiting session.Event)
		machine func(e *env) machine.Machine
	}{
		{"redacted answer", func(e *env, _, waiting session.Event) {
			if err := e.store.Redact(t.Context(), e.s.ID, waiting.ID, e.s.Initiator, ""); err != nil {
				t.Fatal(err)
			}
		}, nil},
		{"redacted files.kept", func(e *env, _, waiting session.Event) {
			p, _, err := KeepImages(t.Context(), m, e.log, waiting)
			if err != nil {
				t.Fatal(err)
			}
			kept, err := session.NewEvent(session.TypeFilesKept, p, t0)
			if err != nil {
				t.Fatal(err)
			}
			e.appendEvents(t.Context(), kept)
			if err := e.store.Redact(t.Context(), e.s.ID, kept.ID, e.s.Initiator, ""); err != nil {
				t.Fatal(err)
			}
		}, nil},
		{"no machine opened", func(*env, session.Event, session.Event) {}, func(e *env) machine.Machine {
			return machine.Defer(t.Context(), machine.KindHost, func(context.Context) (machine.Machine, error) {
				t.Error("the resume opened a machine")
				return m, nil
			})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t, func(cfg *Config) { cfg.Machine = m })
			ctx := t.Context()
			earlier := answerEvent(t, "Before: ![Old](chart.png)")
			use, err := session.NewEvent(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: "toolu_w", Name: "bash", Input: []byte(`{"command":"plot"}`), Verdict: string(VerdictAsk)}, t0)
			if err != nil {
				t.Fatal(err)
			}
			waiting := answerEvent(t, "![Sales](chart.png)")
			e.send(ctx, "Chart it.")
			e.appendEvents(ctx, earlier)
			e.appendEvents(ctx, waiting, use)
			idle, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopToolConfirmation}, t0)
			if err != nil {
				t.Fatal(err)
			}
			e.appendEvents(ctx, idle)
			c.prepare(e, earlier, waiting)
			deny, err := session.NewEvent(session.TypeUserToolConfirmation, session.UserToolConfirmation{Sender: e.s.Initiator, ToolUseID: "toolu_w", Decision: session.DecisionDeny}, t0)
			if err != nil {
				t.Fatal(err)
			}
			e.appendEvents(ctx, deny)
			e.running(ctx)
			if c.machine != nil {
				cfg := e.cfg
				cfg.Machine = c.machine(e)
				h, err := New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				e.h = h
			}
			before := len(e.events(ctx, session.TypeFilesKept))
			// The resume keeps before the step's next request, so what the
			// model answers that request does not matter here.
			e.stub.Script(model, reply(ir.StopEndTurn, text("Fine.")))
			e.turn(ctx)
			if got := e.events(ctx, session.TypeFilesKept); len(got) != before {
				t.Fatalf("the resume appended %d files.kept: %s", len(got)-before, got[len(got)-1].Payload)
			}
		})
	}
}

// TestAResumeStoppedForSpendKeepsItsStep: a resume whose confirmed call
// a core refuses for spend keeps the step's images before the turn's
// closing status, as a step stopped for spend does (spec 055).
func TestAResumeStoppedForSpendKeepsItsStep(t *testing.T) {
	chart := encoded(t, "png", 8, 8)
	m, _ := workMachine(t, map[string][]byte{"chart.png": chart})
	e := setup(t, func(c *Config) { c.Machine = m })
	ctx := t.Context()
	e.write.run = func(tools.Call) (tools.Result, error) {
		return tools.Result{}, &models.SpendError{Core: machine.KindCella, Code: "budget_exhausted", Err: errors.New("the sandbox allowance is spent")}
	}
	e.stub.Script(model, reply(ir.StopToolUse, text("![Sales](chart.png)"), call("toolu_w", "bash", `{"command":"plot"}`)))
	e.send(ctx, "Chart it.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v", out)
	}
	conf, err := session.NewEvent(session.TypeUserToolConfirmation, session.UserToolConfirmation{Sender: e.s.Initiator, ToolUseID: "toolu_w", Decision: session.DecisionAllow}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.appendEvents(ctx, conf)
	e.running(ctx)
	if out := e.turn(ctx); out.StopReason != session.StopBudget {
		t.Fatalf("outcome %+v", out)
	}
	evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	kept := keptOf(t, evs)
	msgs := e.events(ctx, session.TypeAgentMessage)
	if p := kept[msgs[0].ID]; len(p.Files) != 1 || p.Files[0].Blob != session.DigestOf(chart) {
		t.Fatalf("files.kept %+v", kept)
	}
	if evs[len(evs)-1].Type != session.TypeSessionStatus {
		t.Fatalf("the turn ends with %s", evs[len(evs)-1].Type)
	}
}

// TestADestinationIsReadAsCommonMarkReadsIt: an inline image's
// destination has its backslash escapes and character references
// resolved, and an img's src its surrounding white space stripped, so
// the file the message names is the one read (spec 055).
func TestADestinationIsReadAsCommonMarkReadsIt(t *testing.T) {
	got := imageRefs(`![a](a\(b\).png) ![b](my\_chart.png) ![c](x&amp;y.png) ![d](&#x41;.png) <img src=" spaced.png ">`)
	if want := []string{"a(b).png", "my_chart.png", "x&y.png", "A.png", "spaced.png"}; !slices.Equal(got, want) {
		t.Fatalf("references %q, want %q", got, want)
	}
	png := encoded(t, "png", 3, 3)
	m, _ := workMachine(t, map[string][]byte{"a(b).png": png})
	e := setup(t, func(c *Config) { c.Machine = m })
	p, _, err := KeepImages(t.Context(), m, e.log, answerEvent(t, `![a](a\(b\).png)`))
	if err != nil || len(p.Files) != 1 || p.Files[0].Path != "a(b).png" {
		t.Fatalf("files.kept %+v, %v", p, err)
	}
}
