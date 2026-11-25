package fs

import (
	"archive/tar"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/restic/restic/internal/test"
)

func openTestMigrate(t testing.TB, paths []string, warnings *[]string) *MigrateFS {
	t.Helper()
	m, err := NewMigrate(paths, func(format string, args ...any) {
		*warnings = append(*warnings, fmt.Sprintf(format, args...))
	})
	test.OK(t, err)
	t.Cleanup(func() { test.OK(t, m.Close()) })
	return m
}

// TestMigrateMixedDisjoint verifies that NewMigrate merges entries of zip
// and 7z archives, detected by their magic bytes.
func TestMigrateMixedDisjoint(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "a.zip")
	writeTestZip(t, zipPath, []zipTestFile{
		{name: "a.txt", content: []byte("from zip")},
		{name: "shared/fromZip.txt", content: []byte("zip only")},
	})

	var warnings []string
	m := openTestMigrate(t, []string{zipPath, filepath.Join(sevenzipTestdata, "basic.7z")}, &warnings)
	test.Equals(t, 0, len(warnings))

	test.Equals(t, "from zip", readWholeFile(t, m, "/a.txt"))
	test.Equals(t, "zip only", readWholeFile(t, m, "/shared/fromZip.txt"))
	test.Equals(t, "top", readWholeFile(t, m, "/top.txt"))
	test.Equals(t, "hello world", readWholeFile(t, m, "/docs/readme.md"))

	for _, p := range []string{"/a.txt", "/shared", "/top.txt", "/docs"} {
		_, err := m.Lstat(p)
		test.OK(t, err)
	}
}

// TestMigrateMixedDuplicateLastWins verifies that a path present in a zip
// and in a 7z archive keeps the entry of the later archive.
func TestMigrateMixedDuplicateLastWins(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "a.zip")
	writeTestZip(t, zipPath, []zipTestFile{
		{name: "same.txt", content: []byte("zip version\n"), modTime: modTimeA},
	})

	var warnings []string
	m := openTestMigrate(t, []string{zipPath, filepath.Join(sevenzipTestdata, "dup_b.7z")}, &warnings)
	test.Equals(t, 1, len(warnings))

	test.Equals(t, "version B\n", readWholeFile(t, m, "/same.txt"))
	fi, err := m.Lstat("/same.txt")
	test.OK(t, err)
	test.Equals(t, modTimeB, fi.ModTime)
	test.Equals(t, "only in b\n", readWholeFile(t, m, "/only_b.txt"))
}

// TestMigrateMixedTypeConflict verifies that a file/directory conflict
// between a zip and a 7z archive keeps the first occurrence.
func TestMigrateMixedTypeConflict(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "a.zip")
	writeTestZip(t, zipPath, []zipTestFile{
		{name: "d/", mode: 0o755, modTime: modTimeA},
		{name: "d/inside.txt", content: []byte("inside\n"), modTime: modTimeA},
	})

	var warnings []string
	m := openTestMigrate(t, []string{zipPath, filepath.Join(sevenzipTestdata, "conflict_file.7z")}, &warnings)
	test.Equals(t, 1, len(warnings))

	fi, err := m.Lstat("/d")
	test.OK(t, err)
	test.Equals(t, true, fi.Mode.IsDir())
	test.Equals(t, "inside\n", readWholeFile(t, m, "/d/inside.txt"))
	_, err = m.Lstat("/only_in_sevenzip_missing")
	test.Assert(t, err != nil, "expected an error for a missing entry")
}

// TestMigrateUnsupportedFormat verifies the format detection errors:
// unknown magic, file too short and missing file.
func TestMigrateUnsupportedFormat(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "data.tar")
	test.OK(t, os.WriteFile(tarPath, []byte("this is not a zip or 7z archive"), 0o644))
	shortPath := filepath.Join(dir, "short.bin")
	test.OK(t, os.WriteFile(shortPath, []byte("PK\x03"), 0o644))

	var warnings []string
	_, err := NewMigrate([]string{tarPath}, func(string, ...any) {})
	test.Assert(t, err != nil, "expected an error for an unsupported format")
	test.Assert(t, strings.Contains(err.Error(), "unsupported archive format"),
		"unexpected error %v", err)

	_, err = NewMigrate([]string{shortPath}, func(string, ...any) {})
	test.Assert(t, err != nil, "expected an error for a too short file")
	test.Assert(t, strings.Contains(err.Error(), "too short"), "unexpected error %v", err)

	_, err = NewMigrate([]string{filepath.Join(dir, "missing.zip")}, func(string, ...any) {})
	test.Assert(t, err != nil, "expected an error for a missing file")

	_, err = NewMigrate(nil, func(string, ...any) {})
	test.Assert(t, err != nil, "expected an error for an empty list")

	// a valid archive listed together with an invalid one fails fast,
	// without indexing the valid one
	_, err = NewMigrate([]string{filepath.Join(sevenzipTestdata, "basic.7z"), tarPath}, func(string, ...any) {})
	test.Assert(t, err != nil, "expected an error when one archive is unsupported")

	test.Equals(t, 0, len(warnings))
}

// TestMigrateArchiveInfo verifies that NewMigrate records, for every
// archive, the detected format label and the number of entries that were
// indexed and skipped, and that Archives() reports them in the order the
// archives were given.
func TestMigrateArchiveInfo(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "a.zip")
	writeTestZip(t, zipPath, []zipTestFile{
		{name: "ok.txt", content: []byte("zip kept")},
	})
	tarPath := filepath.Join(dir, "b.tar.gz")
	writeTestTar(t, tarPath, "gz", []tarTestEntry{
		{name: "d/", mode: 0o755, typ: tar.TypeDir},
		{name: "d/kept.txt", content: []byte("tar kept"), mode: 0o644},
		{name: "../escape.txt", content: []byte("skipped"), mode: 0o644},
	})

	var warnings []string
	m := openTestMigrate(t, []string{zipPath, tarPath}, &warnings)
	test.Equals(t, 1, len(warnings))

	infos := m.Archives()
	test.Equals(t, 2, len(infos))
	test.Equals(t, zipPath, infos[0].Path)
	test.Equals(t, "zip", infos[0].Label)
	test.Equals(t, 1, infos[0].Indexed)
	test.Equals(t, 0, infos[0].Skipped)
	test.Equals(t, tarPath, infos[1].Path)
	test.Equals(t, "tar.gz", infos[1].Label)
	test.Equals(t, 2, infos[1].Indexed)
	test.Equals(t, 1, infos[1].Skipped)
}
