package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	rtest "github.com/restic/restic/internal/test"
	"github.com/ulikunitz/xz"
)

// makeTestSevenZip creates a 7z archive at outPath containing the content of
// srcDir with relative entry names (as `7z a out.7z .` from within srcDir).
// The 7z command line tool is required; the calling test is skipped when it
// is not installed.
func makeTestSevenZip(t testing.TB, srcDir, outPath string) {
	t.Helper()
	bin, err := exec.LookPath("7z")
	if err != nil {
		t.Skip("7z command not found, skipping sevenzip test")
	}
	cmd := exec.Command(bin, "a", "-y", "-bso0", "-bd", outPath, ".")
	cmd.Dir = srcDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("7z failed: %v\n%s", err, out)
	}
}

// fixTreeMtimes sets the modification and access time of every file and
// directory in dir to a fixed whole-second instant: both zip and 7z store
// timestamps in seconds (7z keeps the nanoseconds, which are zero here), so
// a restored tree can be compared against the source tree.
func fixTreeMtimes(t testing.TB, dir string) {
	t.Helper()
	fixed := time.Date(2023, 5, 4, 3, 2, 1, 0, time.UTC)
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(p, fixed, fixed)
	})
	rtest.OK(t, err)
}

// makeTestZip creates a zip archive at zipPath containing the content of
// srcDir, preserving file modes and modification times. It is used to build
// fixture archives for the --migrate tests.
func makeTestZip(t testing.TB, srcDir, zipPath string) {
	t.Helper()

	zf, err := os.Create(zipPath)
	rtest.OK(t, err)
	zw := zip.NewWriter(zf)

	err = filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		info, err := d.Info()
		rtest.OK(t, err)

		hdr, err := zip.FileInfoHeader(info)
		rtest.OK(t, err)
		hdr.Name = filepath.ToSlash(rel)

		switch {
		case d.IsDir():
			hdr.Name += "/"
			_, err = zw.CreateHeader(hdr)
			rtest.OK(t, err)
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			rtest.OK(t, err)
			hdr.SetMode(os.ModeSymlink | 0o777)
			w, err := zw.CreateHeader(hdr)
			rtest.OK(t, err)
			_, err = io.WriteString(w, target)
			rtest.OK(t, err)
		default:
			w, err := zw.CreateHeader(hdr)
			rtest.OK(t, err)
			f, err := os.Open(path)
			rtest.OK(t, err)
			defer func() { _ = f.Close() }()
			_, err = io.Copy(w, f)
			rtest.OK(t, err)
		}
		return nil
	})
	rtest.OK(t, err)
	rtest.OK(t, zw.Close())
}

// createMigrateSourceTree creates a small fixture tree with files,
// directories, an empty file and a symlink. All modification times are set
// to a fixed instant with whole-second precision, because the zip format
// only stores timestamps in seconds and the restored tree must compare equal
// to the source tree.
func createMigrateSourceTree(t testing.TB, dir string) {
	t.Helper()
	rtest.OK(t, os.MkdirAll(filepath.Join(dir, "sub", "deep"), 0o755))
	rtest.OK(t, os.WriteFile(filepath.Join(dir, "file1.txt"), []byte("content one\n"), 0o644))
	rtest.OK(t, os.WriteFile(filepath.Join(dir, "sub", "file2.txt"), []byte("content two\n"), 0o644))
	rtest.OK(t, os.WriteFile(filepath.Join(dir, "sub", "deep", "file3.bin"), make([]byte, 4096), 0o640))
	rtest.OK(t, os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o644))
	if runtime.GOOS != "windows" {
		rtest.OK(t, os.Symlink("../file1.txt", filepath.Join(dir, "sub", "link1")))
	}

	fixed := time.Date(2023, 5, 4, 3, 2, 1, 0, time.UTC)
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return setSymlinkModTime(path, fixed)
		}
		return os.Chtimes(path, fixed, fixed)
	})
	rtest.OK(t, err)
}

