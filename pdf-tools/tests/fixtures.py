"""Test PDFs, written by hand: content streams as text, a TrueType font built
with fontTools (no font files in the repository)."""

from __future__ import annotations

import io
from functools import lru_cache

import pikepdf
from fontTools.fontBuilder import FontBuilder
from fontTools.pens.ttGlyphPen import TTGlyphPen
from pikepdf import Array, Dictionary, Name

TURKISH = "ğĞıİşŞçÇöÖüÜ"
# A glyph whose ToUnicode is s + U+0327 (ş written in two code points).
DECOMPOSED_S = ""
LIGATURE_FI = "ﬁ"
CHARS = "".join(chr(c) for c in range(33, 127)) + TURKISH + LIGATURE_FI + DECOMPOSED_S


@lru_cache(maxsize=None)
def test_font() -> tuple[bytes, dict[str, int]]:
    """A TrueType font: one filled box per character, widths that vary."""
    names = [".notdef", "space"] + [f"g{ord(c):04X}" for c in CHARS]
    gid = {" ": 1}
    gid.update({c: i + 2 for i, c in enumerate(CHARS)})
    fb = FontBuilder(1000, isTTF=True)
    fb.setupGlyphOrder(names)
    cmap = {32: "space"}
    cmap.update({ord(c): f"g{ord(c):04X}" for c in CHARS if not 0xE000 <= ord(c) <= 0xF8FF})
    fb.setupCharacterMap(cmap)
    glyphs = {}
    metrics = {}
    for n in names:
        pen = TTGlyphPen(None)
        adv = 600
        if n not in ("space",):
            code = int(n[1:], 16) if n.startswith("g") else 0
            adv = 420 + (code % 5) * 60
            top = 500 + (code % 3) * 100
            pen.moveTo((40, 0))
            pen.lineTo((40, top))
            pen.lineTo((adv - 40, top))
            pen.lineTo((adv - 40, 0))
            pen.closePath()
        glyphs[n] = pen.glyph()
        metrics[n] = (adv, 40)
    fb.setupGlyf(glyphs)
    fb.setupHorizontalMetrics(metrics)
    fb.setupHorizontalHeader(ascent=800, descent=-200)
    fb.setupNameTable({"familyName": "TestSans", "styleName": "Bold"})
    fb.setupOS2(sTypoAscender=800, sTypoDescender=-200, usWinAscent=800, usWinDescent=200, usWeightClass=700)
    fb.setupPost()
    buf = io.BytesIO()
    fb.save(buf)
    return buf.getvalue(), gid


def _utf16_hex(text: str) -> str:
    return "".join(f"{b:02X}" for b in text.encode("utf-16-be"))


def to_unicode_cmap(gid: dict[str, int]) -> bytes:
    lines = []
    for ch, g in sorted(gid.items(), key=lambda kv: kv[1]):
        uni = "ş" if ch == DECOMPOSED_S else ch
        lines.append(f"<{g:04X}> <{_utf16_hex(uni)}>")
    chunks = []
    for i in range(0, len(lines), 100):
        part = lines[i : i + 100]
        chunks.append(f"{len(part)} beginbfchar\n" + "\n".join(part) + "\nendbfchar")
    body = "\n".join(chunks)
    return (
        "/CIDInit /ProcSet findresource begin\n12 dict begin\nbegincmap\n"
        "/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def\n"
        "/CMapName /Adobe-Identity-UCS def\n/CMapType 2 def\n"
        "1 begincodespacerange\n<0000> <FFFF>\nendcodespacerange\n"
        f"{body}\nendcmap\nCMapName currentdict /CMap defineresource pop\nend\nend\n"
    ).encode()


