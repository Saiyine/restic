package fs

import (
	"archive/zip"
	"encoding/binary"
	"errors"
	"os"
	"path"
	"strings"
	"time"
)

// zipCreatorUnix and zipCreatorMacOSX mirror the creator-version values used
// by archive/zip to decide whether ExternalAttrs carries Unix mode bits.
const (
	zipCreatorUnix   = 3
	zipCreatorMacOSX = 19
)

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

// zipCleanEntryName normalizes a raw zip entry name to an absolute path
// within the virtual root. Non-UTF-8 names are decoded from CP437 first; the
// rest of the sanitization is shared with the other archive formats.
func zipCleanEntryName(raw string, nonUTF8 bool) (string, bool) {
	if nonUTF8 {
		raw = cp437Decode(raw)
	}
	return archiveCleanEntryName(raw)
}

// NewZip opens the zip archive at zipPath and indexes its central directory.
// Nothing is extracted to disk: only the central directory is read (plus the
// small content of symlink entries). Entries whose names cannot be made safe
// (escaping the virtual root, NUL bytes, ...), or that are encrypted or use
// an unsupported compression method, are skipped with a warning via warnf.
func NewZip(zipPath string, warnf func(format string, args ...any)) (*MigrateFS, error) {
	return NewZipMulti([]string{zipPath}, warnf)
}

// NewZipMulti opens the zip archives at zipPaths and indexes their central
// directories into a single virtual filesystem. The result is equivalent to
// a single virtual archive whose entries are the concatenation of the
// entries of the given archives, in the order given: the usual rules apply
// to the concatenated entries (duplicate paths of the same type keep the
// last entry, entries conflicting with an existing entry of a different
// type are skipped with a warning). Like NewZip, nothing is extracted to
// disk. All archives stay open until the returned MigrateFS is closed.
func NewZipMulti(zipPaths []string, warnf func(format string, args ...any)) (*MigrateFS, error) {
	if len(zipPaths) == 0 {
		return nil, errors.New("no zip archives given")
	}

	m := newMigrateFS(warnf, 0)
	for _, p := range zipPaths {
		info := ArchiveInfo{Path: p, Label: "zip"}
		if err := m.openZipArchive(p, &info); err != nil {
			_ = m.Close()
			return nil, err
		}
		m.archiveInfos = append(m.archiveInfos, info)
	}

	m.finalize()
	return m, nil
}

// openZipArchive opens the zip archive at path and indexes its entries into
// m. Used by NewMigrate, which probes the format first.
func (m *MigrateFS) openZipArchive(path string, info *ArchiveInfo) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	m.addCloser(zr.Close)
	for _, fh := range zr.File {
		if m.indexZipEntry(fh) {
			info.Indexed++
		} else {
			info.Skipped++
		}
	}
	return nil
}

// indexZipEntry validates one zip entry and indexes it if usable. Entries
// whose names cannot be made safe (escaping the virtual root, NUL bytes,
// ...), or that are encrypted or use an unsupported compression method, are
// skipped with a warning. It reports whether the entry was indexed.
func (m *MigrateFS) indexZipEntry(fh *zip.File) bool {
	name, ok := zipCleanEntryName(fh.Name, fh.NonUTF8)
	if !ok {
		m.warn("zip: entry %q ignored, unsafe or invalid path", fh.Name)
		return false
	}

	if fh.Flags&zipEncryptedFlag != 0 {
		m.warn("zip: entry %q ignored, entry is encrypted", fh.Name)
		return false
	}
	if fh.Method != zip.Store && fh.Method != zip.Deflate {
		m.warn("zip: entry %q ignored, unsupported compression method %d", fh.Name, fh.Method)
		return false
	}

	mode := fh.Mode()
	isDir := mode.IsDir()
	isSymlink := mode&os.ModeSymlink != 0

	mtime := fh.Modified.UTC()
	extAtime, extCtime := zipExtTimes(fh.Extra)
	atime, ctime := mtime, mtime
	if !extAtime.IsZero() {
		atime = extAtime
	}
	if !extCtime.IsZero() {
		ctime = extCtime
	}
	e := &archiveEntry{
		path:  name,
		isDir: isDir,
		label: "zip",
		fi: &ExtendedFileInfo{
			Name:       path.Base(name),
			ModTime:    mtime,
			AccessTime: atime,
			ChangeTime: ctime,
		},
	}
	if isSymlink {
		target, err := readLinkTarget(fh.UncompressedSize64, fh.Open)
		if err != nil {
			m.warn("zip: entry %q ignored: %v", fh.Name, err)
			return false
		}
		e.linkTarget = target
		e.fi.Mode = os.ModeSymlink | 0o777
		e.fi.Size = int64(len(target))
	} else {
		e.fi.Mode = zipEntryMode(fh, isDir)
		if !isDir {
			e.fi.Size = int64(fh.UncompressedSize64)
			e.content = fh.Open
		}
	}

	return m.addEntry(e)
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

// extTimeExtraTag is the zip extra-field tag that carries Unix
// timestamps ("Extended timestamp" block, as written by Info-ZIP and by
// Go's zip writer).
const extTimeExtraTag = 0x5455

// zipExtTimes returns the entry's extended-timestamp access and change
// times from the 0x5455 extra fields, if any of them carries them. Each
// block's first byte flags which times are present (bit 0 mtime, bit 1
// atime, bit 2 ctime), stored in that order as big-endian uint32 Unix
// seconds. The blocks' mtimes are ignored: archive/zip already prefers
// them over the MS-DOS field for ModTime.
func zipExtTimes(extra []byte) (atime, ctime time.Time) {
	for len(extra) >= 4 {
		tag := binary.LittleEndian.Uint16(extra[:2])
		size := int(binary.LittleEndian.Uint16(extra[2:4]))
		if size < 1 || len(extra) < 4+size {
			return
		}
		if tag == extTimeExtraTag {
			p := extra[4 : 4+size]
			flags := p[0]
			i := 1
			if flags&0x01 != 0 && size-i >= 4 { // mtime
				i += 4
			}
			if flags&0x02 != 0 && size-i >= 4 { // atime
				if a := time.Unix(int64(binary.BigEndian.Uint32(p[i:i+4])), 0).UTC(); atime.IsZero() {
					atime = a
				}
				i += 4
			}
			if flags&0x04 != 0 && size-i >= 4 { // ctime
				if c := time.Unix(int64(binary.BigEndian.Uint32(p[i:i+4])), 0).UTC(); ctime.IsZero() {
					ctime = c
				}
			}
		}
		extra = extra[4+size:]
	}
	return
}