func TestBackupMigrate(t *testing.T) {
	env, cleanup := withTestEnvironment(t)
	defer cleanup()
	testRunInit(t, env.gopts)

	srcDir := filepath.Join(env.base, "src")
	rtest.OK(t, os.MkdirAll(srcDir, 0o755))
	createMigrateSourceTree(t, srcDir)

	zipPath := filepath.Join(env.base, "archive.zip")
	makeTestZip(t, srcDir, zipPath)

	opts := BackupOptions{Migrate: []string{zipPath}}
	testRunBackup(t, "", nil, opts, env.gopts)
	snapshotIDs := testListSnapshots(t, env.gopts, 1)

	// restic ls must show the entries of the zip archive
	ls := testRunLs(t, env.gopts, snapshotIDs[0].String())
	t.Logf("ls output:\n%s", strings.Join(ls, "\n"))
	paths := []string{"/file1.txt", "/sub", "/sub/file2.txt", "/sub/deep", "/sub/deep/file3.bin", "/empty.txt"}
	if runtime.GOOS != "windows" {
		paths = append(paths, "/sub/link1")
	}
	for _, p := range paths {
		found := false
		for _, line := range ls {
			if strings.HasSuffix(line, p) {
				found = true
				break
			}
		}
		rtest.Assert(t, found, "ls: %q not found in snapshot", p)
	}

	// second backup of the same archive must not add new data to the
	// repository (deduplication still works with --force); only new tree
	// (snapshot) packs are allowed to appear
	packs1 := listPacks(env.gopts, t)
	testRunBackup(t, "", nil, opts, env.gopts)
	snapshotIDs = testListSnapshots(t, env.gopts, 2)

	packs2 := listPacks(env.gopts, t)
	treePacks2 := listTreePacks(env.gopts, t)
	for id := range packs2 {
		if packs1.Has(id) {
			continue
		}
		rtest.Assert(t, treePacks2.Has(id),
			"new data pack %v appeared after re-backup, deduplication failed", id)
	}

	// restore and compare with the source tree, which mirrors the zip content
	for i, snapshotID := range snapshotIDs {
		restoredir := filepath.Join(env.base, fmt.Sprintf("restore%d", i))
		testRunRestore(t, env.gopts, restoredir, snapshotID.String())
		diff := directoriesContentsDiff(t, srcDir, restoredir)
		rtest.Assert(t, diff == "", "restored content differs from source: %v", diff)
	}

	testRunCheck(t, env.gopts)
}

