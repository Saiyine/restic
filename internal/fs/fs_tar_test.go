package fs

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/restic/restic/internal/data"
	"github.com/restic/restic/internal/test"
	"github.com/ulikunitz/xz"
)

const tarTestdata = "testdata/tar"

// tarTestEntry is one entry of a tar archive built in a test.
type tarTestEntry struct {
	name     string
	content  []byte
	mode     int64
	mtime    time.Time
	typ      byte // 0 means a regular file
	linkname string
}

// writeTestTar writes a tar archive to path, optionally wrapped in gzip
// ("gz"), xz ("xz") or zstd ("zst"). (bzip2 has no writer in the standard
// library; bz2 fixtures are committed.) Entry mtimes default to modTimeA.
func writeTestTar(t testing.TB, path, wrap string, entries []tarTestEntry) {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mtime := e.mtime
		if mtime.IsZero() {
			mtime = modTimeA
		}
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     e.mode,
			Size:     0,
			Typeflag: typ,
			Linkname: e.linkname,
			ModTime:  mtime,
		}
		hasContent := typ == tar.TypeReg ||
			(typ == tar.TypeSymlink && e.linkname == "" && e.content != nil)
		if hasContent {
			hdr.Size = int64(len(e.content))
		}
		test.OK(t, w.WriteHeader(hdr))
		if hasContent {
			_, err := w.Write(e.content)
			test.OK(t, err)
		}
	}
	test.OK(t, w.Close())

	f, err := os.Create(path)
	test.OK(t, err)
	var out io.Writer = f
	var closers []io.Closer
	switch {
	case strings.Contains(wrap, "gz"):
		gz := gzip.NewWriter(f)
		out = gz
		closers = append(closers, gz)
	case strings.Contains(wrap, "xz"):
		xzw, err := xz.NewWriter(f)
		test.OK(t, err)
		out = xzw
		closers = append(closers, xzw)
	case strings.Contains(wrap, "zst"):
		zsw, err := zstd.NewWriter(f)
		test.OK(t, err)
		out = zsw
		closers = append(closers, zsw)
	}
	_, err = io.Copy(out, &buf)
	test.OK(t, err)
	for i := len(closers) - 1; i >= 0; i-- {
		test.OK(t, closers[i].Close())
	}
	test.OK(t, f.Close())
}

// TestTarPlain verifies the hierarchy, modes, mtimes and contents of a
// plain (uncompressed) tar archive.
func TestTarPlain(t *testing.T) {
	var warnings []string
	m := openTestMigrate(t, []string{filepath.Join(tarTestdata, "basic.tar")}, &warnings)
	test.Equals(t, 0, len(warnings))

	fi, err := m.Lstat("/top.txt")
	test.OK(t, err)
	test.Equals(t, "top.txt", fi.Name)
	test.Equals(t, os.FileMode(0o640), fi.Mode)
	test.Equals(t, int64(3), fi.Size)
	test.Equals(t, modTimeA, fi.ModTime)

	fi, err = m.Lstat("/docs")
	test.OK(t, err)
	test.Assert(t, fi.Mode.IsDir(), "expected dir, got %v", fi.Mode)
	test.Equals(t, os.FileMode(0o755), fi.Mode.Perm())
	test.Equals(t, modTimeA, fi.ModTime)

	// "deep" and "deep/nested" are not explicit entries: they are
	// synthesized, and their mtime is the newest descendant's mtime
	fi, err = m.Lstat("/deep/nested")
	test.OK(t, err)
	test.Assert(t, fi.Mode.IsDir(), "expected implicit dir, got %v", fi.Mode)
	test.Equals(t, modTimeB, fi.ModTime)

	f, err := m.OpenFile("/", O_RDONLY|O_DIRECTORY|O_NOFOLLOW, false)
	test.OK(t, err)
	names, err := f.Readdirnames(-1)
	test.OK(t, err)
	test.OK(t, f.Close())
	test.Equals(t, []string{"deep", "docs", "empty.bin", "top.txt"}, names)

	f, err = m.OpenFile("/docs", O_RDONLY|O_DIRECTORY|O_NOFOLLOW, true)
	test.OK(t, err)
	names, err = f.Readdirnames(-1)
	test.OK(t, err)
	test.OK(t, f.Close())
	test.Equals(t, []string{"readme.md"}, names)

	test.Equals(t, "top", readWholeFile(t, m, "/top.txt"))
	test.Equals(t, "hello world", readWholeFile(t, m, "/docs/readme.md"))
	test.Equals(t, "\x00\x01\x02\x03", readWholeFile(t, m, "/deep/nested/file.bin"))
	test.Equals(t, "", readWholeFile(t, m, "/empty.bin"))

	_, err = m.Lstat("/missing")
	test.Assert(t, errors.Is(err, os.ErrNotExist), "expected ErrNotExist, got %v", err)
}

