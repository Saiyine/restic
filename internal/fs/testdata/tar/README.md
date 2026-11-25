# Tar test fixtures

Binary tar fixtures for the `fs` package unit tests
(`fs_tar_test.go`): plain (`.tar`), gzipped (`.tar.gz`), xz-compressed
(`.tar.xz`), bzip2-compressed (`.tar.bz2`) and zstd-compressed
(`.tar.zst`) archives, plus standalone `.gz`, `.xz`, `.bz2` and `.zst`
files that do not contain a tar. They are committed so the tests do not
depend on installed `tar`/`gzip`/`xz` tools.

Regenerate all of them with:

    python3 generate.py

The generator uses `tarfile`, `gzip`, `lzma` and `bz2` from the Python
standard library; only zstd compression additionally needs the
`zstandard` package (`pip install zstandard`) to regenerate the `*.zst`
fixtures. It is fully deterministic: every file has a fixed modification
time, and the gzip archives use a fixed mtime in their headers.

Notes:

- `gz.tar.gz` is the "everything" fixture: PAX long name (>100 chars),
  symlink, hard link, directory with mode 0700 and a character device
  (skipped with a warning by the indexer).
- `empty.tar` is two zero blocks: an all-zero first block is how tar
  archives end, so it must probe as a valid empty tar.
- `large.tar.gz` carries a 1 MiB payload to exercise the streaming
  reader and the limited reads.
- `dup_a.tar`/`dup_b.tar` share entries to test the "last one wins" merge
  rule; `alpha.gz`/`beta.xz`/`gamma.bz2`/`delta.zst` are standalone
  single-file archives.
