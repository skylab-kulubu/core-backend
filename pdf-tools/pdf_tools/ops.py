"""The four PDF operations: inspect, strip-text, place and render."""

from __future__ import annotations

import io
import math
from decimal import Decimal

import pikepdf

from .document import Span, analyze, open_pdf, open_pdfium
from .errors import Problem
from .geometry import page_geometry
from .spans import group

# A page side, in pt (PDF's own limit is 14 400).
MAX_PAGE_PT = 14400.0
MAX_RENDER_PX = 4000
DEFAULT_RENDER_PX = 2000


def _span_json(span: Span) -> dict:
    out = {"id": span.id, **span.info, "convertible": span.reason is None}
    if span.reason is not None:
        out["reason"] = span.reason
    return out


def inspect(data: bytes) -> dict:
    analysis = analyze(data, single_page=False)
    try:
        every = [_span_json(s) for s in analysis.spans]
        runs = group(every)
        spans = [s for s in every if s["text"]]
        g = analysis.geometry
        return {
            "pages": analysis.pages,
            "page": {"widthPt": round(g.width, 3), "heightPt": round(g.height, 3), "rotate": g.rotate},
            "encrypted": False,
            "textCount": len(spans),
            "truncated": analysis.truncated,
            "spans": spans,
            "runs": runs,
        }
    finally:
        analysis.pdf.close()


# --- strip-text ----------------------------------------------------------------


def _tj(number: float) -> pikepdf.ContentStreamInstruction:
    value = Decimal(f"{number:.4f}")
    return pikepdf.ContentStreamInstruction([pikepdf.Array([value])], pikepdf.Operator("TJ"))


def _replacement(ins: pikepdf.ContentStreamInstruction, kerning: float | None) -> list:
    """What a removed text-showing operator becomes: its line movement and
    spacing (for ' and "), and a TJ number that moves as far as its text
    did when text follows it on the line."""
    op = str(ins.operator)
    out: list = []
    if op == '"':
        aw, ac = ins.operands[0], ins.operands[1]
        out.append(pikepdf.ContentStreamInstruction([aw], pikepdf.Operator("Tw")))
        out.append(pikepdf.ContentStreamInstruction([ac], pikepdf.Operator("Tc")))
        out.append(pikepdf.ContentStreamInstruction([], pikepdf.Operator("T*")))
    elif op == "'":
        out.append(pikepdf.ContentStreamInstruction([], pikepdf.Operator("T*")))
    if kerning is not None and abs(kerning) >= 1e-4:
        out.append(_tj(kerning))
    return out


def _fingerprint(span: Span) -> tuple:
    i = span.info
    return (i["text"], i["font"]["name"], round(i["size"], 2), i["renderMode"])


def _match(expected: list[Span], found: list[Span], tol: float = 0.05) -> tuple[list[Span], list[Span]]:
    """Pairs expected spans with found ones (same text, font, size, mode and
    place); answers the expected ones not found and the found ones left."""
    buckets: dict[tuple, list[Span]] = {}
    for s in found:
        buckets.setdefault(_fingerprint(s), []).append(s)
    missing: list[Span] = []
    for s in expected:
        cands = buckets.get(_fingerprint(s), [])
        o = s.info["origin"]
        hit = None
        for c in cands:
            co = c.info["origin"]
            if abs(co["x"] - o["x"]) <= tol and abs(co["y"] - o["y"]) <= tol:
                hit = c
                break
        if hit is None:
            missing.append(s)
        else:
            cands.remove(hit)
    left = [s for group in buckets.values() for s in group]
    return missing, left


def strip_text(data: bytes, span_ids: list[str]) -> bytes:
    analysis = analyze(data, single_page=True)
    try:
        if analysis.truncated:
            raise Problem("strip_incomplete", "the PDF has more texts than are read")
        by_id = {s.id: s for s in analysis.spans}
        wanted = list(dict.fromkeys(span_ids))
        unknown = [i for i in wanted if i not in by_id]
        if unknown:
            raise Problem("bad_request", "unknown span ids", spanIds=unknown[:50])
        blocked = [i for i in wanted if by_id[i].reason is not None]
        if blocked:
            raise Problem("bad_request", "spans that cannot be removed", spanIds=blocked[:50])
        chosen = [by_id[i] for i in wanted]
        if not chosen:
            raise Problem("bad_request", "no span chosen")

        per_stream: dict[tuple[int, ...], dict[int, list]] = {}
        for s in chosen:
            assert s.removal is not None and s.show is not None
            stream = analysis.streams[s.holder]
            ins = stream.instructions[s.show.index]
            per_stream.setdefault(s.holder, {})[s.show.index] = _replacement(ins, s.removal.advance_kerning)

        for path, repl in per_stream.items():
            stream = analysis.streams[path]
            new: list = []
            for i, ins in enumerate(stream.instructions):
                if i in repl:
                    new.extend(repl[i])
                else:
                    new.append(ins)
            content = pikepdf.unparse_content_stream(new)
            if stream.is_page:
                stream.owner.Contents = analysis.pdf.make_stream(content)
            else:
                stream.owner.write(content)

        buf = io.BytesIO()
        analysis.pdf.save(buf, compress_streams=True, deterministic_id=True)
        out = buf.getvalue()
    finally:
        analysis.pdf.close()

    # Read the result back: the chosen text gone, every other text object
    # still there, in its place.
    after = analyze(out, single_page=True)
    try:
        chosen_ids = {s.id for s in chosen}
        kept = [s for s in analysis.spans if s.id not in chosen_ids]
        missing, extra = _match(kept, after.spans)
        if missing or extra:
            raise Problem(
                "strip_incomplete",
                "the result does not have exactly the other text",
                missing=[s.id for s in missing][:50],
                remaining=[s.info["text"][:80] for s in extra][:50],
            )
    finally:
        after.pdf.close()
    return out


