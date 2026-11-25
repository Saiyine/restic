package fs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/restic/restic/internal/data"
)

// maxSymlinkTargetLen caps how large a symlink entry's content may be before
// it is no longer treated as a symlink (PATH_MAX on Linux is 4096).
const maxSymlinkTargetLen = 4096

// archive formats recognized by NewMigrate, detected by their magic bytes.
type archiveKind int

const (
	kindUnknown archiveKind = iota
	kindZip
	kindSevenZip
	kindTar    // a tar archive, plain or compressed (see archiveInfo.comp)
	kindSingle // a standalone .gz/.xz file without a tar inside
)

var (
	zipMagic      = []byte{0x50, 0x4B, 0x03, 0x04} // "PK\x03\x04"
	sevenzipMagic = []byte{0x37, 0x7A, 0xBC, 0xAF, 0x27, 0x1C}
	gzipMagic     = []byte{0x1F, 0x8B}
	xzMagic       = []byte{0xFD, 0x37, 0x7A, 0x58, 0x5A, 0x00}
	bzip2Magic    = []byte{0x42, 0x5A, 0x68} // "BZh"
	zstdMagic     = []byte{0x28, 0xB5, 0x2F, 0xFD}
	tarMagic      = []byte("ustar") // at offset 257 of the first tar block
)

// archiveInfo is the result of probing an archive file: its kind and, for
// tar-based archives and standalone files, the compression used.
type archiveInfo struct {
	kind archiveKind
	comp tarCompression
}

// archiveEntry is a single entry (file, directory or symlink) of the virtual
// archive filesystem, independent of the underlying archive format.
type archiveEntry struct {
	path     string // absolute path within the virtual root, e.g. "/dir/file.txt"
	isDir    bool
	implicit bool // synthesized directory, not present in the archive itself
	label    string

	linkTarget string // only for symlinks
	fi         *ExtendedFileInfo
	content    func() (io.ReadCloser, error) // nil for directories
	xattrs     map[string][]byte             // extended attributes, when the archive format stores them
}

// ArchiveInfo describes one indexed archive: its path, the format label
// detected for it, and how many of its entries were indexed into the
// virtual filesystem or skipped (with a warning).
type ArchiveInfo struct {
	Path    string
	Label   string // "zip", "7z", "tar", "tar.gz", ...
	Indexed int
	Skipped int
}

// MigrateFS is a read-only filesystem view over the entries of one or more
// archive files (zip, 7z and tar, in any combination, plus standalone
// compressed files). It implements FS so that the archiver can back up the
// archives' contents in place: entry content is decompressed on the fly and
// nothing is ever extracted to disk. Concurrent OpenFile calls are safe:
// every opened file gets its own decompressor over the (concurrency-safe)
// access of the underlying archive files, and no state is shared between
// Files.
type MigrateFS struct {
	entries      map[string]*archiveEntry
	children     map[string][]string // dir path -> sorted child names
	archiveInfos []ArchiveInfo

	warn    func(format string, args ...any)
	closers []func() error
	closed  bool
}

func newMigrateFS(warnf func(format string, args ...any), capacity int) *MigrateFS {
	if warnf == nil {
		warnf = func(string, ...any) {}
	}
	return &MigrateFS{
		entries:  make(map[string]*archiveEntry, capacity),
		children: make(map[string][]string),
		warn:     warnf,
	}
}

// addCloser registers a closer for one of the underlying archive files.
func (m *MigrateFS) addCloser(close func() error) {
	m.closers = append(m.closers, close)
}

// Close closes the underlying archive files.
func (m *MigrateFS) Close() error {
	if m.closed {
		return nil
	}
	m.closed = true
	var err error
	for _, close := range m.closers {
		err = errors.Join(err, close())
	}
	return err
}

// cleanPath maps a name to its absolute form within the virtual filesystem.
// All paths are relative to the virtual root and use "/" as separator on
// every platform.
func cleanPath(name string) string {
	return path.Clean("/" + name)
}

