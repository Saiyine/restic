package fs

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/restic/restic/internal/test"
)

// sevenzipTestdata is the directory of the committed .7z fixtures (see
// testdata/sevenzip/README.md for how to regenerate them).
const sevenzipTestdata = "testdata/sevenzip"

func openTestSevenZip(t testing.TB, names []string, warnings *[]string) *MigrateFS {
	t.Helper()
	paths := make([]string, len(names))
	for i, n := range names {
		paths[i] = filepath.Join(sevenzipTestdata, n)
	}
	m, err := NewSevenZipMulti(paths, func(format string, args ...any) {
		*warnings = append(*warnings, fmt.Sprintf(format, args...))
	})
	test.OK(t, err)
	t.Cleanup(func() { test.OK(t, m.Close()) })
	return m
}

// TestSevenZipHierarchy verifies the basic fixture: explicit and implicit
// directories, modes, mtimes and file content of an LZMA2 archive.
func TestSevenZipHierarchy(t *testing.T) {
	var warnings []string
	m := openTestSevenZip(t, []string{"basic.7z"}, &warnings)
	test.Equals(t, 0, len(warnings))

	fi, err := m.Lstat("/top.txt")
	test.OK(t, err)
	test.Equals(t, "top.txt", fi.Name)
	test.Equals(t, os.FileMode(0o640), fi.Mode)
	test.Equals(t, int64(3), fi.Size)
	test.Equals(t, modTimeA, fi.ModTime)

	fi, err = m.Lstat("/docs")
	test.OK(t, err)
	test.Equals(t, os.FileMode(os.ModeDir|0o755), fi.Mode)
	test.Equals(t, modTimeA, fi.ModTime)
	f, err := m.OpenFile("/docs", O_RDONLY|O_DIRECTORY, false)
	test.OK(t, err)
	names, err := f.Readdirnames(-1)
	test.OK(t, err)
	test.Equals(t, []string{"readme.md"}, names)
	test.OK(t, f.Close())

	test.Equals(t, "hello world", readWholeFile(t, m, "/docs/readme.md"))

	// implicit directories inherit the newest descendant's mtime
	for _, p := range []string{"/deep", "/deep/nested"} {
		fi, err = m.Lstat(p)
		test.OK(t, err)
		test.Equals(t, os.FileMode(os.ModeDir|0o755), fi.Mode)
		test.Equals(t, modTimeB, fi.ModTime)
	}
	test.Equals(t, "file.bin", firstChild(t, m, "/deep/nested"))
	test.Equals(t, string([]byte{0, 1, 2, 3}), readWholeFile(t, m, "/deep/nested/file.bin"))
	fi, err = m.Lstat("/deep/nested/file.bin")
	test.OK(t, err)
	test.Equals(t, os.FileMode(0o600), fi.Mode)

	fi, err = m.Lstat("/empty.bin")
	test.OK(t, err)
	test.Equals(t, int64(0), fi.Size)
	f, err = m.OpenFile("/empty.bin", O_RDONLY|O_NOFOLLOW, false)
	test.OK(t, err)
	n, err := f.Read(make([]byte, 16))
	test.Equals(t, 0, n, "expected no data for an empty file")
	test.Assert(t, err != nil, "expected EOF for an empty file")
	test.OK(t, f.Close())

	f, err = m.OpenFile("/", O_RDONLY|O_DIRECTORY, false)
	test.OK(t, err)
	names, err = f.Readdirnames(-1)
	test.OK(t, err)
	test.Equals(t, []string{"deep", "docs", "empty.bin", "top.txt"}, names)
	test.OK(t, f.Close())
}

func firstChild(t testing.TB, m *MigrateFS, dir string) string {
	t.Helper()
	f, err := m.OpenFile(dir, O_RDONLY|O_DIRECTORY, false)
	test.OK(t, err)
	names, err := f.Readdirnames(-1)
	test.OK(t, err)
	test.OK(t, f.Close())
	test.Equals(t, 1, len(names))
	return names[0]
}

