// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/session"
)

var pngBytes = "\x89PNG\r\n\x1a\nimage"

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// sendMessage sends a user.message payload as alice.
func (f *fixture) sendMessage(id, payload string) answer {
	f.t.Helper()
	return f.do(http.MethodPost, "/v1/sessions/"+id+"/events", "alice", `{"type":"user.message","payload":`+payload+`}`)
}

// TestAMessageCarriesImagesAndFiles: an inline image stays in the
// message's content, and each file is stored as a blob of the session
// and named by a path under attachments/ in the directory of its
// message's event id, with its size and media type, and without its data
// in the log.
func TestAMessageCarriesImagesAndFiles(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	first := f.sendMessage(s.ID, `{"content":[{"type":"text","text":"Why does March dip?"},{"type":"image","image":{"media_type":"image/png","data":"`+b64(pngBytes)+`"}}],`+
		`"attachments":[{"name":"sales.csv","data":"`+b64("month,total\n")+`"},{"name":"notes","media_type":"text/markdown","data":"`+b64("# notes\n")+`"}]}`)
	if first.status != http.StatusOK {
		t.Fatalf("send: %d %s", first.status, first.body)
	}
	if strings.Contains(string(first.body), b64("month,total\n")) {
		t.Fatal("the answer carries a file's data")
	}
	var ev session.Event
	first.decode(t, &ev)
	var p session.UserMessage
	if err := ev.Decode(&p); err != nil {
		t.Fatal(err)
	}
	if len(p.Content) != 2 || p.Content[1].Type != ir.BlockImage || p.Content[1].Image.Data != b64(pngBytes) {
		t.Fatalf("content %+v", p.Content)
	}
	want := []session.Attachment{
		{Name: "sales.csv", MediaType: "text/plain; charset=utf-8", Size: 12, Blob: session.DigestOf([]byte("month,total\n")), Path: "attachments/" + ev.ID + "/sales.csv"},
		{Name: "notes", MediaType: "text/markdown", Size: 8, Blob: session.DigestOf([]byte("# notes\n")), Path: "attachments/" + ev.ID + "/notes"},
	}
	if len(p.Attachments) != 2 || p.Attachments[0] != want[0] || p.Attachments[1] != want[1] {
		t.Fatalf("attachments %+v, want %+v", p.Attachments, want)
	}
	if a := f.do(http.MethodGet, "/v1/sessions/"+s.ID+"/blobs/"+string(want[0].Blob), "alice", ""); a.status != http.StatusOK || string(a.body) != "month,total\n" {
		t.Fatalf("the file's blob: %d %q", a.status, a.body)
	}
	// A second message's file of a name the first holds is written in the
	// second message's own directory, so neither overwrites the other.
	firstPath := p.Attachments[0].Path
	second := f.sendMessage(s.ID, `{"content":[],"attachments":[{"name":"sales.csv","data":"`+b64("month,total\nmarch,3\n")+`"}]}`)
	if second.status != http.StatusOK {
		t.Fatalf("second send: %d %s", second.status, second.body)
	}
	second.decode(t, &ev)
	if err := ev.Decode(&p); err != nil || len(p.Attachments) != 1 || p.Attachments[0].Path != "attachments/"+ev.ID+"/sales.csv" || p.Attachments[0].Path == firstPath {
		t.Fatalf("the second message's path %+v, %v; the first's is %s", p.Attachments, err, firstPath)
	}
}