// TestTarGzip verifies a gzipped tar: contents, symlink, hard link, PAX
// long name, a restrictive directory mode and the skip of an unsupported
// special file.
func TestTarGzip(t *testing.T) {
	var warnings []string
	m := openTestMigrate(t, []string{filepath.Join(tarTestdata, "gz.tar.gz")}, &warnings)

	// the character device "dev" must be skipped with a warning
	test.Equals(t, 1, len(warnings))
	test.Assert(t, strings.Contains(warnings[0], "dev"), "expected a warning about the character device, got %v", warnings)
	_, err := m.Lstat("/dev")
	test.Assert(t, errors.Is(err, os.ErrNotExist), "character device must be omitted, got %v", err)

	test.Equals(t, "gzip data here\n", readWholeFile(t, m, "/data.txt"))
	test.Equals(t, "the target\n", readWholeFile(t, m, "/target.txt"))

	// hard link: same content and mode as the target
	test.Equals(t, "the target\n", readWholeFile(t, m, "/hard.txt"))
	fi, err := m.Lstat("/hard.txt")
	test.OK(t, err)
	test.Equals(t, os.FileMode(0o644), fi.Mode)
	test.Equals(t, int64(11), fi.Size)
	test.Equals(t, modTimeA, fi.ModTime)

	// symlink
	fi, err = m.Lstat("/link.txt")
	test.OK(t, err)
	test.Assert(t, fi.Mode&os.ModeSymlink != 0, "expected symlink mode, got %v", fi.Mode)

	f, err := m.OpenFile("/link.txt", O_NOFOLLOW, true)
	test.OK(t, err)
	node, err := f.ToNode(false, nil)
	test.OK(t, err)
	test.Equals(t, "target.txt", node.LinkTarget)
	test.Equals(t, "/link.txt", node.Path)
	test.OK(t, f.Close())
	test.Equals(t, "target.txt", readWholeFile(t, m, "/link.txt"))

	// directory mode and content
	fi, err = m.Lstat("/secret")
	test.OK(t, err)
	test.Assert(t, fi.Mode.IsDir(), "expected dir, got %v", fi.Mode)
	test.Equals(t, os.FileMode(0o700), fi.Mode.Perm())
	test.Equals(t, "hidden", readWholeFile(t, m, "/secret/hidden.txt"))

	// PAX long name (> 100 chars)
	longName := "longdir/" + strings.Repeat("a", 120) + ".txt"
	test.Equals(t, "long\n", readWholeFile(t, m, "/"+longName))
}

// TestTarXz verifies an xz-compressed tar.
func TestTarXz(t *testing.T) {
	var warnings []string
	m := openTestMigrate(t, []string{filepath.Join(tarTestdata, "xz.tar.xz")}, &warnings)
	test.Equals(t, 0, len(warnings))

	test.Equals(t, "xz data here\n", readWholeFile(t, m, "/data.txt"))

	fi, err := m.Lstat("/sub")
	test.OK(t, err)
	test.Assert(t, fi.Mode.IsDir(), "expected dir, got %v", fi.Mode)
	test.Equals(t, os.FileMode(0o755), fi.Mode.Perm())

	test.Equals(t, "inner data\n", readWholeFile(t, m, "/sub/inner.txt"))
	fi, err = m.Lstat("/sub/inner.txt")
	test.OK(t, err)
	test.Equals(t, modTimeB, fi.ModTime)
}

