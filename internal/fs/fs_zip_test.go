package fs

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/restic/restic/internal/test"
)

var (
	modTimeA = time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	modTimeB = time.Date(2021, 6, 7, 8, 9, 10, 0, time.UTC)
)

type zipTestFile struct {
	name    string
	content []byte
	mode    os.FileMode
	modTime time.Time
	method  uint16
}

func writeTestZip(t testing.TB, zipPath string, files []zipTestFile) {
	t.Helper()
	f, err := os.Create(zipPath)
	test.OK(t, err)
	zw := zip.NewWriter(f)
	for _, file := range files {
		method := file.method
		if method == 0 {
			method = zip.Deflate
		}
		hdr := &zip.FileHeader{Name: file.name, Method: method}
		if !file.modTime.IsZero() {
			hdr.Modified = file.modTime.UTC()
		}
		if file.mode != 0 {
			hdr.SetMode(file.mode)
		}
		w, err := zw.CreateHeader(hdr)
		test.OK(t, err)
		_, err = w.Write(file.content)
		test.OK(t, err)
	}
	test.OK(t, zw.Close())
	test.OK(t, f.Close())
}

func openTestZip(t testing.TB, zipPath string, warnings *[]string) *Zip {
	t.Helper()
	z, err := NewZip(zipPath, func(format string, args ...any) {
		*warnings = append(*warnings, fmt.Sprintf(format, args...))
	})
	test.OK(t, err)
	t.Cleanup(func() { test.OK(t, z.Close()) })
	return z
}

func readWholeFile(t testing.TB, z *Zip, name string) string {
	t.Helper()
	f, err := z.OpenFile(name, O_RDONLY|O_NOFOLLOW, false)
	test.OK(t, err)
	data, err := io.ReadAll(f)
	test.OK(t, err)
	test.OK(t, f.Close())
	return string(data)
}

func TestZipHierarchy(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "test.zip")
	writeTestZip(t, zipPath, []zipTestFile{
		{name: "top.txt", content: []byte("top"), mode: 0o640, modTime: modTimeA},
		{name: "docs/", mode: 0o755, modTime: modTimeA},
		{name: "docs/readme.md", content: []byte("hello world"), mode: 0o644, modTime: modTimeB},
		// "deep" is not an explicit directory entry: it must be synthesized
		{name: "deep/nested/file.bin", content: []byte{0, 1, 2, 3}, method: zip.Store, modTime: modTimeB},
		{name: "empty.bin", content: nil, mode: 0o600, modTime: modTimeA},
	})

	z := openTestZip(t, zipPath, new([]string))

	fi, err := z.Lstat("/top.txt")
	test.OK(t, err)
	test.Equals(t, "top.txt", fi.Name)
	test.Equals(t, os.FileMode(0o640), fi.Mode)
	test.Equals(t, int64(3), fi.Size)
	test.Equals(t, modTimeA, fi.ModTime)

	fi, err = z.Lstat("/docs")
	test.OK(t, err)
	test.Assert(t, fi.Mode.IsDir(), "expected dir, got %v", fi.Mode)
	test.Equals(t, modTimeA, fi.ModTime)

	// implicit directory
	fi, err = z.Lstat("/deep")
	test.OK(t, err)
	test.Assert(t, fi.Mode.IsDir(), "expected implicit dir, got %v", fi.Mode)
	test.Equals(t, os.FileMode(0o755), fi.Mode.Perm())
	test.Equals(t, modTimeB, fi.ModTime, "implicit dir mtime must be the newest descendant's mtime")

	fi, err = z.Lstat("/deep/nested")
	test.OK(t, err)
	test.Assert(t, fi.Mode.IsDir(), "expected implicit dir, got %v", fi.Mode)

	fi, err = z.Lstat("/")
	test.OK(t, err)
	test.Assert(t, fi.Mode.IsDir(), "root must be a dir")

	_, err = z.Lstat("/missing")
	test.Assert(t, errors.Is(err, os.ErrNotExist), "expected ErrNotExist, got %v", err)

	f, err := z.OpenFile("/", O_RDONLY|O_DIRECTORY|O_NOFOLLOW, false)
	test.OK(t, err)
	names, err := f.Readdirnames(-1)
	test.OK(t, err)
	test.OK(t, f.Close())
	test.Equals(t, []string{"deep", "docs", "empty.bin", "top.txt"}, names)

	f, err = z.OpenFile("/docs", O_RDONLY|O_DIRECTORY|O_NOFOLLOW, true)
	test.OK(t, err)
	names, err = f.Readdirnames(-1)
	test.OK(t, err)
	test.OK(t, f.Close())
	test.Equals(t, []string{"readme.md"}, names)

	test.Equals(t, "hello world", readWholeFile(t, z, "/docs/readme.md"))
	test.Equals(t, "", readWholeFile(t, z, "/empty.bin"))
	test.Equals(t, "\x00\x01\x02\x03", readWholeFile(t, z, "/deep/nested/file.bin"))

	if runtime.GOOS != "windows" {
		// on Windows O_DIRECTORY is 0 and not enforced
		_, err = z.OpenFile("/top.txt", O_DIRECTORY, false)
		test.Assert(t, errors.Is(err, syscall.ENOTDIR), "expected ENOTDIR opening a file with O_DIRECTORY, got %v", err)
	}

	// invalid flag combination
	_, err = z.OpenFile("/top.txt", O_WRONLY, false)
	test.Assert(t, err != nil, "expected error for write flag")
}

