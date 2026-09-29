package pkg

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"
)

// Archive limits guard against zip bombs and oversized uploads.
const (
	MaxArchiveFiles = 10_000
	MaxArchiveBytes = 64 << 20 // total uncompressed
)

// IsArchive reports whether name has a supported archive extension.
func IsArchive(name string) bool {
	n := strings.ToLower(name)
	return strings.HasSuffix(n, ".zip") || strings.HasSuffix(n, ".tar.gz") || strings.HasSuffix(n, ".tgz")
}

// OpenArchive reads a .zip or .tar.gz archive fully into memory and returns
// its contents as an fs.FS. When every entry sits under one top-level
// directory, that directory becomes the root.
func OpenArchive(name string, data []byte) (fs.FS, error) {
	var files map[string][]byte
	var err error
	switch n := strings.ToLower(name); {
	case strings.HasSuffix(n, ".zip"):
		files, err = readZip(data)
	case strings.HasSuffix(n, ".tar.gz"), strings.HasSuffix(n, ".tgz"):
		files, err = readTarGz(data)
	default:
		return nil, fmt.Errorf("%s: unsupported archive type (want .zip, .tar.gz or .tgz)", name)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s: archive is empty", name)
	}
	return stripCommonRoot(newMemFS(files)), nil
}

type budget struct{ files, bytes int }

func (b *budget) add(size int64) error {
	b.files++
	b.bytes += int(size)
	if b.files > MaxArchiveFiles {
		return fmt.Errorf("archive has more than %d files", MaxArchiveFiles)
	}
	if size < 0 || b.bytes > MaxArchiveBytes {
		return fmt.Errorf("archive expands to more than %d bytes", MaxArchiveBytes)
	}
	return nil
}

// cleanEntry validates an archive entry name; it rejects absolute paths and
// ".." traversal instead of silently rewriting them.
func cleanEntry(name string) (string, error) {
	n := strings.TrimPrefix(strings.ReplaceAll(name, "\\", "/"), "./")
	n = strings.TrimSuffix(n, "/")
	if n == "" {
		return "", nil
	}
	if !fs.ValidPath(n) {
		return "", fmt.Errorf("unsafe archive entry %q", name)
	}
	return n, nil
}

func readZip(data []byte) (map[string][]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	var b budget
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name, err := cleanEntry(f.Name)
		if err != nil || name == "" {
			return nil, cmpErr(err, "empty entry name")
		}
		if !f.Mode().IsRegular() {
			continue // symlinks and devices are ignored
		}
		// UncompressedSize64 is attacker-controlled: check it before the
		// signed conversion, and cap the actual read below as well.
		if f.UncompressedSize64 > MaxArchiveBytes {
			return nil, fmt.Errorf("archive expands to more than %d bytes", MaxArchiveBytes)
		}
		if err := b.add(int64(f.UncompressedSize64)); err != nil {
			return nil, err
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		content, err := readCapped(rc, MaxArchiveBytes)
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		files[name] = content
	}
	return files, nil
}

func readTarGz(data []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	var b budget
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue // directories, links and devices are ignored
		}
		name, err := cleanEntry(h.Name)
		if err != nil || name == "" {
			return nil, cmpErr(err, "empty entry name")
		}
		if err := b.add(h.Size); err != nil {
			return nil, err
		}
		content, err := readCapped(tr, MaxArchiveBytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		files[name] = content
	}
}

func cmpErr(err error, msg string) error {
	if err != nil {
		return err
	}
	return errors.New(msg)
}

func readCapped(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("entry expands to more than %d bytes", limit)
	}
	return b, nil
}

func stripCommonRoot(m *memFS) fs.FS {
	var root string
	for name := range m.files {
		first, _, found := strings.Cut(name, "/")
		if !found || (root != "" && first != root) {
			return m
		}
		root = first
	}
	sub, err := fs.Sub(m, root)
	if err != nil {
		return m
	}
	return sub
}

// memFS is a read-only in-memory fs.FS with implied directories.
type memFS struct {
	files map[string][]byte
	dirs  map[string][]string // dir → sorted child names
}

func newMemFS(files map[string][]byte) *memFS {
	sets := map[string]map[string]struct{}{".": {}}
	for name := range files {
		for p := name; p != "."; p = path.Dir(p) {
			dir := path.Dir(p)
			if sets[dir] == nil {
				sets[dir] = map[string]struct{}{}
			}
			sets[dir][path.Base(p)] = struct{}{}
		}
	}
	m := &memFS{files: files, dirs: make(map[string][]string, len(sets))}
	for dir, set := range sets {
		children := make([]string, 0, len(set))
		for c := range set {
			children = append(children, c)
		}
		slices.Sort(children)
		m.dirs[dir] = children
	}
	return m
}

func (m *memFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if data, ok := m.files[name]; ok {
		return &memFile{name: path.Base(name), r: bytes.NewReader(data), size: int64(len(data))}, nil
	}
	if children, ok := m.dirs[name]; ok {
		return &memDir{m: m, name: name, children: children}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// ReadFile implements fs.ReadFileFS without copying through a reader.
func (m *memFS) ReadFile(name string) ([]byte, error) {
	if data, ok := m.files[name]; ok {
		return slices.Clone(data), nil
	}
	return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrNotExist}
}

type memInfo struct {
	name string
	size int64
	dir  bool
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return i.size }
func (i memInfo) ModTime() time.Time { return time.Time{} }
func (i memInfo) IsDir() bool        { return i.dir }
func (i memInfo) Sys() any           { return nil }
func (i memInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (i memInfo) Type() fs.FileMode          { return i.Mode().Type() }
func (i memInfo) Info() (fs.FileInfo, error) { return i, nil }

type memFile struct {
	name string
	r    *bytes.Reader
	size int64
}

func (f *memFile) Stat() (fs.FileInfo, error) { return memInfo{name: f.name, size: f.size}, nil }
func (f *memFile) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *memFile) Close() error               { return nil }

type memDir struct {
	m        *memFS
	name     string
	children []string
	pos      int
}

func (d *memDir) Stat() (fs.FileInfo, error) {
	return memInfo{name: path.Base(d.name), dir: true}, nil
}
func (d *memDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: errors.New("is a directory")}
}
func (d *memDir) Close() error { return nil }

func (d *memDir) ReadDir(n int) ([]fs.DirEntry, error) {
	rest := d.children[d.pos:]
	if n > 0 && len(rest) == 0 {
		return nil, io.EOF
	}
	if n > 0 && n < len(rest) {
		rest = rest[:n]
	}
	out := make([]fs.DirEntry, 0, len(rest))
	for _, c := range rest {
		full := c
		if d.name != "." {
			full = d.name + "/" + c
		}
		if data, ok := d.m.files[full]; ok {
			out = append(out, memInfo{name: c, size: int64(len(data))})
		} else {
			out = append(out, memInfo{name: c, dir: true})
		}
	}
	d.pos += len(rest)
	return out, nil
}
