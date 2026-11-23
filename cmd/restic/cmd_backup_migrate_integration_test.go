package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	rtest "github.com/restic/restic/internal/test"
)

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

	opts := BackupOptions{Migrate: zipPath}
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
		{"positional args", BackupOptions{Migrate: "archive.zip"}, []string{"somefile"}, "cannot be combined with file or directory arguments"},
		{"stdin", BackupOptions{Migrate: "archive.zip", Stdin: true}, nil, "cannot be combined with --stdin"},
		{"stdin from command", BackupOptions{Migrate: "archive.zip", StdinCommand: true}, nil, "cannot be combined with --stdin"},
		{"files-from", BackupOptions{Migrate: "archive.zip", FilesFrom: []string{"files.txt"}}, nil, "cannot be combined with --files-from"},
		{"parent", BackupOptions{Migrate: "archive.zip", Parent: "latest"}, nil, "cannot be combined with --parent"},
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
		BackupOptions{Migrate: filepath.Join(env.base, "missing.zip")}, env.gopts)
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
		BackupOptions{Migrate: zipPath}, env.gopts)
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

	opts := BackupOptions{Migrate: zipPath}
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

	opts := BackupOptions{Migrate: zipPath, DryRun: true}
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

	opts := BackupOptions{Migrate: zipPath}
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