// TestSevenZipCodecs verifies that the common 7z codecs are decoded on the
// fly: Zstd, LZMA and stored (Copy).
func TestSevenZipCodecs(t *testing.T) {
	var warnings []string
	for _, tc := range []struct {
		name string
		want string
	}{
		{"zstd.7z", "zstd data here\n"},
		{"lzma.7z", "lzma data here\n"},
		{"store.7z", "0123456789" + strings.Repeat("0123456789", 15) + "!\n"},
	} {
		var w []string
		m := openTestSevenZip(t, []string{tc.name}, &w)
		test.Equals(t, 0, len(w))
		test.Equals(t, tc.want, readWholeFile(t, m, "/data.txt"))
		warnings = append(warnings, w...)
	}
	test.Equals(t, 0, len(warnings))
}

// TestSevenZipModes verifies that Unix permissions are preserved.
func TestSevenZipModes(t *testing.T) {
	var warnings []string
	m := openTestSevenZip(t, []string{"modes.7z"}, &warnings)
	test.Equals(t, 0, len(warnings))

	for _, tc := range []struct {
		path string
		mode os.FileMode
	}{
		{"/secret", os.ModeDir | 0o700},
		{"/secret/hidden.txt", 0o600},
		{"/script.sh", 0o755},
	} {
		fi, err := m.Lstat(tc.path)
		test.OK(t, err)
		test.Equals(t, tc.mode, fi.Mode, tc.path)
	}
}

// TestSevenZipSymlink verifies that a symlink entry (sIFLNK attribute) is
// exposed as a symlink whose content is the link target.
func TestSevenZipSymlink(t *testing.T) {
	var warnings []string
	m := openTestSevenZip(t, []string{"symlink.7z"}, &warnings)
	test.Equals(t, 0, len(warnings))

	fi, err := m.Lstat("/link.txt")
	test.OK(t, err)
	test.Equals(t, os.FileMode(os.ModeSymlink|0o777), fi.Mode)
	test.Equals(t, int64(len("target.txt")), fi.Size)

	// reading the symlink yields the target, not the linked file's content
	f, err := m.OpenFile("/link.txt", O_RDONLY|O_NOFOLLOW, false)
	test.OK(t, err)
	buf := make([]byte, 64)
	n, err := f.Read(buf)
	test.OK(t, err)
	test.Equals(t, "target.txt", string(buf[:n]))
	test.OK(t, f.Close())

	test.Equals(t, "the target", readWholeFile(t, m, "/target.txt"))
}

// TestSevenZipUnicode verifies that non-ASCII (UTF-8) entry names work.
func TestSevenZipUnicode(t *testing.T) {
	var warnings []string
	m := openTestSevenZip(t, []string{"unicode.7z"}, &warnings)
	test.Equals(t, 0, len(warnings))
	test.Equals(t, "contenidor\n", readWholeFile(t, m, "/café/ñandú.txt"))
}

// TestSevenZipEmptyArchive verifies that an archive without entries yields a
// virtual filesystem with only the root.
func TestSevenZipEmptyArchive(t *testing.T) {
	var warnings []string
	m := openTestSevenZip(t, []string{"empty.7z"}, &warnings)
	test.Equals(t, 0, len(warnings))

	fi, err := m.Lstat("/")
	test.OK(t, err)
	test.Equals(t, os.FileMode(os.ModeDir|0o755), fi.Mode)

	f, err := m.OpenFile("/", O_RDONLY|O_DIRECTORY, false)
	test.OK(t, err)
	names, err := f.Readdirnames(-1)
	test.OK(t, err)
	test.Equals(t, 0, len(names))
	test.OK(t, f.Close())

	_, err = m.Lstat("/anything")
	test.Assert(t, err != nil, "expected an error for a missing entry")
}

