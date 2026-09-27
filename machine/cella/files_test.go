// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"latere.ai/x/cella/client"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/test/stubs/cellastub"
)

func read(t *testing.T, m *Machine, p string) (string, error) {
	t.Helper()
	rc, err := m.ReadFile(t.Context(), p)
	if err != nil {
		return "", err
	}
	b, rerr := io.ReadAll(rc)
	return string(b), errors.Join(rerr, rc.Close())
}

func names(infos []machine.FileInfo) string {
	var out []string
	for _, fi := range infos {
		out = append(out, filepath.Base(fi.Path))
	}
	return strings.Join(out, ",")
}

// fileOps runs every file operation under dir, a directory in one of the
// machine's roots, and checks what the host machine would answer.
func fileOps(t *testing.T, m *Machine, dir string) {
	t.Helper()
	ctx := t.Context()
	p := dir + "/a/b.txt"
	if err := m.WriteFile(ctx, p, strings.NewReader("hello"), 0); err != nil {
		t.Fatal(err)
	}
	if got, err := read(t, m, p); err != nil || got != "hello" {
		t.Errorf("read = %q %v", got, err)
	}
	fi, err := m.Stat(ctx, p)
	if err != nil || fi.Path != p || fi.Size != 5 || fi.IsDir || fi.Mode.Perm()&0o600 != 0o600 {
		t.Errorf("stat = %+v %v", fi, err)
	}
	if fi, err := m.Stat(ctx, dir+"/a"); err != nil || !fi.IsDir || !fi.Mode.IsDir() {
		t.Errorf("stat of a directory = %+v %v", fi, err)
	}
	script := dir + "/run.sh"
	if err := m.WriteFile(ctx, script, strings.NewReader("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteFile(ctx, script, bytes.NewReader([]byte("#!/bin/sh\necho\n")), 0); err != nil {
		t.Fatal(err)
	}
	if fi, err := m.Stat(ctx, script); err != nil || fi.Mode.Perm() != 0o755 {
		t.Errorf("a write with mode 0 keeps the mode: %v %v", fi.Mode, err)
	}
	if err := m.WriteFile(ctx, dir+"/stream.txt", io.MultiReader(strings.NewReader("not "), strings.NewReader("seekable")), 0); err != nil {
		t.Fatal(err)
	}
	if got, err := read(t, m, dir+"/stream.txt"); err != nil || got != "not seekable" {
		t.Errorf("a reader that does not seek = %q %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	infos, err := m.List(ctx, dir)
	if err != nil || names(infos) != "a,run.sh,stream.txt" {
		t.Errorf("list = %s %v: sorted, without .env", names(infos), err)
	}
	if err := m.Rename(ctx, p, dir+"/c.txt"); err != nil {
		t.Fatal(err)
	}
	if got, err := read(t, m, dir+"/c.txt"); err != nil || got != "hello" {
		t.Errorf("the renamed file = %q %v", got, err)
	}
	if err := m.Remove(ctx, dir+"/a"); err != nil {
		t.Errorf("remove an empty directory: %v", err)
	}
	if err := m.Remove(ctx, dir+"/c.txt"); err != nil {
		t.Errorf("remove: %v", err)
	}
	if err := m.WriteFile(ctx, dir+"/full/x", strings.NewReader("x"), 0); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		err  error
		want error
	}{
		"read missing":      {second(read(t, m, dir+"/gone")), fs.ErrNotExist},
		"stat missing":      {statErr(t, m, dir+"/gone"), fs.ErrNotExist},
		"list missing":      {second(m.List(ctx, dir+"/gone")), fs.ErrNotExist},
		"remove missing":    {m.Remove(ctx, dir+"/gone"), fs.ErrNotExist},
		"rename missing":    {m.Rename(ctx, dir+"/gone", dir+"/other"), fs.ErrNotExist},
		"remove non-empty":  {m.Remove(ctx, dir+"/full"), syscall.ENOTEMPTY},
		"read denied":       {second(read(t, m, dir+"/.env")), machine.ErrDenied},
		"write denied":      {m.WriteFile(ctx, dir+"/id_rsa", strings.NewReader(""), 0), machine.ErrDenied},
		"stat denied":       {statErr(t, m, dir+"/key.pem"), machine.ErrDenied},
		"rename to denied":  {m.Rename(ctx, dir+"/run.sh", dir+"/.env.local"), machine.ErrDenied},
		"read outside":      {second(read(t, m, "/elsewhere/x")), machine.ErrOutside},
		"list outside":      {second(m.List(ctx, "/")), machine.ErrOutside},
		"remove outside":    {m.Remove(ctx, "/elsewhere"), machine.ErrOutside},
		"rename outside":    {m.Rename(ctx, "/elsewhere", dir+"/x"), machine.ErrOutside},
		"rename to outside": {m.Rename(ctx, dir+"/run.sh", "/elsewhere"), machine.ErrOutside},
	} {
		if !errors.Is(c.err, c.want) {
			t.Errorf("%s: %v, want %v", name, c.err, c.want)
		}
	}
	if err := m.Remove(ctx, dir); err == nil || !strings.Contains(err.Error(), "is a root") {
		t.Errorf("remove a root: %v", err)
	}
}

func second[T any](_ T, err error) error { return err }

func TestFilesInTheWorkspace(t *testing.T) {
	f := open(t)
	sessions := f.stub.Count(cellastub.OpSession)
	fileOps(t, f.m, f.ws())
	if f.stub.Count(cellastub.OpSession) != sessions {
		t.Error("a workspace file went through the helper rather than Cella's file routes")
	}
	if f.stub.Count(cellastub.OpFiles) == 0 {
		t.Error("no file route was called")
	}
	ctx := t.Context()
	if err := f.m.WriteFile(ctx, "rel/x.txt", strings.NewReader("rel"), 0); err != nil {
		t.Fatal(err)
	}
	if got, err := read(t, f.m, filepath.Join(f.ws(), "rel", "x.txt")); err != nil || got != "rel" {
		t.Errorf("a relative path resolves against the working directory: %q %v", got, err)
	}
	if err := f.m.WriteFile(ctx, f.ws(), strings.NewReader(""), 0); err == nil {
		t.Error("the working directory was written as a file")
	}
	if err := f.m.Remove(ctx, f.ws()); err == nil || !strings.Contains(err.Error(), "is a root") {
		t.Errorf("remove the working directory: %v", err)
	}
	if err := f.m.Rename(ctx, "rel/x.txt", f.m.SpillDir()+"/x.txt"); err == nil || !strings.Contains(err.Error(), "different roots") {
		t.Errorf("a rename out of the workspace: %v", err)
	}
	if _, err := read(t, f.m, "rel"); err == nil {
		t.Error("a directory was read as a file")
	}
	f.stub.Fail(cellastub.OpFiles, cellastub.Failure{Status: 403, Code: "forbidden"})
	if _, err := f.m.Stat(ctx, "rel"); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("a forbidden stat: %v", err)
	}
	f.stub.Fail(cellastub.OpFiles, cellastub.Failure{Status: 503, Code: "driver_unavailable"})
	if err := f.m.WriteFile(ctx, "rel/x.txt", strings.NewReader("x"), 0o644); err == nil || !strings.Contains(err.Error(), "driver_unavailable") {
		t.Errorf("a write Cella could not do: %v", err)
	}
	f.stub.Fail(cellastub.OpFiles, cellastub.Failure{Status: 503, Code: "driver_unavailable"})
	if err := f.m.WriteFile(ctx, "rel/new.txt", strings.NewReader("x"), 0); err == nil {
		t.Error("a write whose stat failed went ahead")
	}
	if err := f.m.WriteFile(ctx, "rel/y.txt", failingReader{}, 0); err == nil {
		t.Error("a write of a failing reader went ahead")
	}
}

func TestFilesInTheSpillDirectory(t *testing.T) {
	f := open(t)
	if err := os.MkdirAll(f.m.SpillDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	files := f.stub.Count(cellastub.OpFiles)
	fileOps(t, f.m, f.m.SpillDir())
	if f.stub.Count(cellastub.OpFiles) != files {
		t.Error("a spill file went through Cella's file routes, which serve the workspace alone")
	}
	ctx := t.Context()
	if err := f.m.Rename(ctx, f.m.SpillDir()+"/run.sh", "run.sh"); err == nil || !strings.Contains(err.Error(), "different roots") {
		t.Errorf("a rename into the workspace: %v", err)
	}
	big := strings.Repeat("spill ", 40<<10)
	if err := f.m.WriteFile(ctx, f.m.SpillDir()+"/big.txt", strings.NewReader(big), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := read(t, f.m, f.m.SpillDir()+"/big.txt"); err != nil || got != big {
		t.Errorf("a large spill file came back as %d bytes, want %d: %v", len(got), len(big), err)
	}
	if _, err := read(t, f.m, f.m.SpillDir()); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("a directory read as a file: %v", err)
	}
	if err := f.m.WriteFile(ctx, f.m.SpillDir(), strings.NewReader(""), 0); err == nil {
		t.Error("the spill directory was written as a file")
	}
	f.stub.Fail(cellastub.OpSession, cellastub.Failure{Status: 503, Code: "driver_unavailable"})
	if err := f.m.WriteFile(ctx, f.m.SpillDir()+"/x", strings.NewReader(""), 0); err == nil {
		t.Error("a write Cella could not start was reported done")
	}
	f.stub.Fail(cellastub.OpSession, cellastub.Failure{Status: 503, Code: "driver_unavailable"})
	if _, err := f.m.ReadFile(ctx, f.m.SpillDir()+"/big.txt"); err == nil {
		t.Error("a read Cella could not start was reported open")
	}
	f.stub.Fail(cellastub.OpExec, cellastub.Failure{Status: 503, Code: "driver_unavailable"})
	if _, err := f.m.Stat(ctx, f.m.SpillDir()); err == nil {
		t.Error("a stat Cella could not run was answered")
	}
}

func TestTar(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for name, body := range map[string]string{"repo/main.go": "package main\n", "repo/README": "read me\n"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	f.stub.Stop(f.m.Info().ID)
	if err := f.m.ImportTar(ctx, ".", &b); err != nil {
		t.Fatal(err)
	}
	if got, err := read(t, f.m, "repo/main.go"); err != nil || got != "package main\n" {
		t.Errorf("imported = %q %v", got, err)
	}
	rc, err := f.m.ExportTar(ctx, []string{"repo"})
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(rc)
	var got []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, h.Name)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if strings.Join(got, ",") != "repo,repo/README,repo/main.go" {
		t.Errorf("exported %q", got)
	}
	if err := os.Symlink("/", filepath.Join(f.ws(), "repo", "link")); err != nil {
		t.Fatal(err)
	}
	rc, err = f.m.ExportTar(ctx, []string{"repo"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, rc); err == nil || !strings.Contains(err.Error(), "the archive") {
		t.Errorf("an archive whose transfer failed ended with %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"import to the spill":  f.m.ImportTar(ctx, f.m.SpillDir(), strings.NewReader("")),
		"import outside":       f.m.ImportTar(ctx, "/elsewhere", strings.NewReader("")),
		"export the spill":     second(f.m.ExportTar(ctx, []string{f.m.SpillDir()})),
		"export outside":       second(f.m.ExportTar(ctx, []string{"/elsewhere"})),
		"export missing":       second(f.m.ExportTar(ctx, []string{"gone"})),
		"import a bad archive": f.m.ImportTar(ctx, ".", strings.NewReader(strings.Repeat("x", 1024))),
	} {
		if err == nil {
			t.Errorf("%s was done", name)
		}
	}
	f.stub.Remove(f.m.Info().ID)
	if err := f.m.ImportTar(ctx, ".", strings.NewReader("")); Code(err) != machine.CodeLost {
		t.Errorf("an import to a lost sandbox: %v", err)
	}
}

func TestRouteInfo(t *testing.T) {
	fi, err := routeInfo(client.FileEntry{Path: "/w/d", Mode: "0755", IsDir: true, ModTime: time.Unix(1, 0)})
	if err != nil || fi.Mode != fs.ModeDir|0o755 || !fi.IsDir {
		t.Errorf("routeInfo = %+v %v", fi, err)
	}
	if _, err := routeInfo(client.FileEntry{Path: "/w/x", Mode: "rwx"}); err == nil {
		t.Error("a mode that is not octal was read")
	}
	for kind, want := range map[string]error{
		"not_exist": fs.ErrNotExist, "exist": fs.ErrExist, "not_empty": syscall.ENOTEMPTY, "not_dir": syscall.ENOTDIR, "permission": fs.ErrPermission,
		"outside": machine.ErrOutside, "denied": machine.ErrDenied,
	} {
		if err := (&errorBody{Kind: kind, Message: "m"}).err("op", "/p"); !errors.Is(err, want) {
			t.Errorf("%s: %v", kind, err)
		}
	}
	if err := (&errorBody{Kind: "other"}).err("op", "/p"); err.Error() != "machine: op /p: other" {
		t.Errorf("a failure with no sentence = %q", err)
	}
	if err := (&errorBody{Kind: "other", Message: "machine: said"}).err("op", "/p"); err.Error() != "machine: said" {
		t.Errorf("a failure = %q", err)
	}
}
