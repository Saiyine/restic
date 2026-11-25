package fs

import (
	"archive/tar"
	"compress/bzip2"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// tarCompression is the wrapper of a tar archive, or the compression of a
// standalone compressed file.
type tarCompression int

const (
	tarPlain tarCompression = iota
	tarGzip
	tarXz
	tarBzip2
	tarZstd
)

// tarLabel is the warning label of a tar-based archive with the given
// wrapper.
func tarLabel(comp tarCompression) string {
	switch comp {
	case tarGzip:
		return "tar.gz"
	case tarXz:
		return "tar.xz"
	case tarBzip2:
		return "tar.bz2"
	case tarZstd:
		return "tar.zst"
	default:
		return "tar"
	}
}

// singleLabel is the warning label for a standalone (non-tar) compressed
// file of the given wrapper.
func singleLabel(comp tarCompression) string {
	switch comp {
	case tarXz:
		return "xz"
	case tarBzip2:
		return "bz2"
	case tarZstd:
		return "zst"
	default:
		return "gz"
	}
}

// xzReadCloser pairs an xz.Reader with the file it reads from, so that
// closing it also closes the file.
type xzReadCloser struct {
	*xz.Reader
	f *os.File
}

func (c *xzReadCloser) Close() error {
	return c.f.Close()
}

// fileReadCloser pairs a decompression reader with the file it reads from,
// so that closing the reader also closes the file (the stdlib bzip2 reader
// has no Close method and the zstd decoder does not close its input).
type fileReadCloser struct {
	io.Reader
	f *os.File
}

func (c *fileReadCloser) Close() error {
	return c.f.Close()
}

// openTarStream opens the file at path and returns a reader over its
// decompressed content. Closing the returned reader closes the file.
func openTarStream(path string, comp tarCompression) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	switch comp {
	case tarPlain:
		return f, nil
	case tarGzip:
		gz, err := gzip.NewReader(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		return gz, nil
	case tarXz:
		xzr, err := xz.NewReader(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		return &xzReadCloser{Reader: xzr, f: f}, nil
	case tarBzip2:
		return &fileReadCloser{Reader: bzip2.NewReader(f), f: f}, nil
	case tarZstd:
		zr, err := zstd.NewReader(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		return &fileReadCloser{Reader: zr, f: f}, nil
	}
	_ = f.Close()
	return nil, errors.New("internal error: unknown tar compression")
}

// tarArchive gives lazy, on-demand access to one indexed tar archive (or
// standalone compressed file). Each content read gets its own decompressed
// stream over the archive, so nothing is shared between concurrent readers.
// To avoid re-decompressing from the start for every entry, the last-used
// stream is kept around as a forward cursor: an entry read that lies ahead
// of the cursor adopts the cursor stream, while any earlier entry gets a
// fresh stream. Uncompressed archives can seek, so adopting is cheap there.
// Memory usage stays bounded (at most a few open streams) no matter how
// large the archive is.
type tarArchive struct {
	path string
	comp tarCompression

	mu      sync.Mutex
	cur     io.ReadCloser // the last-used stream; nil when none is kept
	curPos  uint64        // position of cur in the decompressed stream
	curBusy bool          // cur is currently being read by an entry reader
}

// openAt opens a fresh decompressed stream positioned at start.
func (a *tarArchive) openAt(start uint64) (io.ReadCloser, error) {
	rc, err := openTarStream(a.path, a.comp)
	if err != nil {
		return nil, err
	}
	if start > 0 {
		if s, ok := rc.(io.Seeker); ok && a.comp == tarPlain {
			if _, err := s.Seek(int64(start), io.SeekStart); err != nil {
				_ = rc.Close()
				return nil, err
			}
			return rc, nil
		}
		if _, err := io.CopyN(io.Discard, rc, int64(start)); err != nil {
			_ = rc.Close()
			return nil, err
		}
	}
	return rc, nil
}

// openContent returns a reader positioned at the start of the entry whose
// content occupies [start, start+size) of the decompressed stream. The
// reader must be closed; if it is fully read, its stream is kept as the
// forward cursor for the next read.
func (a *tarArchive) openContent(start, size uint64) (io.ReadCloser, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	adopt := false
	if a.cur != nil && !a.curBusy && start >= a.curPos {
		// the cursor stream is free and at or before the entry: adopt it
		// after fast-forwarding
		if _, err := io.CopyN(io.Discard, a.cur, int64(start-a.curPos)); err == nil {
			adopt = true
		} else {
			_ = a.cur.Close()
			a.cur = nil
		}
	}

	var stream io.ReadCloser
	if adopt {
		stream = a.cur
	} else {
		var err error
		stream, err = a.openAt(start)
		if err != nil {
			return nil, err
		}
		if a.cur != nil && !a.curBusy {
			// the cursor stream is free and stale: replace it
			_ = a.cur.Close()
			a.cur = stream
			a.curPos = start
			a.curBusy = true
		} else {
			// the cursor stream is in use (or absent): keep the new stream
			// private to this reader
			return &tarFileReader{arc: a, r: io.LimitReader(stream, int64(size)), stream: stream, private: true, start: start, size: size}, nil
		}
	}
	a.curPos = start
	a.curBusy = true
	return &tarFileReader{arc: a, r: io.LimitReader(stream, int64(size)), stream: stream, start: start, size: size}, nil
}

// closeStream closes the archive's cursor stream, if any.
func (a *tarArchive) closeStream() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cur == nil {
		return nil
	}
	err := a.cur.Close()
	a.cur = nil
	return err
}

// tarFileReader reads one entry through a limited view of a decompressed
// stream.
type tarFileReader struct {
	arc     *tarArchive
	r       io.Reader // the entry's content, capped at its size
	stream  io.ReadCloser
	private bool // the stream is private (not the archive's cursor)
	start   uint64
	size    uint64
	read    uint64
}

func (r *tarFileReader) Read(p []byte) (int, error) {
	if r.stream == nil {
		return 0, os.ErrClosed
	}
	n, err := r.r.Read(p)
	r.read += uint64(n)
	return n, err
}

func (r *tarFileReader) Close() error {
	if r.stream == nil {
		return nil
	}
	stream := r.stream
	r.stream = nil
	r.arc.mu.Lock()
	defer r.arc.mu.Unlock()

	if r.private {
		return stream.Close()
	}
	if r.read != r.size {
		// the entry was abandoned mid-read: the cursor stream's position
		// is unknown, so close it
		err := stream.Close()
		r.arc.cur = nil
		return err
	}
	// fully read: keep the cursor stream open for the next entry
	r.arc.curPos = r.start + r.size
	r.arc.curBusy = false
	return nil
}

// countReader counts the bytes read through it.
type countReader struct {
	r io.Reader
	n uint64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += uint64(n)
	return n, err
}

// openTarArchive indexes the entries of the tar archive at path (with the
// given wrapper) into m. Nothing is extracted to disk: the archive is
// walked once to learn the entries' positions in the (decompressed)
// stream, which also validates the stream; the entries' content is
// decompressed on the fly, one stream per opened entry.
func (m *MigrateFS) openTarArchive(path string, comp tarCompression) error {
	arc := &tarArchive{path: path, comp: comp}
	m.addCloser(arc.closeStream)

	rc, err := openTarStream(path, comp)
	if err != nil {
		return err
	}
	counter := &countReader{r: rc}
	tr := tar.NewReader(counter)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if errors.Is(err, tar.ErrInsecurePath) {
			// raised when GODEBUG=tarinsecurepath=0 and the entry name is
			// not local; the header is still valid, so index the entry and
			// let archiveCleanEntryName sanitize or reject the name
			if hdr == nil {
				return fmt.Errorf("%s: %v", path, err)
			}
		} else if err != nil {
			return fmt.Errorf("%s: %v", path, err)
		}
		// counter.n now points at the start of this entry's content
		// (the header and any preceding PAX/GNU metadata blocks have been
		// consumed)
		m.indexTarEntry(arc, hdr, counter.n)
	}
	return rc.Close()
}

// indexTarEntry indexes one tar entry. Entries whose names cannot be made
// safe (escaping the virtual root, NUL bytes, ...) or that are of an
// unsupported type are skipped with a warning.
func (m *MigrateFS) indexTarEntry(arc *tarArchive, hdr *tar.Header, dataStart uint64) {
	label := tarLabel(arc.comp)
	name, ok := archiveCleanEntryName(hdr.Name)
	if !ok {
		m.warn("%s: entry %q ignored, unsafe or invalid path", label, hdr.Name)
		return
	}

	mtime := hdr.ModTime.UTC()
	base := path.Base(name)

	switch hdr.Typeflag {
	case tar.TypeDir:
		m.addEntry(&archiveEntry{
			path:  name,
			isDir: true,
			label: label,
			fi:    &ExtendedFileInfo{Name: base, Mode: os.FileMode(hdr.Mode) | os.ModeDir, ModTime: mtime, AccessTime: mtime, ChangeTime: mtime},
		})

	case tar.TypeSymlink:
		// the target is in the Linkname field; the tar reader treats
		// symlinks as header-only, so there is no entry content to read
		target := hdr.Linkname
		if target == "" {
			m.warn("%s: entry %q ignored, empty symlink target", label, hdr.Name)
			return
		}
		m.addEntry(&archiveEntry{
			path:       name,
			label:      label,
			linkTarget: target,
			fi:         &ExtendedFileInfo{Name: base, Mode: os.ModeSymlink | 0o777, Size: int64(len(target)), ModTime: mtime, AccessTime: mtime, ChangeTime: mtime},
		})

	case tar.TypeLink:
		// hard link: the target precedes the link in the archive
		tname, ok := archiveCleanEntryName(hdr.Linkname)
		if !ok {
			m.warn("%s: entry %q ignored, unsafe or invalid link target", label, hdr.Name)
			return
		}
		t, ok := m.entries[tname]
		if !ok || t.isDir {
			m.warn("%s: entry %q ignored, hard link target %q not found", label, hdr.Name, hdr.Linkname)
			return
		}
		e := &archiveEntry{
			path:  name,
			label: label,
			fi:    &ExtendedFileInfo{Name: base, Mode: t.fi.Mode, ModTime: mtime, AccessTime: mtime, ChangeTime: mtime},
		}
		if t.fi.Mode&os.ModeSymlink != 0 {
			// a hard link to a symlink is stored as a regular file whose
			// content is the target
			e.fi.Mode = 0o644
			e.fi.Size = int64(len(t.linkTarget))
			e.content = func() (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(t.linkTarget)), nil
			}
		} else {
			e.fi.Size = t.fi.Size
			e.content = t.content
		}
		m.addEntry(e)

	// TypeRegA is the legacy '1' regular-file type: deprecated by
	// archive/tar but still present in old tar archives, so it must be
	// accepted.
	case tar.TypeReg, tar.TypeRegA, tar.TypeGNUSparse: //nolint:staticcheck
		m.addEntry(&archiveEntry{
			path:  name,
			label: label,
			fi:    &ExtendedFileInfo{Name: base, Mode: os.FileMode(hdr.Mode), Size: hdr.Size, ModTime: mtime, AccessTime: mtime, ChangeTime: mtime},
			content: func() (io.ReadCloser, error) {
				return arc.openContent(dataStart, uint64(hdr.Size))
			},
		})

	default:
		m.warn("%s: entry %q ignored, unsupported special file", label, hdr.Name)
	}
}

