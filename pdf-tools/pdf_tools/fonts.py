"""POST /v1/font/sanitize: a font someone uploaded, checked the way browsers
check web fonts (OTS), read with fontTools, and written back as WOFF2
(README.md)."""

from __future__ import annotations

import io
import os
import string
import subprocess
import tempfile
import unicodedata

import brotli
import ots
from fontTools.ttLib import TTFont, woff2

from .errors import Problem

MAX_FONT_BYTES = 10 << 20
# The sanitized sfnt OTS may write (a WOFF2 inflates): more is refused.
MAX_SFNT_BYTES = 48 << 20

# The letters whose absence missingTurkish lists, in this order.
TURKISH = "ğĞıİşŞçÇöÖüÜ"
CHECKED = TURKISH + string.ascii_uppercase + string.ascii_lowercase + string.digits

# The sanitizer's command (the tests put another in its place).
OTS_COMMAND = [ots.OTS_SANITIZE]

# fsType bit 1: Restricted License embedding; bit 9: bitmap embedding only
# (the outlines may not be embedded, which a web page and a PDF need).
FS_TYPE_RESTRICTED = 0x0002
FS_TYPE_BITMAP_ONLY = 0x0200

# brotli quality by the sfnt's size: 11 (WOFF2's usual) is slow on large
# fonts, and a request has 10 s on half a CPU.
_QUALITY = ((1 << 20, 11), (4 << 20, 9))
_QUALITY_LARGE = 5
# Above this the glyf table is not transformed (WOFF2's glyf transform
# reads every glyph: a large font would need more memory than a worker has).
TRANSFORM_MAX_SFNT_BYTES = 2 << 20


def sniff(data: bytes) -> str:
    """ttf, otf or woff2 by the file's first bytes; a collection or anything
    else is refused."""
    head = data[:4]
    if head in (b"\x00\x01\x00\x00", b"true"):
        return "ttf"
    if head == b"OTTO":
        return "otf"
    if head == b"wOF2":
        # A WOFF2 holds a collection when its flavor is ttcf.
        if data[4:8] == b"ttcf":
            raise Problem("font_collection", "a font collection (TTC); send one font")
        return "woff2"
    if head == b"ttcf":
        raise Problem("font_collection", "a font collection (TTC); send one font")
    if head == b"wOFF":
        raise Problem("font_unreadable", "WOFF 1 is not taken; send TTF, OTF or WOFF2")
    raise Problem("font_unreadable", "not a TrueType, OpenType or WOFF2 font")


def _ots(data: bytes, timeout: float) -> bytes:
    """The font through OTS (ots-sanitize): the sanitized sfnt. The input
    goes in on stdin; OTS seeks in its output, so that is a file in TMPDIR
    (a tmpfs in the container)."""
    fd, out = tempfile.mkstemp(prefix="ots-", suffix=".sfnt")
    os.close(fd)
    try:
        try:
            proc = subprocess.run(
                [*OTS_COMMAND, "/dev/stdin", out],
                input=data,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                timeout=timeout,
                check=False,
            )
        except subprocess.TimeoutExpired:
            raise Problem("font_timeout", "sanitizing the font took too long") from None
        if proc.returncode != 0:
            raise Problem("font_unreadable", "the font did not pass the sanitizer", reason=_ots_reason(proc))
        size = os.path.getsize(out)
        if size > MAX_SFNT_BYTES:
            raise Problem("font_unreadable", "the sanitized font is too large")
        with open(out, "rb") as f:
            return f.read()
    finally:
        try:
            os.unlink(out)
        except OSError:
            pass


def _ots_reason(proc: subprocess.CompletedProcess) -> str:
    """OTS's first error line, without paths, short."""
    text = (proc.stderr or b"").decode("utf-8", "replace") + (proc.stdout or b"").decode("utf-8", "replace")
    for line in text.splitlines():
        line = line.strip()
        if line.startswith("ERROR:"):
            return line[len("ERROR:") :].strip()[:200]
    return "rejected"


def _clean(name: str | None) -> str:
    if not name:
        return ""
    out = "".join(" " if unicodedata.category(c)[0] in ("C", "Z") else c for c in name)
    return " ".join(out.split())


