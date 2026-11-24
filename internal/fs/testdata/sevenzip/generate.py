#!/usr/bin/env python3
"""Regenerates the .7z fixtures in this directory.

Requires the 7z command line tool (7-Zip >= 24) on the PATH. The fixtures
are deterministic: all mtimes are fixed to whole seconds and all modes are
set explicitly.

Run from anywhere:  python3 generate.py
"""

import os
import shutil
import struct
import subprocess
import sys
import tempfile
import zlib
from datetime import datetime, timezone

HERE = os.path.dirname(os.path.abspath(__file__))

# Same fixed mtimes as the zip unit tests (internal/fs/fs_zip_test.go).
MTIME_A = datetime(2020, 1, 2, 3, 4, 5, tzinfo=timezone.utc)
MTIME_B = datetime(2021, 6, 7, 8, 9, 10, tzinfo=timezone.utc)


def ts(dt):
    return (dt.timestamp(), dt.timestamp())


def write(path, content, mode, mtime):
    with open(path, "wb") as f:
        f.write(content)
    os.chmod(path, mode)
    os.utime(path, ts(mtime), follow_symlinks=False)


def mkdir(path, mode, mtime):
    os.makedirs(path, exist_ok=True)
    os.chmod(path, mode)
    os.utime(path, ts(mtime))


def sevenzip(*args, cwd=None):
    out = args[0]
    if os.path.exists(out):
        os.remove(out)
    subprocess.run(["7z", "a", "-y", "-bso0", "-bd"] + list(args), check=True, cwd=cwd)


def retouch(path, mtime):
    # Creating entries in a directory updates its mtime; 7z stores explicit
    # directory mtimes, so re-fix them after the children are written.
    os.utime(path, ts(mtime))


def gen_basic(root):
    """LZMA2 (default), solid: hierarchy, explicit and implicit dirs,
    modes and empty file."""
    src = os.path.join(root, "basic")
    mkdir(os.path.join(src, "docs"), 0o755, MTIME_A)
    mkdir(os.path.join(src, "deep", "nested"), 0o755, MTIME_B)
    write(os.path.join(src, "top.txt"), b"top", 0o640, MTIME_A)
    write(os.path.join(src, "docs", "readme.md"), b"hello world", 0o644, MTIME_B)
    write(os.path.join(src, "deep", "nested", "file.bin"), bytes([0, 1, 2, 3]), 0o600, MTIME_B)
    write(os.path.join(src, "empty.bin"), b"", 0o600, MTIME_A)
    retouch(os.path.join(src, "docs"), MTIME_A)
    retouch(os.path.join(src, "deep", "nested"), MTIME_B)
    retouch(os.path.join(src, "deep"), MTIME_B)
    sevenzip(os.path.join(HERE, "basic.7z"), cwd=src)


def gen_single(root, name, method_args, content, mtime=MTIME_A, mode=0o644):
    src = os.path.join(root, name)
    os.makedirs(src, exist_ok=True)
    write(os.path.join(src, "data.txt"), content, mode, mtime)
    sevenzip(os.path.join(HERE, name + ".7z"), *method_args, cwd=src)


def gen_zstd(root):
    """Single file compressed with Zstd. Most 7z builds support the codec;
    if this one does not (E_INVALIDARG), fall back to py7zr + zstandard."""
    src = os.path.join(root, "zstd")
    os.makedirs(src, exist_ok=True)
    write(os.path.join(src, "data.txt"), b"zstd data here\n", 0o644, MTIME_A)
    out = os.path.join(HERE, "zstd.7z")
    try:
        sevenzip(out, "-m0=ZSTD", cwd=src)
    except subprocess.CalledProcessError:
        import py7zr  # pip install py7zr zstandard
        with py7zr.SevenZipFile(out, "w", filters=[{"id": py7zr.FILTER_ZSTD}]) as z:
            z.write(os.path.join(src, "data.txt"), "data.txt")


def gen_modes(root):
    src = os.path.join(root, "modes")
    mkdir(os.path.join(src, "secret"), 0o700, MTIME_A)
    write(os.path.join(src, "script.sh"), b"#!/bin/sh\necho hi\n", 0o755, MTIME_A)
    write(os.path.join(src, "secret", "hidden.txt"), b"hidden", 0o600, MTIME_A)
    retouch(os.path.join(src, "secret"), MTIME_A)
    sevenzip(os.path.join(HERE, "modes.7z"), cwd=src)