// TestTarBzip2 verifies a bzip2-compressed tar.
func TestTarBzip2(t *testing.T) {
	var warnings []string
	m := openTestMigrate(t, []string{filepath.Join(tarTestdata, "bz2.tar.bz2")}, &warnings)
	test.Equals(t, 0, len(warnings))

	test.Equals(t, "bz2 data here\n", readWholeFile(t, m, "/data.txt"))

	fi, err := m.Lstat("/sub")
	test.OK(t, err)
	test.Assert(t, fi.Mode.IsDir(), "expected dir, got %v", fi.Mode)
	test.Equals(t, os.FileMode(0o755), fi.Mode.Perm())

	test.Equals(t, "inner data\n", readWholeFile(t, m, "/sub/inner.txt"))
	fi, err = m.Lstat("/sub/inner.txt")
	test.OK(t, err)
	test.Equals(t, modTimeB, fi.ModTime)
}

// TestTarZstd verifies a zstd-compressed tar.
func TestTarZstd(t *testing.T) {
	var warnings []string
	m := openTestMigrate(t, []string{filepath.Join(tarTestdata, "zst.tar.zst")}, &warnings)
	test.Equals(t, 0, len(warnings))

	test.Equals(t, "zst data here\n", readWholeFile(t, m, "/data.txt"))

	fi, err := m.Lstat("/sub")
	test.OK(t, err)
	test.Assert(t, fi.Mode.IsDir(), "expected dir, got %v", fi.Mode)
	test.Equals(t, os.FileMode(0o755), fi.Mode.Perm())

	test.Equals(t, "inner data\n", readWholeFile(t, m, "/sub/inner.txt"))
	fi, err = m.Lstat("/sub/inner.txt")
	test.OK(t, err)
	test.Equals(t, modTimeB, fi.ModTime)
}

// TestTarStandaloneGz verifies that a .gz file without a tar inside is
// backed up as a single virtual file named after the archive, with the mtime
// of the gzip header.
func TestTarStandaloneGz(t *testing.T) {
	var warnings []string
	m := openTestMigrate(t, []string{filepath.Join(tarTestdata, "alpha.gz")}, &warnings)
	test.Equals(t, 0, len(warnings))

	const content = "gzip standalone content\n"
	fi, err := m.Lstat("/alpha")
	test.OK(t, err)
	test.Equals(t, "alpha", fi.Name)
	test.Equals(t, os.FileMode(0o644), fi.Mode)
	test.Equals(t, int64(len(content)), fi.Size)
	test.Equals(t, modTimeA, fi.ModTime, "mtime must come from the gzip header")

	test.Equals(t, content, readWholeFile(t, m, "/alpha"))

	f, err := m.OpenFile("/", O_RDONLY|O_DIRECTORY|O_NOFOLLOW, false)
	test.OK(t, err)
	names, err := f.Readdirnames(-1)
	test.OK(t, err)
	test.OK(t, f.Close())
	test.Equals(t, []string{"alpha"}, names)
}

// TestTarStandaloneXz verifies the same for a .xz file: the mtime is that
// of the file itself, as the xz format stores no mtime.
func TestTarStandaloneXz(t *testing.T) {
	var warnings []string
	m := openTestMigrate(t, []string{filepath.Join(tarTestdata, "beta.xz")}, &warnings)
	test.Equals(t, 0, len(warnings))

	const content = "xz standalone content\n"
	fi, err := m.Lstat("/beta")
	test.OK(t, err)
	test.Equals(t, int64(len(content)), fi.Size)

	ost, err := os.Stat(filepath.Join(tarTestdata, "beta.xz"))
	test.OK(t, err)
	test.Equals(t, ost.ModTime().UTC(), fi.ModTime)

	test.Equals(t, content, readWholeFile(t, m, "/beta"))
}

// TestTarStandaloneBz2 verifies the same for a .bz2 file: the mtime is that
// of the file itself, as the bzip2 format stores no mtime.
func TestTarStandaloneBz2(t *testing.T) {
	var warnings []string
	m := openTestMigrate(t, []string{filepath.Join(tarTestdata, "gamma.bz2")}, &warnings)
	test.Equals(t, 0, len(warnings))

	const content = "bzip2 standalone content\n"
	fi, err := m.Lstat("/gamma")
	test.OK(t, err)
	test.Equals(t, "gamma", fi.Name)
	test.Equals(t, os.FileMode(0o644), fi.Mode)
	test.Equals(t, int64(len(content)), fi.Size)

	ost, err := os.Stat(filepath.Join(tarTestdata, "gamma.bz2"))
	test.OK(t, err)
	test.Equals(t, ost.ModTime().UTC(), fi.ModTime)

	test.Equals(t, content, readWholeFile(t, m, "/gamma"))
}

