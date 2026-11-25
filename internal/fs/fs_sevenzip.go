package fs

import (
	"errors"
	"io"
	"os"
	"path"
	"strings"

	"github.com/bodgit/sevenzip"
)

// sevenZipStreamState tracks the validation state of one compressed folder
// (stream) of a 7z archive, which may hold several entries.
type sevenZipStreamState struct {
	verified bool
	err      error
}

// NewSevenZip opens the 7z archive at path and indexes its entries into a
// virtual filesystem. Nothing is extracted to disk: only the archive header
// is read (plus the small content of symlink entries). Entries whose names
// cannot be made safe (escaping the virtual root, NUL bytes, ...) or that
// belong to an unreadable stream (encrypted, unsupported codec, ...) are
// skipped with a warning via warnf.
func NewSevenZip(path string, warnf func(format string, args ...any)) (*MigrateFS, error) {
	return NewSevenZipMulti([]string{path}, warnf)
}

// NewSevenZipMulti opens the 7z archives at path(s) and indexes their
// entries into a single virtual filesystem. The result is equivalent to a
// single virtual archive whose entries are the concatenation of the entries
// of the given archives, in the order given: the usual rules apply to the
// concatenated entries (duplicate paths of the same type keep the last
// entry, entries conflicting with an existing entry of a different type are
// skipped with a warning). Like NewSevenZip, nothing is extracted to disk.
// All archives stay open until the returned MigrateFS is closed.
func NewSevenZipMulti(paths []string, warnf func(format string, args ...any)) (*MigrateFS, error) {
	if len(paths) == 0 {
		return nil, errors.New("no 7z archives given")
	}

	m := newMigrateFS(warnf, 0)
	for _, p := range paths {
		info := ArchiveInfo{Path: p, Label: "7z"}
		if err := m.openSevenZipArchive(p, &info); err != nil {
			_ = m.Close()
			return nil, err
		}
		m.archiveInfos = append(m.archiveInfos, info)
	}

	m.finalize()
	return m, nil
}

// openSevenZipArchive opens the 7z archive at path and indexes its entries
// into m. Used by NewMigrate, which probes the format first.
func (m *MigrateFS) openSevenZipArchive(path string, info *ArchiveInfo) error {
	zr, err := sevenzip.OpenReader(path)
	if err != nil {
		return err
	}
	m.addCloser(zr.Close)
	m.indexSevenZip(zr, info)
	return nil
}

// indexSevenZip indexes all entries of one 7z archive.
func (m *MigrateFS) indexSevenZip(zr *sevenzip.ReadCloser, info *ArchiveInfo) {
	streams := make(map[int]*sevenZipStreamState, len(zr.File))
	for _, f := range zr.File {
		if m.indexSevenZipEntry(f, streams) {
			info.Indexed++
		} else {
			info.Skipped++
		}
	}
}

// indexSevenZipEntry validates one 7z entry and indexes it if usable.
// Entries whose names cannot be made safe (escaping the virtual root, NUL
// bytes, ...) or that belong to an unreadable stream (encrypted, unsupported
// codec, ...) are skipped with a warning. It reports whether the entry was
// indexed.
func (m *MigrateFS) indexSevenZipEntry(f *sevenzip.File, streams map[int]*sevenZipStreamState) bool {
	name, ok := archiveCleanEntryName(f.Name)
	if !ok {
		m.warn("7z: entry %q ignored, unsafe or invalid path", f.Name)
		return false
	}

	mode := f.Mode()
	switch {
	case mode&os.ModeDevice != 0, mode&os.ModeNamedPipe != 0, mode&os.ModeSocket != 0:
		m.warn("7z: entry %q ignored, unsupported special file", f.Name)
		return false
	}

	isDir := mode.IsDir() || strings.HasSuffix(f.Name, "/")
	isSymlink := mode&os.ModeSymlink != 0

	mtime := f.Modified.UTC()
	e := &archiveEntry{
		path:  name,
		isDir: isDir,
		label: "7z",
		fi: &ExtendedFileInfo{
			Name:       path.Base(name),
			ModTime:    mtime,
			AccessTime: mtime,
			ChangeTime: mtime,
		},
	}

	if isDir {
		e.fi.Mode = mode
		return m.addEntry(e)
	}

	// A 7z folder (stream) may hold several entries and is decompressed
	// once, so errors (unsupported codec, encryption, corruption) are
	// detected by peeking the first entry of each stream; the rest of the
	// stream is then either trusted or rejected wholesale.
	st := streams[f.Stream]
	if st == nil {
		st = &sevenZipStreamState{}
		streams[f.Stream] = st
	}
	if st.err != nil {
		m.warn("7z: entry %q ignored: %v", f.Name, st.err)
		return false
	}

	if isSymlink {
		// the link content is the target; reading it also validates the
		// stream
		target, err := readLinkTarget(f.UncompressedSize, f.Open)
		if err != nil {
			m.failSevenZipStream(st, err)
			m.warn("7z: entry %q ignored: %v", f.Name, sevenZipStreamErrorMessage(st.err))
			return false
		}
		e.linkTarget = target
		e.fi.Mode = os.ModeSymlink | 0o777
		e.fi.Size = int64(len(target))
		st.verified = true
	} else {
		e.fi.Mode = mode
		e.fi.Size = int64(f.UncompressedSize)
		e.content = func() (io.ReadCloser, error) { return f.Open() }
		if !st.verified && f.UncompressedSize > 0 {
			if err := peekSevenZipEntry(f); err != nil {
				m.failSevenZipStream(st, err)
				m.warn("7z: entry %q ignored: %v", f.Name, sevenZipStreamErrorMessage(st.err))
				return false
			}
			st.verified = true
		}
	}

	return m.addEntry(e)
}

// failSevenZipStream records a validation error for the stream, using a
// clearer message for encrypted archives.
func (m *MigrateFS) failSevenZipStream(st *sevenZipStreamState, err error) {
	var re *sevenzip.ReadError
	if errors.As(err, &re) && re.Encrypted {
		st.err = errors.New("entry is encrypted")
		return
	}
	st.err = err
}

// sevenZipStreamErrorMessage formats a recorded stream error.
func sevenZipStreamErrorMessage(err error) string {
	if err == nil {
		return "unreadable stream"
	}
	return err.Error()
}

// peekSevenZipEntry opens the entry and reads a single byte so that codec,
// encryption and stream errors surface at indexing time instead of failing
// the backup at read time. The reader is closed again; the sevenzip library
// pools folder readers, so the cost of the peek is negligible.
func peekSevenZipEntry(f *sevenzip.File) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	buf := make([]byte, 1)
	_, rerr := rc.Read(buf)
	cerr := rc.Close()
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return rerr
	}
	return cerr
}