class Builder:
    """One page (or more) of hand-written content."""

    def __init__(self, width: float = 842, height: float = 595) -> None:
        self.pdf = pikepdf.new()
        self.width = width
        self.height = height
        self.fonts: dict[str, Dictionary] = {}
        self.xobjects: dict[str, object] = {}
        self.gstates: dict[str, Dictionary] = {}
        self.shadings: dict[str, Dictionary] = {}
        self.content: list[str] = []
        self.gid = test_font()[1]

    # resources
    def type0(self, name: str = "F1", *, to_unicode: bool = True, base: str = "ABCDEF+TestSans-Bold", vertical: bool = False) -> str:
        data, gid = test_font()
        file2 = self.pdf.make_stream(data)
        descriptor = Dictionary(
            Type=Name.FontDescriptor,
            FontName=Name("/" + base),
            Flags=4,
            FontBBox=Array([0, -200, 1000, 800]),
            ItalicAngle=0,
            Ascent=800,
            Descent=-200,
            CapHeight=700,
            StemV=80,
            FontWeight=700,
            FontFile2=file2,
        )
        cid = Dictionary(
            Type=Name.Font,
            Subtype=Name.CIDFontType2,
            BaseFont=Name("/" + base),
            CIDSystemInfo=Dictionary(Registry=pikepdf.String("Adobe"), Ordering=pikepdf.String("Identity"), Supplement=0),
            FontDescriptor=descriptor,
            DW=600,
            W=Array(self._widths(gid)),
            CIDToGIDMap=Name.Identity,
        )
        font = Dictionary(
            Type=Name.Font,
            Subtype=Name.Type0,
            BaseFont=Name("/" + base),
            Encoding=Name("/Identity-V" if vertical else "/Identity-H"),
            DescendantFonts=Array([self.pdf.make_indirect(cid)]),
        )
        if to_unicode:
            font.ToUnicode = self.pdf.make_stream(to_unicode_cmap(gid))
        self.fonts[name] = self.pdf.make_indirect(font)
        return name

    @staticmethod
    def _widths(gid: dict[str, int]) -> list:
        out: list = []
        for ch, g in sorted(gid.items(), key=lambda kv: kv[1]):
            code = ord(ch) if ch != " " else 0
            adv = 600 if ch == " " else 420 + (code % 5) * 60
            out.extend([g, Array([adv])])
        return out

    def std(self, name: str = "H", base: str = "Helvetica") -> str:
        self.fonts[name] = self.pdf.make_indirect(
            Dictionary(Type=Name.Font, Subtype=Name.Type1, BaseFont=Name("/" + base), Encoding=Name.WinAnsiEncoding)
        )
        return name

    def gstate(self, name: str, **kw: object) -> str:
        self.gstates[name] = Dictionary(Type=Name.ExtGState, **kw)
        return name

    def axial(self, name: str, x0: float, y0: float, x1: float, y1: float, c0=(0.1, 0.3, 0.8), c1=(0.95, 0.8, 0.2)) -> str:
        fn = Dictionary(FunctionType=2, Domain=Array([0, 1]), C0=Array(list(c0)), C1=Array(list(c1)), N=1)
        self.shadings[name] = Dictionary(
            ShadingType=2, ColorSpace=Name.DeviceRGB, Coords=Array([x0, y0, x1, y1]), Function=fn, Extend=Array([True, True])
        )
        return name

    def form(self, name: str, content: str, bbox, *, matrix=None, group: bool = False, own_resources: bool = True) -> str:
        d = Dictionary(Type=Name.XObject, Subtype=Name.Form, BBox=Array(list(bbox)))
        if matrix is not None:
            d.Matrix = Array(list(matrix))
        if group:
            d.Group = Dictionary(Type=Name.Group, S=Name.Transparency, I=True)
        stream = self.pdf.make_stream(content.encode("latin-1"), d)
        if own_resources:
            stream.Resources = self._resources()
        self.xobjects[name] = stream
        return name

    # text helpers
    def hex(self, text: str) -> str:
        return "<" + "".join(f"{self.gid[c]:04X}" for c in text) + ">"

    @staticmethod
    def lit(text: str) -> str:
        raw = text.encode("cp1252")
        out = raw.replace(b"\\", b"\\\\").replace(b"(", b"\\(").replace(b")", b"\\)")
        return "(" + out.decode("latin-1") + ")"

    def add(self, *lines: str) -> None:
        self.content.extend(lines)

    def _resources(self) -> Dictionary:
        res = Dictionary()
        if self.fonts:
            res.Font = Dictionary({"/" + k: v for k, v in self.fonts.items()})
        if self.xobjects:
            res.XObject = Dictionary({"/" + k: v for k, v in self.xobjects.items()})
        if self.gstates:
            res.ExtGState = Dictionary({"/" + k: v for k, v in self.gstates.items()})
        if self.shadings:
            res.Shading = Dictionary({"/" + k: v for k, v in self.shadings.items()})
        return res

    def build(self, *, cropbox=None, rotate: int | None = None, pages: int = 1, encrypt: bool = False, annotate: bool = False) -> bytes:
        res = self._resources()
        for _ in range(pages):
            page = Dictionary(
                Type=Name.Page,
                MediaBox=Array([0, 0, self.width, self.height]),
                Resources=res,
                Contents=self.pdf.make_stream("\n".join(self.content).encode("latin-1")),
            )
            if cropbox is not None:
                page.CropBox = Array(list(cropbox))
            if rotate is not None:
                page.Rotate = rotate
            if annotate:
                page.Annots = Array(
                    [
                        self.pdf.make_indirect(
                            Dictionary(
                                Type=Name.Annot,
                                Subtype=Name.Square,
                                Rect=Array([10, 10, 60, 60]),
                                C=Array([1, 0, 0]),
                                Border=Array([0, 0, 5]),
                            )
                        )
                    ]
                )
            self.pdf.pages.append(pikepdf.Page(page))
        buf = io.BytesIO()
        kwargs: dict = {}
        if encrypt:
            kwargs["encryption"] = pikepdf.Encryption(owner="owner-test", user="", R=6)
        self.pdf.save(buf, **kwargs)
        return buf.getvalue()


