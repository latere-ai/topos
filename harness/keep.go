// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // DecodeConfig reads a GIF's header.
	_ "image/jpeg" // DecodeConfig reads a JPEG's header.
	_ "image/png"  // DecodeConfig reads a PNG's header.
	"io"
	"io/fs"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	mdtext "github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"golang.org/x/net/html"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// The bounds of an image an answer shows (spec 055), beside the count of
// images one message keeps, tools.MaxImages, and the bytes of one image,
// tools.ReadMaxImage, which a person's message holds to as well.
const (
	// MaxKeptImageSide is the widest and the tallest an image is kept, in
	// pixels, as its format's header gives them.
	MaxKeptImageSide = 8192
	// MaxKeptImagePixels is the most pixels an image is kept with, width
	// times height from its header, so a client never receives a picture
	// whose decoding costs more than its bytes suggest.
	MaxKeptImagePixels = 40_000_000
)

// keepTimeout bounds the reads and the stores of one message's images,
// so a machine that stops answering holds the turn no longer. The keep
// runs past a canceled step, as the step's other records are kept.
const keepTimeout = 30 * time.Second

// imageRefs are the destinations of the images the Markdown of text
// names, in order, each once: an inline or reference-style image and the
// src of an HTML img element, read with a CommonMark parser, so a
// reference in a code span or a fenced block is text and no image, and a
// link that is no image is not read.
func imageRefs(src string) []string {
	source := []byte(src)
	doc := goldmark.New().Parser().Parse(mdtext.NewReader(source))
	var out []string
	seen := map[string]bool{}
	add := func(dest string) {
		if dest != "" && !seen[dest] {
			seen[dest] = true
			out = append(out, dest)
		}
	}
	// Walk never fails: the walker returns no error.
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Image:
			// The destination as CommonMark reads it: backslash escapes
			// and character references resolved, which the parser leaves
			// to its renderer.
			add(string(util.ResolveEntityNames(util.ResolveNumericReferences(util.UnescapePunctuations(n.Destination)))))
		case *ast.RawHTML:
			var b bytes.Buffer
			for i := range n.Segments.Len() {
				s := n.Segments.At(i)
				b.Write(s.Value(source))
			}
			for _, d := range imgSources(b.Bytes()) {
				add(d)
			}
		case *ast.HTMLBlock:
			var b bytes.Buffer
			for i := range n.Lines().Len() {
				s := n.Lines().At(i)
				b.Write(s.Value(source))
			}
			if n.HasClosure() {
				b.Write(n.ClosureLine.Value(source))
			}
			for _, d := range imgSources(b.Bytes()) {
				add(d)
			}
		}
		return ast.WalkContinue, nil
	})
	return out
}

// imgSources are the src attributes of the img elements of an HTML
// fragment, entities decoded.
func imgSources(fragment []byte) []string {
	var out []string
	z := html.NewTokenizer(bytes.NewReader(fragment))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return out
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			if tok.Data != "img" {
				continue
			}
			for _, a := range tok.Attr {
				if a.Namespace == "" && a.Key == "src" {
					// A src is a URL with its surrounding white space
					// stripped, as a browser reads it.
					out = append(out, strings.TrimSpace(a.Val))
					break
				}
			}
		}
	}
}

// localPath is the machine path a destination names, false for one that
// names no file of the machine: a destination with a scheme other than
// file, a network-path reference, and one that is empty once its query
// and fragment are dropped. A file: URL names its path, and any other
// destination its percent-decoded path.
func localPath(dest string) (string, bool) {
	if strings.HasPrefix(dest, "//") {
		return "", false
	}
	if scheme, _, ok := strings.Cut(dest, ":"); ok && isScheme(scheme) {
		if !strings.EqualFold(scheme, "file") {
			return "", false
		}
		u, err := url.Parse(dest)
		if err != nil || (u.Host != "" && u.Host != "localhost") || u.Path == "" {
			return "", false
		}
		return u.Path, true
	}
	p := dest
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if d, err := url.PathUnescape(p); err == nil {
		p = d
	}
	return p, p != ""
}

// isScheme reports whether s is a URI scheme: a letter, then letters,
// digits, "+", "-" and ".".
func isScheme(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && (r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.'):
		default:
			return false
		}
	}
	return true
}

