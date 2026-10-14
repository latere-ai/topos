// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// runScript runs a script through the run mode with the helper's own
// environment and answers its output and exit code.
func runScript(t *testing.T, dir, script string) (string, int) {
	t.Helper()
	d := drive(t, t.Context(), "-dir", dir, "--", script)
	d.started(t)
	d.send(t, frameEOF, "")
	frames, code := d.rest(t)
	out, _, exit := outcome(t, frames)
	if code != 0 {
		t.Fatalf("the helper exited %d: %q", code, out)
	}
	return out, exit.Code
}

// writeCommand writes an executable script that prints says.
func writeCommand(t *testing.T, p, says string) {
	t.Helper()
	writeFile(t, p, "#!/bin/sh\necho "+says+"\n")
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

// readOnly takes write permission from dir and everything in it, and
// gives it back when the test ends so the temporary directory is removed.
func readOnly(t *testing.T, dir string) {
	t.Helper()
	chmodDirs := func(mode os.FileMode) error {
		return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return err
			}
			return os.Chmod(p, mode)
		})
	}
	if err := chmodDirs(0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := chmodDirs(0o755); err != nil {
			t.Error(err)
		}
	})
}

// TestACommandRunsWhatAPersonInstalledUnderHome: a command the person
// installed for their user, as `pip install` does under $HOME/.local/bin
// when the system's directories are read-only, runs by its name, in the
// foreground and as a background job.
func TestACommandRunsWhatAPersonInstalledUnderHome(t *testing.T) {
	dir := tempDir(t)
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv(npmPrefix, "")
	writeCommand(t, filepath.Join(home, ".local", "bin", "installed"), "ran")
	out, code := runScript(t, dir, `installed; echo "$PATH"`)
	if want := "ran\n/usr/bin:/bin:" + filepath.Join(home, ".local", "bin") + "\n"; code != 0 || out != want {
		t.Errorf("exit %d, output %q, want %q", code, out, want)
	}
	var stdout bytes.Buffer
	if code := run(t.Context(), []string{"job", "-jobs", filepath.Join(dir, "jobs"), "-dir", dir, "--", "installed"}, nil, &stdout, io.Discard); code != 0 {
		t.Fatalf("job exit %d", code)
	}
	res := decode(t, &stdout)
	if res.Job == nil {
		t.Fatalf("job = %+v", res)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		b, err := os.ReadFile(res.Job.Log)
		if err == nil && strings.Contains(string(b), "exited with code") {
			if !strings.HasPrefix(string(b), "ran\n") {
				t.Errorf("job log = %q", b)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job log = %q %v", b, err)
		}
	}
}

// TestNpmsGlobalDirectoryMovesUnderHomeWhenItCannotBeWritten: with the
// image's npm directory read-only, npm's global directory is
// $HOME/.npm-global, made once with a link to each of the image's
// commands and packages, a scope's packages each on their own, so what the
// image put there resolves and an install writes beside it; its commands
// take the image's place on PATH.
func TestNpmsGlobalDirectoryMovesUnderHomeWhenItCannotBeWritten(t *testing.T) {
	dir := tempDir(t)
	home, image := filepath.Join(dir, "home"), filepath.Join(dir, "image", ".npm-global")
	writeCommand(t, filepath.Join(image, "bin", "tool"), "from the image")
	writeFile(t, filepath.Join(image, "lib", "node_modules", "pkg", "package.json"), `{"name":"pkg"}`)
	writeFile(t, filepath.Join(image, "lib", "node_modules", "@scope", "inner", "package.json"), `{"name":"@scope/inner"}`)
	readOnly(t, image)
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin:"+filepath.Join(image, "bin"))
	t.Setenv(npmPrefix, image)

	moved := filepath.Join(home, ".npm-global")
	out, code := runScript(t, dir, `echo "$NPM_CONFIG_PREFIX"; echo "$PATH"; tool &&
cat "$NPM_CONFIG_PREFIX/lib/node_modules/pkg/package.json" "$NPM_CONFIG_PREFIX/lib/node_modules/@scope/inner/package.json" && echo &&
mkdir "$NPM_CONFIG_PREFIX/lib/node_modules/new" "$NPM_CONFIG_PREFIX/lib/node_modules/@scope/new" && echo installed`)
	want := strings.Join([]string{
		moved,
		"/usr/bin:/bin:" + filepath.Join(moved, "bin") + ":" + filepath.Join(home, ".local", "bin"),
		"from the image",
		`{"name":"pkg"}{"name":"@scope/inner"}`,
		"installed",
	}, "\n") + "\n"
	if code != 0 || out != want {
		t.Fatalf("exit %d, output\n%s\nwant\n%s", code, out, want)
	}
	for rel, target := range map[string]string{
		"bin/tool":                      filepath.Join(image, "bin", "tool"),
		"lib/node_modules/pkg":          filepath.Join(image, "lib", "node_modules", "pkg"),
		"lib/node_modules/@scope/inner": filepath.Join(image, "lib", "node_modules", "@scope", "inner"),
	} {
		if got, err := os.Readlink(filepath.Join(moved, rel)); err != nil || got != target {
			t.Errorf("%s links to %q %v, want %q", rel, got, err, target)
		}
	}
	// The directory is made once: a package the person removed from it
	// stays removed, and the next command uses it as it is.
	if err := os.Remove(filepath.Join(moved, "lib", "node_modules", "pkg")); err != nil {
		t.Fatal(err)
	}
	out, code = runScript(t, dir, `echo "$NPM_CONFIG_PREFIX"; LC_ALL=C ls "$NPM_CONFIG_PREFIX/lib/node_modules"`)
	if want := moved + "\n@scope\nnew\n"; code != 0 || out != want {
		t.Errorf("the next command: exit %d, output %q, want %q", code, out, want)
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 1 {
		t.Errorf("HOME holds %v %v, want the npm directory alone", entries, err)
	}
}

// TestAWritableNpmDirectoryIsLeftAsItIs: where the image's npm directory
// can be written, as on a sandbox whose root filesystem is writable, npm
// keeps it and nothing is made under HOME.
func TestAWritableNpmDirectoryIsLeftAsItIs(t *testing.T) {
	dir := tempDir(t)
	home, image := filepath.Join(dir, "home"), filepath.Join(dir, "image", ".npm-global")
	if err := os.MkdirAll(filepath.Join(image, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	path := "/usr/bin:/bin:" + filepath.Join(image, "bin")
	t.Setenv("PATH", path)
	t.Setenv(npmPrefix, image)
	out, code := runScript(t, dir, `echo "$NPM_CONFIG_PREFIX"; echo "$PATH"`)
	if want := image + "\n" + path + ":" + filepath.Join(home, ".local", "bin") + "\n"; code != 0 || out != want {
		t.Errorf("exit %d, output %q, want %q", code, out, want)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("HOME was made: %v", err)
	}
}

func TestCommandEnv(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	for name, c := range map[string]struct{ in, want []string }{
		"no HOME":          {[]string{"PATH=/bin"}, []string{"PATH=/bin"}},
		"a relative HOME":  {[]string{"HOME=home", "PATH=/bin"}, []string{"HOME=home", "PATH=/bin"}},
		"no PATH":          {[]string{"HOME=/h"}, []string{"HOME=/h"}},
		"on PATH already":  {[]string{"HOME=/h", "PATH=/h/.local/bin/:/bin"}, []string{"HOME=/h", "PATH=/h/.local/bin/:/bin"}},
		"a repeated PATH":  {[]string{"PATH=/x", "HOME=/h", "PATH=/bin"}, []string{"HOME=/h", "PATH=/bin:/h/.local/bin"}},
		"a missing prefix": {[]string{"HOME=/h", "PATH=/bin", npmPrefix + "=" + missing}, []string{"HOME=/h", npmPrefix + "=" + missing, "PATH=/bin:/h/.local/bin"}},
	} {
		if got := commandEnv(c.in); !slices.Equal(got, c.want) {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
	if got := replaceDir([]string{"/a", "/new", "/old", "/b"}, "/old", "/new"); !slices.Equal(got, []string{"/a", "/new", "/b"}) {
		t.Errorf("replaceDir keeps one: %q", got)
	}
}

// TestNpmGlobalIsMadeOnceByCommandsThatStartTogether: commands that start
// together each find the directory whole, and one of them made it.
func TestNpmGlobalIsMadeOnceByCommandsThatStartTogether(t *testing.T) {
	dir := tempDir(t)
	image, moved := filepath.Join(dir, "image"), filepath.Join(dir, "home", ".npm-global")
	writeCommand(t, filepath.Join(image, "bin", "tool"), "tool")
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() { errs[i] = npmGlobal(image, moved) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("command %d: %v", i, err)
		}
	}
	if entries, err := os.ReadDir(filepath.Dir(moved)); err != nil || len(entries) != 1 {
		t.Errorf("HOME holds %v %v, want the npm directory alone", entries, err)
	}
	if _, err := os.Stat(filepath.Join(moved, "bin", "tool")); err != nil {
		t.Error(err)
	}
	// A HOME that cannot be written makes no directory, and says so.
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := npmGlobal(image, filepath.Join(locked, ".npm-global")); err == nil {
		t.Error("a directory was made in a HOME that refuses it")
	}
}
