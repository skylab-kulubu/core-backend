"""Opening a PDF and pairing PDFium's text objects with content stream
operators.

PDFium (pypdfium2) reads the text: one text object per text-showing operator
(Tj, TJ, ' and ") that shows at least one character, in content stream order,
and the same inside each Form XObject. pikepdf parses the same content
streams. The k-th operator that shows something is the k-th text object of
its stream (the page's or a Form XObject's); the pairing is checked operator
by operator (font size, font name, counts), and a stream where it does not
hold is "unmatched": its text is listed but cannot be removed.
"""

from __future__ import annotations

import ctypes
import io
import math
import unicodedata
from dataclasses import dataclass, field

import pikepdf
import pypdfium2 as pdfium
import pypdfium2.raw as pdfium_c

from .errors import Problem
from .geometry import IDENTITY, Matrix, PageGeometry, apply, linear, mul, page_geometry

MAX_SPANS = 5000

SHOW_OPS = frozenset({"Tj", "TJ", "'", '"'})
# Operators after which the next text starts from a new line start, so a
# removed text's advance no longer matters.
NEW_LINE_OPS = frozenset({"BT", "ET", "Td", "TD", "Tm", "T*", "'", '"'})

_LIGATURES = str.maketrans(
    {
        "ﬀ": "ff",
        "ﬁ": "fi",
        "ﬂ": "fl",
        "ﬃ": "ffi",
        "ﬄ": "ffl",
        "ﬅ": "st",
        "ﬆ": "st",
    }
)

_WEIGHT_WORDS = (
    ("extralight", 200),
    ("ultralight", 200),
    ("semibold", 600),
    ("demibold", 600),
    ("extrabold", 800),
    ("ultrabold", 800),
    ("hairline", 100),
    ("thin", 100),
    ("light", 300),
    ("medium", 500),
    ("bold", 700),
    ("black", 900),
    ("heavy", 900),
)


def open_pdf(data: bytes, *, single_page: bool) -> pikepdf.Pdf:
    """The PDF, or pdf_unreadable, pdf_encrypted, or pdf_pages (more than one
    page when single_page)."""
    try:
        pdf = pikepdf.open(io.BytesIO(data))
    except pikepdf.PasswordError:
        raise Problem("pdf_encrypted", "the PDF is password protected") from None
    except Exception:
        raise Problem("pdf_unreadable", "not a PDF that can be read") from None
    if pdf.is_encrypted:
        pdf.close()
        raise Problem("pdf_encrypted", "the PDF is encrypted")
    try:
        count = len(pdf.pages)
    except Exception:
        pdf.close()
        raise Problem("pdf_unreadable", "the page tree cannot be read") from None
    if count == 0:
        pdf.close()
        raise Problem("pdf_unreadable", "the PDF has no page")
    if single_page and count > 1:
        pdf.close()
        raise Problem("pdf_pages", f"the PDF has {count} pages; one is allowed", pages=count)
    return pdf


def open_pdfium(data: bytes) -> pdfium.PdfDocument:
    try:
        return pdfium.PdfDocument(data)
    except pdfium.PdfiumError:
        raise Problem("pdf_unreadable", "not a PDF that can be read") from None


def normalize_text(text: str) -> str:
    """NFC (a decomposed ş becomes one character) with ligatures spelled
    out."""
    return unicodedata.normalize("NFC", text.translate(_LIGATURES))


def weight_from_name(name: str) -> int | None:
    low = name.lower().replace("-", "").replace(" ", "").replace("_", "")
    for word, weight in _WEIGHT_WORDS:
        if word in low:
            return weight
    if "regular" in low or "book" in low or "normal" in low or "roman" in low:
        return 400
    return None


def strip_subset(name: str) -> str:
    if len(name) > 7 and name[6] == "+" and name[:6].isalpha() and name[:6].isupper():
        return name[7:]
    return name


# --- PDFium side -------------------------------------------------------------


@dataclass
class PdfiumText:
    index: int  # in its holder's object list
    handle: object
    matrix: Matrix  # holder space (PDFium: Th·Tm·CTM, no font size)
    size: float  # Tf operand
    base_font: str
    text: str


