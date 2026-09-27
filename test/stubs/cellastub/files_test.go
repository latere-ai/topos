// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellastub

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/cella/client"
)

// get reads a file to its end.
func get(ctx context.Context, c *client.Client, id, p string) error {
	rc, err := c.FileGet(ctx, id, p)
	if err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, rc)
	return errors.Join(err, rc.Close())
}

func stat(ctx context.Context, c *client.Client, id, p string) error {
	_, _, err := c.FileStat(ctx, id, p)
	return err
}

func list(ctx context.Context, c *client.Client, id, p string) error {
	_, _, err := c.FileList(ctx, id, p)
	return err
}

func TestFileRoutes(t *testing.T) {
	s := New(t)
	c, sb := create(t, s, "ses-files")
	id, ws := sb.Status.ID, sb.Spec.Workspace.Path
	ctx := t.Context()
	p := ws + "/a/b.txt"
	if err := c.FilePut(ctx, id, p, "0755", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	rc, err := c.FileGet(ctx, id, p)
	if err != nil {
		t.Fatal(err)
	}
	b, rerr := io.ReadAll(rc)
	if err := errors.Join(rerr, rc.Close()); err != nil || string(b) != "hello" {
		t.Errorf("get = %q %v", b, err)
	}
	e, _, err := c.FileStat(ctx, id, p)
	if err != nil || e.Path != p || e.Name != "b.txt" || e.Size != 5 || e.Mode != "0755" || e.IsDir {
		t.Errorf("stat = %+v %v", e, err)
	}
	if err := c.FileMkdir(ctx, id, ws+"/d/e"); err != nil {
		t.Fatal(err)
	}
	entries, _, err := c.FileList(ctx, id, ws)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	if err != nil || strings.Join(names, ",") != "a,d" || !entries[0].IsDir || entries[0].Size != 0 {
		t.Errorf("list = %+v %v", entries, err)
	}
	if err := c.FileMove(ctx, id, p, ws+"/moved/c.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "moved", "c.txt")); err != nil {
		t.Errorf("the moved file: %v", err)
	}
	if err := c.FileRemove(ctx, id, ws+"/moved"); err != nil {
		t.Fatal(err)
	}
	if err := c.FileRemove(ctx, id, ws+"/moved"); err != nil {
		t.Errorf("a remove of what is gone: %v", err)
	}
	if err := c.FilePut(ctx, id, ws+"/plain", "", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if e, _, err := c.FileStat(ctx, id, ws+"/plain"); err != nil || e.Mode != "0644" {
		t.Errorf("a write with no mode = %+v %v", e, err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}

	for name, c := range map[string]struct {
		err  error
		code string
	}{
		"get outside":      {get(ctx, c, id, "/etc/hosts"), "invalid_field"},
		"get unclean":      {get(ctx, c, id, ws+"/a/../plain"), "invalid_field"},
		"get missing":      {get(ctx, c, id, ws+"/gone"), "not_found"},
		"get a directory":  {get(ctx, c, id, ws+"/d"), "invalid_field"},
		"get through link": {get(ctx, c, id, ws+"/link/x"), "invalid_field"},
		"stat missing":     {stat(ctx, c, id, ws+"/gone"), "not_found"},
		"list a file":      {list(ctx, c, id, ws+"/plain"), "invalid_field"},
		"list missing":     {list(ctx, c, id, ws+"/gone"), "not_found"},
		"put the root":     {c.FilePut(ctx, id, ws, "", strings.NewReader("")), "invalid_field"},
		"put a bad mode":   {c.FilePut(ctx, id, ws+"/x", "9", strings.NewReader("")), "invalid_field"},
		"put below a file": {c.FilePut(ctx, id, ws+"/plain/x", "", strings.NewReader("")), "invalid_field"},
		"remove the root":  {c.FileRemove(ctx, id, ws), "invalid_field"},
		"mkdir the root":   {c.FileMkdir(ctx, id, ws), "invalid_field"},
		"mkdir outside":    {c.FileMkdir(ctx, id, "/elsewhere/x"), "invalid_field"},
		"mkdir below file": {c.FileMkdir(ctx, id, ws+"/plain/x"), "invalid_field"},
		"move onto a dir":  {c.FileMove(ctx, id, ws+"/plain", ws+"/d"), "invalid_field"},
		"move missing":     {c.FileMove(ctx, id, ws+"/gone", ws+"/x"), "not_found"},
		"move the root":    {c.FileMove(ctx, id, ws, ws+"/x"), "invalid_field"},
		"move below file":  {c.FileMove(ctx, id, ws+"/d", ws+"/plain/x"), "invalid_field"},
		"missing sandbox":  {stat(ctx, c, "gone", ws), "not_found"},
	} {
		if got := client.CodeOf(c.err); got != c.code {
			t.Errorf("%s: %q (%v), want %q", name, got, c.err, c.code)
		}
	}
	for name, c := range map[string]struct {
		query string
		media string
		code  int
	}{
		"both":      {"?dest=" + ws + "&path=" + ws + "/x", "application/x-tar", http.StatusBadRequest},
		"neither":   {"", "application/x-tar", http.StatusBadRequest},
		"not a tar": {"?dest=" + ws, "application/octet-stream", http.StatusUnsupportedMediaType},
		"repeated":  {"?path=" + ws + "/x&path=" + ws + "/y", "application/octet-stream", http.StatusBadRequest},
	} {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.URL()+"/v1/sandboxes/"+id+"/files"+c.query, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", c.media)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil || resp.StatusCode != c.code {
			t.Errorf("%s: %d %v, want %d", name, resp.StatusCode, err, c.code)
		}
	}
	for _, c := range []struct {
		path string
		code int
	}{
		{"/v1/sandboxes/" + id + "/files/mkdir", http.StatusBadRequest},
		{"/v1/sandboxes/gone/files/move", http.StatusNotFound},
	} {
		resp, err := http.Post(s.URL()+c.path, "application/json", strings.NewReader("{"))
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil || resp.StatusCode != c.code {
			t.Errorf("%s answers %d %v, want %d", c.path, resp.StatusCode, err, c.code)
		}
	}
	s.Stop(id)
	code(t, stat(ctx, c, id, ws), "phase_conflict")
}

// archive builds a tar of the given entries: a name ending in / is a
// directory, a name starting with @ is a symlink.
func archive(t *testing.T, entries map[string]string) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, name := range []string{"dir/", "dir/f.txt", "top.txt", "@link"} {
		body, ok := entries[name]
		if !ok {
			continue
		}
		h := &tar.Header{Name: name, Mode: 0o640, Typeflag: tar.TypeReg, Size: int64(len(body))}
		switch {
		case strings.HasSuffix(name, "/"):
			h.Typeflag, h.Mode, h.Size = tar.TypeDir, 0o755, 0
		case strings.HasPrefix(name, "@"):
			h.Name, h.Typeflag, h.Linkname, h.Size = name[1:], tar.TypeSymlink, "/etc", 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &b
}

func TestTarRoutes(t *testing.T) {
	s := New(t)
	c, sb := create(t, s, "ses-tar")
	id, ws := sb.Status.ID, sb.Spec.Workspace.Path
	ctx := t.Context()
	if err := c.ImportTar(ctx, id, ws+"/repo", archive(t, map[string]string{"dir/": "", "dir/f.txt": "inner", "top.txt": "top"})); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "repo", "dir", "f.txt")); err != nil || string(b) != "inner" {
		t.Errorf("imported = %q %v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(ws, "repo", "top.txt")); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("an imported file's mode is %v %v", fi.Mode(), err)
	}
	ts, err := c.ExportTar(ctx, id, []string{ws + "/repo"})
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(ts)
	var names []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	if err := errors.Join(ts.Err(), ts.Close()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "repo,repo/dir,repo/dir/f.txt,repo/top.txt" {
		t.Errorf("exported %q", names)
	}
	whole, err := c.ExportTar(ctx, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, whole); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(whole.Err(), whole.Close()); err != nil {
		t.Errorf("the whole workspace: %v", err)
	}

	code(t, c.ImportTar(ctx, id, ws, archive(t, map[string]string{"@link": ""})), "invalid_field")
	code(t, c.ImportTar(ctx, id, ws, strings.NewReader("not a tar archive, and long enough to be read as one header block")), "bad_request")
	code(t, c.ImportTar(ctx, id, "/elsewhere", archive(t, map[string]string{"top.txt": "x"})), "invalid_field")
	code(t, c.ImportTar(ctx, id, ws+"/repo/top.txt", archive(t, map[string]string{"top.txt": "x"})), "invalid_field")
	var bad bytes.Buffer
	tw := tar.NewWriter(&bad)
	if err := errors.Join(tw.WriteHeader(&tar.Header{Name: "../out", Typeflag: tar.TypeReg}), tw.Close()); err != nil {
		t.Fatal(err)
	}
	code(t, c.ImportTar(ctx, id, ws, &bad), "invalid_field")
	var dirOnFile bytes.Buffer
	tw = tar.NewWriter(&dirOnFile)
	if err := errors.Join(tw.WriteHeader(&tar.Header{Name: "top.txt/sub/", Typeflag: tar.TypeDir, Mode: 0o755}), tw.Close()); err != nil {
		t.Fatal(err)
	}
	code(t, c.ImportTar(ctx, id, ws+"/repo", &dirOnFile), "invalid_field")

	_, err = c.ExportTar(ctx, id, []string{ws + "/gone"})
	code(t, err, "not_found")
	_, err = c.ExportTar(ctx, id, []string{"/elsewhere"})
	code(t, err, "invalid_field")
	if err := os.Symlink("/etc", filepath.Join(ws, "repo", "link")); err != nil {
		t.Fatal(err)
	}
	ts, err = c.ExportTar(ctx, id, []string{ws + "/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, ts); err != nil {
		t.Fatal(err)
	}
	if err := ts.Err(); err == nil {
		t.Error("an archive over a symlink ended with no failure in its trailer")
	}
	if err := ts.Close(); err != nil {
		t.Fatal(err)
	}
	s.Stop(id)
	_, err = c.ExportTar(ctx, id, nil)
	code(t, err, "phase_conflict")
}
