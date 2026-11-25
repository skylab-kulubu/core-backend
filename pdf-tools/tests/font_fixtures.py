"""Test fonts, built with fontTools (no font files in the repository)."""

from __future__ import annotations

import io
import random
import string
from functools import lru_cache

from fontTools.fontBuilder import FontBuilder
from fontTools.pens.t2CharStringPen import T2CharStringPen
from fontTools.pens.ttGlyphPen import TTGlyphPen
from fontTools.ttLib import TTCollection, TTFont

TURKISH = "ğĞıİşŞçÇöÖüÜ"
ALL = string.ascii_letters + string.digits + TURKISH


def _names(chars: str) -> list[str]:
    return [".notdef", "space"] + [f"u{ord(c):04X}" for c in chars]


def _box(pen, code: int) -> int:
    adv = 500 + (code % 4) * 50
    pen.moveTo((50, 0))
    pen.lineTo((50, 700))
    pen.lineTo((adv - 50, 700))
    pen.lineTo((adv - 50, 0))
    pen.closePath()
    return adv


def _common(fb: FontBuilder, names: list[str], family: str, style: str, weight: int, italic: bool, fs_type: int, typographic: str) -> None:
    fb.setupHorizontalHeader(ascent=800, descent=-200)
    name = {"familyName": family, "styleName": style}
    if typographic:
        name["typographicFamily"] = typographic
        name["typographicSubfamily"] = style
    fb.setupNameTable(name)
    fs_selection = 0x0001 if italic else 0x0040
    fb.setupOS2(
        sTypoAscender=800,
        sTypoDescender=-200,
        usWinAscent=800,
        usWinDescent=200,
        usWeightClass=weight,
        fsType=fs_type,
        fsSelection=fs_selection,
    )
    fb.setupPost()


def _cmap(names: list[str]) -> dict[int, str]:
    cmap = {32: "space"}
    cmap.update({int(n[1:], 16): n for n in names if n.startswith("u")})
    return cmap


def ttf(
    family: str = "Deneme Sans",
    style: str = "Regular",
    weight: int = 400,
    italic: bool = False,
    chars: str = ALL,
    fs_type: int = 0,
    typographic: str = "",
    fvar: bool = False,
) -> bytes:
    names = _names(chars)
    fb = FontBuilder(1000, isTTF=True)
    fb.setupGlyphOrder(names)
    fb.setupCharacterMap(_cmap(names))
    glyphs, metrics = {}, {}
    for n in names:
        pen = TTGlyphPen(None)
        adv = 300 if n == "space" else _box(pen, int(n[1:], 16) if n.startswith("u") else 0)
        glyphs[n] = pen.glyph()
        metrics[n] = (adv, 50)
    fb.setupGlyf(glyphs)
    fb.setupHorizontalMetrics(metrics)
    _common(fb, names, family, style, weight, italic, fs_type, typographic)
    if fvar:
        fb.setupFvar(axes=[("wght", 100, 400, 900, "Weight")], instances=[])
        fb.setupGvar({})
    return _save(fb.font)


def otf(family: str = "Deneme Serif", style: str = "Bold", weight: int = 700, chars: str = ALL) -> bytes:
    """An OpenType font with CFF outlines."""
    names = _names(chars)
    fb = FontBuilder(1000, isTTF=False)
    fb.setupGlyphOrder(names)
    fb.setupCharacterMap(_cmap(names))
    charstrings, metrics = {}, {}
    for n in names:
        code = int(n[1:], 16) if n.startswith("u") else 0
        adv = 300 if n == "space" else 500 + (code % 4) * 50
        pen = T2CharStringPen(adv, None)
        if n != "space":
            _box(pen, code)
        charstrings[n] = pen.getCharString()
        metrics[n] = (adv, 50)
    fb.setupCFF(family.replace(" ", ""), {"FullName": f"{family} {style}"}, charstrings, {})
    fb.setupHorizontalMetrics(metrics)
    _common(fb, names, family, style, weight, False, 0, "")
    return _save(fb.font)


def woff2(data: bytes) -> bytes:
    font = TTFont(io.BytesIO(data))
    font.flavor = "woff2"
    return _save(font)


def ttc() -> bytes:
    coll = TTCollection()
    coll.fonts = [TTFont(io.BytesIO(ttf(style="Regular"))), TTFont(io.BytesIO(ttf(style="Bold", weight=700)))]
    buf = io.BytesIO()
    coll.save(buf)
    return buf.getvalue()


def bad_head() -> bytes:
    """A TrueType font whose head table has a wrong magic number: OTS
    refuses it."""
    data = bytearray(ttf())
    num = int.from_bytes(data[4:6], "big")
    for i in range(num):
        rec = 12 + 16 * i
        if bytes(data[rec : rec + 4]) == b"head":
            off = int.from_bytes(data[rec + 8 : rec + 12], "big")
            data[off + 12 : off + 16] = b"\xde\xad\xbe\xef"
            return bytes(data)
    raise AssertionError("no head table")


@lru_cache(maxsize=None)
def large(glyph_count: int = 19000, points: int = 30) -> bytes:
    """A TrueType font of about 9.6 MiB: many glyphs of random outlines
    (they compress badly, a worse case than a real CJK font)."""
    rnd = random.Random(7)
    names = [".notdef"] + [f"g{i}" for i in range(1, glyph_count)]
    fb = FontBuilder(1000, isTTF=True)
    fb.setupGlyphOrder(names)
    cmap = {0x4E00 + i: f"g{i}" for i in range(1, glyph_count)}
    cmap.update({ord(c): "g1" for c in ALL})
    fb.setupCharacterMap(cmap)
    glyphs = {}
    for n in names:
        pen = TTGlyphPen(None)
        for _ in range(4):
            pen.moveTo((rnd.randint(0, 900), rnd.randint(0, 900)))
            for _ in range(points):
                pen.lineTo((rnd.randint(0, 900), rnd.randint(0, 900)))
            pen.closePath()
        glyphs[n] = pen.glyph()
    fb.setupGlyf(glyphs)
    fb.setupHorizontalMetrics({n: (1000, 0) for n in names})
    fb.setupHorizontalHeader(ascent=800, descent=-200)
    fb.setupNameTable({"familyName": "Large", "styleName": "Regular"})
    fb.setupOS2()
    fb.setupPost()
    return _save(fb.font)


def _save(font: TTFont) -> bytes:
    buf = io.BytesIO()
    font.save(buf)
    return buf.getvalue()