# --- place -----------------------------------------------------------------------


def _finite(name: str, value: object, low: float, high: float) -> float:
    try:
        v = float(value)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        raise Problem("bad_request", f"{name} is not a number") from None
    if not math.isfinite(v) or v < low or v > high:
        raise Problem("bad_request", f"{name} is out of range")
    return v


def place_matrix(params: dict, src_width: float, src_height: float) -> tuple[float, float, tuple]:
    """The new page's size and the cm matrix that draws the visible source
    page (src_width × src_height pt) into rect (top left origin, pt)."""
    if not isinstance(params, dict):
        raise Problem("bad_request", "params must be an object")
    width = _finite("pageWidthPt", params.get("pageWidthPt"), 1.0, MAX_PAGE_PT)
    height = _finite("pageHeightPt", params.get("pageHeightPt"), 1.0, MAX_PAGE_PT)
    rect = params.get("rect")
    if not isinstance(rect, dict):
        raise Problem("bad_request", "rect is missing")
    big = 100 * MAX_PAGE_PT
    x = _finite("rect.x", rect.get("x"), -big, big)
    y = _finite("rect.y", rect.get("y"), -big, big)
    w = _finite("rect.w", rect.get("w"), 1e-3, big)
    h = _finite("rect.h", rect.get("h"), 1e-3, big)
    sx = w / src_width
    sy = h / src_height
    # PDF's y goes up: the rect's top-left y becomes its bottom.
    return width, height, (sx, 0.0, 0.0, sy, x, height - y - h)


def _num(v: float) -> Decimal:
    return Decimal(f"{v:.6f}").normalize() if v != 0 else Decimal(0)


def place(data: bytes, params: dict) -> bytes:
    src = open_pdf(data, single_page=True)
    try:
        page = src.pages[0]
        geom = page_geometry(page.obj)
        width, height, cm = place_matrix(params, geom.width, geom.height)
        form = page.as_form_xobject(handle_transformations=False)
        form.BBox = pikepdf.Array([_num(v) for v in geom.box])
        form.Matrix = pikepdf.Array([_num(v) for v in geom.to_visible])
        for key in ("/StructParents", "/PieceInfo", "/Metadata", "/LastModified"):
            if key in form:
                del form[key]

        out = pikepdf.new()
        fx = out.copy_foreign(form)
        content = (
            f"q 0 0 {_num(width)} {_num(height)} re W n "
            f"{' '.join(str(_num(v)) for v in cm)} cm /Fx0 Do Q"
        ).encode()
        page_dict = pikepdf.Dictionary(
            Type=pikepdf.Name.Page,
            MediaBox=pikepdf.Array([0, 0, _num(width), _num(height)]),
            CropBox=pikepdf.Array([0, 0, _num(width), _num(height)]),
            Resources=pikepdf.Dictionary(XObject=pikepdf.Dictionary(Fx0=fx)),
            Contents=out.make_stream(content),
        )
        out.pages.append(pikepdf.Page(page_dict))
        buf = io.BytesIO()
        out.save(buf, compress_streams=True, deterministic_id=True)
        out.close()
        return buf.getvalue()
    finally:
        src.close()


# --- render ----------------------------------------------------------------------


def render(data: bytes, max_px: int = DEFAULT_RENDER_PX) -> bytes:
    if not isinstance(max_px, int) or isinstance(max_px, bool) or not 16 <= max_px <= MAX_RENDER_PX:
        raise Problem("bad_request", f"maxPx must be an integer from 16 to {MAX_RENDER_PX}")
    open_pdf(data, single_page=True).close()
    doc = open_pdfium(data)
    try:
        page = doc[0]
        try:
            w, h = page.get_size()
            if w <= 0 or h <= 0:
                raise Problem("pdf_unreadable", "the page has no size")
            scale = max_px / max(w, h)
            bitmap = page.render(scale=scale, draw_annots=False, may_draw_forms=False, rev_byteorder=True)
            image = bitmap.to_pil()
            bitmap.close()
        finally:
            page.close()
    finally:
        doc.close()
    buf = io.BytesIO()
    image.save(buf, format="PNG", compress_level=1)
    return buf.getvalue()


__all__ = ["inspect", "place", "place_matrix", "render", "strip_text"]