@dataclass
class PdfiumHolder:
    path: tuple[int, ...]
    to_page: Matrix  # this holder's space to page user space
    texts: list[PdfiumText] = field(default_factory=list)
    forms: list[tuple[int, ...]] = field(default_factory=list)  # child paths


def _obj_matrix(obj: object) -> Matrix:
    m = pdfium_c.FS_MATRIX()
    if not pdfium_c.FPDFPageObj_GetMatrix(obj, m):
        return IDENTITY
    return (m.a, m.b, m.c, m.d, m.e, m.f)


def _font_name(font: object) -> str:
    if not font:
        return ""
    n = pdfium_c.FPDFFont_GetBaseFontName(font, None, 0)
    if n <= 1:
        return ""
    buf = ctypes.create_string_buffer(n)
    pdfium_c.FPDFFont_GetBaseFontName(font, buf, n)
    return buf.raw[: n - 1].decode("latin-1")


def _font_family(font: object) -> str:
    if not font:
        return ""
    n = pdfium_c.FPDFFont_GetFamilyName(font, None, 0)
    if n <= 1:
        return ""
    buf = ctypes.create_string_buffer(n)
    pdfium_c.FPDFFont_GetFamilyName(font, buf, n)
    return buf.raw[: n - 1].decode("utf-8", errors="replace")


