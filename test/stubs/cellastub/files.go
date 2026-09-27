// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellastub

import (
	"archive/tar"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/httpjson"
)

// fileEntry is one file as Cella's file routes render it: the mode is
// octal permissions.
type fileEntry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	ModTime time.Time `json:"modTime"`
	IsDir   bool      `json:"isDir"`
}

func rendered(p string, fi fs.FileInfo) fileEntry {
	size := fi.Size()
	if fi.IsDir() {
		size = 0
	}
	return fileEntry{
		Name: path.Base(p), Path: p, Size: size, Mode: "0" + strconv.FormatUint(uint64(fi.Mode().Perm()), 8),
		ModTime: fi.ModTime().UTC(), IsDir: fi.IsDir(),
	}
}

// errNotInWorkspace is a path outside the workspace, Cella's invalid_field.
var errNotInWorkspace = errors.New("the path must be absolute and below the workspace")

// inWorkspace holds a path to the workspace by Cella's lexical rule and
// returns it relative to the workspace; os.Root holds it there through
// links.
func inWorkspace(sb sandbox, p string) (string, error) {
	ws := sb.workspace()
	if !path.IsAbs(p) || path.Clean(p) != p || strings.ContainsRune(p, 0) {
		return "", errNotInWorkspace
	}
	if p == ws {
		return ".", nil
	}
	rel, ok := strings.CutPrefix(p, ws+"/")
	if !ok {
		return "", errNotInWorkspace
	}
	return rel, nil
}

