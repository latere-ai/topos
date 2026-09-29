// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/session"
)

// The limits of what a person's message carries (spec 015). Each image
// and each file is counted decoded; the event body, MaxEventBody, bounds
// them together in their base64 form. An image is at most what the read
// tool gives a model, the largest a provider takes.
const (
	MaxImages          = 8
	MaxImageBytes      = tools.ReadMaxImage
	MaxAttachments     = 8
	MaxAttachmentBytes = 5 << 20
	// MaxAttachmentName bounds a file's name in bytes, a file system's
	// usual limit on one path segment.
	MaxAttachmentName = 255
)

// CodeAttachmentTooLarge is an image or a file past its limit.
const CodeAttachmentTooLarge = "attachment_too_large"

// messageBody is a user.message as a client sends it: content blocks,
// text and inline images, and the files it attaches as base64 data,
// which the server stores as blobs of the session. A sender in the body
// is read and ignored, since the verified subject is the sender.
type messageBody struct {
	Sender      json.RawMessage  `json:"sender,omitempty"`
	Content     []lux.Block      `json:"content"`
	Attachments []attachmentBody `json:"attachments,omitempty"`
}

// attachmentBody is one file as a client sends it.
type attachmentBody struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data"`
}

// file is one attached file checked and decoded, before it is stored.
type file struct {
	name, media string
	data        []byte
}

// checkContent refuses content that is not text and inline images: an
// image is base64 data of PNG, JPEG, GIF or WebP whose bytes are the
// format its media type names, at most MaxImageBytes, and a message holds
// at most MaxImages of them. An image by URL is refused, since its size
// cannot be held to the limit.
func checkContent(blocks []lux.Block) error {
	images := 0
	for i, b := range blocks {
		switch b.Type {
		case ir.BlockText:
			if b.Image != nil || b.ToolUse != nil || b.ToolResult != nil || b.Opaque != nil {
				return refuse(CodeInvalidRequest, "content[%d] is a text block with fields of another type", i)
			}
		case ir.BlockImage:
			images++
			if images > MaxImages {
				return refuse(CodeInvalidRequest, "a user.message holds at most %d images", MaxImages)
			}
			img := b.Image
			switch {
			case img == nil || img.Data == "":
				return refuse(CodeInvalidRequest, "content[%d] is an image without inline data", i)
			case img.URL != "":
				return refuse(CodeInvalidRequest, "content[%d] is an image by URL; an image is sent inline, as base64 data", i)
			}
			if limit := base64.StdEncoding.EncodedLen(MaxImageBytes); len(img.Data) > limit {
				return refuse(CodeAttachmentTooLarge, "content[%d] is an image past %d bytes", i, MaxImageBytes)
			}
			data, err := base64.StdEncoding.DecodeString(img.Data)
			if err != nil {
				return refuse(CodeInvalidRequest, "content[%d]: the image's data is not base64", i)
			}
			if len(data) > MaxImageBytes {
				return refuse(CodeAttachmentTooLarge, "content[%d] is an image of %d bytes, at most %d", i, len(data), MaxImageBytes)
			}
			if media := tools.ImageType(data); media == "" || media != img.MediaType {
				return refuse(CodeInvalidRequest, "content[%d]: an image is PNG, JPEG, GIF or WebP whose media_type names its format; the data is %q and media_type says %q", i, media, img.MediaType)
			}
		default:
			return refuse(CodeInvalidRequest, "content[%d] is of type %q; a user.message holds text and image blocks", i, b.Type)
		}
	}
	return nil
}

// checkAttachments decodes a message's files: at most MaxAttachments,
// each a name that is one path segment of at most MaxAttachmentName
// bytes without a control or a bidirectional character, base64 data of
// at most MaxAttachmentBytes, and a media type, the one given or the
// one the bytes give.
func checkAttachments(in []attachmentBody) ([]file, error) {
	if len(in) > MaxAttachments {
		return nil, refuse(CodeInvalidRequest, "a user.message attaches at most %d files", MaxAttachments)
	}
	out := make([]file, 0, len(in))
	for i, a := range in {
		if err := checkName(a.Name); err != nil {
			return nil, refuse(CodeInvalidRequest, "attachments[%d]: %v", i, err)
		}
		if limit := base64.StdEncoding.EncodedLen(MaxAttachmentBytes); len(a.Data) > limit {
			return nil, refuse(CodeAttachmentTooLarge, "attachments[%d] is past %d bytes", i, MaxAttachmentBytes)
		}
		data, err := base64.StdEncoding.DecodeString(a.Data)
		if err != nil {
			return nil, refuse(CodeInvalidRequest, "attachments[%d]: the data is not base64", i)
		}
		if len(data) > MaxAttachmentBytes {
			return nil, refuse(CodeAttachmentTooLarge, "attachments[%d] is %d bytes, at most %d", i, len(data), MaxAttachmentBytes)
		}
		media := a.MediaType
		if media == "" {
			media = http.DetectContentType(data)
		} else if _, _, err := mime.ParseMediaType(media); err != nil {
			return nil, refuse(CodeInvalidRequest, "attachments[%d]: media_type %q is not a media type", i, media)
		}
		out = append(out, file{name: a.Name, media: media, data: data})
	}
	return out, nil
}

// checkName refuses a file name that is not one plain path segment.
func checkName(name string) error {
	switch {
	case name == "" || name == "." || name == "..":
		return fmt.Errorf("the name %q is not a file's", name)
	case len(name) > MaxAttachmentName:
		return fmt.Errorf("the name is %d bytes, at most %d", len(name), MaxAttachmentName)
	case !utf8.ValidString(name):
		return fmt.Errorf("the name is not UTF-8")
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("the name %q holds a path separator; a file is named by one path segment", name)
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return fmt.Errorf("the name holds a control or a bidirectional character")
		}
	}
	return nil
}

// storeAttachments stores each file as a blob of the session and names
// the path it is written at, unique among the session's files.
func (c *call) storeAttachments(ctx context.Context, s session.Session, files []file) ([]session.Attachment, error) {
	if len(files) == 0 {
		return nil, nil
	}
	evs, err := c.s.o.Sessions.Events(ctx, s.ID, 1, 0)
	if err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	for _, a := range session.Attachments(evs) {
		taken[a.Path] = true
	}
	out := make([]session.Attachment, 0, len(files))
	for _, f := range files {
		d, err := c.s.o.Sessions.PutBlob(ctx, s.ID, bytes.NewReader(f.data))
		if err != nil {
			return nil, err
		}
		out = append(out, session.Attachment{Name: f.name, MediaType: f.media, Size: int64(len(f.data)), Blob: d, Path: session.AttachmentPath(f.name, taken)})
	}
	return out, nil
}