# --- the fixtures ----------------------------------------------------------------


def turkish() -> bytes:
    b = Builder()
    f = b.type0()
    title = "Katılım Belgesi " + TURKISH
    b.add(
        "BT",
        f"/{f} 28 Tf 1 0 0 1 60 480 Tm {b.hex(title)} Tj",
        f"1 0 0 1 60 420 Tm {b.hex('Ay' + DECOMPOSED_S + 'e Yıldız')} Tj",
        f"1 0 0 1 60 360 Tm {b.hex('O' + LIGATURE_FI + 'S')} Tj",
        "ET",
    )
    return b.build()


def rotated() -> bytes:
    b = Builder()
    f = b.type0()
    b.add(
        "BT",
        f"/{f} 20 Tf 0 1 -1 0 100 150 Tm {b.hex('Dikey yazı 90')} Tj",
        f"0.8660254 0.5 -0.5 0.8660254 300 200 Tm {b.hex('Eğik değil 30')} Tj",
        "ET",
    )
    return b.build()


def cropbox() -> bytes:
    b = Builder()
    f = b.type0()
    b.add("BT", f"/{f} 24 Tf 1 0 0 1 150 300 Tm {b.hex('Kırpılmış sayfa')} Tj", "ET")
    return b.build(cropbox=[100, 50, 742, 545])


def rotate90() -> bytes:
    b = Builder()
    f = b.type0()
    b.add("BT", f"/{f} 24 Tf 1 0 0 1 100 300 Tm {b.hex('Döndürülmüş sayfa')} Tj", "ET")
    return b.build(rotate=90)


def render_modes() -> bytes:
    b = Builder()
    f = b.type0()
    b.add(
        "0 0 1 RG 0.8 w",
        "BT",
        f"/{f} 24 Tf 1 0 0 1 60 500 Tm 1 Tr {b.hex('Kontur')} Tj",
        f"1 0 0 1 60 440 Tm 3 Tr {b.hex('Görünmez OCR')} Tj",
        f"1 0 0 1 60 380 Tm 2 Tr {b.hex('Dolgu ve kontur')} Tj",
        f"1 0 0 1 60 320 Tm 0 Tr {b.hex('Düz')} Tj",
        f"1 0 0 1 60 260 Tm 7 Tr {b.hex('Kırpma')} Tj",
        "ET",
        "0 1 0 rg 40 240 300 60 re f",
    )
    return b.build()


def no_tounicode() -> bytes:
    b = Builder()
    f = b.type0("F1", to_unicode=False, base="GHIJKL+Mystery")
    b.add("BT", f"/{f} 24 Tf 1 0 0 1 60 400 Tm {b.hex('Gizemli')} Tj", "ET")
    return b.build()


def vertical() -> bytes:
    b = Builder()
    f = b.type0("F1", vertical=True)
    b.add("BT", f"/{f} 24 Tf 1 0 0 1 400 500 Tm {b.hex('Dikey')} Tj", "ET")
    return b.build()