// TestTarStandaloneZst verifies the same for a .zst file: the mtime is that
// of the file itself, as the zstd format stores no mtime.
func TestTarStandaloneZst(t *testing.T) {
	var warnings []string
	m := openTestMigrate(t, []string{filepath.Join(tarTestdata, "delta.zst")}, &warnings)
	test.Equals(t, 0, len(warnings))

	const content = "zstd standalone content\n"
	fi, err := m.Lstat("/delta")
	test.OK(t, err)
	test.Equals(t, "delta", fi.Name)
	test.Equals(t, os.FileMode(0o644), fi.Mode)
	test.Equals(t, int64(len(content)), fi.Size)

	ost, err := os.Stat(filepath.Join(tarTestdata, "delta.zst"))
	test.OK(t, err)
	test.Equals(t, ost.ModTime().UTC(), fi.ModTime)

	test.Equals(t, content, readWholeFile(t, m, "/delta"))
}

// TestTarMultiDuplicateLastWins verifies that a path present in two tar
// archives keeps the entry of the later archive.
func TestTarMultiDuplicateLastWins(t *testing.T) {
	var warnings []string
	m := openTestMigrate(t, []string{
		filepath.Join(tarTestdata, "dup_a.tar"),
		filepath.Join(tarTestdata, "dup_b.tar"),
	}, &warnings)
	test.Equals(t, 1, len(warnings))
	test.Assert(t, strings.Contains(warnings[0], "duplicate"), "expected a duplicate warning, got %v", warnings)

	test.Equals(t, "version B\n", readWholeFile(t, m, "/same.txt"))
	fi, err := m.Lstat("/same.txt")
	test.OK(t, err)
	test.Equals(t, modTimeB, fi.ModTime)

	test.Equals(t, "only in a\n", readWholeFile(t, m, "/only_a.txt"))
	test.Equals(t, "only in b\n", readWholeFile(t, m, "/only_b.txt"))
}

// TestTarEmpty verifies that an empty tar archive yields only the virtual
// root.
func TestTarEmpty(t *testing.T) {
	var warnings []string
	m := openTestMigrate(t, []string{filepath.Join(tarTestdata, "empty.tar")}, &warnings)
	test.Equals(t, 0, len(warnings))

	f, err := m.OpenFile("/", O_RDONLY|O_DIRECTORY|O_NOFOLLOW, false)
	test.OK(t, err)
	names, err := f.Readdirnames(-1)
	test.OK(t, err)
	test.OK(t, f.Close())
	test.Assert(t, len(names) == 0, "expected empty root, got %v", names)
}

// TestTarLarge verifies reading a large (1 MiB) entry from a gzipped tar.
func TestTarLarge(t *testing.T) {
	var warnings []string
	m := openTestMigrate(t, []string{filepath.Join(tarTestdata, "large.tar.gz")}, &warnings)
	test.Equals(t, 0, len(warnings))

	fi, err := m.Lstat("/large.bin")
	test.OK(t, err)
	test.Equals(t, int64(1<<20), fi.Size)

	test.Equals(t, string(bytes.Repeat([]byte("x"), 1<<20)), readWholeFile(t, m, "/large.bin"))
}

// TestTarCorruptGz verifies that a truncated (hence corrupt) gzipped tar
// fails with a content error at indexing time, before any entry is read.
func TestTarCorruptGz(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "corrupt.tar.gz")
	data, err := os.ReadFile(filepath.Join(tarTestdata, "gz.tar.gz"))
	test.OK(t, err)
	test.OK(t, os.WriteFile(dst, data[:len(data)/2], 0o644))

	var warnings []string
	_, err = NewMigrate([]string{dst}, func(format string, args ...any) {
		warnings = append(warnings, format)
	})
	test.Assert(t, err != nil, "expected an error for a corrupt gzip stream")
	test.Assert(t, !strings.Contains(err.Error(), "unsupported archive format"),
		"expected a content error, not a format error, got: %v", err)
	test.Equals(t, 0, len(warnings))
}

// TestTarCorruptXz verifies the same for a truncated .xz file.
func TestTarCorruptXz(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "corrupt.xz")
	data, err := os.ReadFile(filepath.Join(tarTestdata, "beta.xz"))
	test.OK(t, err)
	test.OK(t, os.WriteFile(dst, data[:len(data)/2], 0o644))

	_, err = NewMigrate([]string{dst}, func(string, ...any) {})
	test.Assert(t, err != nil, "expected an error for a corrupt xz stream")
}