// TestBackupMigrateMultipleArchives backs up two archives at once with a
// repeated --migrate option: the snapshot contains the union of both
// archives, and an entry present in both keeps the version of the later
// archive.
func TestBackupMigrateMultipleArchives(t *testing.T) {
	env, cleanup := withTestEnvironment(t)
	defer cleanup()
	testRunInit(t, env.gopts)

	// the zip format only stores timestamps in seconds, so all fixture
	// trees use a fixed whole-second mtime
	fixed := time.Date(2023, 5, 4, 3, 2, 1, 0, time.UTC)
	fixMtimes := func(dir string) {
		t.Helper()
		err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Chtimes(p, fixed, fixed)
		})
		rtest.OK(t, err)
	}

	// archive A: a.txt, shared/fromA.txt, shared/onlyA.txt
	dirA := filepath.Join(env.base, "srcA")
	rtest.OK(t, os.MkdirAll(filepath.Join(dirA, "shared"), 0o755))
	rtest.OK(t, os.WriteFile(filepath.Join(dirA, "a.txt"), []byte("from A\n"), 0o644))
	rtest.OK(t, os.WriteFile(filepath.Join(dirA, "shared", "fromA.txt"), []byte("A version\n"), 0o644))
	rtest.OK(t, os.WriteFile(filepath.Join(dirA, "shared", "onlyA.txt"), []byte("only in A\n"), 0o644))
	fixMtimes(dirA)
	zipA := filepath.Join(env.base, "a.zip")
	makeTestZip(t, dirA, zipA)

	// archive B: b.txt, shared/fromA.txt (overrides A), shared/deep/onlyB.txt
	dirB := filepath.Join(env.base, "srcB")
	rtest.OK(t, os.MkdirAll(filepath.Join(dirB, "shared", "deep"), 0o755))
	rtest.OK(t, os.WriteFile(filepath.Join(dirB, "b.txt"), []byte("from B\n"), 0o644))
	rtest.OK(t, os.WriteFile(filepath.Join(dirB, "shared", "fromA.txt"), []byte("B version\n"), 0o644))
	rtest.OK(t, os.WriteFile(filepath.Join(dirB, "shared", "deep", "onlyB.txt"), []byte("only in B\n"), 0o644))
	fixMtimes(dirB)
	zipB := filepath.Join(env.base, "b.zip")
	makeTestZip(t, dirB, zipB)

	opts := BackupOptions{Migrate: []string{zipA, zipB}}
	testRunBackup(t, "", nil, opts, env.gopts)
	snapshotIDs := testListSnapshots(t, env.gopts, 1)

	ls := testRunLs(t, env.gopts, snapshotIDs[0].String())
	t.Logf("ls output:\n%s", strings.Join(ls, "\n"))
	for _, p := range []string{"/a.txt", "/b.txt", "/shared", "/shared/fromA.txt", "/shared/onlyA.txt", "/shared/deep", "/shared/deep/onlyB.txt"} {
		found := false
		for _, line := range ls {
			if strings.HasSuffix(line, p) {
				found = true
				break
			}
		}
		rtest.Assert(t, found, "ls: %q not found in snapshot", p)
	}

	// restore and compare against the expected merged tree, where the entry
	// present in both archives has the version of the second one
	expected := filepath.Join(env.base, "expected")
	rtest.OK(t, os.MkdirAll(filepath.Join(expected, "shared", "deep"), 0o755))
	rtest.OK(t, os.WriteFile(filepath.Join(expected, "a.txt"), []byte("from A\n"), 0o644))
	rtest.OK(t, os.WriteFile(filepath.Join(expected, "b.txt"), []byte("from B\n"), 0o644))
	rtest.OK(t, os.WriteFile(filepath.Join(expected, "shared", "fromA.txt"), []byte("B version\n"), 0o644))
	rtest.OK(t, os.WriteFile(filepath.Join(expected, "shared", "onlyA.txt"), []byte("only in A\n"), 0o644))
	rtest.OK(t, os.WriteFile(filepath.Join(expected, "shared", "deep", "onlyB.txt"), []byte("only in B\n"), 0o644))
	fixMtimes(expected)

	restoredir := filepath.Join(env.base, "restored")
	testRunRestore(t, env.gopts, restoredir, snapshotIDs[0].String())
	diff := directoriesContentsDiff(t, expected, restoredir)
	rtest.Assert(t, diff == "", "restored content differs from the merged archives: %v", diff)

	testRunCheck(t, env.gopts)
}

func TestBackupMigrateIncompatibleOptions(t *testing.T) {
	env, cleanup := withTestEnvironment(t)
	defer cleanup()
	testRunInit(t, env.gopts)

	cases := []struct {
		name string
		opts BackupOptions
		args []string
		want string
	}{
		{"positional args", BackupOptions{Migrate: []string{"archive.zip"}}, []string{"somefile"}, "cannot be combined with file or directory arguments"},
		{"stdin", BackupOptions{Migrate: []string{"archive.zip"}, Stdin: true}, nil, "cannot be combined with --stdin"},
		{"stdin from command", BackupOptions{Migrate: []string{"archive.zip"}, StdinCommand: true}, nil, "cannot be combined with --stdin"},
		{"files-from", BackupOptions{Migrate: []string{"archive.zip"}, FilesFrom: []string{"files.txt"}}, nil, "cannot be combined with --files-from"},
		{"parent", BackupOptions{Migrate: []string{"archive.zip"}, Parent: "latest"}, nil, "cannot be combined with --parent"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := testRunBackupAssumeFailure(t, "", tc.args, tc.opts, env.gopts)
			rtest.Assert(t, err != nil, "expected error for %s", tc.name)
			rtest.Assert(t, strings.Contains(err.Error(), tc.want), "unexpected error: %v", err)
		})
	}
}