func TestZipCleanEntryName(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"a.txt", "/a.txt", true},
		{"a/b.txt", "/a/b.txt", true},
		{"a/b/../c.txt", "/a/c.txt", true},
		{"./a/./b.txt", "/a/b.txt", true},
		{"/absolute.txt", "/absolute.txt", true},
		{`C:\Windows\evil.txt`, "/Windows/evil.txt", true},
		{`a\b\c.txt`, "/a/b/c.txt", true},
		{"dir/", "/dir", true},
		{"a/../../etc/passwd", "", false},
		{"../etc/passwd", "", false},
		{"a/../../..", "", false},
		{"a\x00b.txt", "", false},
		{".", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := zipCleanEntryName(tc.in, true)
		test.Assert(t, ok == tc.wantOK, "zipCleanEntryName(%q): ok=%v, want %v (got %q)", tc.in, ok, tc.wantOK, got)
		if ok {
			test.Equals(t, tc.want, got)
		}
	}
}

func TestZipDuplicateEntries(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "dup.zip")
	writeTestZip(t, zipPath, []zipTestFile{
		{name: "file.txt", content: []byte("first")},
		{name: "file.txt", content: []byte("second")},
	})

	var warnings []string
	z := openTestZip(t, zipPath, &warnings)
	test.Equals(t, 1, len(warnings))
	test.Assert(t, strings.Contains(warnings[0], "duplicate"), "expected duplicate warning, got %q", warnings)

	test.Equals(t, "second", readWholeFile(t, z, "/file.txt"))
}

func TestZipFileDirConflict(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "conflict.zip")
	// file "a" first, then a file inside "a/" -> the latter must be dropped
	writeTestZip(t, zipPath, []zipTestFile{
		{name: "a", content: []byte("i am a file")},
		{name: "a/b.txt", content: []byte("impossible")},
	})

	var warnings []string
	z := openTestZip(t, zipPath, &warnings)
	test.Equals(t, 1, len(warnings))
	test.Equals(t, "i am a file", readWholeFile(t, z, "/a"))
	_, err := z.Lstat("/a/b.txt")
	test.Assert(t, errors.Is(err, os.ErrNotExist), "conflicting entry must be omitted, got err=%v", err)

	// and the reverse: explicit dir first, file later -> file is dropped
	zipPath2 := filepath.Join(dir, "conflict2.zip")
	writeTestZip(t, zipPath2, []zipTestFile{
		{name: "a/", mode: 0o755},
		{name: "a", content: []byte("i am a file")},
	})
	warnings = nil
	z = openTestZip(t, zipPath2, &warnings)
	test.Equals(t, 1, len(warnings))
	fi, err := z.Lstat("/a")
	test.OK(t, err)
	test.Assert(t, fi.Mode.IsDir(), "expected /a to remain a directory, got %v", fi.Mode)
}

func TestZipSymlink(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "link.zip")
	writeTestZip(t, zipPath, []zipTestFile{
		{name: "target.txt", content: []byte("content")},
		{name: "link", content: []byte("target.txt"), mode: 0o777 | os.ModeSymlink},
	})

	var warnings []string
	z := openTestZip(t, zipPath, &warnings)
	test.Equals(t, 0, len(warnings))

	fi, err := z.Lstat("/link")
	test.OK(t, err)
	test.Assert(t, fi.Mode&os.ModeSymlink != 0, "expected symlink mode, got %v", fi.Mode)

	f, err := z.OpenFile("/link", O_NOFOLLOW, true)
	test.OK(t, err)
	node, err := f.ToNode(false, nil)
	test.OK(t, err)
	test.Equals(t, "target.txt", node.LinkTarget)
	test.Equals(t, "/link", node.Path)
	test.OK(t, f.Close())

	// reading a symlink yields its target
	test.Equals(t, "target.txt", readWholeFile(t, z, "/link"))
}