def form_text() -> bytes:
    b = Builder()
    f = b.type0()
    b.form(
        "Fm1",
        f"BT /{f} 18 Tf 1 0 0 1 10 10 Tm {b.hex('Form içinde')} Tj ET 0 0 1 rg 5 40 100 10 re f",
        [0, 0, 300, 100],
        matrix=[1, 0, 0, 1, 50, 50],
    )
    b.form("Fm2", f"BT /{f} 14 Tf 1 0 0 1 5 5 Tm {b.hex('İki kez')} Tj ET", [0, 0, 200, 40])
    b.add(
        "q 1 0 0 1 100 300 cm /Fm1 Do Q",
        "q 1 0 0 1 400 100 cm /Fm2 Do Q",
        "q 1 0 0 1 400 200 cm /Fm2 Do Q",
        "BT",
        f"/{f} 20 Tf 1 0 0 1 60 520 Tm {b.hex('Sayfada')} Tj",
        "ET",
    )
    return b.build()


def quote_ops() -> bytes:
    """' and ", two Tj on one line, a TJ with kerning, a numbers-only TJ."""
    b = Builder()
    f = b.type0()
    h = b.std()
    b.add(
        "BT",
        f"/{f} 20 Tf 24 TL 1 0 0 1 60 520 Td",
        f"{b.hex('Birinci ')} Tj {b.hex('ikinci')} Tj {b.hex(' üçüncü')} Tj",
        f"{b.hex('Tırnak satırı')} '",
        f"2 1 {b.hex('Çift tırnak')} \"",
        f"[{b.hex('Ker')} -120 {b.hex('nli')} 300 {b.hex(' yazı')}] TJ",
        f"T* [-500] TJ {b.hex('sonra')} Tj",
        f"/{h} 16 Tf T* {b.lit('Helvetica ')} Tj {b.lit('devam')} Tj",
        "ET",
    )
    return b.build()


def two_pages() -> bytes:
    b = Builder()
    f = b.type0()
    b.add("BT", f"/{f} 24 Tf 1 0 0 1 60 400 Tm {b.hex('Sayfa')} Tj", "ET")
    return b.build(pages=2)


def encrypted() -> bytes:
    b = Builder()
    f = b.type0()
    b.add("BT", f"/{f} 24 Tf 1 0 0 1 60 400 Tm {b.hex('Gizli')} Tj", "ET")
    return b.build(encrypt=True)


def mixed_line() -> bytes:
    b = Builder()
    f = b.type0()
    h = b.std("H")
    hb = b.std("HB", "Helvetica-Bold")
    b.add(
        "BT",
        f"/{h} 18 Tf 1 0 0 1 60 400 Tm {b.lit('Bu belge ')} Tj",
        f"/{hb} 18 Tf 0.8 0.1 0.1 rg {b.lit('Ahmet Y')} Tj",
        f"/{h} 18 Tf 0 0 0 rg {b.lit(' adina')} Tj",
        "ET",
    )
    _ = f
    return b.build()


def paragraphs() -> bytes:
    b = Builder()
    h = b.std("H")
    lines = ["Birinci satir uzun", "ikinci kisa", "ucuncu satir"]
    out = ["BT", f"/{h} 14 Tf"]
    for col, align in ((60, "left"), (421, "center"), (780, "right")):
        for i, text in enumerate(lines):
            width = len(text) * 7.0
            x = col if align == "left" else (col - width / 2 if align == "center" else col - width)
            out.append(f"1 0 0 1 {x:.2f} {300 - i * 18} Tm {b.lit(text)} Tj")
    out.append("ET")
    b.add(*out)
    return b.build()


def gradient_transparency() -> bytes:
    """A shading over the page, a transparency group with text, half
    transparent text and a half transparent box over text."""
    b = Builder()
    f = b.type0()
    b.axial("Sh0", 0, 0, 842, 595)
    b.gstate("GS0", ca=0.5, CA=0.5)
    b.gstate("GS1", ca=0.35, BM=Name.Multiply)
    b.form(
        "Grp",
        f"/GS1 gs 1 0 0 rg 0 0 300 120 re f BT /{f} 22 Tf 0 0 0 rg 1 0 0 1 20 50 Tm {b.hex('Grup içinde')} Tj ET",
        [0, 0, 300, 120],
        group=True,
    )
    b.add(
        "q /Sh0 sh Q",
        "q 1 0 0 1 450 380 cm /Grp Do Q",
        "BT",
        f"/{f} 30 Tf /GS0 gs 1 1 1 rg 1 0 0 1 60 480 Tm {b.hex('Yarı saydam')} Tj",
        "ET",
        "BT",
        f"/{f} 26 Tf 0 0 0 rg 1 0 0 1 60 300 Tm {b.hex('Kutunun altında')} Tj",
        "ET",
        "q /GS0 gs 0 0.6 0 rg 50 290 250 40 re f Q",
    )
    return b.build()