func TestBackupMigrateInvalidZip(t *testing.T) {
	env, cleanup := withTestEnvironment(t)
	defer cleanup()

	// no repository is initialized: the zip must be validated before the
	// repository is opened, so the error must come from the zip file, not
	// from the repository
	err := testRunBackupAssumeFailure(t, "", nil,
		BackupOptions{Migrate: []string{filepath.Join(env.base, "missing.zip")}}, env.gopts)
	rtest.Assert(t, err != nil, "expected error for missing zip file")
	rtest.Assert(t, !strings.Contains(err.Error(), "repository"),
		"zip must be validated before the repository is opened, got: %v", err)
	t.Logf("error as expected: %v", err)

	// a valid zip without a repository must fail because of the repository
	srcDir := filepath.Join(env.base, "src")
	rtest.OK(t, os.MkdirAll(srcDir, 0o755))
	rtest.OK(t, os.WriteFile(filepath.Join(srcDir, "file.txt"), []byte("content"), 0o644))
	zipPath := filepath.Join(env.base, "archive.zip")
	makeTestZip(t, srcDir, zipPath)

	err = testRunBackupAssumeFailure(t, "", nil,
		BackupOptions{Migrate: []string{zipPath}}, env.gopts)
	rtest.Assert(t, err != nil, "expected error for missing repository")
	t.Logf("error as expected: %v", err)
}

func TestBackupMigrateExclude(t *testing.T) {
	env, cleanup := withTestEnvironment(t)
	defer cleanup()
	testRunInit(t, env.gopts)

	srcDir := filepath.Join(env.base, "src")
	rtest.OK(t, os.MkdirAll(srcDir, 0o755))
	createMigrateSourceTree(t, srcDir)
	zipPath := filepath.Join(env.base, "archive.zip")
	makeTestZip(t, srcDir, zipPath)

	opts := BackupOptions{Migrate: []string{zipPath}}
	opts.Excludes = []string{"*.bin"}
	testRunBackup(t, "", nil, opts, env.gopts)
	snapshotIDs := testListSnapshots(t, env.gopts, 1)

	ls := testRunLs(t, env.gopts, snapshotIDs[0].String())
	t.Logf("ls output:\n%s", strings.Join(ls, "\n"))
	for _, line := range ls {
		rtest.Assert(t, !strings.Contains(line, ".bin"), "excluded file found in snapshot: %s", line)
	}
	for _, p := range []string{"/file1.txt", "/sub/file2.txt"} {
		found := false
		for _, line := range ls {
			if strings.HasSuffix(line, p) {
				found = true
				break
			}
		}
		rtest.Assert(t, found, "ls: %q missing from snapshot", p)
	}
}

func TestBackupMigrateDryRun(t *testing.T) {
	env, cleanup := withTestEnvironment(t)
	defer cleanup()
	testRunInit(t, env.gopts)

	srcDir := filepath.Join(env.base, "src")
	rtest.OK(t, os.MkdirAll(srcDir, 0o755))
	createMigrateSourceTree(t, srcDir)
	zipPath := filepath.Join(env.base, "archive.zip")
	makeTestZip(t, srcDir, zipPath)

	opts := BackupOptions{Migrate: []string{zipPath}, DryRun: true}
	testRunBackup(t, "", nil, opts, env.gopts)

	// no snapshot must be created
	testListSnapshots(t, env.gopts, 0)
}

