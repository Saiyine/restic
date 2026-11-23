package fs

import (
	"archive/zip"
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

// zipCreatorUnix and zipCreatorMacOSX mirror the creator-version values used
// by archive/zip to decide whether ExternalAttrs carries Unix mode bits.
const (
	zipCreatorUnix   = 3
	zipCreatorMacOSX = 19
)

// maxSymlinkTargetLen caps how large a symlink entry's content may be before
// it is no longer treated as a symlink (PATH_MAX on Linux is 4096).
const maxSymlinkTargetLen = 4096

// zipEncryptedFlag is bit 0 of the zip general-purpose flag word.
const zipEncryptedFlag = 1 << 0

// cp437High maps CP437 bytes 0x80-0xFF to Unicode code points. Bytes below
// 0x80 are plain ASCII. The table is IBM Code Page 437, the encoding used by
// zip entries that do not set the UTF-8 flag.
var cp437High = []rune{
	0x00C7, 0x00FC, 0x00E9, 0x00E2, 0x00E4, 0x00E0, 0x00E5, 0x00E7,
	0x00EA, 0x00EB, 0x00E8, 0x00EF, 0x00EE, 0x00EC, 0x00C4, 0x00C5,
	0x00C9, 0x00E6, 0x00C6, 0x00F4, 0x00F6, 0x00F2, 0x00FB, 0x00F9,
	0x00FF, 0x00D6, 0x00DC, 0x00A2, 0x00A3, 0x00A5, 0x20A7, 0x0192,
	0x00E1, 0x00ED, 0x00F3, 0x00FA, 0x00F1, 0x00D1, 0x00AA, 0x00BA,
	0x00BF, 0x2310, 0x00AC, 0x00BD, 0x00BC, 0x00A1, 0x00AB, 0x00BB,
	0x2591, 0x2592, 0x2593, 0x2502, 0x2524, 0x2561, 0x2562, 0x2556,
	0x2555, 0x2563, 0x2551, 0x2557, 0x255D, 0x255C, 0x255B, 0x2510,
	0x2514, 0x2534, 0x252C, 0x251C, 0x2500, 0x253C, 0x255E, 0x255F,
	0x255A, 0x2554, 0x2569, 0x2566, 0x2560, 0x2550, 0x256C, 0x2567,
	0x2568, 0x2564, 0x2565, 0x2559, 0x2558, 0x2552, 0x2553, 0x256B,
	0x256A, 0x2518, 0x250C, 0x2588, 0x2584, 0x258C, 0x2590, 0x2580,
	0x03B1, 0x00DF, 0x0393, 0x03C0, 0x03A3, 0x03C3, 0x00B5, 0x03C4,
	0x03A6, 0x0398, 0x03A9, 0x03B4, 0x221E, 0x03C6, 0x03B5, 0x2229,
	0x2261, 0x00B1, 0x2265, 0x2264, 0x2320, 0x2321, 0x00F7, 0x2248,
	0x00B0, 0x2219, 0x00B7, 0x221A, 0x207F, 0x00B2, 0x25A0, 0x00A0,
}

// cp437Decode converts a non-UTF-8 zip name (CP437 per the zip spec) to
// UTF-8. Bytes below 0x80 are ASCII and pass through unchanged.
func cp437Decode(raw string) string {
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c < 0x80 {
			b.WriteByte(c)
		} else {
			b.WriteRune(cp437High[c-0x80])
		}
	}
	return b.String()
}

// zipEntry is one entry of the in-memory index of a Zip filesystem.
type zipEntry struct {
	path     string // absolute path within the virtual root, e.g. "/dir/file.txt"
	isDir    bool
	implicit bool // synthesized directory, not present in the archive itself

	header     *zip.File
	linkTarget string // only for symlinks
	fi         *ExtendedFileInfo
}

// Zip is a read-only filesystem view over the entries of a .zip archive. It
// implements FS so that the archiver can back up the archive's contents in
// place: entry content is decompressed on the fly and nothing is ever
// extracted to disk. Concurrent OpenFile calls are safe: every opened file
// gets its own decompressor over the (concurrency-safe) io.ReaderAt of the
// underlying archive file, and no state is shared between Files.
type Zip struct {
	rc *zip.ReadCloser

	entries  map[string]*zipEntry
	children map[string][]string // dir path -> sorted child names

	warn   func(format string, args ...any)
	closed bool
}