def gen_symlink(root):
    """A symlink entry: 7-Zip on Linux stores symlinks as regular files, so
    the sIFLNK attribute (high word 0xA1FF) is patched into the
    (uncompressed) header after the archive is created, and the header CRC
    is recomputed. Header layout: the start header is 32 bytes,
    NextHeaderOffset (offset 12) is relative to the end of it, and
    NextHeaderCRC (offset 28) covers the header bytes."""
    src = os.path.join(root, "symlink")
    os.makedirs(src, exist_ok=True)
    write(os.path.join(src, "link.txt"), b"target.txt", 0o640, MTIME_A)
    write(os.path.join(src, "target.txt"), b"the target", 0o644, MTIME_A)
    out = os.path.join(HERE, "symlink.7z")
    sevenzip(out, "-mhc=off", cwd=src)

    data = bytearray(open(out, "rb").read())
    nh_off, nh_size = struct.unpack_from("<QQ", data, 12)
    base = 32 + nh_off
    header = bytes(data[base:base + nh_size])
    old = bytes([0x20, 0x80, 0xA0, 0x81])  # attrs of a 0640 regular file
    new = bytes([0x20, 0x80, 0xFF, 0xA1])  # sIFLNK | 0777
    count = header.count(old)
    if count != 1:
        sys.exit(f"expected exactly 1 occurrence of {old.hex()}, found {count}")
    header = header.replace(old, new)
    data[base:base + nh_size] = header
    # the start header CRC (bytes 8-11) covers bytes 12-32, which include
    # the NextHeaderCRC field, so update it last
    struct.pack_into("<I", data, 28, zlib.crc32(header) & 0xFFFFFFFF)
    struct.pack_into("<I", data, 8, zlib.crc32(bytes(data[12:32])) & 0xFFFFFFFF)
    open(out, "wb").write(bytes(data))


def gen_encrypted(root):
    src = os.path.join(root, "encrypted")
    os.makedirs(src, exist_ok=True)
    write(os.path.join(src, "data.txt"), b"secret payload", 0o640, MTIME_A)
    # -mhe=off: only the payload is encrypted, so the header (and the
    # archive itself) still parses without the password.
    sevenzip(os.path.join(HERE, "encrypted.7z"), "-psecret", "-mhe=off", cwd=src)


def gen_dups(root):
    for suffix, content in (("a", b"version A\n"), ("b", b"version B\n")):
        src = os.path.join(root, f"dup_{suffix}")
        os.makedirs(src, exist_ok=True)
        write(os.path.join(src, "same.txt"), content, 0o644, MTIME_A if suffix == "a" else MTIME_B)
        write(os.path.join(src, f"only_{suffix}.txt"), f"only in {suffix}\n".encode(), 0o644, MTIME_A)
        sevenzip(os.path.join(HERE, f"dup_{suffix}.7z"), cwd=src)


def gen_conflicts(root):
    """Two archives with the same path as directory / as file."""
    src = os.path.join(root, "conflict_dir")
    mkdir(os.path.join(src, "d"), 0o755, MTIME_A)
    write(os.path.join(src, "d", "inside.txt"), b"inside\n", 0o644, MTIME_A)
    retouch(os.path.join(src, "d"), MTIME_A)
    sevenzip(os.path.join(HERE, "conflict_dir.7z"), cwd=src)

    src = os.path.join(root, "conflict_file")
    os.makedirs(src, exist_ok=True)
    write(os.path.join(src, "d"), b"a file\n", 0o644, MTIME_B)
    sevenzip(os.path.join(HERE, "conflict_file.7z"), cwd=src)


def gen_unicode(root):
    src = os.path.join(root, "unicode")
    os.makedirs(os.path.join(src, "café"), exist_ok=True)
    write(os.path.join(src, "café", "ñandú.txt"), "contenidor\n".encode(), 0o644, MTIME_A)
    retouch(os.path.join(src, "café"), MTIME_A)
    sevenzip(os.path.join(HERE, "unicode.7z"), cwd=src)


def gen_empty(root):
    """An archive with no entries (7z has no CLI switch for this)."""
    import py7zr  # pip install py7zr
    out = os.path.join(HERE, "empty.7z")
    if os.path.exists(out):
        os.remove(out)
    with py7zr.SevenZipFile(out, "w"):
        pass


def gen_solid(root):
    """Several files in one solid LZMA2 block, uncompressed header: for
    the corrupt-payload test (the header parses, the entry read fails)."""
    src = os.path.join(root, "solid")
    os.makedirs(src, exist_ok=True)
    for i in range(1, 4):
        write(os.path.join(src, f"part{i}.txt"), f"part {i} of the solid block\n".encode() * 8, 0o644, MTIME_A)
    sevenzip(os.path.join(HERE, "solid.7z"), "-mhc=off", cwd=src)


def gen_large(root):
    """1 MiB file, compressed: for the no-disk-access test."""
    src = os.path.join(root, "large")
    os.makedirs(src, exist_ok=True)
    write(os.path.join(src, "large.bin"), b"x" * (1 << 20), 0o644, MTIME_A)
    sevenzip(os.path.join(HERE, "large.7z"), cwd=src)


def main():
    root = tempfile.mkdtemp(prefix="7zfixtures.")
    try:
        gen_basic(root)
        gen_zstd(root)
        gen_single(root, "lzma", ["-m0=LZMA"], b"lzma data here\n")
        gen_single(root, "store", ["-mx=0"], b"0123456789" * 16 + b"!\n")
        gen_modes(root)
        gen_symlink(root)
        gen_encrypted(root)
        gen_dups(root)
        gen_conflicts(root)
        gen_unicode(root)
        gen_solid(root)
        gen_empty(root)
        gen_large(root)
    finally:
        shutil.rmtree(root, ignore_errors=True)
    print("fixtures regenerated in", HERE)


if __name__ == "__main__":
    main()