// within reports whether p is dir or below it; both are clean.
func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// messageText is the text of an agent.message, its text blocks joined as
// paragraphs, so a reference's definition is found in any block of the
// same message.
func messageText(m session.AgentMessage) string {
	var parts []string
	for _, b := range m.Message.Blocks {
		if b.Type == ir.BlockText && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// KeepImages reads the local images msg, an agent.message, names from m,
// the session's machine, and stores each as a blob through log, the
// first tools.MaxImages references only. It returns the payload of the
// files.kept that records them, and false when msg names no local image.
// m is nil for a turn that opened no machine: every reference is then
// skipped no_machine and nothing is read, since keeping an image never
// opens one. A reference is resolved against m's working directory and
// read through m, which refuses a symbolic link that leads out of the
// root it is in.
func KeepImages(ctx context.Context, m machine.Machine, log Log, msg session.Event) (session.FilesKept, bool, error) {
	var am session.AgentMessage
	if err := msg.Decode(&am); err != nil {
		return session.FilesKept{}, false, err
	}
	kept := session.FilesKept{Message: msg.ID, Files: []session.KeptFile{}, Skipped: []session.SkippedFile{}}
	n := 0
	for _, dest := range imageRefs(messageText(am)) {
		p, ok := localPath(dest)
		if !ok {
			continue
		}
		n++
		skip := func(reason string) {
			kept.Skipped = append(kept.Skipped, session.SkippedFile{Path: dest, Reason: reason})
		}
		switch {
		case n > tools.MaxImages:
			skip(session.KeptLimit)
			continue
		case m == nil:
			skip(session.KeptNoMachine)
			continue
		}
		workdir := path.Clean(m.Info().Workdir)
		resolved := path.Clean(p)
		if !path.IsAbs(resolved) {
			resolved = path.Join(workdir, resolved)
		}
		if !within(resolved, workdir) {
			skip(session.KeptOutsideWorkdir)
			continue
		}
		f, reason := keepOne(ctx, m, log, resolved)
		if reason != "" {
			skip(reason)
			continue
		}
		f.Path = dest
		kept.Files = append(kept.Files, f)
	}
	return kept, len(kept.Files)+len(kept.Skipped) > 0, nil
}

// keepOne reads the image at resolved and stores it, or says why not.
func keepOne(ctx context.Context, m machine.Machine, log Log, resolved string) (session.KeptFile, string) {
	fi, err := m.Stat(ctx, resolved)
	if err != nil {
		return session.KeptFile{}, readReason(err)
	}
	switch {
	case fi.IsDir:
		return session.KeptFile{}, session.KeptNotAnImage
	case fi.Size > tools.ReadMaxImage:
		return session.KeptFile{}, session.KeptTooLarge
	}
	rc, err := m.ReadFile(ctx, resolved)
	if err != nil {
		return session.KeptFile{}, readReason(err)
	}
	data, err := io.ReadAll(io.LimitReader(rc, tools.ReadMaxImage+1))
	if err = errors.Join(err, rc.Close()); err != nil {
		return session.KeptFile{}, readReason(err)
	}
	if len(data) > tools.ReadMaxImage {
		return session.KeptFile{}, session.KeptTooLarge
	}
	media := tools.ImageType(data)
	if media == "" {
		return session.KeptFile{}, session.KeptNotAnImage
	}
	w, h, err := imageSize(media, data)
	if err != nil {
		return session.KeptFile{}, session.KeptNotAnImage
	}
	if w > MaxKeptImageSide || h > MaxKeptImageSide || int64(w)*int64(h) > MaxKeptImagePixels {
		return session.KeptFile{}, session.KeptTooLarge
	}
	d, err := log.PutBlob(ctx, bytes.NewReader(data))
	if err != nil {
		return session.KeptFile{}, session.KeptUnavailable
	}
	return session.KeptFile{Resolved: resolved, Blob: d, MediaType: media, Size: int64(len(data)), Width: w, Height: h}, ""
}

// readReason is the reason a failed stat or read skips a reference.
func readReason(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return session.KeptNotFound
	case errors.Is(err, machine.ErrOutside):
		return session.KeptOutsideWorkdir
	}
	return session.KeptUnavailable
}

// imageSize is the width and height an image's header gives, read
// without decoding its pixels.
func imageSize(media string, data []byte) (int, int, error) {
	if media == "image/webp" {
		return webpSize(data)
	}
	c, format, err := image.DecodeConfig(bytes.NewReader(data))
	switch {
	case err != nil:
		return 0, 0, err
	case "image/"+format != media:
		return 0, 0, fmt.Errorf("harness: the header is %s where the bytes begin as %s", format, media)
	case c.Width <= 0 || c.Height <= 0:
		return 0, 0, fmt.Errorf("harness: a %s of %dx%d", media, c.Width, c.Height)
	}
	return c.Width, c.Height, nil
}

// webpSize reads a WebP's canvas from its first chunk: VP8X's canvas,
// VP8L's image size, or the key frame of VP8.
func webpSize(b []byte) (int, int, error) {
	if len(b) < 30 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return 0, 0, errors.New("harness: a WebP too short for its header")
	}
	chunk := b[20:]
	switch string(b[12:16]) {
	case "VP8X":
		w := int(chunk[4]) | int(chunk[5])<<8 | int(chunk[6])<<16
		h := int(chunk[7]) | int(chunk[8])<<8 | int(chunk[9])<<16
		return w + 1, h + 1, nil
	case "VP8L":
		if chunk[0] != 0x2f {
			return 0, 0, errors.New("harness: a VP8L chunk without its signature")
		}
		bits := binary.LittleEndian.Uint32(chunk[1:5])
		return int(bits&0x3fff) + 1, int(bits>>14&0x3fff) + 1, nil
	case "VP8 ":
		if chunk[3] != 0x9d || chunk[4] != 0x01 || chunk[5] != 0x2a {
			return 0, 0, errors.New("harness: a VP8 chunk without a key frame")
		}
		w := int(binary.LittleEndian.Uint16(chunk[6:8]) & 0x3fff)
		h := int(binary.LittleEndian.Uint16(chunk[8:10]) & 0x3fff)
		if w == 0 || h == 0 {
			return 0, 0, errors.New("harness: a VP8 frame of no size")
		}
		return w, h, nil
	}
	return 0, 0, fmt.Errorf("harness: a WebP whose first chunk is %q", b[12:16])
}

