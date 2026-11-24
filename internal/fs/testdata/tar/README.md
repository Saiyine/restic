# Tar test fixtures

Binary tar fixtures for the `fs` package unit tests
(`fs_tar_test.go`): plain (`.tar`), gzipped (`.tar.gz`) and xz-compressed
(`.tar.xz`) archives, plus standalone `.gz` and `.xz` files that do not
contain a tar. They are committed so the tests do not depend on installed
`tar`/`gzip`/`xz` tools.

Regenerate all of them with:

    python3 generate.py

The generator only needs Python 3 (`tarfile`, `gzip` and `lzma` are in the
standard library) and is fully deterministic: every file has a fixed
modification time, and the gzip archives use a fixed mtime in their
headers.

Notes:

- `gz.tar.gz` is the "everything" fixture: PAX long name (>100 chars),
  symlink, hard link, directory with mode 0700 and a character device
  (skipped with a warning by the indexer).
- `empty.tar` is two zero blocks: an all-zero first block is how tar
  archives end, so it must probe as a valid empty tar.
- `large.tar.gz` carries a 100 KiB payload to exercise the streaming
  reader and the limited reads.
- `dup_a.tar`/`dup_b.tar` share entries to test the "last one wins" merge
  rule; `alpha.gz`/`beta.xz` are standalone single-file archives.