// TestSevenZipDuplicateLastWins verifies that, across several archives, a
// path present in both keeps the entry of the last one.
func TestSevenZipDuplicateLastWins(t *testing.T) {
	var warnings []string
	m := openTestSevenZip(t, []string{"dup_a.7z", "dup_b.7z"}, &warnings)
	test.Equals(t, 1, len(warnings))
	test.Equals(t, "version B\n", readWholeFile(t, m, "/same.txt"))

	fi, err := m.Lstat("/same.txt")
	test.OK(t, err)
	test.Equals(t, modTimeB, fi.ModTime)

	test.Equals(t, "only in a\n", readWholeFile(t, m, "/only_a.txt"))
	test.Equals(t, "only in b\n", readWholeFile(t, m, "/only_b.txt"))
}

// TestSevenZipTypeConflict verifies that a file/directory conflict between
// archives keeps the first occurrence, in both orders.
func TestSevenZipTypeConflict(t *testing.T) {
	// later file against earlier directory: the directory is kept
	var warnings []string
	m := openTestSevenZip(t, []string{"conflict_dir.7z", "conflict_file.7z"}, &warnings)
	test.Equals(t, 1, len(warnings))
	fi, err := m.Lstat("/d")
	test.OK(t, err)
	test.Equals(t, true, fi.Mode.IsDir())
	test.Equals(t, "inside\n", readWholeFile(t, m, "/d/inside.txt"))

	// later directory against earlier file: the file is kept, the directory
	// and its entries are skipped
	var warnings2 []string
	m = openTestSevenZip(t, []string{"conflict_file.7z", "conflict_dir.7z"}, &warnings2)
	test.Equals(t, 2, len(warnings2))
	fi, err = m.Lstat("/d")
	test.OK(t, err)
	test.Equals(t, false, fi.Mode.IsDir())
	test.Equals(t, "a file\n", readWholeFile(t, m, "/d"))
	_, err = m.Lstat("/d/inside.txt")
	test.Assert(t, err != nil, "entry under the skipped directory must not exist, got %v", err)
}

// TestSevenZipEncrypted verifies that entries of an encrypted archive are
// skipped with a warning, leaving an empty snapshot.
func TestSevenZipEncrypted(t *testing.T) {
	var warnings []string
	m := openTestSevenZip(t, []string{"encrypted.7z"}, &warnings)
	test.Equals(t, 1, len(warnings))
	test.Assert(t, strings.Contains(warnings[0], "encrypted"),
		"warning should mention encryption, got %q", warnings[0])

	_, err := m.Lstat("/data.txt")
	test.Assert(t, err != nil, "encrypted entry must not be indexed")
	f, err := m.OpenFile("/", O_RDONLY|O_DIRECTORY, false)
	test.OK(t, err)
	names, err := f.Readdirnames(-1)
	test.OK(t, err)
	test.Equals(t, 0, len(names))
	test.OK(t, f.Close())
}

// TestSevenZipCorruptContent verifies that an entry whose compressed stream
// is corrupt is rejected at indexing time, with a warning per entry.
func TestSevenZipCorruptContent(t *testing.T) {
	orig, err := os.ReadFile(filepath.Join(sevenzipTestdata, "solid.7z"))
	test.OK(t, err)

	// flip a byte in the middle of the payload (before the header)
	var off uint64
	for i := 7; i >= 0; i-- {
		off = off<<8 | uint64(orig[12+i])
	}
	mid := 32 + int(off/2)
	orig[mid] ^= 0xFF

	corruptPath := filepath.Join(t.TempDir(), "solid.7z")
	test.OK(t, os.WriteFile(corruptPath, orig, 0o644))

	var warnings []string
	m, err := NewSevenZip(corruptPath, func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	})
	test.OK(t, err)
	t.Cleanup(func() { test.OK(t, m.Close()) })

	test.Equals(t, 3, len(warnings))
	for _, w := range warnings {
		test.Assert(t, strings.Contains(w, "ignored"), "expected a skip warning, got %q", w)
	}
	_, err = m.Lstat("/part1.txt")
	test.Assert(t, err != nil, "corrupt entry must not be indexed")
}