// archiveCleanEntryName normalizes a raw archive entry name to an absolute
// path within the virtual root. It converts backslashes (Windows-made
// archives) to slashes, strips leading separators and Windows drive letters,
// removes "." components and resolves ".." components. It reports whether
// the name is usable: names containing NUL bytes or escaping the virtual
// root (archive-slip) are rejected.
func archiveCleanEntryName(raw string) (string, bool) {
	if strings.ContainsRune(raw, '\x00') {
		return "", false
	}

	name := strings.ReplaceAll(raw, "\\", "/")
	name = strings.TrimPrefix(name, "/")
	// strip a leading drive letter like "C:" from Windows-made archives
	if len(name) >= 2 && name[1] == ':' {
		if c := name[0]; (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			name = name[2:]
		}
	}

	parts := make([]string, 0, 8)
	for _, part := range strings.Split(name, "/") {
		switch part {
		case "", ".":
			// nothing to do
		case "..":
			if len(parts) == 0 {
				return "", false // would escape the virtual root
			}
			parts = parts[:len(parts)-1]
		default:
			parts = append(parts, part)
		}
	}

	if len(parts) == 0 {
		return "", false // entry points at the root itself
	}
	return "/" + strings.Join(parts, "/"), true
}

// addEntry indexes e. A second entry with the same path and same type
// replaces the first one (last one wins, like `unzip -o`); an entry whose
// path conflicts with an existing entry of a different type, or that would
// live inside a regular file, is rejected.
func (m *MigrateFS) addEntry(e *archiveEntry) bool {
	label := e.label
	if prev, ok := m.entries[e.path]; ok {
		if prev.isDir != e.isDir {
			m.warn("%s: entry %q ignored, name conflicts with existing %s", label, e.path, kindOf(prev))
			return false
		}
		m.warn("%s: duplicate entry %q, keeping the last one", label, e.path)
	}

	// the path must not cross an existing regular file or symlink
	for dir := path.Dir(e.path); dir != "/"; dir = path.Dir(dir) {
		if p, ok := m.entries[dir]; ok && !p.isDir {
			m.warn("%s: entry %q ignored, parent %q is not a directory", label, e.path, dir)
			return false
		}
	}

	if _, ok := m.entries[e.path]; !ok {
		m.children[path.Dir(e.path)] = append(m.children[path.Dir(e.path)], path.Base(e.path))
	}
	m.entries[e.path] = e
	return true
}

func kindOf(e *archiveEntry) string {
	switch {
	case e.isDir:
		return "directory"
	case e.fi.Mode&os.ModeSymlink != 0:
		return "symlink"
	default:
		return "file"
	}
}

// finalize synthesizes the directories implied by file paths but not present
// in the archives (like `unzip` does), plus the virtual root itself, and
// gives implicit directories the most recent mtime of their descendants.
func (m *MigrateFS) finalize() {
	for p, e := range m.entries {
		if e.isDir {
			continue
		}
		for dir := path.Dir(p); dir != "/"; dir = path.Dir(dir) {
			if _, ok := m.entries[dir]; ok {
				break
			}
			m.entries[dir] = &archiveEntry{
				path:     dir,
				isDir:    true,
				implicit: true,
				label:    e.label,
				fi: &ExtendedFileInfo{
					Name: path.Base(dir),
					Mode: os.ModeDir | 0o755,
				},
			}
			m.children[path.Dir(dir)] = append(m.children[path.Dir(dir)], path.Base(dir))
		}
	}
	m.entries["/"] = &archiveEntry{
		path:     "/",
		isDir:    true,
		implicit: true,
		fi:       &ExtendedFileInfo{Name: "/", Mode: os.ModeDir | 0o755},
	}

	// An implicit directory's mtime is the most recent of its descendants,
	// computed deepest-first so that child directories are resolved first.
	var dirPaths []string
	for p, e := range m.entries {
		if e.isDir {
			dirPaths = append(dirPaths, p)
		}
	}
	slices.SortFunc(dirPaths, func(a, b string) int {
		da, db := strings.Count(a, "/"), strings.Count(b, "/")
		if da != db {
			return db - da
		}
		return strings.Compare(a, b)
	})
	for _, p := range dirPaths {
		e := m.entries[p]
		if !e.implicit {
			continue
		}
		var latest time.Time
		for _, name := range m.children[p] {
			if t := m.entries[path.Join(p, name)].fi.ModTime; t.After(latest) {
				latest = t
			}
		}
		e.fi.ModTime = latest
		e.fi.AccessTime = latest
		e.fi.ChangeTime = latest
	}

	for _, names := range m.children {
		slices.Sort(names)
	}
}