// TestTarCorruptBz2 verifies the same for a truncated .bz2 file.
func TestTarCorruptBz2(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "corrupt.bz2")
	data, err := os.ReadFile(filepath.Join(tarTestdata, "gamma.bz2"))
	test.OK(t, err)
	test.OK(t, os.WriteFile(dst, data[:len(data)/2], 0o644))

	_, err = NewMigrate([]string{dst}, func(string, ...any) {})
	test.Assert(t, err != nil, "expected an error for a corrupt bzip2 stream")
}

// TestTarCorruptZst verifies the same for a truncated .zst file.
func TestTarCorruptZst(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "corrupt.zst")
	data, err := os.ReadFile(filepath.Join(tarTestdata, "delta.zst"))
	test.OK(t, err)
	test.OK(t, os.WriteFile(dst, data[:len(data)/2], 0o644))

	_, err = NewMigrate([]string{dst}, func(string, ...any) {})
	test.Assert(t, err != nil, "expected an error for a corrupt zstd stream")
}

// TestTarArchiveSlip verifies that unsafe entry names are either re-rooted
// inside the virtual filesystem (absolute paths, resolvable "..") or
// skipped with a warning when they would escape it.
func TestTarArchiveSlip(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "slip.tar")
	writeTestTar(t, tarPath, "", []tarTestEntry{
		{name: "/abs/evil.txt", content: []byte("absolute"), mode: 0o644},
		{name: "a/../rel.txt", content: []byte("relative"), mode: 0o644},
		{name: "a/../../evil.txt", content: []byte("escape"), mode: 0o644},
		{name: "../escape.txt", content: []byte("escape"), mode: 0o644},
	})

	var warnings []string
	m := openTestMigrate(t, []string{tarPath}, &warnings)

	// names that can be re-rooted are kept
	test.Equals(t, "absolute", readWholeFile(t, m, "/abs/evil.txt"))
	test.Equals(t, "relative", readWholeFile(t, m, "/rel.txt"))

	// names that would escape the virtual root are skipped
	for _, p := range []string{"/evil.txt", "/escape.txt"} {
		_, err := m.Lstat(p)
		test.Assert(t, errors.Is(err, os.ErrNotExist), "escaping entry %s must be omitted, got %v", p, err)
	}

	test.Equals(t, 2, len(warnings))
	for _, w := range warnings {
		test.Assert(t, strings.Contains(w, "unsafe or invalid path"), "unexpected warning %q", w)
	}
}

// TestTarConcurrentRead verifies that many goroutines reading different
// entries of a compressed tar concurrently get the right content (run with
// -race).
func TestTarConcurrentRead(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "concurrent.tar.gz")
	var entries []tarTestEntry
	for i := 0; i < 8; i++ {
		entries = append(entries, tarTestEntry{
			name:    "dir" + string(rune('0'+i%2)) + "/file" + string(rune('0'+i)) + ".txt",
			content: bytes.Repeat([]byte{byte(i)}, 1<<14),
		})
	}
	writeTestTar(t, tarPath, "gz", entries)

	var warnings []string
	m := openTestMigrate(t, []string{tarPath}, &warnings)

	var (
		mu   sync.Mutex
		errs []error
	)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				name := "/dir" + string(rune('0'+i%2)) + "/file" + string(rune('0'+i)) + ".txt"
				f, err := m.OpenFile(name, O_RDONLY|O_NOFOLLOW, false)
				if err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					return
				}
				want := bytes.Repeat([]byte{byte(i)}, 1<<14)
				got, err := io.ReadAll(f)
				_ = f.Close()
				if err != nil {
					mu.Lock()
					errs = append(errs, fmt.Errorf("goroutine %d: %w", g, err))
					mu.Unlock()
					return
				}
				if !bytes.Equal(got, want) {
					mu.Lock()
					errs = append(errs, fmt.Errorf("goroutine %d: wrong content for %s", g, name))
					mu.Unlock()
					return
				}
			}
		}(g)
	}
	wg.Wait()
	test.Assert(t, len(errs) == 0, "unexpected errors: %v", errs)
}