// TestBackupMigrateMemoryUsage verifies that backing up a large zip file
// keeps memory usage bounded (streaming) instead of growing with the size of
// the archive. It is skipped in short test mode.
func TestBackupMigrateMemoryUsage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping memory usage test in short mode")
	}
	if _, ok := peakRSSKiB(); !ok {
		t.Skip("maxrss measurement not available on this platform")
	}

	env, cleanup := withTestEnvironment(t)
	defer cleanup()
	testRunInit(t, env.gopts)

	// create a zip archive with a large (512 MiB), highly compressible file
	const totalSize = 512 << 20
	const blockSize = 64 << 10
	baseBlock := bytes.Repeat([]byte("restic-migrate-memory-test-"), blockSize/25)

	zipPath := filepath.Join(env.base, "big.zip")
	zf, err := os.Create(zipPath)
	rtest.OK(t, err)
	zw := zip.NewWriter(zf)

	hdr := &zip.FileHeader{Name: "bigfile.dat", Method: zip.Deflate}
	w, err := zw.CreateHeader(hdr)
	rtest.OK(t, err)

	done := make(chan error, 1)
	go func() {
		defer close(done)
		block := make([]byte, blockSize)
		copy(block, baseBlock)
		var err error
		for written := 0; written < totalSize; written += blockSize {
			// make each block unique so restic cannot skip the read
			// entirely, but keep it highly compressible
			block[0] = byte(written >> 24)
			block[1] = byte(written >> 16)
			block[2] = byte(written >> 8)
			block[3] = byte(written)
			if _, err = w.Write(block); err != nil {
				break
			}
		}
		done <- err
	}()
	if err := <-done; err != nil {
		t.Fatalf("writing zip: %v", err)
	}
	rtest.OK(t, zw.Close())

	opts := BackupOptions{Migrate: []string{zipPath}}
	testRunBackup(t, "", nil, opts, env.gopts)
	testListSnapshots(t, env.gopts, 1)

	peakKiB, ok := peakRSSKiB()
	if !ok {
		t.Skip("maxrss measurement not available on this platform")
	}

	peakMiB := peakKiB / 1024
	t.Logf("peak RSS after backing up %d MiB zip: %d MiB", totalSize>>20, peakMiB)

	// the backup must stream the file; allow a generous margin for the Go
	// runtime and the repository, but an implementation that buffers the
	// whole file would exceed this by far
	const maxPeakMiB = 768
	if peakMiB > maxPeakMiB {
		t.Errorf("peak RSS %d MiB exceeds %d MiB, backup does not stream the zip", peakMiB, maxPeakMiB)
	}

	testRunCheck(t, env.gopts)
}