// readLinkTarget reads the content of a symlink entry, which is the link
// target. It returns an error if the target is longer than
// maxSymlinkTargetLen or cannot be read.
func readLinkTarget(size uint64, open func() (io.ReadCloser, error)) (string, error) {
	if size > maxSymlinkTargetLen {
		return "", fmt.Errorf("symlink target too long (%d bytes)", size)
	}
	rc, err := open()
	if err != nil {
		return "", err
	}
	defer func() {
		_ = rc.Close()
	}()

	target, err := io.ReadAll(io.LimitReader(rc, maxSymlinkTargetLen+1))
	if err != nil {
		return "", err
	}
	if len(target) > maxSymlinkTargetLen {
		return "", fmt.Errorf("symlink target too long (%d bytes)", len(target))
	}
	// some tools NUL-terminate the target (a tar convention)
	return strings.TrimSuffix(string(target), "\x00"), nil
}

// probeArchive reports the format of the archive at path: zip and 7z by
// their leading magic bytes; tar (plain, or compressed with gzip, xz, bzip2
// or zstd) by sniffing the first 512 bytes of the (decompressed) content for
// the ustar magic; and any other gzip/xz/bzip2/zstd-compressed file as a
// standalone single-file archive.
func probeArchive(path string) (archiveInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return archiveInfo{}, err
	}
	defer func() { _ = f.Close() }()

	var magic [6]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return archiveInfo{}, fmt.Errorf("%s: file too short to be an archive", path)
		}
		return archiveInfo{}, err
	}

	switch {
	case bytes.Equal(magic[:4], zipMagic):
		return archiveInfo{kind: kindZip}, nil
	case bytes.Equal(magic[:], sevenzipMagic):
		return archiveInfo{kind: kindSevenZip}, nil
	case bytes.Equal(magic[:2], gzipMagic):
		return probeWrappedArchive(path, tarGzip)
	case bytes.Equal(magic[:], xzMagic):
		return probeWrappedArchive(path, tarXz)
	case bytes.Equal(magic[:3], bzip2Magic):
		return probeWrappedArchive(path, tarBzip2)
	case bytes.Equal(magic[:4], zstdMagic):
		return probeWrappedArchive(path, tarZstd)
	}

	// an uncompressed tar has no magic at the start: the ustar magic sits
	// at offset 257 of the first 512-byte block
	if ok, err := sniffTar(f); err != nil {
		return archiveInfo{}, err
	} else if ok {
		return archiveInfo{kind: kindTar, comp: tarPlain}, nil
	}
	return archiveInfo{}, fmt.Errorf("%s: unsupported archive format (only zip, 7z and tar are supported)", path)
}

// sniffTar reports whether the file (positioned anywhere) is an
// uncompressed tar, by its first block: either a header with the ustar
// magic, or an all-zero block (an empty archive, as tar itself treats
// leading zero blocks as end-of-archive).
func sniffTar(f *os.File) (bool, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	buf := make([]byte, 512)
	if _, err := io.ReadFull(f, buf); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil // shorter than one tar block: not a tar
		}
		return false, err
	}
	return isTarBlock(buf), nil
}

// isTarBlock reports whether buf is the first block of a tar archive: a
// ustar header or an all-zero (empty archive) block.
func isTarBlock(buf []byte) bool {
	if bytes.Equal(buf[257:262], tarMagic) {
		return true
	}
	for _, b := range buf {
		if b != 0 {
			return false
		}
	}
	return true
}

// probeWrappedArchive sniffs the decompressed content of a compressed file
// (gzip, xz, bzip2 or zstd): a tar inside, or a standalone file. A wrapper
// that cannot be decompressed at all is classified as a standalone file so
// that the indexing pass reports the actual error.
func probeWrappedArchive(path string, comp tarCompression) (archiveInfo, error) {
	rc, err := openTarStream(path, comp)
	if err != nil {
		return archiveInfo{kind: kindSingle, comp: comp}, nil
	}
	buf := make([]byte, 512)
	_, rerr := io.ReadFull(rc, buf)
	_ = rc.Close()
	if rerr == nil && isTarBlock(buf) {
		return archiveInfo{kind: kindTar, comp: comp}, nil
	}
	return archiveInfo{kind: kindSingle, comp: comp}, nil
}

