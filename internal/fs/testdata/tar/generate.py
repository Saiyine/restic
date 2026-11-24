#!/usr/bin/env python3
"""Regenerates the tar fixtures in this directory.

Pure Python (tarfile + gzip + lzma); no external tools are required. The
fixtures are deterministic: all mtimes are fixed to whole seconds, all
modes are set explicitly, and the gzip header timestamp is pinned.

Run from anywhere:  python3 generate.py
"""

import gzip
import io
import lzma
import os
import tarfile
from datetime import datetime, timezone

HERE = os.path.dirname(os.path.abspath(__file__))

# Same fixed mtimes as the zip and 7z unit tests.
MTIME_A = datetime(2020, 1, 2, 3, 4, 5, tzinfo=timezone.utc)
MTIME_B = datetime(2021, 6, 7, 8, 9, 10, tzinfo=timezone.utc)


def info(name, size=0, mode=0o644, mtime=MTIME_A,
         type=tarfile.REGTYPE, linkname=""):
    ti = tarfile.TarInfo(name)
    ti.size = size
    ti.mode = mode
    ti.mtime = int(mtime.timestamp())
    ti.type = type
    ti.linkname = linkname
    return ti


def make_tar(entries, name, format=tarfile.USTAR_FORMAT):
    """Builds the tar bytes of entries (a list of
    (tarinfo, bytes-or-None)) and writes them to name."""
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w", format=format) as tf:
        for ti, data in entries:
            tf.addfile(ti, io.BytesIO(data) if data is not None else None)
    with open(name, "wb") as f:
        f.write(buf.getvalue())


def write_gz(data, name, mtime=MTIME_A):
    # pin the gzip header timestamp so the fixture is byte-stable
    with open(name, "wb") as f:
        with gzip.GzipFile(filename="", mode="wb", fileobj=f, mtime=int(mtime.timestamp())) as gz:
            gz.write(data)


def write_xz(data, name):
    with lzma.open(name, "wb", format=lzma.FORMAT_XZ) as f:
        f.write(data)


def gen_basic():
    """Plain tar: hierarchy, explicit and implicit dirs, modes, empty file."""
    make_tar([
        (info("top.txt", 3, 0o640, MTIME_A), b"top"),
        (info("docs/", 0, 0o755, MTIME_A, type=tarfile.DIRTYPE), None),
        (info("docs/readme.md", 11, 0o644, MTIME_B), b"hello world"),
        (info("deep/nested/file.bin", 4, 0o600, MTIME_B), bytes([0, 1, 2, 3])),
        (info("empty.bin", 0, 0o600, MTIME_A), b""),
    ], os.path.join(HERE, "basic.tar"))


def gen_gz():
    """gzipped tar: codecs, symlink, hard link, PAX long name, modes,
    a character device (skipped with a warning)."""
    longname = "longdir/" + "a" * 120 + ".txt"
    make_tar([
        (info("data.txt", 15, 0o644, MTIME_A), b"gzip data here\n"),
        (info("target.txt", 11, 0o644, MTIME_A), b"the target\n"),
        (info("link.txt", 0, 0o777, MTIME_A, type=tarfile.SYMTYPE,
              linkname="target.txt"), None),
        (info("hard.txt", 0, 0o644, MTIME_A, type=tarfile.LNKTYPE,
              linkname="target.txt"), None),
        (info("secret/", 0, 0o700, MTIME_A, type=tarfile.DIRTYPE), None),
        (info("secret/hidden.txt", 6, 0o600, MTIME_A), b"hidden"),
        (info(longname, 5, 0o644, MTIME_B), b"long\n"),
        (info("dev", 0, 0o600, MTIME_A, type=tarfile.CHRTYPE), None),
    ], os.path.join(HERE, "gz.tar.gz"), format=tarfile.PAX_FORMAT)
    # wrap in gzip with a pinned timestamp
    with open(os.path.join(HERE, "gz.tar.gz"), "rb") as f:
        data = f.read()
    os.remove(os.path.join(HERE, "gz.tar.gz"))
    write_gz(data, os.path.join(HERE, "gz.tar.gz"))


def gen_xz():
    """xz-compressed tar."""
    path = os.path.join(HERE, "xz.tar.xz")
    make_tar([
        (info("data.txt", 13, 0o644, MTIME_A), b"xz data here\n"),
        (info("sub/", 0, 0o755, MTIME_A, type=tarfile.DIRTYPE), None),
        (info("sub/inner.txt", 11, 0o644, MTIME_B), b"inner data\n"),
    ], path, format=tarfile.PAX_FORMAT)
    with open(path, "rb") as f:
        data = f.read()
    os.remove(path)
    write_xz(data, path)


def gen_standalone():
    """Standalone .gz/.xz files (no tar inside): one virtual file each."""
    write_gz(b"gzip standalone content\n", os.path.join(HERE, "alpha.gz"))
    write_xz(b"xz standalone content\n", os.path.join(HERE, "beta.xz"))


def gen_dups():
    """Two archives with an overlapping entry: last one wins."""
    make_tar([
        (info("same.txt", 10, 0o644, MTIME_A), b"version A\n"),
        (info("only_a.txt", 10, 0o644, MTIME_A), b"only in a\n"),
    ], os.path.join(HERE, "dup_a.tar"))
    make_tar([
        (info("same.txt", 10, 0o644, MTIME_B), b"version B\n"),
        (info("only_b.txt", 10, 0o644, MTIME_A), b"only in b\n"),
    ], os.path.join(HERE, "dup_b.tar"))


def gen_empty():
    make_tar([], os.path.join(HERE, "empty.tar"))


def gen_large():
    """1 MiB file, gzipped: for the no-disk-access test."""
    path = os.path.join(HERE, "large.tar.gz")
    make_tar([
        (info("large.bin", 1 << 20, 0o644, MTIME_A), b"x" * (1 << 20)),
    ], path, format=tarfile.PAX_FORMAT)
    with open(path, "rb") as f:
        data = f.read()
    os.remove(path)
    write_gz(data, path)


def main():
    gen_basic()
    gen_gz()
    gen_xz()
    gen_standalone()
    gen_dups()
    gen_empty()
    gen_large()
    print("fixtures regenerated in", HERE)


if __name__ == "__main__":
    main()