// TestBackupMigrateSevenZip backs up a 7z archive and verifies that the
// snapshot matches the archive entries and that a restore reproduces the
// source tree (content, modes and mtimes). Note: 7z on Linux dereferences
// symlinks when creating an archive, so the fixture tree has none; symlink
// semantics are covered by the unit tests of the fs package.
func TestBackupMigrateSevenZip(t *testing.T) {
	env, cleanup := withTestEnvironment(t)
	defer cleanup()
	testRunInit(t, env.gopts)

	srcDir := filepath.Join(env.base, "src")
	rtest.OK(t, os.MkdirAll(filepath.Join(srcDir, "sub", "deep"), 0o755))
	rtest.OK(t, os.WriteFile(filepath.Join(srcDir, "file1.txt"), []byte("content one\n"), 0o644))
	rtest.OK(t, os.WriteFile(filepath.Join(srcDir, "sub", "file2.txt"), []byte("content two\n"), 0o640))
	rtest.OK(t, os.WriteFile(filepath.Join(srcDir, "sub", "deep", "file3.bin"), make([]byte, 4096), 0o640))
	rtest.OK(t, os.WriteFile(filepath.Join(srcDir, "empty.txt"), nil, 0o644))
	fixTreeMtimes(t, srcDir)

	sevenZipPath := filepath.Join(env.base, "archive.7z")
	makeTestSevenZip(t, srcDir, sevenZipPath)

	opts := BackupOptions{Migrate: []string{sevenZipPath}}
	testRunBackup(t, "", nil, opts, env.gopts)
	snapshotIDs := testListSnapshots(t, env.gopts, 1)

	// restic ls must show the entries of the 7z archive
	ls := testRunLs(t, env.gopts, snapshotIDs[0].String())
	t.Logf("ls output:\n%s", strings.Join(ls, "\n"))
	for _, p := range []string{"/file1.txt", "/sub", "/sub/file2.txt", "/sub/deep", "/sub/deep/file3.bin", "/empty.txt"} {
		found := false
		for _, line := range ls {
			if strings.HasSuffix(line, p) {
				found = true
				break
			}
		}
		rtest.Assert(t, found, "ls: %q not found in snapshot", p)
	}

	// restore and compare with the source tree
	restoredir := filepath.Join(env.base, "restore")
	testRunRestore(t, env.gopts, restoredir, snapshotIDs[0].String())
	diff := directoriesContentsDiff(t, srcDir, restoredir)
	rtest.Assert(t, diff == "", "restored content differs from source: %v", diff)
	for p, want := range map[string]string{
		"file1.txt":                       "content one\n",
		filepath.Join("sub", "file2.txt"): "content two\n",
	} {
		got, err := os.ReadFile(filepath.Join(restoredir, p))
		rtest.OK(t, err)
		rtest.Equals(t, want, string(got), "content of %s", p)
	}

	testRunCheck(t, env.gopts)
}

// TestBackupMigrateMixedZipSevenZip backs up a zip archive and a 7z archive
// together in one run: the snapshot contains the union of both archives, and
// an entry present in both keeps the version of the later archive.
func TestBackupMigrateMixedZipSevenZip(t *testing.T) {
	env, cleanup := withTestEnvironment(t)
	defer cleanup()
	testRunInit(t, env.gopts)

	// zip archive: a.txt, shared/fromZip.txt, shared/onlyZip.txt
	dirZip := filepath.Join(env.base, "srcZip")
	rtest.OK(t, os.MkdirAll(filepath.Join(dirZip, "shared"), 0o755))
	rtest.OK(t, os.WriteFile(filepath.Join(dirZip, "a.txt"), []byte("from zip\n"), 0o644))
	rtest.OK(t, os.WriteFile(filepath.Join(dirZip, "shared", "fromZip.txt"), []byte("zip version\n"), 0o644))
	rtest.OK(t, os.WriteFile(filepath.Join(dirZip, "shared", "onlyZip.txt"), []byte("only in zip\n"), 0o644))
	fixTreeMtimes(t, dirZip)
	zipPath := filepath.Join(env.base, "a.zip")
	makeTestZip(t, dirZip, zipPath)

	// 7z archive: b.txt, shared/fromZip.txt (overrides the zip entry),
	// shared/only7z.txt
	dir7z := filepath.Join(env.base, "src7z")
	rtest.OK(t, os.MkdirAll(filepath.Join(dir7z, "shared"), 0o755))
	rtest.OK(t, os.WriteFile(filepath.Join(dir7z, "b.txt"), []byte("from 7z\n"), 0o644))
	rtest.OK(t, os.WriteFile(filepath.Join(dir7z, "shared", "fromZip.txt"), []byte("7z version\n"), 0o644))
	rtest.OK(t, os.WriteFile(filepath.Join(dir7z, "shared", "only7z.txt"), []byte("only in 7z\n"), 0o644))
	fixTreeMtimes(t, dir7z)
	sevenZipPath := filepath.Join(env.base, "b.7z")
	makeTestSevenZip(t, dir7z, sevenZipPath)

	opts := BackupOptions{Migrate: []string{zipPath, sevenZipPath}}
	testRunBackup(t, "", nil, opts, env.gopts)
	snapshotIDs := testListSnapshots(t, env.gopts, 1)

	ls := testRunLs(t, env.gopts, snapshotIDs[0].String())
	t.Logf("ls output:\n%s", strings.Join(ls, "\n"))
	for _, p := range []string{"/a.txt", "/b.txt", "/shared", "/shared/fromZip.txt", "/shared/onlyZip.txt", "/shared/only7z.txt"} {
		found := false
		for _, line := range ls {
			if strings.HasSuffix(line, p) {
				found = true
				break
			}
		}
		rtest.Assert(t, found, "ls: %q not found in snapshot", p)
	}

	// restore and compare against the expected merged tree: the duplicated
	// entry keeps the version of the last archive (the 7z one)
	expected := filepath.Join(env.base, "expected")
	rtest.OK(t, os.MkdirAll(filepath.Join(expected, "shared"), 0o755))
	rtest.OK(t, os.WriteFile(filepath.Join(expected, "a.txt"), []byte("from zip\n"), 0o644))
	rtest.OK(t, os.WriteFile(filepath.Join(expected, "b.txt"), []byte("from 7z\n"), 0o644))
	rtest.OK(t, os.WriteFile(filepath.Join(expected, "shared", "fromZip.txt"), []byte("7z version\n"), 0o644))
	rtest.OK(t, os.WriteFile(filepath.Join(expected, "shared", "onlyZip.txt"), []byte("only in zip\n"), 0o644))
	rtest.OK(t, os.WriteFile(filepath.Join(expected, "shared", "only7z.txt"), []byte("only in 7z\n"), 0o644))
	fixTreeMtimes(t, expected)

	restoredir := filepath.Join(env.base, "restored")
	testRunRestore(t, env.gopts, restoredir, snapshotIDs[0].String())
	diff := directoriesContentsDiff(t, expected, restoredir)
	rtest.Assert(t, diff == "", "restored content differs from the merged archives: %v", diff)

	testRunCheck(t, env.gopts)
}