def certificate() -> bytes:
    """An A4 landscape certificate: a gradient, a transparent box, a form,
    and 30 text objects of several kinds."""
    b = Builder(842, 595)
    f = b.type0()
    h = b.std("H")
    hb = b.std("HB", "Helvetica-Bold")
    b.axial("Sh0", 0, 0, 842, 595, (0.96, 0.94, 0.88), (0.85, 0.9, 0.97))
    b.gstate("GS0", ca=0.4)
    b.form("Logo", f"0.2 0.3 0.7 rg 0 0 80 80 re f BT /{f} 12 Tf 1 1 1 rg 1 0 0 1 8 34 Tm {b.hex('SKY LAB')} Tj ET", [0, 0, 80, 80])
    out = [
        "q /Sh0 sh Q",
        "q /GS0 gs 0.1 0.2 0.5 rg 30 30 782 535 re 40 40 762 515 re f* Q",
        "q 1 0 0 1 380 470 cm /Logo Do Q",
        "BT",
        f"/{f} 40 Tf 0.1 0.15 0.35 rg 1 0 0 1 220 400 Tm {b.hex('Katılım Belgesi')} Tj",
        f"/{h} 16 Tf 0.2 0.2 0.2 rg 1 0 0 1 300 360 Tm {b.lit('Bu belge')} Tj",
        f"/{f} 32 Tf 0 0 0 rg 1 0 0 1 260 315 Tm {b.hex('Ad Soyad')} Tj",
        f"/{h} 14 Tf 1 0 0 1 230 280 Tm {b.lit('SKY LAB etkinli')} Tj {b.lit('gine')} Tj",
        f"/{hb} 14 Tf {b.lit(' katildigi icin')} Tj",
        f"/{h} 14 Tf 1 0 0 1 300 260 Tm {b.lit('verilmistir.')} Tj",
        f"/{f} 12 Tf 1 0 0 1 90 120 Tm {b.hex('Tarih: 27 Kasım 2026')} Tj",
        f"0 1 -1 0 800 150 Tm {b.hex('Seri: 2026-0001')} Tj",
        f"0.8660254 0.5 -0.5 0.8660254 640 110 Tm {b.hex('İmza')} Tj",
        "ET",
    ]
    # Small labels: 19 more text objects in a grid (30 in all).
    out.append("BT")
    out.append(f"/{h} 9 Tf 0.3 0.3 0.3 rg")
    for n in range(19):
        row, col = divmod(n, 6)
        out.append(f"1 0 0 1 {80 + col * 115} {220 - row * 22} Tm {b.lit(f'Etiket {n + 1}')} Tj")
    out.append("ET")
    b.add(*out)
    return b.build()


def split_streams() -> bytes:
    """The page content in two streams, a text object across them, an
    inline image, and resources inherited from the page tree."""
    b = Builder()
    f = b.type0()
    b.add(
        "q 20 0 0 20 400 300 cm BI /W 2 /H 2 /CS /G /BPC 8 ID \x00\xff\xff\x00 EI Q",
        "BT",
        f"/{f} 22 Tf 1 0 0 1 60 400 Tm {b.hex('Birinci akış')} Tj",
    )
    data = b.build()
    pdf = pikepdf.open(io.BytesIO(data))
    page = pdf.pages[0].obj
    first = page.Contents.read_bytes()
    second = f"{b.hex('ikinci akış')} Tj ET 1 0 0 rg 60 300 100 20 re f".encode("latin-1")
    page.Contents = Array([pdf.make_stream(first), pdf.make_stream(b"1 0 0 1 60 360 Tm " + second)])
    pdf.Root.Pages.Resources = page.Resources
    del page["/Resources"]
    buf = io.BytesIO()
    pdf.save(buf)
    return buf.getvalue()


ALL = {
    "turkish": turkish,
    "rotated": rotated,
    "cropbox": cropbox,
    "rotate90": rotate90,
    "render_modes": render_modes,
    "no_tounicode": no_tounicode,
    "vertical": vertical,
    "form_text": form_text,
    "quote_ops": quote_ops,
    "mixed_line": mixed_line,
    "paragraphs": paragraphs,
    "gradient_transparency": gradient_transparency,
    "certificate": certificate,
    "split_streams": split_streams,
}