// NewZip opens the zip archive at zipPath and indexes its central directory.
// Nothing is extracted to disk: only the central directory is read (plus the
// small content of symlink entries). Entries whose names cannot be made safe
// (escaping the virtual root, NUL bytes, ...), or that are encrypted or use
// an unsupported compression method, are skipped with a warning via warnf.
func NewZip(zipPath string, warnf func(format string, args ...any)) (*Zip, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, err
	}

	if warnf == nil {
		warnf = func(string, ...any) {}
	}

	z := &Zip{
		rc:       zr,
		entries:  make(map[string]*zipEntry, len(zr.File)),
		children: make(map[string][]string),
		warn:     warnf,
	}

	for _, fh := range zr.File {
		name, ok := zipCleanEntryName(fh.Name, fh.NonUTF8)
		if !ok {
			warnf("zip: entry %q ignored, unsafe or invalid path", fh.Name)
			continue
		}

		if fh.Flags&zipEncryptedFlag != 0 {
			warnf("zip: entry %q ignored, entry is encrypted", fh.Name)
			continue
		}
		if fh.Method != zip.Store && fh.Method != zip.Deflate {
			warnf("zip: entry %q ignored, unsupported compression method %d", fh.Name, fh.Method)
			continue
		}

		mode := fh.Mode()
		isDir := mode.IsDir()
		isSymlink := mode&os.ModeSymlink != 0

		mtime := fh.Modified.UTC()
		e := &zipEntry{
			path:   name,
			isDir:  isDir,
			header: fh,
			fi: &ExtendedFileInfo{
				Name:       path.Base(name),
				ModTime:    mtime,
				AccessTime: mtime,
				ChangeTime: mtime,
			},
		}
		if isSymlink {
			target, err := readSymlinkTarget(fh)
			if err != nil {
				warnf("zip: entry %q ignored: %v", fh.Name, err)
				continue
			}
			e.linkTarget = target
			e.fi.Mode = os.ModeSymlink | 0o777
			e.fi.Size = int64(len(target))
		} else {
			e.fi.Mode = zipEntryMode(fh, isDir)
			if !isDir {
				e.fi.Size = int64(fh.UncompressedSize64)
			}
		}

		if !z.addEntry(e) {
			continue
		}
	}

	// Synthesize the directories implied by file paths but not present in
	// the archive (like `unzip` does), plus the virtual root itself.
	for p, e := range z.entries {
		if e.isDir {
			continue
		}
		for dir := path.Dir(p); dir != "/"; dir = path.Dir(dir) {
			if _, ok := z.entries[dir]; ok {
				break
			}
			z.entries[dir] = &zipEntry{
				path:     dir,
				isDir:    true,
				implicit: true,
				fi: &ExtendedFileInfo{
					Name: path.Base(dir),
					Mode: os.ModeDir | 0o755,
				},
			}
			z.children[path.Dir(dir)] = append(z.children[path.Dir(dir)], path.Base(dir))
		}
	}
	z.entries["/"] = &zipEntry{
		path:     "/",
		isDir:    true,
		implicit: true,
		fi:       &ExtendedFileInfo{Name: "/", Mode: os.ModeDir | 0o755},
	}

	// An implicit directory's mtime is the most recent of its descendants,
	// computed deepest-first so that child directories are resolved first.
	var dirPaths []string
	for p, e := range z.entries {
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
		e := z.entries[p]
		if !e.implicit {
			continue
		}
		var latest time.Time
		for _, name := range z.children[p] {
			if t := z.entries[path.Join(p, name)].fi.ModTime; t.After(latest) {
				latest = t
			}
		}
		e.fi.ModTime = latest
		e.fi.AccessTime = latest
		e.fi.ChangeTime = latest
	}

	for _, names := range z.children {
		slices.Sort(names)
	}

	return z, nil
}

// Close closes the underlying zip archive.
func (z *Zip) Close() error {
	if z.closed {
		return nil
	}
	z.closed = true
	return z.rc.Close()
}

// zipCleanPath maps a name to its absolute form within the virtual
// filesystem. All paths are relative to the virtual root and use "/" as
// separator on every platform.
func zipCleanPath(name string) string {
	return path.Clean("/" + name)
}