// makeTestTar creates a tar archive at tarPath containing the content of
// srcDir with relative entry names (as `tar -cf out.tar -C srcDir .`),
// preserving file modes and modification times. When format is "w:gz" or
// "w:xz" the archive is compressed with gzip or xz.
func makeTestTar(t testing.TB, srcDir, tarPath, format string) {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	err := filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		rtest.OK(t, err)
		linkTarget := ""
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err = os.Readlink(path)
			rtest.OK(t, err)
		}
		hdr, err := tar.FileInfoHeader(info, linkTarget)
		rtest.OK(t, err)
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		rtest.OK(t, w.WriteHeader(hdr))
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			rtest.OK(t, err)
			defer func() { _ = f.Close() }()
			_, err = io.Copy(w, f)
			rtest.OK(t, err)
		}
		return nil
	})
	rtest.OK(t, err)
	rtest.OK(t, w.Close())

	f, err := os.Create(tarPath)
	rtest.OK(t, err)
	defer func() { _ = f.Close() }()
	var out io.Writer = f
	var closeOut io.Closer
	switch format {
	case "w:gz":
		gz := gzip.NewWriter(f)
		out = gz
		closeOut = gz
	case "w:xz":
		xzw, err := xz.NewWriter(f)
		rtest.OK(t, err)
		out = xzw
		closeOut = xzw
	}
	_, err = io.Copy(out, &buf)
	rtest.OK(t, err)
	if closeOut != nil {
		rtest.OK(t, closeOut.Close())
	}
}