// openSingleFile indexes a standalone compressed file (a .gz or .xz that
// does not contain a tar) as a single virtual file named after the archive
// (e.g. "report.gz" becomes "/report"). A full decompression pass measures
// the size of the uncompressed data and validates the stream.
func (m *MigrateFS) openSingleFile(path string, comp tarCompression) error {
	label := singleLabel(comp)

	arc := &tarArchive{path: path, comp: comp}
	m.addCloser(arc.closeStream)

	rc, err := openTarStream(path, comp)
	if err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	n, err := io.Copy(io.Discard, rc)
	_ = rc.Close()
	if err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	mtime := fi.ModTime().UTC()
	if comp == tarGzip {
		if t := gzipHeaderMtime(path); t != nil {
			mtime = *t
		}
	}

	name := singleFileName(path)
	m.addEntry(&archiveEntry{
		path:  "/" + name,
		label: label,
		fi:    &ExtendedFileInfo{Name: name, Mode: 0o644, Size: n, ModTime: mtime, AccessTime: mtime, ChangeTime: mtime},
		content: func() (io.ReadCloser, error) {
			return arc.openContent(0, uint64(n))
		},
	})
	return nil
}

// singleFileName derives the virtual file name of a standalone compressed
// file from the archive file name, stripping the compression suffix.
func singleFileName(archivePath string) string {
	base := path.Base(archivePath)
	for _, suf := range []string{
		".tar.gz", ".tgz",
		".tar.xz", ".txz",
		".tar.bz2", ".tbz", ".tbz2",
		".tar.zst", ".tzst",
		".gz", ".xz", ".bz2", ".zst",
	} {
		if strings.HasSuffix(base, suf) {
			if name := strings.TrimSuffix(base, suf); name != "" {
				return name
			}
		}
	}
	return base
}

// gzipHeaderMtime returns the original file mtime stored in a gzip header,
// or nil when it is not set.
func gzipHeaderMtime(path string) *time.Time {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var hdr [10]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return nil
	}
	// magic, compression method (deflate), then the 4-byte mtime
	if hdr[0] != 0x1F || hdr[1] != 0x8B || hdr[2] != 8 {
		return nil
	}
	ms := binary.LittleEndian.Uint32(hdr[4:8])
	if ms == 0 {
		return nil
	}
	t := time.Unix(int64(ms), 0).UTC()
	return &t
}