def _name(font: TTFont, *ids: int) -> str:
    table = font.get("name")
    if table is None:
        return ""
    for name_id in ids:
        value = _clean(table.getDebugName(name_id))
        if value:
            return value
    return ""


def _weight(os2) -> int:
    if os2 is None:
        return 400
    w = int(getattr(os2, "usWeightClass", 400))
    # Some old fonts write 1–9 for 100–900.
    if 1 <= w <= 9:
        return w * 100
    if 1 <= w <= 1000:
        return w
    return 400


def _quality(size: int) -> int:
    for limit, q in _QUALITY:
        if size <= limit:
            return q
    return _QUALITY_LARGE


class _Brotli:
    """brotli for fontTools' WOFF2 writer at a chosen quality."""

    MODE_GENERIC = brotli.MODE_GENERIC
    MODE_TEXT = brotli.MODE_TEXT
    MODE_FONT = brotli.MODE_FONT
    error = brotli.error

    def __init__(self, quality: int) -> None:
        self.quality = quality

    def compress(self, data: bytes, mode: int = brotli.MODE_GENERIC, **_: object) -> bytes:
        return brotli.compress(data, mode=mode, quality=self.quality)

    @staticmethod
    def decompress(data: bytes) -> bytes:
        return brotli.decompress(data)


def _woff2(sfnt: bytes) -> bytes:
    out = io.BytesIO()
    saved = woff2.brotli
    woff2.brotli = _Brotli(_quality(len(sfnt)))  # type: ignore[assignment]
    try:
        transform = None if len(sfnt) <= TRANSFORM_MAX_SFNT_BYTES else ()
        woff2.compress(io.BytesIO(sfnt), out, transform_tables=transform)
    finally:
        woff2.brotli = saved
    data = out.getvalue()
    if not data.startswith(b"wOF2"):
        raise Problem("font_unreadable", "the font could not be written as WOFF2")
    return data


def sanitize(data: bytes, timeout: float = 10.0) -> dict:
    """The checks of README.md in order: size and signature, OTS, fvar and
    fsType, the facts, WOFF2."""
    if len(data) > MAX_FONT_BYTES:
        raise Problem("font_too_large", f"at most {MAX_FONT_BYTES >> 20} MiB")
    sniff(data)
    sfnt = _ots(data, timeout)
    if sfnt[:4] == b"ttcf":
        raise Problem("font_collection", "a font collection (TTC); send one font")
    font = TTFont(io.BytesIO(sfnt), lazy=True, recalcBBoxes=False, recalcTimestamp=False)
    try:
        if "fvar" in font:
            raise Problem("font_variable", "a variable font; send one static instance")
        os2 = font["OS/2"] if "OS/2" in font else None
        fs_type = int(getattr(os2, "fsType", 0)) if os2 is not None else 0
        if fs_type & FS_TYPE_RESTRICTED:
            raise Problem("font_embedding_restricted", "the font's embedding permission is Restricted License", fsType=fs_type)
        if fs_type & FS_TYPE_BITMAP_ONLY:
            raise Problem("font_embedding_restricted", "the font allows bitmap embedding only", fsType=fs_type)
        family = _name(font, 16, 1)
        if not family:
            raise Problem("font_unreadable", "the font has no family name")
        subfamily = _name(font, 17, 2)
        italic = False
        if os2 is not None and int(getattr(os2, "fsSelection", 0)) & 0x0001:
            italic = True
        if "head" in font and int(font["head"].macStyle) & 0x0002:
            italic = True
        glyphs = int(font["maxp"].numGlyphs) if "maxp" in font else len(font.getGlyphOrder())
        cmap = font.getBestCmap() or {}
        missing = "".join(c for c in CHECKED if ord(c) not in cmap)
        facts = {
            "family": family,
            "subfamily": subfamily,
            "weight": _weight(os2),
            "italic": italic,
            "glyphCount": glyphs,
            "missingTurkish": missing,
            "fsType": fs_type,
        }
    finally:
        font.close()
    facts["woff2"] = _woff2(sfnt)
    return facts
