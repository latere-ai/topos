// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"slices"
	"strings"
)

// WalkRequest asks a machine for the regular files under its working
// directory, the copy of spec 073 is made from: each directory LeftOut
// names is left out at any depth, and the files are hashed in walk order
// until MaxFiles files or MaxBytes bytes are hashed, a file past
// MaxFileBytes never. At most MaxListed files are listed at all.
type WalkRequest struct {
	LeftOut      []string `json:"left_out,omitempty"`
	MaxFiles     int      `json:"max_files"`
	MaxBytes     int64    `json:"max_bytes"`
	MaxFileBytes int64    `json:"max_file_bytes"`
	MaxListed    int      `json:"max_listed"`
}

// The reasons a walked file carries no digest.
const (
	// WalkTooLarge is a file past MaxFileBytes.
	WalkTooLarge = "too_large"
	// WalkLimit is a file past MaxFiles or MaxBytes.
	WalkLimit = "limit"
)

// WalkedFile is one regular file a walk listed: its path relative to the
// working directory, slash-separated, its size, its permission bits, and
// the hex SHA-256 of its bytes, or Reason, why it was not hashed.
type WalkedFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
	SHA256 string `json:"sha256,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// WalkResult is a walk's files in walk order. Cut is set when the walk
// stopped at MaxListed: a file after the last one listed was not seen.
type WalkResult struct {
	Files []WalkedFile `json:"files"`
	Cut   bool         `json:"cut,omitempty"`
}

// Walker is the optional interface of a machine that walks its working
// directory where the files are (spec 073), so no file's bytes leave it to
// be hashed.
type Walker interface {
	Walk(ctx context.Context, q WalkRequest) (WalkResult, error)
}

// errListed stops a walk at MaxListed.
var errListed = errors.New("machine: the walk listed the most files it lists")

// WalkFS walks fsys, a working directory, as a Walker answers q: in the
// order fs.WalkDir reads it, each directory's entries by name. A
// directory LeftOut names is skipped, and so is anything that is no
// regular file: a symbolic link, which is never followed, a device, a
// pipe, a socket. The deny-list is fsys's to apply, as a machine's file
// system hides what the list names.
func WalkFS(ctx context.Context, fsys fs.FS, q WalkRequest) (WalkResult, error) {
	out := WalkResult{Files: []WalkedFile{}}
	var hashed int
	var bytes int64
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		switch {
		case err != nil && name != "." && unreadable(err) && d != nil && d.IsDir():
			// A directory a command made unreadable holds nothing the
			// tools could read either.
			return fs.SkipDir
		case err != nil:
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if name != "." && slices.Contains(q.LeftOut, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if len(out.Files) >= q.MaxListed {
			out.Cut = true
			return errListed
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			// Gone since its directory was read.
			return nil
		}
		if err != nil {
			return err
		}
		f := WalkedFile{Path: name, Size: info.Size(), Mode: uint32(info.Mode().Perm())}
		switch {
		case f.Size > q.MaxFileBytes:
			f.Reason = WalkTooLarge
		case hashed >= q.MaxFiles || bytes+f.Size > q.MaxBytes:
			f.Reason = WalkLimit
		default:
			sum, err := hashFile(fsys, name)
			if unreadable(err) {
				// Gone since its directory was read, or not readable.
				return nil
			}
			if err != nil {
				return err
			}
			f.SHA256 = sum
			hashed++
			bytes += f.Size
		}
		out.Files = append(out.Files, f)
		return nil
	})
	if err != nil && !errors.Is(err, errListed) {
		return WalkResult{}, err
	}
	return out, nil
}

// unreadable reports whether err is a file or a directory that is gone,
// or that the walk may not read.
func unreadable(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission)
}

// hashFile is the hex SHA-256 of the file name of fsys.
func hashFile(fsys fs.FS, name string) (string, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	if err := errors.Join(err, f.Close()); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// WalkOrder compares two slash-separated paths in the order a walk lists
// them: segment by segment, each by its bytes, so a directory's files
// come before a sibling whose name sorts after the directory's.
func WalkOrder(a, b string) int {
	return slices.Compare(strings.Split(a, "/"), strings.Split(b, "/"))
}