// openMachine is the turn's machine when it is open, nil when it is a
// machine opened on demand that no call of the turn opened.
func (t *turn) openMachine() machine.Machine {
	m := t.h.c.Machine
	if d, ok := m.(*machine.Deferred); ok {
		if o := d.Opened(); o != nil {
			return o
		}
		return nil
	}
	return m
}

// redactedSince reports whether the log the turn holds records a
// redaction of the event id: the turn's own copy of an event a person
// redacted while the step ran keeps its content, and the event.redacted
// that another writer appended is what tells it.
func redactedSince(evs []session.Event, id string) bool {
	for _, e := range evs {
		var p session.EventRedacted
		if e.Type == session.TypeEventRedacted && e.Decode(&p) == nil && p.EventID == id {
			return true
		}
	}
	return false
}

// keepAnswer appends the files.kept of msg, an agent.message of the
// session's own thread whose step is committed, when it names a local
// image. It reads only a machine that is open: a machine opened on
// demand that no call of the turn opened is not opened to keep an image.
// An answer a person redacted while its step ran keeps nothing.
func (t *turn) keepAnswer(ctx context.Context, msg session.Event) error {
	if t.thread != "" || redactedSince(t.events(), msg.ID) {
		return nil
	}
	return t.keep(ctx, t.openMachine(), msg)
}

// keep reads msg's images from m, nil for none, and appends their
// files.kept.
func (t *turn) keep(ctx context.Context, m machine.Machine, msg session.Event) error {
	kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), keepTimeout)
	defer cancel()
	p, ok, err := KeepImages(kctx, m, t.l, msg)
	if err != nil || !ok {
		return err
	}
	e, err := session.NewEvent(session.TypeFilesKept, p, t.h.c.Clock())
	if err != nil {
		return err
	}
	e.Turn, e.Step = msg.Turn, msg.Step
	return t.commit(ctx, e)
}

// keepResumed keeps the images of the step whose open calls a resume
// settled, the agent.message last before the first of them: the step is
// committed once its calls are answered, which for a call that waited on
// a person is in this claim. A message that is redacted, or that a
// files.kept names already, a redacted one included, keeps nothing
// again. A claim that settled the calls without opening the machine
// reads nothing and records nothing, since the step did run on a machine
// and its images may be in it.
func (t *turn) keepResumed(ctx context.Context, open []pendingCall) error {
	if t.thread != "" || len(open) == 0 {
		return nil
	}
	evs := t.events()
	var msg *session.Event
	for i := range evs {
		e := evs[i]
		if e.Seq >= open[0].seq {
			break
		}
		if e.Type == session.TypeAgentMessage && e.Thread == "" {
			msg = &evs[i]
		}
	}
	m := t.openMachine()
	if msg == nil || m == nil || msg.Redacted() || redactedSince(evs, msg.ID) || session.KeptFor(msg.ID, evs) {
		return nil
	}
	return t.keep(ctx, m, *msg)
}