// zipCleanEntryName normalizes a raw zip entry name to an absolute path
// within the virtual root. It converts backslashes (Windows-made archives)
// to slashes, strips leading separators and Windows drive letters, removes
// "." components and resolves ".." components. It reports whether the name
// is usable: names containing NUL bytes or escaping the virtual root
// (zip-slip) are rejected.
func zipCleanEntryName(raw string, nonUTF8 bool) (string, bool) {
	if nonUTF8 {
		raw = cp437Decode(raw)
	}

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

// readSymlinkTarget reads the content of a symlink entry, which is the link
// target. It returns an error if the target is longer than
// maxSymlinkTargetLen or cannot be read.
func readSymlinkTarget(fh *zip.File) (string, error) {
	if fh.UncompressedSize64 > maxSymlinkTargetLen {
		return "", fmt.Errorf("symlink target too long (%d bytes)", fh.UncompressedSize64)
	}
	rc, err := fh.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()

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

// zipEntryMode returns the permission bits to store for an entry. Archives
// created on Unix (or macOS) carry the mode in the external attributes; for
// all other creators sensible defaults are used (0644 files, 0755 dirs).
func zipEntryMode(fh *zip.File, isDir bool) os.FileMode {
	if creator := fh.CreatorVersion >> 8; creator == zipCreatorUnix || creator == zipCreatorMacOSX {
		return fh.Mode()
	}
	if isDir {
		return os.ModeDir | 0o755
	}
	return 0o644
}

// addEntry indexes e. A second entry with the same path and same type
// replaces the first one (last one wins, like `unzip -o`); an entry whose
// path conflicts with an existing entry of a different type, or that would
// live inside a regular file, is rejected.
func (z *Zip) addEntry(e *zipEntry) bool {
	if prev, ok := z.entries[e.path]; ok {
		if prev.isDir != e.isDir {
			z.warn("zip: entry %q ignored, name conflicts with existing %s", e.path, kindOf(prev))
			return false
		}
		z.warn("zip: duplicate entry %q, keeping the last one", e.path)
	}

	// the path must not cross an existing regular file or symlink
	for dir := path.Dir(e.path); dir != "/"; dir = path.Dir(dir) {
		if p, ok := z.entries[dir]; ok && !p.isDir {
			z.warn("zip: entry %q ignored, parent %q is not a directory", e.path, dir)
			return false
		}
	}

	if _, ok := z.entries[e.path]; !ok {
		z.children[path.Dir(e.path)] = append(z.children[path.Dir(e.path)], path.Base(e.path))
	}
	z.entries[e.path] = e
	return true
}

func kindOf(e *zipEntry) string {
	switch {
	case e.isDir:
		return "directory"
	case e.fi.Mode&os.ModeSymlink != 0:
		return "symlink"
	default:
		return "file"
	}
}

// OpenFile implements FS.
func (z *Zip) OpenFile(name string, flag int, _ bool) (File, error) {
	if flag&^(O_RDONLY|O_NOFOLLOW|O_DIRECTORY) != 0 {
		return nil, pathError("open", name, fmt.Errorf("invalid combination of flags 0x%x", flag))
	}

	if z.closed {
		return nil, pathError("open", name, os.ErrClosed)
	}

	name = zipCleanPath(name)
	e, ok := z.entries[name]
	if !ok {
		return nil, pathError("open", name, syscall.ENOENT)
	}
	if flag&O_DIRECTORY != 0 && !e.isDir {
		return nil, pathError("open", name, syscall.ENOTDIR)
	}

	if e.isDir {
		return &zipDir{
			name:  name,
			fi:    e.fi,
			names: slices.Clone(z.children[name]),
		}, nil
	}
	return &zipFile{
		name:  name,
		entry: e,
	}, nil
}

// Lstat implements FS.
func (z *Zip) Lstat(name string) (*ExtendedFileInfo, error) {
	name = zipCleanPath(name)
	e, ok := z.entries[name]
	if !ok {
		return nil, pathError("lstat", name, os.ErrNotExist)
	}
	return e.fi, nil
}

// Join implements FS.
func (z *Zip) Join(elem ...string) string {
	return path.Join(elem...)
}

// Separator implements FS.
func (z *Zip) Separator() string {
	return "/"
}

// IsAbs implements FS.
func (z *Zip) IsAbs(p string) bool {
	return path.IsAbs(p)
}

// Abs implements FS.
func (z *Zip) Abs(p string) (string, error) {
	return zipCleanPath(p), nil
}

// Clean implements FS.
func (z *Zip) Clean(p string) string {
	return path.Clean(p)
}

// VolumeName implements FS. The virtual filesystem has no volumes.
func (z *Zip) VolumeName(string) string {
	return ""
}

// Dir implements FS.
func (z *Zip) Dir(p string) string {
	return path.Dir(p)
}

// Base implements FS.
func (z *Zip) Base(p string) string {
	return path.Base(p)
}

// nodeFromEntry builds the data.Node for a zip entry. The metadata comes
// from the in-memory central directory index, so Stat() and ToNode() are
// always consistent.
func nodeFromEntry(name string, e *zipEntry) *data.Node {
	node := buildBasicNode(name, e.fi)
	node.AccessTime = e.fi.ModTime
	node.ChangeTime = e.fi.ModTime
	if e.fi.Mode&os.ModeSymlink != 0 {
		node.LinkTarget = e.linkTarget
		if !utf8.ValidString(e.linkTarget) {
			node.LinkTargetRaw = []byte(e.linkTarget)
		}
	}
	return node
}

// zipFile is a File backed by a single zip entry. The content is decompressed
// on the fly from the archive; nothing is written to disk.
type zipFile struct {
	name  string
	entry *zipEntry

	rc io.ReadCloser // lazily opened on first read
}

// statically ensure that zipFile implements File.
var _ File = (*zipFile)(nil)

// MakeReadable opens the (decompressing) reader for the entry's content. It
// is a no-op for entries that have no content to read.
func (f *zipFile) MakeReadable() error {
	if f.rc != nil {
		return nil
	}

	if f.entry.fi.Mode&os.ModeSymlink != 0 {
		f.rc = io.NopCloser(strings.NewReader(f.entry.linkTarget))
		return nil
	}

	rc, err := f.entry.header.Open()
	if err != nil {
		return pathError("open", f.name, err)
	}
	f.rc = rc
	return nil
}

func (f *zipFile) reader() (io.ReadCloser, error) {
	if f.rc == nil {
		if err := f.MakeReadable(); err != nil {
			return nil, err
		}
	}
	return f.rc, nil
}

// Read implements io.Reader.
func (f *zipFile) Read(p []byte) (int, error) {
	rc, err := f.reader()
	if err != nil {
		return 0, err
	}
	return rc.Read(p)
}

// Close implements io.Closer.
func (f *zipFile) Close() error {
	if f.rc == nil {
		return nil
	}
	err := f.rc.Close()
	f.rc = nil
	return err
}

// Readdirnames implements File.
func (f *zipFile) Readdirnames(_ int) ([]string, error) {
	return nil, pathError("readdirnames", f.name, os.ErrInvalid)
}

// Stat implements File.
func (f *zipFile) Stat() (*ExtendedFileInfo, error) {
	return f.entry.fi, nil
}

// ToNode implements File.
func (f *zipFile) ToNode(_ bool, _ func(format string, args ...any)) (*data.Node, error) {
	return nodeFromEntry(f.name, f.entry), nil
}

// zipDir is a directory of the virtual filesystem.
type zipDir struct {
	name  string
	fi    *ExtendedFileInfo
	names []string
}

// statically ensure that zipDir implements File.
var _ File = (*zipDir)(nil)

// MakeReadable implements File.
func (d *zipDir) MakeReadable() error {
	return nil
}

// Read implements io.Reader.
func (d *zipDir) Read(_ []byte) (int, error) {
	return 0, pathError("read", d.name, os.ErrInvalid)
}

// Close implements io.Closer.
func (d *zipDir) Close() error {
	return nil
}

// Readdirnames implements File.
func (d *zipDir) Readdirnames(n int) ([]string, error) {
	if n > 0 {
		return nil, pathError("readdirnames", d.name, os.ErrInvalid)
	}
	return slices.Clone(d.names), nil
}

// Stat implements File.
func (d *zipDir) Stat() (*ExtendedFileInfo, error) {
	return d.fi, nil
}

// ToNode implements File.
func (d *zipDir) ToNode(_ bool, _ func(format string, args ...any)) (*data.Node, error) {
	return nodeFromEntry(d.name, &zipEntry{path: d.name, isDir: true, fi: d.fi}), nil
}
