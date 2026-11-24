# Seven-zip test fixtures

Binary `.7z` fixtures for the `fs` package unit tests
(`fs_sevenzip_test.go`). They are committed so the tests do not depend on an
installed 7z tool.

Regenerate all of them with:

    python3 generate.py

The generator needs `7z` on the PATH. If the local `7z` build does not
support the Zstd codec (`7z a -m0=ZSTD` fails with E_INVALIDARG), `zstd.7z`
is generated with `py7zr` instead (`pip install py7zr zstandard`); `empty.7z`
always uses `py7zr`.

Notes:

- `symlink.7z` was created by hand-patching the archive: 7-Zip on Linux
  stores symlinks as regular files, so the generator rewrites the `sIFLNK`
  (0xA1FF) attribute in the uncompressed header and recomputes the two
  header CRCs.
- `store.7z` is stored uncompressed so tests can corrupt a payload byte.