// fileError answers a file operation's failure with Cella's code.
func fileError(w http.ResponseWriter, err error) {
	if errors.Is(err, fs.ErrNotExist) {
		refuse(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	refuse(w, http.StatusBadRequest, "invalid_field", err.Error())
}

// target is the preamble of every file route: a running sandbox, the one
// path the route names inside its workspace, and the workspace opened.
func (s *Server) target(w http.ResponseWriter, r *http.Request, key string) (*os.Root, string, string, bool) {
	sb, ok := s.running(w, r)
	if !ok {
		return nil, "", "", false
	}
	values := r.URL.Query()[key]
	if len(values) != 1 {
		refuse(w, http.StatusBadRequest, "invalid_field", "exactly one "+key+" is required")
		return nil, "", "", false
	}
	rel, err := inWorkspace(sb, values[0])
	if err != nil {
		refuse(w, http.StatusBadRequest, "invalid_field", err.Error())
		return nil, "", "", false
	}
	root, err := os.OpenRoot(sb.workspace())
	if err != nil {
		refuse(w, http.StatusServiceUnavailable, "driver_unavailable", err.Error())
		return nil, "", "", false
	}
	return root, rel, values[0], true
}

func (s *Server) fileContent(w http.ResponseWriter, r *http.Request) {
	root, rel, p, ok := s.target(w, r, "path")
	if !ok {
		return
	}
	defer func() { s.record(root.Close()) }()
	f, err := root.Open(rel)
	if err != nil {
		fileError(w, err)
		return
	}
	defer func() { s.record(f.Close()) }()
	fi, err := f.Stat()
	if err == nil && fi.IsDir() {
		err = errors.New(p + " is a directory")
	}
	if err != nil {
		fileError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	w.Header().Set("Last-Modified", fi.ModTime().UTC().Format(http.TimeFormat))
	_, err = io.Copy(w, f)
	s.record(err)
}

func (s *Server) fileStat(w http.ResponseWriter, r *http.Request) {
	root, rel, p, ok := s.target(w, r, "path")
	if !ok {
		return
	}
	defer func() { s.record(root.Close()) }()
	fi, err := root.Stat(rel)
	if err != nil {
		fileError(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, rendered(p, fi))
}

func (s *Server) fileList(w http.ResponseWriter, r *http.Request) {
	root, rel, p, ok := s.target(w, r, "path")
	if !ok {
		return
	}
	defer func() { s.record(root.Close()) }()
	fi, err := root.Stat(rel)
	if err == nil && !fi.IsDir() {
		err = errors.New(p + " is not a directory")
	}
	if err != nil {
		fileError(w, err)
		return
	}
	entries, err := fs.ReadDir(root.FS(), rel)
	if err != nil {
		fileError(w, err)
		return
	}
	items := make([]fileEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, rendered(path.Join(p, e.Name()), info))
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"items": items, "next": ""})
}

// filesPut is the two writes of the files collection, told apart by the
// selector: dest extracts an archive and path writes one file.
func (s *Server) filesPut(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	_, archive := q["dest"]
	_, single := q["path"]
	switch {
	case archive && single:
		refuse(w, http.StatusBadRequest, "exclusive_fields", "dest extracts an archive and path writes one file")
	case archive:
		if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/x-tar" {
			refuse(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "extracting below dest requires application/x-tar")
			return
		}
		s.importTar(w, r)
	case single:
		s.filePut(w, r)
	default:
		refuse(w, http.StatusBadRequest, "invalid_field", "name dest or path")
	}
}

// filePut stages the body beside its path and renames it over the path,
// creating the parents, as Cella's drivers write.
func (s *Server) filePut(w http.ResponseWriter, r *http.Request) {
	root, rel, _, ok := s.target(w, r, "path")
	if !ok {
		return
	}
	defer func() { s.record(root.Close()) }()
	mode := fs.FileMode(0o644)
	if m := r.URL.Query().Get("mode"); m != "" {
		v, err := strconv.ParseUint(m, 8, 32)
		if err != nil || v > 0o777 {
			refuse(w, http.StatusBadRequest, "invalid_field", "the mode is octal text of at most 0777")
			return
		}
		mode = fs.FileMode(v)
	}
	if rel == "." {
		refuse(w, http.StatusBadRequest, "invalid_field", "the workspace itself is not a file")
		return
	}
	if err := write(root, rel, mode, r.Body); err != nil {
		fileError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// write writes body to rel through a temporary file beside it.
func write(root *os.Root, rel string, mode fs.FileMode, body io.Reader) error {
	if dir := path.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp := path.Join(path.Dir(rel), ".cella-write-"+rand.Text())
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, body)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err = errors.Join(err, f.Close()); err == nil {
		err = root.Rename(tmp, rel)
	}
	if err != nil {
		return errors.Join(err, root.Remove(tmp))
	}
	return nil
}

func (s *Server) fileRemove(w http.ResponseWriter, r *http.Request) {
	root, rel, _, ok := s.target(w, r, "path")
	if !ok {
		return
	}
	defer func() { s.record(root.Close()) }()
	if rel == "." {
		refuse(w, http.StatusBadRequest, "invalid_field", "the workspace itself is not removable")
		return
	}
	if err := root.RemoveAll(rel); err != nil {
		fileError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// jsonTarget reads a JSON body naming paths, for mkdir and move.
func (s *Server) jsonTarget(w http.ResponseWriter, r *http.Request, v any) (sandbox, *os.Root, bool) {
	sb, ok := s.running(w, r)
	if !ok {
		return sandbox{}, nil, false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		refuse(w, http.StatusBadRequest, "invalid_field", err.Error())
		return sandbox{}, nil, false
	}
	root, err := os.OpenRoot(sb.workspace())
	if err != nil {
		refuse(w, http.StatusServiceUnavailable, "driver_unavailable", err.Error())
		return sandbox{}, nil, false
	}
	return sb, root, true
}

func (s *Server) fileMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	sb, root, ok := s.jsonTarget(w, r, &req)
	if !ok {
		return
	}
	defer func() { s.record(root.Close()) }()
	rel, err := inWorkspace(sb, req.Path)
	if err == nil && rel == "." {
		err = errors.New("the workspace itself is not the caller's to make")
	}
	if err != nil {
		refuse(w, http.StatusBadRequest, "invalid_field", err.Error())
		return
	}
	if err := root.MkdirAll(rel, 0o755); err != nil {
		fileError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// fileMove renames onto the exact destination and never nests the source
// inside a directory there.
func (s *Server) fileMove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	sb, root, ok := s.jsonTarget(w, r, &req)
	if !ok {
		return
	}
	defer func() { s.record(root.Close()) }()
	from, ferr := inWorkspace(sb, req.From)
	to, terr := inWorkspace(sb, req.To)
	if err := errors.Join(ferr, terr); err != nil || from == "." || to == "." {
		refuse(w, http.StatusBadRequest, "invalid_field", "both paths are below the workspace, and neither is the workspace itself")
		return
	}
	if _, err := root.Lstat(from); err != nil {
		fileError(w, err)
		return
	}
	if fi, err := root.Stat(to); err == nil && fi.IsDir() {
		refuse(w, http.StatusBadRequest, "invalid_field", req.To+" is a directory, and a move never nests the source inside one")
		return
	}
	if dir := path.Dir(to); dir != "." {
		if err := root.MkdirAll(dir, 0o700); err != nil {
			fileError(w, err)
			return
		}
	}
	if err := root.Rename(from, to); err != nil {
		fileError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// importTar extracts regular files and directories below dest; links,
// special files and names that leave dest are refused, as Cella's drivers
// refuse them.
func (s *Server) importTar(w http.ResponseWriter, r *http.Request) {
	base, rel, _, ok := s.target(w, r, "dest")
	if !ok {
		return
	}
	defer func() { s.record(base.Close()) }()
	if err := base.MkdirAll(rel, 0o700); err != nil {
		fileError(w, err)
		return
	}
	root, err := base.OpenRoot(rel)
	if err != nil {
		fileError(w, err)
		return
	}
	defer func() { s.record(root.Close()) }()
	tr := tar.NewReader(r.Body)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			refuse(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		name := strings.TrimSuffix(h.Name, "/")
		if !fs.ValidPath(name) {
			refuse(w, http.StatusBadRequest, "invalid_field", "the archive names "+h.Name)
			return
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = root.MkdirAll(name, 0o755)
		case tar.TypeReg:
			err = write(root, name, fs.FileMode(h.Mode).Perm(), tr)
		default:
			refuse(w, http.StatusBadRequest, "invalid_field", "the archive holds an entry that is not a file or a directory")
			return
		}
		if err != nil {
			fileError(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// exportTar streams the paths as one archive of workspace-relative names.
func (s *Server) exportTar(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.running(w, r)
	if !ok {
		return
	}
	paths := r.URL.Query()["path"]
	if len(paths) == 0 {
		paths = []string{sb.workspace()}
	}
	rels := make([]string, 0, len(paths))
	for _, p := range paths {
		rel, err := inWorkspace(sb, p)
		if err != nil {
			refuse(w, http.StatusBadRequest, "invalid_field", err.Error())
			return
		}
		rels = append(rels, rel)
	}
	root, err := os.OpenRoot(sb.workspace())
	if err != nil {
		refuse(w, http.StatusServiceUnavailable, "driver_unavailable", err.Error())
		return
	}
	defer func() { s.record(root.Close()) }()
	for _, rel := range rels {
		if _, err := root.Stat(rel); err != nil {
			fileError(w, err)
			return
		}
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Trailer", "X-Cella-Error")
	tw := tar.NewWriter(w)
	for _, rel := range slices.Compact(rels) {
		if err := fs.WalkDir(root.FS(), rel, func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if !info.IsDir() && !info.Mode().IsRegular() {
				return errors.New("the path holds an entry that is not a file or a directory")
			}
			h, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			h.Name = name
			if err := tw.WriteHeader(h); err != nil || info.IsDir() {
				return err
			}
			f, err := root.Open(name)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			return errors.Join(err, f.Close())
		}); err != nil {
			w.Header().Set("X-Cella-Error", "invalid_field")
			s.record(err)
			return
		}
	}
	s.record(tw.Close())
}