func TestZipCP437Name(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "latin1.zip")
	// 0x82 is "é" in CP437 and is not valid UTF-8 on its own. Go's zip writer
	// leaves the name raw and clears the UTF-8 flag for such names, so
	// FileHeader.NonUTF8 is set and we must decode CP437 -> UTF-8.
	writeTestZip(t, zipPath, []zipTestFile{
		{name: string([]byte{0x82}) + ".txt", content: []byte("café")},
	})

	var warnings []string
	z := openTestZip(t, zipPath, &warnings)

	fi, err := z.Lstat("/é.txt")
	test.Assert(t, err == nil, "expected CP437 name to be decoded to UTF-8, got err: %v", err)
	test.Equals(t, "é.txt", fi.Name)
	test.Equals(t, "café", readWholeFile(t, z, "/é.txt"))
}

func TestZipEncryptedEntryIsSkipped(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "enc.zip")
	writeTestZip(t, zipPath, []zipTestFile{
		{name: "plain.txt", content: []byte("visible")},
		{name: "secret.txt", content: []byte("hidden")},
	})
	patchZipHeaderFlags(t, zipPath, "secret.txt", 1<<0, true)

	var warnings []string
	z := openTestZip(t, zipPath, &warnings)
	test.Equals(t, 1, len(warnings))
	test.Assert(t, strings.Contains(warnings[0], "encrypted"), "expected encryption warning, got %v", warnings)

	test.Equals(t, "visible", readWholeFile(t, z, "/plain.txt"))
	_, err := z.Lstat("/secret.txt")
	test.Assert(t, errors.Is(err, os.ErrNotExist), "encrypted entry must be omitted, got %v", err)
}

func TestZipUnsupportedCompressionIsSkipped(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "method.zip")
	writeTestZip(t, zipPath, []zipTestFile{
		{name: "ok.txt", content: []byte("fine")},
		{name: "weird.bin", content: []byte("compressed with something else")},
	})
	patchZipEntryMethod(t, zipPath, "weird.bin", 99)

	var warnings []string
	z := openTestZip(t, zipPath, &warnings)
	test.Equals(t, 1, len(warnings))
	test.Assert(t, strings.Contains(warnings[0], "compression"), "expected compression warning, got %v", warnings)

	test.Equals(t, "fine", readWholeFile(t, z, "/ok.txt"))
	_, err := z.Lstat("/weird.bin")
	test.Assert(t, errors.Is(err, os.ErrNotExist), "entry with unsupported method must be omitted, got %v", err)
}

func TestZipCorruptContent(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "corrupt.zip")
	const fname = "data.bin"
	content := bytes.Repeat([]byte("restic"), 64)
	writeTestZip(t, zipPath, []zipTestFile{
		{name: fname, content: content, method: zip.Store},
	})

	// corrupt one byte of the (stored, uncompressed) file data: the local
	// file header is 30 bytes plus the file name length.
	data, err := os.ReadFile(zipPath)
	test.OK(t, err)
	data[30+len(fname)+3] ^= 0xFF
	test.OK(t, os.WriteFile(zipPath, data, 0o644))

	var warnings []string
	z := openTestZip(t, zipPath, &warnings)

	f, err := z.OpenFile("/data.bin", O_RDONLY, false)
	test.OK(t, err)
	defer func() {
		_ = f.Close()
	}()
	_, err = io.ReadAll(f)
	test.Assert(t, errors.Is(err, zip.ErrChecksum), "expected checksum error, got %v", err)
}

// TestZipZip64Sizes verifies that entries whose sizes or header offset
// overflow the 32-bit fields are read correctly when the real values are
// carried in the zip64 extended information extra field.
func TestZipZip64Sizes(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "zip64.zip")
	const fname = "big.bin"
	content := bytes.Repeat([]byte("zip64 test content "), 32)
	writeTestZip(t, zipPath, []zipTestFile{
		{name: fname, content: content},
	})
	patchZip64Central(t, zipPath, fname)

	var warnings []string
	z := openTestZip(t, zipPath, &warnings)
	test.Assert(t, len(warnings) == 0, "unexpected warnings: %v", warnings)

	fi, err := z.Lstat("/" + fname)
	test.OK(t, err)
	test.Equals(t, int64(len(content)), fi.Size)

	test.Equals(t, string(content), readWholeFile(t, z, "/"+fname))
}

func TestZipConcurrentRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("not supported on windows")
	}
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "concurrent.zip")
	var files []zipTestFile
	for i := 0; i < 8; i++ {
		files = append(files, zipTestFile{
			name:    fmt.Sprintf("dir%d/file%d.txt", i%2, i),
			content: bytes.Repeat([]byte{byte(i)}, 1<<16),
			modTime: modTimeA,
		})
	}
	writeTestZip(t, zipPath, files)

	var warnings []string
	z := openTestZip(t, zipPath, &warnings)

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
				name := fmt.Sprintf("/dir%d/file%d.txt", i%2, i)
				f, err := z.OpenFile(name, O_RDONLY|O_NOFOLLOW, false)
				if err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					return
				}
				want := bytes.Repeat([]byte{byte(i)}, 1<<16)
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

// TestZipNoDiskAccess verifies that NewZip and reading entries never create
// or modify files on disk: the directory holding the zip keeps exactly the
// zip file, and a TMPDIR pointed at an empty directory stays empty.
func TestZipNoDiskAccess(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	zipDir := t.TempDir()
	zipPath := filepath.Join(zipDir, "data.zip")
	writeTestZip(t, zipPath, []zipTestFile{
		{name: "a/b/c.txt", content: bytes.Repeat([]byte("x"), 1<<20), modTime: modTimeA},
	})

	before, err := os.ReadDir(zipDir)
	test.OK(t, err)

	z, err := NewZip(zipPath, func(string, ...any) {})
	test.OK(t, err)
	test.Equals(t, strings.Repeat("x", 1<<20), readWholeFile(t, z, "/a/b/c.txt"))
	test.OK(t, z.Close())

	after, err := os.ReadDir(zipDir)
	test.OK(t, err)
	test.Equals(t, len(before), len(after), "new files appeared in the zip directory")

	emptyDir, err := os.ReadDir(tmp)
	test.OK(t, err)
	test.Equals(t, 0, len(emptyDir), "files were written to TMPDIR")
}

func TestZipEmptyArchive(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "empty.zip")
	writeTestZip(t, zipPath, nil)

	z := openTestZip(t, zipPath, new([]string))
	f, err := z.OpenFile("/", O_RDONLY|O_DIRECTORY, false)
	test.OK(t, err)
	names, err := f.Readdirnames(-1)
	test.OK(t, err)
	test.Assert(t, len(names) == 0, "expected empty root, got %v", names)
	test.OK(t, f.Close())
}

