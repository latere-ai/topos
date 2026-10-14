// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// npmPrefix is the variable that names npm's global directory, where
// `npm install -g` writes packages and the links to their commands.
const npmPrefix = "NPM_CONFIG_PREFIX"

// wOK is access(2)'s write check, W_OK, which syscall does not name on
// every platform.
const wOK = 0x2

// commandEnv is the environment of a command and of a background job: the
// helper's own, which is the sandbox's, with the directories a person's
// installs land in under HOME made usable.
//
// machine/cella points HOME at a directory of its own, because a hosted
// sandbox's root filesystem is read-only and only /tmp and the workspace
// can be written. An image puts its user's install directories on PATH
// under the image's home and cannot know the HOME the machine chose; the
// environment a sandbox is created with sets PATH whole and cannot add to
// the image's; and a command runs under /bin/sh, which reads no profile.
// So the helper, which runs in the sandbox and sees both, adds them here,
// each at most once:
//
//   - $HOME/.local/bin, where `pip install` for the user and other
//     user-level installers put commands, joins PATH at its end, where an
//     image appends its own.
//   - npm's global directory moves to $HOME/.npm-global when
//     NPM_CONFIG_PREFIX names one that cannot be written, so
//     `npm install -g` writes there, and its bin takes the old one's place
//     on PATH; see npmGlobal. A writable one, as where the root filesystem
//     can be written, is left as it is.
//
// An environment with no absolute HOME or no PATH is returned as it is.
func commandEnv(environ []string) []string {
	home, path := lookup(environ, "HOME"), lookup(environ, "PATH")
	if !filepath.IsAbs(home) || path == "" {
		return environ
	}
	dirs := filepath.SplitList(path)
	if old := lookup(environ, npmPrefix); old != "" && !writable(old) {
		moved := filepath.Join(home, ".npm-global")
		// A directory that cannot be made, as in a HOME that cannot be
		// written, leaves npm's as it is: the command still
		// runs, an `npm install -g` in it fails naming the directory it
		// cannot write, and the next command tries again.
		if npmGlobal(old, moved) == nil {
			environ = setenv(environ, npmPrefix, moved)
			dirs = replaceDir(dirs, filepath.Join(old, "bin"), filepath.Join(moved, "bin"))
		}
	}
	dirs = appendDir(dirs, filepath.Join(home, ".local", "bin"))
	return setenv(environ, "PATH", strings.Join(dirs, string(filepath.ListSeparator)))
}

// writable reports whether a command may write in dir, or create it: a
// missing directory is as writable as the nearest directory above it.
func writable(dir string) bool {
	for {
		err := syscall.Access(dir, wOK)
		if !errors.Is(err, fs.ErrNotExist) {
			return err == nil
		}
		up := filepath.Dir(dir)
		if up == dir {
			return false
		}
		dir = up
	}
}

// npmGlobal makes moved npm's global directory in the place of old, once:
// a moved directory that exists is used as it is. The image may have put
// commands and packages in old that a command relies on: npx finds a
// command in the project and then in the global directory, never on PATH,
// and asks the registry for anything else, which without a network hangs
// for about seventy seconds; and `npm link <package>` links a package
// from the global directory. So moved holds a link to each of old's
// entries, and the directories npm writes into are made, so an install
// writes beside the links. It is built beside its place and renamed into
// it, so two commands that start together make it once and no command
// sees a part of it.
func npmGlobal(old, moved string) error {
	if _, err := os.Lstat(moved); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(moved), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(moved), ".npm-global-")
	if err != nil {
		return err
	}
	if err := errors.Join(os.Chmod(tmp, 0o755), linkEntries(old, tmp)); err != nil {
		return errors.Join(err, os.RemoveAll(tmp))
	}
	if err := os.Rename(tmp, moved); err != nil {
		if _, serr := os.Lstat(moved); serr == nil {
			// Another command made it first.
			return os.RemoveAll(tmp)
		}
		return errors.Join(err, os.RemoveAll(tmp))
	}
	return nil
}

// linkEntries fills dst with old's entries: a directory npm writes into,
// such as bin, lib/node_modules or a scope in it, is made, and every other
// entry, a command, a package or a file, is a link to old's. An old that
// does not exist has nothing to link.
func linkEntries(old, dst string) error {
	root, err := filepath.EvalSymlinks(old)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." {
			return err
		}
		to := filepath.Join(dst, rel)
		if d.IsDir() && !isPackage(rel) {
			return os.Mkdir(to, 0o755)
		}
		if err := os.Symlink(filepath.Join(old, rel), to); err != nil {
			return err
		}
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
}

// isPackage reports whether rel, a directory in npm's global directory, is
// one package: an entry of lib/node_modules other than a scope, or an
// entry of a scope.
func isPackage(rel string) bool {
	modules := filepath.Join("lib", "node_modules")
	parent, name := filepath.Dir(rel), filepath.Base(rel)
	if parent == modules {
		return !strings.HasPrefix(name, "@")
	}
	return filepath.Dir(parent) == modules && strings.HasPrefix(filepath.Base(parent), "@")
}

// replaceDir puts to in old's place in dirs, or at the end when old is
// not there, and keeps one of to.
func replaceDir(dirs []string, old, to string) []string {
	out := make([]string, 0, len(dirs)+1)
	placed := false
	for _, d := range dirs {
		switch filepath.Clean(d) {
		case old, to:
			if !placed {
				out, placed = append(out, to), true
			}
		default:
			out = append(out, d)
		}
	}
	if !placed {
		out = append(out, to)
	}
	return out
}

// appendDir adds dir at the end of dirs unless it is there.
func appendDir(dirs []string, dir string) []string {
	for _, d := range dirs {
		if filepath.Clean(d) == dir {
			return dirs
		}
	}
	return append(dirs, dir)
}

// lookup is the value of key in environ, the last one when it repeats, as
// a process started with it reads it.
func lookup(environ []string, key string) string {
	v := ""
	for _, kv := range environ {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}

// setenv sets key in environ to value, in place of every value it had.
func setenv(environ []string, key, value string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if k, _, _ := strings.Cut(kv, "="); k != key {
			out = append(out, kv)
		}
	}
	return append(out, key+"="+value)
}