// TestTarPartialRead verifies that abandoning a read mid-entry (cursor
// invalidation) does not corrupt later reads, in plain and compressed
// archives.
func TestTarPartialRead(t *testing.T) {
	for _, tc := range []struct {
		path      string
		abandoned string
		check     []string
	}{
		{
			path:      filepath.Join(tarTestdata, "basic.tar"),
			abandoned: "/top.txt",
			check:     []string{"/top.txt", "/docs/readme.md"},
		},
		{
			path:      filepath.Join(tarTestdata, "gz.tar.gz"),
			abandoned: "/data.txt",
			check:     []string{"/data.txt", "/target.txt"},
		},
		{
			path:      filepath.Join(tarTestdata, "bz2.tar.bz2"),
			abandoned: "/data.txt",
			check:     []string{"/data.txt", "/sub/inner.txt"},
		},
		{
			path:      filepath.Join(tarTestdata, "zst.tar.zst"),
			abandoned: "/data.txt",
			check:     []string{"/data.txt", "/sub/inner.txt"},
		},
	} {
		t.Run(filepath.Base(tc.path), func(t *testing.T) {
			var warnings []string
			m := openTestMigrate(t, []string{tc.path}, &warnings)
			// (gz.tar.gz warns about its character device entry; that is
			// asserted in TestTarGzip)

			// read one byte and abandon: the forward cursor (if any) is
			// at an unknown position and must be discarded
			f, err := m.OpenFile(tc.abandoned, O_RDONLY|O_NOFOLLOW, false)
			test.OK(t, err)
			_, err = f.Read(make([]byte, 1))
			test.OK(t, err)
			test.OK(t, f.Close())

			// everything must still be readable, including the abandoned
			// entry itself and later entries
			for _, p := range tc.check {
				test.Assert(t, readWholeFile(t, m, p) != "", "entry %s must be fully readable after an abandoned read", p)
			}
		})
	}
}

// TestTarNoDiskAccess verifies that opening and reading a tar archive never
// creates or modifies files on disk: the directory holding the archive keeps
// exactly its files, and a TMPDIR pointed at an empty directory stays empty.
func TestTarNoDiskAccess(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	archivePath := filepath.Join(tarTestdata, "large.tar.gz")
	archiveDir := filepath.Dir(archivePath)
	before, err := os.ReadDir(archiveDir)
	test.OK(t, err)

	m, err := NewMigrate([]string{archivePath}, func(string, ...any) {})
	test.OK(t, err)
	test.Equals(t, string(bytes.Repeat([]byte("x"), 1<<20)), readWholeFile(t, m, "/large.bin"))
	test.OK(t, m.Close())

	after, err := os.ReadDir(archiveDir)
	test.OK(t, err)
	test.Equals(t, len(before), len(after), "new files appeared in the archive directory")

	emptyDir, err := os.ReadDir(tmp)
	test.OK(t, err)
	test.Equals(t, 0, len(emptyDir), "files were written to TMPDIR")
}

// TestTarProbeFormats verifies the format detection: plain tar and tar
// compressed with gzip, xz, bzip2 or zstd are indexed as archives, while
// compressed files without a tar inside are indexed as a single virtual
// file.
func TestTarProbeFormats(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{filepath.Join(tarTestdata, "basic.tar"), "/top.txt"},
		{filepath.Join(tarTestdata, "gz.tar.gz"), "/data.txt"},
		{filepath.Join(tarTestdata, "xz.tar.xz"), "/data.txt"},
		{filepath.Join(tarTestdata, "bz2.tar.bz2"), "/data.txt"},
		{filepath.Join(tarTestdata, "zst.tar.zst"), "/data.txt"},
		{filepath.Join(tarTestdata, "alpha.gz"), "/alpha"},
		{filepath.Join(tarTestdata, "beta.xz"), "/beta"},
		{filepath.Join(tarTestdata, "gamma.bz2"), "/gamma"},
		{filepath.Join(tarTestdata, "delta.zst"), "/delta"},
	}
	for _, tc := range cases {
		t.Run(filepath.Base(tc.path), func(t *testing.T) {
			var warnings []string
			m := openTestMigrate(t, []string{tc.path}, &warnings)
			_, err := m.Lstat(tc.want)
			test.OK(t, err)
		})
	}

	// a file that is neither zip, 7z, tar nor compressed is rejected
	dir := t.TempDir()
	badPath := filepath.Join(dir, "data.bin")
	test.OK(t, os.WriteFile(badPath, []byte("plain text, no compression"), 0o644))
	_, err := NewMigrate([]string{badPath}, func(string, ...any) {})
	test.Assert(t, err != nil, "expected an error for an unsupported format")
	test.Assert(t, strings.Contains(err.Error(), "unsupported archive format"),
		"unexpected error %v", err)
}