// TestSevenZipConcurrentRead reads the fixtures from many goroutines at the
// same time; run with -race.
func TestSevenZipConcurrentRead(t *testing.T) {
	var warnings []string
	m := openTestSevenZip(t, []string{"basic.7z", "large.7z"}, &warnings)

	fis := make(map[string]*ExtendedFileInfo)
	for _, p := range []string{"/top.txt", "/docs/readme.md", "/deep/nested/file.bin", "/empty.bin", "/large.bin"} {
		fi, err := m.Lstat(p)
		test.OK(t, err)
		fis[p] = fi
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p, fi := range fis {
				f, err := m.OpenFile(p, O_RDONLY|O_NOFOLLOW, false)
				if err != nil {
					errs <- err
					return
				}
				data, err := io.ReadAll(f)
				cerr := f.Close()
				if err != nil || cerr != nil {
					errs <- fmt.Errorf("%s: read/close: %v / %v", p, err, cerr)
					return
				}
				if int64(len(data)) != fi.Size {
					errs <- fmt.Errorf("%s: size mismatch %d != %d", p, len(data), fi.Size)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestSevenZipNoDiskAccess verifies that NewSevenZip and entry reads never
// create files anywhere: the archive is read in place.
func TestSevenZipNoDiskAccess(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	dir := filepath.Dir(filepath.Join(sevenzipTestdata, "large.7z"))
	before, err := os.ReadDir(dir)
	test.OK(t, err)

	var warnings []string
	m := openTestSevenZip(t, []string{"large.7z"}, &warnings)
	fi, err := m.Lstat("/large.bin")
	test.OK(t, err)
	test.Equals(t, int64(1<<20), fi.Size)

	f, err := m.OpenFile("/large.bin", O_RDONLY|O_NOFOLLOW, false)
	test.OK(t, err)
	data, err := io.ReadAll(f)
	test.OK(t, err)
	test.Equals(t, 1<<20, len(data))
	test.Assert(t, bytes.Equal(data, bytes.Repeat([]byte("x"), 1<<20)), "content mismatch")
	test.OK(t, f.Close())

	after, err := os.ReadDir(dir)
	test.OK(t, err)
	test.Equals(t, len(before), len(after), "new files appeared in the archive directory")
	emptyDir, err := os.ReadDir(tmp)
	test.OK(t, err)
	test.Equals(t, 0, len(emptyDir), "files written to TMPDIR")
}

// TestSevenZipLstatConsistency verifies that Lstat and OpenFile+Stat agree
// for every entry of the archive.
func TestSevenZipLstatConsistency(t *testing.T) {
	var warnings []string
	m := openTestSevenZip(t, []string{"basic.7z"}, &warnings)

	var count int
	var walk func(dir string, f File)
	walk = func(dir string, f File) {
		names, err := f.Readdirnames(-1)
		test.OK(t, err)
		for _, name := range names {
			p := filepath.Join(dir, name)
			child, err := m.OpenFile(p, O_RDONLY|O_NOFOLLOW, true)
			test.OK(t, err)
			fi1, err := m.Lstat(p)
			test.OK(t, err)
			fi2, err := child.Stat()
			test.OK(t, err)
			test.Equals(t, fi1.Mode, fi2.Mode)
			test.Equals(t, fi1.ModTime, fi2.ModTime)
			test.Equals(t, fi1.Size, fi2.Size)
			count++
			if fi1.Mode.IsDir() {
				walk(p, child)
			}
			test.OK(t, child.Close())
		}
	}
	root, err := m.OpenFile("/", O_RDONLY|O_DIRECTORY, true)
	test.OK(t, err)
	walk("/", root)
	test.OK(t, root.Close())
	test.Assert(t, count >= 6, "expected at least 6 entries, walked %d", count)
}