def _obj_text(obj: object, textpage: object) -> str:
    n = pdfium_c.FPDFTextObj_GetText(obj, textpage, None, 0)
    if n <= 2:
        return ""
    buf = (ctypes.c_ushort * (n // 2))()
    pdfium_c.FPDFTextObj_GetText(obj, textpage, buf, n)
    raw = bytes(buf)[: n - 2]
    return raw.decode("utf-16-le", errors="replace")


def _font_size(obj: object) -> float:
    size = ctypes.c_float(0)
    if not pdfium_c.FPDFTextObj_GetFontSize(obj, ctypes.byref(size)):
        return 0.0
    return float(size.value)


def pdfium_holders(page: pdfium.PdfPage, textpage: pdfium.PdfTextPage) -> dict[tuple[int, ...], PdfiumHolder]:
    holders: dict[tuple[int, ...], PdfiumHolder] = {}

    def visit(path: tuple[int, ...], count: int, get, to_page: Matrix, depth: int) -> None:
        holder = PdfiumHolder(path=path, to_page=to_page)
        holders[path] = holder
        for i in range(count):
            obj = get(i)
            kind = pdfium_c.FPDFPageObj_GetType(obj)
            if kind == pdfium_c.FPDF_PAGEOBJ_TEXT:
                font = pdfium_c.FPDFTextObj_GetFont(obj)
                holder.texts.append(
                    PdfiumText(
                        index=i,
                        handle=obj,
                        matrix=_obj_matrix(obj),
                        size=_font_size(obj),
                        base_font=_font_name(font),
                        text=_obj_text(obj, textpage.raw),
                    )
                )
            elif kind == pdfium_c.FPDF_PAGEOBJ_FORM:
                child = path + (i,)
                holder.forms.append(child)
                if depth < 32:
                    visit(
                        child,
                        pdfium_c.FPDFFormObj_CountObjects(obj),
                        lambda j, o=obj: pdfium_c.FPDFFormObj_GetObject(o, j),
                        mul(_obj_matrix(obj), to_page),
                        depth + 1,
                    )

    raw = page.raw
    visit((), pdfium_c.FPDFPage_CountObjects(raw), lambda i: pdfium_c.FPDFPage_GetObject(raw, i), IDENTITY, 0)
    return holders


# --- content stream side -----------------------------------------------------


@dataclass
class FontInfo:
    key: str  # resource name
    base_font: str | None
    vertical: bool
    # Its codes have no known Unicode: a Type0 font with an Identity
    # encoding and no ToUnicode (what PDFium reads from it is a guess).
    unmapped: bool = False


@dataclass
class ShowOp:
    index: int  # instruction index in the holder's stream
    op: str
    creates: bool  # PDFium makes a text object of it
    font: FontInfo | None
    size: float
    line: int  # the same number: the same line (no new line between)
    tz: int  # Tz operators seen so far in the stream
    cm: int  # cm operators seen so far (one inside BT moves the text after it)
    rise: float
    init_kerning: float  # TJ numbers before its first non-empty string
    total_kerning: float  # all TJ numbers (for an operator that shows nothing)


@dataclass
class StreamHolder:
    path: tuple[int, ...]
    owner: pikepdf.Object  # the page dictionary or the Form XObject stream
    is_page: bool
    instructions: list = field(default_factory=list)
    shows: list[ShowOp] = field(default_factory=list)
    # (instruction index, form, the font and size in force there)
    form_dos: list[tuple[int, pikepdf.Object, FontInfo | None, float]] = field(default_factory=list)
    error: str | None = None


@dataclass
class _TextState:
    font: FontInfo | None = None
    size: float = 0.0
    rise: float = 0.0


def _num(obj: object) -> float:
    try:
        v = float(obj)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return 0.0
    return v if math.isfinite(v) else 0.0


def _resource(res: object, page_res: object, kind: str, name: str) -> object | None:
    """PDFium's lookup: the stream's own resources; when they have no such
    category at all, the page's."""
    cat = res.get(kind) if isinstance(res, pikepdf.Dictionary) else None
    if not isinstance(cat, pikepdf.Dictionary) and isinstance(page_res, pikepdf.Dictionary):
        cat = page_res.get(kind)
    return cat.get(name) if isinstance(cat, pikepdf.Dictionary) else None


def _font_info(key: str, font: object) -> FontInfo:
    base = None
    vertical = False
    unmapped = False
    if isinstance(font, pikepdf.Dictionary):
        bf = font.get("/BaseFont")
        if isinstance(bf, pikepdf.Name):
            base = str(bf)[1:]
        if font.get("/Subtype") == pikepdf.Name.Type0:
            enc = font.get("/Encoding")
            if isinstance(enc, pikepdf.Name) and str(enc).endswith("-V"):
                vertical = True
            elif isinstance(enc, pikepdf.Stream) and int(enc.get("/WMode", 0)) == 1:
                vertical = True
            identity = isinstance(enc, pikepdf.Name) and str(enc) in ("/Identity-H", "/Identity-V")
            unmapped = identity and "/ToUnicode" not in font
    return FontInfo(key=key, base_font=base, vertical=vertical, unmapped=unmapped)


def _string_len(obj: object) -> int:
    if isinstance(obj, pikepdf.String):
        return len(bytes(obj))
    return 0


def parse_holder(
    path: tuple[int, ...],
    owner: pikepdf.Object,
    is_page: bool,
    res: object,
    page_res: object,
    inherited_font: FontInfo | None,
    inherited_size: float,
) -> StreamHolder:
    holder = StreamHolder(path=path, owner=owner, is_page=is_page)
    try:
        holder.instructions = list(pikepdf.parse_content_stream(owner))
    except Exception as exc:  # malformed content: nothing in it can be removed
        holder.error = f"content stream: {exc.__class__.__name__}"
        return holder
    state = _TextState(font=inherited_font, size=inherited_size)
    stack: list[_TextState] = []
    line = 0
    tz = 0
    cm = 0
    for i, ins in enumerate(holder.instructions):
        if isinstance(ins, pikepdf.ContentStreamInlineImage):
            continue
        op = str(ins.operator)
        operands = ins.operands
        if op == "q":
            stack.append(_TextState(state.font, state.size, state.rise))
        elif op == "Q":
            if stack:
                state = stack.pop()
        elif op == "Tf" and len(operands) >= 2:
            key = str(operands[0]) if isinstance(operands[0], pikepdf.Name) else ""
            state.font = _font_info(key, _resource(res, page_res, "/Font", key) if key else None)
            state.size = _num(operands[1])
        elif op == "Ts" and operands:
            state.rise = _num(operands[0])
        elif op == "Tz":
            tz += 1
        elif op == "cm":
            cm += 1
        elif op == "Do" and operands and isinstance(operands[0], pikepdf.Name):
            xobj = _resource(res, page_res, "/XObject", str(operands[0]))
            if isinstance(xobj, pikepdf.Stream) and xobj.get("/Subtype") == pikepdf.Name.Form:
                holder.form_dos.append((i, xobj, state.font, state.size))
        if op in NEW_LINE_OPS:
            line += 1
        if op not in SHOW_OPS:
            continue
        creates = False
        init_k = 0.0
        total_k = 0.0
        if op == "TJ":
            arr = operands[0] if operands else None
            if isinstance(arr, pikepdf.Array):
                seen_text = False
                for item in arr:
                    if isinstance(item, pikepdf.String):
                        if _string_len(item) > 0:
                            seen_text = True
                    else:
                        n = _num(item)
                        total_k += n
                        if not seen_text:
                            init_k += n
                creates = seen_text
        elif operands:
            creates = _string_len(operands[-1]) > 0
        creates = creates and state.font is not None
        holder.shows.append(
            ShowOp(
                index=i,
                op=op,
                creates=creates,
                font=state.font,
                size=state.size,
                line=line,
                tz=tz,
                cm=cm,
                rise=state.rise,
                init_kerning=init_k if creates else 0.0,
                total_kerning=total_k,
            )
        )
    return holder


# --- the pairing -------------------------------------------------------------


@dataclass
class Removal:
    """How to take one text-showing operator out of its stream, keeping the
    text after it on the same line where it was."""

    op: ShowOp
    advance_kerning: float | None  # a TJ number that moves as far; None: none needed


@dataclass
class Span:
    id: str
    holder: tuple[int, ...]
    pdfium: PdfiumText
    show: ShowOp | None
    removal: Removal | None
    reason: str | None
    info: dict


@dataclass
class Analysis:
    pdf: pikepdf.Pdf
    geometry: PageGeometry
    pages: int
    streams: dict[tuple[int, ...], StreamHolder]
    spans: list[Span]  # every text object, in order
    truncated: bool


def _same_font(base: str | None, pdfium_base: str) -> bool:
    if base is None or not pdfium_base:
        return True
    return strip_subset(base) == strip_subset(pdfium_base) or base == pdfium_base


def _solve(u: tuple[float, float], s: tuple[float, float], v: tuple[float, float]) -> tuple[float, float] | None:
    det = u[0] * s[1] - u[1] * s[0]
    if abs(det) < 1e-9:
        return None
    x = (v[0] * s[1] - v[1] * s[0]) / det
    y = (u[0] * v[1] - u[1] * v[0]) / det
    return x, y


def _removals(stream: StreamHolder, texts: list[PdfiumText]) -> dict[int, Removal | None]:
    """For each text-creating operator (by its position among them), how to
    remove it, or None when the text after it on its line cannot be kept in
    place."""
    shows = stream.shows
    positions = [i for i, s in enumerate(shows) if s.creates]
    out: dict[int, Removal | None] = {}
    for k, pos in enumerate(positions):
        d = shows[pos]
        # The next operator on the same line that PDFium made an object of;
        # the ones between show nothing but may move the text position.
        between: list[ShowOp] = []
        nxt: ShowOp | None = None
        for s in shows[pos + 1 :]:
            if s.line != d.line:
                break
            if s.creates:
                nxt = s
                break
            between.append(s)
        if nxt is None:
            out[k] = Removal(op=d, advance_kerning=None)
            continue
        if nxt.tz != d.tz or nxt.cm != d.cm or d.size == 0 or nxt.size == 0:
            out[k] = None
            continue
        td, te = texts[k], texts[k + 1]
        u = (td.matrix[0], td.matrix[1])
        s = (td.matrix[2], td.matrix[3])
        ue = (te.matrix[0], te.matrix[1])
        if math.dist(u, ue) > 1e-4 * max(1.0, math.hypot(*u)):
            out[k] = None
            continue
        delta = (te.matrix[4] - td.matrix[4], te.matrix[5] - td.matrix[5])
        solved = _solve(u, s, delta)
        if solved is None:
            out[k] = None
            continue
        x, y = solved
        if abs(y - (nxt.rise - d.rise)) > 0.01:
            out[k] = None
            continue
        # In units of the text's x axis (PDFium's matrix carries Th): where
        # each operator started, before its leading TJ numbers.
        init_d = -d.init_kerning / 1000.0 * d.size
        init_e = -nxt.init_kerning / 1000.0 * nxt.size
        shifts = sum(-p.total_kerning / 1000.0 * p.size for p in between)
        advance = (x - init_e) - (0.0 - init_d) - shifts
        out[k] = Removal(op=d, advance_kerning=-1000.0 * advance / d.size)
    return out


def analyze(data: bytes, *, single_page: bool) -> Analysis:
    pdf = open_pdf(data, single_page=single_page)
    page = pdf.pages[0]
    geometry = page_geometry(page.obj)
    doc = open_pdfium(data)
    try:
        ppage = doc[0]
        textpage = ppage.get_textpage()
        try:
            holders = pdfium_holders(ppage, textpage)
            spans, streams, truncated = _pair(pdf, page, holders, geometry, ppage)
        finally:
            textpage.close()
            ppage.close()
    finally:
        doc.close()
    return Analysis(pdf=pdf, geometry=geometry, pages=len(pdf.pages), streams=streams, spans=spans, truncated=truncated)


def _pair(pdf, page, holders, geometry, ppage):
    from .spans import describe  # the span fields; kept apart from the pairing

    page_res = _page_resources(page.obj)
    streams: dict[tuple[int, ...], StreamHolder] = {}
    # How many times each Form XObject is drawn: one drawn twice cannot lose
    # text in one place only.
    uses: dict[tuple[int, int], int] = {}
    matched: dict[tuple[int, ...], bool] = {}

    def walk(path, owner, is_page, res, font, size):
        stream = parse_holder(path, owner, is_page, res, page_res, font, size)
        streams[path] = stream
        ph = holders.get(path)
        ok = stream.error is None and ph is not None
        creating = [s for s in stream.shows if s.creates]
        if ok and len(creating) != len(ph.texts):
            ok = False
        if ok:
            for s, t in zip(creating, ph.texts):
                if abs(s.size - t.size) > 1e-3 or not _same_font(s.font.base_font if s.font else None, t.base_font):
                    ok = False
                    break
        forms_ok = ph is not None and stream.error is None and len(stream.form_dos) == len(ph.forms)
        matched[path] = ok
        if not forms_ok:
            return
        for (_, form, font_at, size_at), child in zip(stream.form_dos, ph.forms):
            key = form.objgen
            uses[key] = uses.get(key, 0) + 1
            form_res = form.get("/Resources")
            walk(
                child,
                form,
                False,
                form_res if isinstance(form_res, pikepdf.Dictionary) else res,
                font_at,
                size_at,
            )

    walk((), page.obj, True, page_res, None, 0.0)

    spans: list[Span] = []
    truncated = False
    for path in sorted(holders, key=lambda p: (len(p), p)):
        ph = holders[path]
        stream = streams.get(path)
        ok = matched.get(path, False)
        shared = False
        if stream is not None and not stream.is_page:
            shared = uses.get(stream.owner.objgen, 0) > 1
        creating = [s for s in stream.shows if s.creates] if (stream is not None and ok) else []
        removals = _removals(stream, ph.texts) if creating else {}
        for k, t in enumerate(ph.texts):
            if len(spans) >= MAX_SPANS:
                truncated = True
                break
            show = creating[k] if creating else None
            removal = removals.get(k) if creating else None
            sid = ".".join(str(i) for i in path + (t.index,))
            info, reason = describe(t, ph, show, geometry)
            if reason is None:
                if not ok or show is None:
                    reason = "unmatched"
                elif shared:
                    reason = "shared"
                elif removal is None:
                    reason = "unmatched"
            spans.append(Span(id=sid, holder=path, pdfium=t, show=show, removal=removal, reason=reason, info=info))
    # PDFium's object order for the page: by holder depth first is fine for
    # ids, but list them in drawing order.
    spans.sort(key=lambda s: _draw_order(s.id))
    return spans, streams, truncated


def _draw_order(sid: str) -> tuple[int, ...]:
    return tuple(int(p) for p in sid.split("."))


def _page_resources(page_obj: pikepdf.Dictionary) -> object:
    from .geometry import inherited

    res = inherited(page_obj, "/Resources")
    return res if isinstance(res, pikepdf.Dictionary) else pikepdf.Dictionary()


def to_top_left(holder: PdfiumHolder, geometry: PageGeometry) -> Matrix:
    return mul(holder.to_page, geometry.to_top_left)


__all__ = [
    "Analysis",
    "MAX_SPANS",
    "Span",
    "analyze",
    "apply",
    "linear",
    "normalize_text",
    "open_pdf",
    "open_pdfium",
    "strip_subset",
    "weight_from_name",
]