// TestTarPaxTimesAndXattrs verifies that atime/ctime stored in the archive
// (PAX atime/ctime records) and extended attributes (PAX SCHILY.xattr.*
// records, including the octal-escaped form for non-printable values) are
// exposed by the virtual filesystem; entries without them fall back to
// the mtime.
func TestTarPaxTimesAndXattrs(t *testing.T) {
	atimeVal := time.Date(2022, 3, 4, 5, 6, 7, 0, time.UTC)
	ctimeVal := time.Date(2023, 4, 5, 6, 7, 8, 0, time.UTC)

	path := filepath.Join(t.TempDir(), "pax.tar")
	f, err := os.Create(path)
	test.OK(t, err)
	tw := tar.NewWriter(f)
	write := func(hdr *tar.Header, content string) {
		test.OK(t, tw.WriteHeader(hdr))
		if content != "" {
			var n int
			n, err = tw.Write([]byte(content))
			test.OK(t, err)
			test.Assert(t, n == len(content), "short write")
		}
	}

	// explicit atime and ctime (only written with an explicit PAX format)
	write(&tar.Header{
		Name: "times.txt", Mode: 0o644, Size: int64(len("with times\n")),
		ModTime: modTimeA, AccessTime: atimeVal, ChangeTime: ctimeVal,
		Format: tar.FormatPAX,
	}, "with times\n")

	// no atime/ctime stored: must fall back to the mtime
	write(&tar.Header{
		Name: "plain.txt", Mode: 0o644, Size: int64(len("plain\n")),
		ModTime: modTimeB,
		Format:  tar.FormatPAX,
	}, "plain\n")

	// extended attributes: one plain value, one octal-escaped
	// (backslash + 0231 001 = bytes 0x99 0x01, the PAX encoding of
	// non-printable values)
	write(&tar.Header{
		Name: "xattr.txt", Mode: 0o644, Size: int64(len("attrs\n")),
		ModTime: modTimeA,
		Format:  tar.FormatPAX,
		Xattrs:  map[string]string{"user.plain": "plain-value"},
		PAXRecords: map[string]string{
			"SCHILY.xattr.user.raw": "\\231001",
		},
	}, "attrs\n")

	// a directory with an extended attribute
	write(&tar.Header{
		Name: "dir/", Mode: 0o755,
		ModTime: modTimeA,
		Format:  tar.FormatPAX,
		Xattrs:  map[string]string{"user.dir": "dir-value"},
	}, "")

	test.OK(t, tw.Close())
	test.OK(t, f.Close())

	var warnings []string
	m := openTestMigrate(t, []string{path}, &warnings)
	test.Equals(t, 0, len(warnings))

	fi, err := m.Lstat("/times.txt")
	test.OK(t, err)
	test.Equals(t, atimeVal, fi.AccessTime)
	test.Equals(t, ctimeVal, fi.ChangeTime)
	test.Equals(t, modTimeA, fi.ModTime)

	fi, err = m.Lstat("/plain.txt")
	test.OK(t, err)
	test.Equals(t, modTimeB, fi.AccessTime)
	test.Equals(t, modTimeB, fi.ChangeTime)
	test.Equals(t, modTimeB, fi.ModTime)

	toNode := func(name string) *data.Node {
		f, err := m.OpenFile(name, O_RDONLY|O_NOFOLLOW, false)
		test.OK(t, err)
		node, err := f.ToNode(false, func(format string, args ...any) {
			warnings = append(warnings, fmt.Sprintf(format, args...))
		})
		test.OK(t, err)
		test.OK(t, f.Close())
		return node
	}

	node := toNode("/xattr.txt")
	test.Equals(t, []data.ExtendedAttribute{
		{Name: "user.plain", Value: []byte("plain-value")},
		{Name: "user.raw", Value: []byte{0x99, 0x01}},
	}, node.ExtendedAttributes)

	node = toNode("/dir")
	test.Equals(t, []data.ExtendedAttribute{
		{Name: "user.dir", Value: []byte("dir-value")},
	}, node.ExtendedAttributes)
}