// TestZipLstatConsistency walks the virtual tree and checks that Lstat and
// OpenFile().Stat() always agree.
func TestZipLstatConsistency(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "consistency.zip")
	writeTestZip(t, zipPath, []zipTestFile{
		{name: "a/b/c.txt", content: []byte("x"), modTime: modTimeA},
		{name: "top.txt", content: []byte("y"), modTime: modTimeB},
	})

	z := openTestZip(t, zipPath, &[]string{})

	var count int
	var walk func(dir string, f File)
	walk = func(dir string, f File) {
		names, err := f.Readdirnames(-1)
		test.OK(t, err)
		for _, name := range names {
			p := filepath.Join(dir, name)
			child, err := z.OpenFile(p, O_RDONLY|O_NOFOLLOW, true)
			test.OK(t, err)
			fi1, err := z.Lstat(p)
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

	root, err := z.OpenFile("/", O_RDONLY|O_DIRECTORY, true)
	test.OK(t, err)
	walk("/", root)
	test.OK(t, root.Close())
	test.Assert(t, count >= 4, "expected at least 4 entries, walked %d", count)
}

// patchZipHeaderFlags flips a bit of the general-purpose bit flag field of
// the named entry in both the local file header and the central directory
// record of the archive.
func patchZipHeaderFlags(t testing.TB, zipPath, name string, mask byte, set bool) {
	t.Helper()
	data, err := os.ReadFile(zipPath)
	test.OK(t, err)

	nameBytes := []byte(name)
	// Local file header: flags at offset 6, method at 8, name length at 26,
	// name at 30. Central directory record: flags at 8, method at 10,
	// name length at 28, name at 46.
	type hdr struct {
		sig        []byte
		flagsOff   int
		methodOff  int
		nameLenOff int
		nameOff    int
	}
	for _, h := range []hdr{
		{[]byte("PK\x03\x04"), 6, 8, 26, 30},
		{[]byte("PK\x01\x02"), 8, 10, 28, 46},
	} {
		off := 0
		for {
			idx := bytes.Index(data[off:], h.sig)
			if idx < 0 {
				break
			}
			start := off + idx
			nlen := int(data[start+h.nameLenOff]) | int(data[start+h.nameLenOff+1])<<8
			if nlen == len(nameBytes) && bytes.Equal(data[start+h.nameOff:start+h.nameOff+nlen], nameBytes) {
				if set {
					data[start+h.flagsOff] |= mask
				} else {
					data[start+h.flagsOff] &^= mask
				}
			}
			off = start + len(h.sig)
		}
	}
	test.OK(t, os.WriteFile(zipPath, data, 0o644))
}

// patchZipEntryMethod sets the compression method in the local file header
// and central directory record of the named entry.
func patchZipEntryMethod(t testing.TB, zipPath, name string, method uint16) {
	t.Helper()
	data, err := os.ReadFile(zipPath)
	test.OK(t, err)

	nameBytes := []byte(name)
	patch := func(sig []byte, methodOff, nameLenOff, nameOff int) {
		off := 0
		for {
			idx := bytes.Index(data[off:], sig)
			if idx < 0 {
				break
			}
			start := off + idx
			nlen := int(data[start+nameLenOff]) | int(data[start+nameLenOff+1])<<8
			if nlen == len(nameBytes) && bytes.Equal(data[start+nameOff:start+nameOff+nlen], nameBytes) {
				data[start+methodOff] = byte(method)
				data[start+methodOff+1] = byte(method >> 8)
			}
			off = start + len(sig)
		}
	}
	patch([]byte("PK\x03\x04"), 8, 26, 30)
	patch([]byte("PK\x01\x02"), 10, 28, 46)

	test.OK(t, os.WriteFile(zipPath, data, 0o644))
}

// patchZip64Central marks the named entry's central directory record as
// zip64: it sets the compressed size, uncompressed size and local header
// offset fields to 0xFFFFFFFF and appends a zip64 extended information
// extra field with the real values. The local file header keeps its real
// 32-bit sizes, which is a valid combination.
func patchZip64Central(t testing.TB, zipPath, name string) {
	t.Helper()
	data, err := os.ReadFile(zipPath)
	test.OK(t, err)

	nameBytes := []byte(name)
	start, nlen := -1, 0
	off := 0
	for {
		idx := bytes.Index(data[off:], []byte("PK\x01\x02"))
		if idx < 0 {
			break
		}
		cand := off + idx
		candNlen := int(data[cand+28]) | int(data[cand+29])<<8
		if candNlen == len(nameBytes) && bytes.Equal(data[cand+46:cand+46+candNlen], nameBytes) {
			start, nlen = cand, candNlen
			break
		}
		off = cand + 4
	}
	test.Assert(t, start >= 0, "central directory record for %q not found", name)

	xlen := int(data[start+30]) | int(data[start+31])<<8
	clen := int(data[start+32]) | int(data[start+33])<<8
	test.Equals(t, 0, clen, "entry must not have a zip comment")
	compSize := uint64(binary.LittleEndian.Uint32(data[start+20 : start+24]))
	uncompSize := uint64(binary.LittleEndian.Uint32(data[start+24 : start+28]))
	headerOffset := uint64(binary.LittleEndian.Uint32(data[start+42 : start+46]))

	for _, f := range []int{20, 24, 42} {
		binary.LittleEndian.PutUint32(data[start+f:start+f+4], ^uint32(0))
	}

	extra := make([]byte, 4+24)
	binary.LittleEndian.PutUint16(extra[0:2], 0x0001)
	binary.LittleEndian.PutUint16(extra[2:4], 24)
	binary.LittleEndian.PutUint64(extra[4:12], uncompSize)
	binary.LittleEndian.PutUint64(extra[12:20], compSize)
	binary.LittleEndian.PutUint64(extra[20:28], headerOffset)

	pos := start + 46 + nlen + xlen
	data = append(data[:pos], append(extra, data[pos:]...)...)
	binary.LittleEndian.PutUint16(data[start+30:start+32], uint16(xlen+len(extra)))

	test.OK(t, os.WriteFile(zipPath, data, 0o644))
}