// TestBackupMigrateTar backs up a tar archive in each of its plain,
// gzipped and xz-compressed forms and verifies that the snapshot matches the
// archive entries and that a restore reproduces the source tree (content,
// modes and mtimes).
func TestBackupMigrateTar(t *testing.T) {
	for _, format := range []string{"w", "w:gz", "w:xz"} {
		t.Run(format, func(t *testing.T) {
			env, cleanup := withTestEnvironment(t)
			defer cleanup()
			testRunInit(t, env.gopts)

			srcDir := filepath.Join(env.base, "src")
			rtest.OK(t, os.MkdirAll(srcDir, 0o755))
			createMigrateSourceTree(t, srcDir)

			suffix := ".tar"
			if format == "w:gz" {
				suffix = ".tar.gz"
			}
			if format == "w:xz" {
				suffix = ".tar.xz"
			}
			tarPath := filepath.Join(env.base, "archive"+suffix)
			makeTestTar(t, srcDir, tarPath, format)

			opts := BackupOptions{Migrate: []string{tarPath}}
			testRunBackup(t, "", nil, opts, env.gopts)
			snapshotIDs := testListSnapshots(t, env.gopts, 1)

			ls := testRunLs(t, env.gopts, snapshotIDs[0].String())
			t.Logf("ls output:\n%s", strings.Join(ls, "\n"))
			paths := []string{"/file1.txt", "/sub", "/sub/file2.txt", "/sub/deep", "/sub/deep/file3.bin", "/empty.txt"}
			if runtime.GOOS != "windows" {
				paths = append(paths, "/sub/link1")
			}
			for _, p := range paths {
				found := false
				for _, line := range ls {
					if strings.HasSuffix(line, p) {
						found = true
						break
					}
				}
				rtest.Assert(t, found, "ls: %q not found in snapshot", p)
			}

			restoredir := filepath.Join(env.base, "restore")
			testRunRestore(t, env.gopts, restoredir, snapshotIDs[0].String())
			diff := directoriesContentsDiff(t, srcDir, restoredir)
			rtest.Assert(t, diff == "", "restored content differs from source: %v", diff)

			testRunCheck(t, env.gopts)
		})
	}
}

// TestBackupMigrateStandaloneGzXz backs up .gz and .xz files that do not
// contain a tar archive: each file is stored as a single virtual file named
// after the archive, without the compression suffix.
func TestBackupMigrateStandaloneGzXz(t *testing.T) {
	env, cleanup := withTestEnvironment(t)
	defer cleanup()
	testRunInit(t, env.gopts)

	gzPath := filepath.Join(env.base, "report.gz")
	f, err := os.Create(gzPath)
	rtest.OK(t, err)
	gz := gzip.NewWriter(f)
	_, err = gz.Write([]byte("gzip report content\n"))
	rtest.OK(t, err)
	rtest.OK(t, gz.Close())
	rtest.OK(t, f.Close())

	xzPath := filepath.Join(env.base, "notes.xz")
	f, err = os.Create(xzPath)
	rtest.OK(t, err)
	xzw, err := xz.NewWriter(f)
	rtest.OK(t, err)
	_, err = xzw.Write([]byte("xz notes content\n"))
	rtest.OK(t, err)
	rtest.OK(t, xzw.Close())
	rtest.OK(t, f.Close())

	opts := BackupOptions{Migrate: []string{gzPath, xzPath}}
	testRunBackup(t, "", nil, opts, env.gopts)
	snapshotIDs := testListSnapshots(t, env.gopts, 1)

	ls := testRunLs(t, env.gopts, snapshotIDs[0].String())
	t.Logf("ls output:\n%s", strings.Join(ls, "\n"))
	for _, p := range []string{"/report", "/notes"} {
		found := false
		for _, line := range ls {
			if strings.HasSuffix(line, p) {
				found = true
				break
			}
		}
		rtest.Assert(t, found, "ls: %q not found in snapshot", p)
	}

	restoredir := filepath.Join(env.base, "restore")
	testRunRestore(t, env.gopts, restoredir, snapshotIDs[0].String())
	report, err := os.ReadFile(filepath.Join(restoredir, "report"))
	rtest.OK(t, err)
	rtest.Equals(t, "gzip report content\n", string(report))
	notes, err := os.ReadFile(filepath.Join(restoredir, "notes"))
	rtest.OK(t, err)
	rtest.Equals(t, "xz notes content\n", string(notes))

	testRunCheck(t, env.gopts)
}