// NewMigrate opens the archives at archivePaths (zip, 7z and tar in any
// combination, with tar optionally compressed with gzip, xz, bzip2 or zstd,
// plus standalone .gz/.xz/.bz2/.zst files) and indexes their entries into a
// single virtual filesystem. The format of each archive is detected by its
// magic bytes and, for tar, by sniffing its content. The result is
// equivalent to a single
// virtual archive whose entries are the concatenation of the entries of the
// given archives, in the order given: the usual rules apply to the
// concatenated entries (duplicate paths of the same type keep the last
// entry, entries conflicting with an existing entry of a different type are
// skipped with a warning). Nothing is extracted to disk. All archives stay
// open until the returned MigrateFS is closed.
func NewMigrate(archivePaths []string, warnf func(format string, args ...any)) (*MigrateFS, error) {
	if len(archivePaths) == 0 {
		return nil, errors.New("no archives given")
	}

	// Fail fast: probe every archive before opening any of them.
	infos := make([]archiveInfo, len(archivePaths))
	for i, p := range archivePaths {
		info, err := probeArchive(p)
		if err != nil {
			return nil, err
		}
		infos[i] = info
	}

	m := newMigrateFS(warnf, 0)
	for i, p := range archivePaths {
		var err error
		var info ArchiveInfo
		switch infos[i].kind {
		case kindZip:
			info = ArchiveInfo{Path: p, Label: "zip"}
			err = m.openZipArchive(p, &info)
		case kindSevenZip:
			info = ArchiveInfo{Path: p, Label: "7z"}
			err = m.openSevenZipArchive(p, &info)
		case kindTar:
			info = ArchiveInfo{Path: p, Label: tarLabel(infos[i].comp)}
			err = m.openTarArchive(p, infos[i].comp, &info)
		case kindSingle:
			info = ArchiveInfo{Path: p, Label: singleLabel(infos[i].comp)}
			err = m.openSingleFile(p, infos[i].comp, &info)
		}
		if err != nil {
			_ = m.Close()
			return nil, err
		}
		m.archiveInfos = append(m.archiveInfos, info)
	}

	m.finalize()
	return m, nil
}

// Archives returns the indexed archives in the order they were given to
// NewMigrate, with their entry and skip counts.
func (m *MigrateFS) Archives() []ArchiveInfo {
	return m.archiveInfos
}

// OpenFile implements FS.
func (m *MigrateFS) OpenFile(name string, flag int, _ bool) (File, error) {
	if flag&^(O_RDONLY|O_NOFOLLOW|O_DIRECTORY) != 0 {
		return nil, pathError("open", name, fmt.Errorf("invalid combination of flags 0x%x", flag))
	}

	if m.closed {
		return nil, pathError("open", name, os.ErrClosed)
	}

	name = cleanPath(name)
	e, ok := m.entries[name]
	if !ok {
		return nil, pathError("open", name, syscall.ENOENT)
	}
	if flag&O_DIRECTORY != 0 && !e.isDir {
		return nil, pathError("open", name, syscall.ENOTDIR)
	}

	if e.isDir {
		return &migrateDir{
			name:   name,
			fi:     e.fi,
			names:  slices.Clone(m.children[name]),
			xattrs: e.xattrs,
		}, nil
	}
	return &migrateFile{
		name:  name,
		entry: e,
	}, nil
}

// Lstat implements FS.
func (m *MigrateFS) Lstat(name string) (*ExtendedFileInfo, error) {
	name = cleanPath(name)
	e, ok := m.entries[name]
	if !ok {
		return nil, pathError("lstat", name, os.ErrNotExist)
	}
	return e.fi, nil
}

// Join implements FS.
func (m *MigrateFS) Join(elem ...string) string {
	return path.Join(elem...)
}

// Separator implements FS.
func (m *MigrateFS) Separator() string {
	return "/"
}

// IsAbs implements FS.
func (m *MigrateFS) IsAbs(p string) bool {
	return path.IsAbs(p)
}

// Abs implements FS.
func (m *MigrateFS) Abs(p string) (string, error) {
	return cleanPath(p), nil
}

// Clean implements FS.
func (m *MigrateFS) Clean(p string) string {
	return path.Clean(p)
}

// VolumeName implements FS. The virtual filesystem has no volumes.
func (m *MigrateFS) VolumeName(string) string {
	return ""
}

// Dir implements FS.
func (m *MigrateFS) Dir(p string) string {
	return path.Dir(p)
}

// Base implements FS.
func (m *MigrateFS) Base(p string) string {
	return path.Base(p)
}