// TestAMessagesLimits: an image or a file past its limit is
// attachment_too_large, and more images or files than a message holds,
// two files of one name, a name that is no plain file name, data that is not base64, an image by
// URL, an image whose bytes are not its media type, and a block that is
// neither text nor an image are invalid_request; none reaches the log.
func TestAMessagesLimits(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	before, err := f.sessions.Get(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	image := func(media, data string) string {
		return `{"type":"image","image":{"media_type":"` + media + `","data":"` + data + `"}}`
	}
	files := func(n int) string {
		var out []string
		for i := range n {
			out = append(out, `{"name":"a`+strconv.Itoa(i)+`.txt","data":"`+b64("a")+`"}`)
		}
		return strings.Join(out, ",")
	}
	images := func(n int) string {
		var out []string
		for range n {
			out = append(out, image("image/png", b64(pngBytes)))
		}
		return strings.Join(out, ",")
	}
	for name, c := range map[string]struct {
		payload string
		code    string
	}{
		"an image past its limit":    {`{"content":[` + image("image/png", b64(pngBytes+strings.Repeat("\x00", MaxImageBytes))) + `]}`, CodeAttachmentTooLarge},
		"a file past its limit":      {`{"content":[],"attachments":[{"name":"big.bin","data":"` + b64(strings.Repeat("\x00", MaxAttachmentBytes+1)) + `"}]}`, CodeAttachmentTooLarge},
		"too many images":            {`{"content":[` + images(MaxImages+1) + `]}`, CodeInvalidRequest},
		"too many files":             {`{"content":[],"attachments":[` + files(MaxAttachments+1) + `]}`, CodeInvalidRequest},
		"a name with a slash":        {`{"content":[],"attachments":[{"name":"../x","data":"` + b64("a") + `"}]}`, CodeInvalidRequest},
		"a name with a line break":   {`{"content":[],"attachments":[{"name":"a\nb","data":"` + b64("a") + `"}]}`, CodeInvalidRequest},
		"a bidirectional name":       {`{"content":[],"attachments":[{"name":"a\u202eb","data":"` + b64("a") + `"}]}`, CodeInvalidRequest},
		"a long name":                {`{"content":[],"attachments":[{"name":"` + strings.Repeat("a", MaxAttachmentName+1) + `","data":"` + b64("a") + `"}]}`, CodeInvalidRequest},
		"two files of one name":      {`{"content":[],"attachments":[{"name":"a.txt","data":"` + b64("a") + `"},{"name":"a.txt","data":"` + b64("b") + `"}]}`, CodeInvalidRequest},
		"a dot name":                 {`{"content":[],"attachments":[{"name":"..","data":"` + b64("a") + `"}]}`, CodeInvalidRequest},
		"a bad media type":           {`{"content":[],"attachments":[{"name":"a","media_type":"not a type","data":"` + b64("a") + `"}]}`, CodeInvalidRequest},
		"a file not base64":          {`{"content":[],"attachments":[{"name":"a","data":"%%%"}]}`, CodeInvalidRequest},
		"an image not base64":        {`{"content":[` + image("image/png", "%%%") + `]}`, CodeInvalidRequest},
		"an image by URL":            {`{"content":[{"type":"image","image":{"url":"https://img.example/a.png","data":"x"}}]}`, CodeInvalidRequest},
		"an image without data":      {`{"content":[{"type":"image","image":{"media_type":"image/png"}}]}`, CodeInvalidRequest},
		"an image of another type":   {`{"content":[` + image("image/jpeg", b64(pngBytes)) + `]}`, CodeInvalidRequest},
		"not an image":               {`{"content":[` + image("image/png", b64("plain text")) + `]}`, CodeInvalidRequest},
		"a tool_use block":           {`{"content":[{"type":"tool_use","tool_use":{"id":"t","name":"bash"}}]}`, CodeInvalidRequest},
		"a text block with an image": {`{"content":[{"type":"text","text":"x","image":{"data":"x"}}]}`, CodeInvalidRequest},
		"nothing":                    {`{"content":[]}`, CodeInvalidRequest},
	} {
		a := f.sendMessage(s.ID, c.payload)
		if a.code() != c.code {
			t.Errorf("%s: %d %s, want %s", name, a.status, a.body, c.code)
		}
		if c.code == CodeAttachmentTooLarge && a.status != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: status %d", name, a.status)
		}
	}
	after, err := f.sessions.Get(t.Context(), s.ID)
	if err != nil || after.LastSeq != before.LastSeq {
		t.Fatalf("a refused message reached the log: %d from %d, %v", after.LastSeq, before.LastSeq, err)
	}
	// The same limits hold at the edge: an image and a file of exactly
	// their limit are taken.
	a := f.sendMessage(s.ID, `{"content":[`+image("image/png", b64(pngBytes+strings.Repeat("\x00", MaxImageBytes-len(pngBytes))))+`]}`)
	if a.status != http.StatusOK {
		t.Fatalf("an image at its limit: %d %.200s", a.status, a.body)
	}
	a = f.sendMessage(s.ID, `{"content":[],"attachments":[{"name":"edge.bin","data":"`+b64(strings.Repeat("\x00", MaxAttachmentBytes))+`"}]}`)
	var ev session.Event
	if a.status != http.StatusOK || json.Unmarshal(a.body, &ev) != nil {
		t.Fatalf("a file at its limit: %d %.200s", a.status, a.body)
	}
}