// nodeFromEntry builds the data.Node for an archive entry. The metadata
// comes from the in-memory index, so Stat() and ToNode() are always
// consistent.
func nodeFromEntry(name string, e *archiveEntry) *data.Node {
	node := buildBasicNode(name, e.fi)
	// atime and ctime fall back to the mtime for archive entries that do
	// not store them; the archiver maps atime to mtime unless --with-atime
	node.AccessTime = e.fi.AccessTime
	node.ChangeTime = e.fi.ChangeTime
	if e.fi.Mode&os.ModeSymlink != 0 {
		node.LinkTarget = e.linkTarget
		if !utf8.ValidString(e.linkTarget) {
			node.LinkTargetRaw = []byte(e.linkTarget)
		}
	}
	if len(e.xattrs) > 0 {
		// sorted names: deterministic node content and snapshot trees
		names := make([]string, 0, len(e.xattrs))
		for n := range e.xattrs {
			names = append(names, n)
		}
		slices.Sort(names)
		node.ExtendedAttributes = make([]data.ExtendedAttribute, 0, len(names))
		for _, n := range names {
			node.ExtendedAttributes = append(node.ExtendedAttributes, data.ExtendedAttribute{Name: n, Value: e.xattrs[n]})
		}
	}
	return node
}

// migrateFile is a File backed by a single archive entry. The content is
// decompressed on the fly from the archive; nothing is written to disk.
type migrateFile struct {
	name  string
	entry *archiveEntry

	rc io.ReadCloser // lazily opened on first read
}

// statically ensure that migrateFile implements File.
var _ File = (*migrateFile)(nil)

// MakeReadable opens the (decompressing) reader for the entry's content. It
// is a no-op for entries that have no content to read.
func (f *migrateFile) MakeReadable() error {
	if f.rc != nil {
		return nil
	}

	if f.entry.fi.Mode&os.ModeSymlink != 0 {
		f.rc = io.NopCloser(strings.NewReader(f.entry.linkTarget))
		return nil
	}

	rc, err := f.entry.content()
	if err != nil {
		return pathError("open", f.name, err)
	}
	f.rc = rc
	return nil
}

func (f *migrateFile) reader() (io.ReadCloser, error) {
	if f.rc == nil {
		if err := f.MakeReadable(); err != nil {
			return nil, err
		}
	}
	return f.rc, nil
}

// Read implements io.Reader.
func (f *migrateFile) Read(p []byte) (int, error) {
	rc, err := f.reader()
	if err != nil {
		return 0, err
	}
	return rc.Read(p)
}

// Close implements io.Closer.
func (f *migrateFile) Close() error {
	if f.rc == nil {
		return nil
	}
	err := f.rc.Close()
	f.rc = nil
	return err
}

// Readdirnames implements File.
func (f *migrateFile) Readdirnames(_ int) ([]string, error) {
	return nil, pathError("readdirnames", f.name, os.ErrInvalid)
}

// Stat implements File.
func (f *migrateFile) Stat() (*ExtendedFileInfo, error) {
	return f.entry.fi, nil
}

// ToNode implements File.
func (f *migrateFile) ToNode(_ bool, _ func(format string, args ...any)) (*data.Node, error) {
	return nodeFromEntry(f.name, f.entry), nil
}

// migrateDir is a directory of the virtual filesystem.
type migrateDir struct {
	name   string
	fi     *ExtendedFileInfo
	names  []string
	xattrs map[string][]byte
}

// statically ensure that migrateDir implements File.
var _ File = (*migrateDir)(nil)

// MakeReadable implements File.
func (d *migrateDir) MakeReadable() error {
	return nil
}

// Read implements io.Reader.
func (d *migrateDir) Read(_ []byte) (int, error) {
	return 0, pathError("read", d.name, os.ErrInvalid)
}

// Close implements io.Closer.
func (d *migrateDir) Close() error {
	return nil
}

// Readdirnames implements File.
func (d *migrateDir) Readdirnames(n int) ([]string, error) {
	if n > 0 {
		return nil, pathError("readdirnames", d.name, os.ErrInvalid)
	}
	return slices.Clone(d.names), nil
}

// Stat implements File.
func (d *migrateDir) Stat() (*ExtendedFileInfo, error) {
	return d.fi, nil
}

// ToNode implements File.
func (d *migrateDir) ToNode(_ bool, _ func(format string, args ...any)) (*data.Node, error) {
	return nodeFromEntry(d.name, &archiveEntry{path: d.name, isDir: true, fi: d.fi, xattrs: d.xattrs}), nil
}

// statically ensure that MigrateFS implements FS.
var _ FS = (*MigrateFS)(nil)
