"""What /inspect says about one text object, and the line and block hints."""

from __future__ import annotations

import ctypes
import math

import pypdfium2.raw as pdfium_c

from .geometry import PageGeometry, apply, linear, mul

# PDF font descriptor flags.
_FIXED_PITCH = 1 << 0
_SERIF = 1 << 1
_SCRIPT = 1 << 3
_ITALIC = 1 << 6


def _round(v: float, nd: int = 3) -> float:
    r = round(v, nd)
    return 0.0 if r == 0 else r


def _color(obj: object, stroke: bool) -> tuple[str, float]:
    r, g, b, a = (ctypes.c_uint(0) for _ in range(4))
    get = pdfium_c.FPDFPageObj_GetStrokeColor if stroke else pdfium_c.FPDFPageObj_GetFillColor
    if not get(obj, ctypes.byref(r), ctypes.byref(g), ctypes.byref(b), ctypes.byref(a)):
        return "#000000", 1.0
    return f"#{r.value:02x}{g.value:02x}{b.value:02x}", _round(a.value / 255.0)


def _quad(obj: object) -> list[tuple[float, float]] | None:
    q = pdfium_c.FS_QUADPOINTSF()
    if not pdfium_c.FPDFPageObj_GetRotatedBounds(obj, ctypes.byref(q)):
        return None
    return [(q.x1, q.y1), (q.x2, q.y2), (q.x3, q.y3), (q.x4, q.y4)]


def _font_metric(fn, font: object) -> float:
    v = ctypes.c_float(0)
    if not font or not fn(font, ctypes.c_float(1.0), ctypes.byref(v)):
        return 0.0
    return float(v.value)


def describe(t, holder, show, geometry: PageGeometry) -> tuple[dict, str | None]:
    """The span's fields, and why it cannot become a layer (None: it can,
    as far as the text itself goes)."""
    from .document import normalize_text, strip_subset, weight_from_name

    obj = t.handle
    to_view = mul(holder.to_page, geometry.to_top_left)
    full = mul(t.matrix, to_view)
    x_axis = linear(full, 1.0, 0.0)
    y_axis = linear(full, 0.0, 1.0)
    x_len = math.hypot(*x_axis)
    y_len = math.hypot(*y_axis)
    origin = (full[4], full[5])
    angle = math.degrees(math.atan2(x_axis[1], x_axis[0])) if x_len > 0 else 0.0

    quad_src = _quad(obj)
    if quad_src is not None:
        quad = [apply(to_view, x, y) for x, y in quad_src]
    else:
        quad = [origin] * 4
    xs = [p[0] for p in quad]
    ys = [p[1] for p in quad]

    mode = int(pdfium_c.FPDFTextObj_GetTextRenderMode(obj))
    stroke_only = mode in (1, 5)
    color, opacity = _color(obj, stroke_only)

    font = pdfium_c.FPDFTextObj_GetFont(obj)
    base = strip_subset(t.base_font)
    flags = ctypes.c_int(0)
    flags_v = pdfium_c.FPDFFont_GetFlags(font) if font else 0
    flags.value = flags_v if flags_v > 0 else 0
    desc_weight = pdfium_c.FPDFFont_GetWeight(font) if font else -1
    italic_angle = ctypes.c_int(0)
    has_angle = bool(font) and pdfium_c.FPDFFont_GetItalicAngle(font, ctypes.byref(italic_angle))
    name_low = base.lower()
    weight = weight_from_name(base) or (desc_weight if desc_weight and desc_weight > 0 else 400)
    italic = (
        "italic" in name_low
        or "oblique" in name_low
        or bool(flags.value & _ITALIC)
        or (has_angle and italic_angle.value != 0)
    )
    from .document import _font_family  # PDFium's family name

    full_text = normalize_text(t.text)
    text = full_text.strip()
    info = {
        "text": text,
        # Removed before answering: spaces at the ends (a word gap to the
        # neighbouring span).
        "_lead": full_text[:1].isspace(),
        "_trail": full_text[-1:].isspace(),
        "bbox": {
            "x": _round(min(xs)),
            "y": _round(min(ys)),
            "w": _round(max(xs) - min(xs)),
            "h": _round(max(ys) - min(ys)),
        },
        "quad": [[_round(x), _round(y)] for x, y in quad],
        "origin": {"x": _round(origin[0]), "y": _round(origin[1])},
        "angle": _round(angle, 2),
        "size": _round(t.size * y_len),
        "hscale": _round(x_len / y_len) if y_len > 0 else 1.0,
        "font": {
            "name": base,
            "family": _font_family(font),
            "weight": int(weight),
            "italic": italic,
            "serif": bool(flags.value & _SERIF),
            "mono": bool(flags.value & _FIXED_PITCH),
            "script": bool(flags.value & _SCRIPT),
            "embedded": bool(font) and bool(pdfium_c.FPDFFont_GetIsEmbedded(font)),
            "ascent": _round(_font_metric(pdfium_c.FPDFFont_GetAscent, font)),
            "descent": _round(_font_metric(pdfium_c.FPDFFont_GetDescent, font)),
        },
        "color": color,
        "opacity": opacity,
        "renderMode": mode,
    }

    reason = None
    unmapped = show is not None and show.font is not None and show.font.unmapped
    if unmapped or not text or any(c == "\ufffd" or 0xE000 <= ord(c) <= 0xF8FF for c in text):
        reason = "unreadable"
    elif mode in (3, 7):
        reason = "invisible" if mode == 3 else "clip"
    elif mode >= 4:
        reason = "clip"
    elif mode == 1:
        reason = "stroke"
    elif show is not None and show.font is not None and show.font.vertical:
        reason = "vertical"
    else:
        # Mirrored, sheared or a zero axis: a layer cannot show it so.
        det = x_axis[0] * y_axis[1] - x_axis[1] * y_axis[0]
        if x_len == 0 or y_len == 0:
            reason = "skewed"
        else:
            cos = (x_axis[0] * y_axis[0] + x_axis[1] * y_axis[1]) / (x_len * y_len)
            # The visible page has y down, so upright text has det < 0.
            if abs(cos) > 0.02 or det > 0:
                reason = "skewed"
    return info, reason


def _style(s: dict) -> tuple:
    return (s["font"]["name"], round(s["size"] * 4) / 4, s["color"], s["opacity"], s["renderMode"])


def group(spans: list[dict]) -> list[dict]:
    """Hints for the caller, set on each span: line (spans on one baseline,
    next to each other), block (lines of one style stacked closely), run
    (one style along a line: what a reader takes for one piece of text) and
    spaceBefore (a word gap before the span in its run). Spans of spaces
    only (text "") take part and are dropped by the caller. Answers the
    runs: their span ids (no blank ones) and text."""
    if not spans:
        return []
    items = []
    for i, s in enumerate(spans):
        th = math.radians(s["angle"])
        u = (math.cos(th), math.sin(th))
        v = (-math.sin(th), math.cos(th))
        ox, oy = s["origin"]["x"], s["origin"]["y"]
        along = [x * u[0] + y * u[1] for x, y in s["quad"]]
        items.append(
            {
                "i": i,
                "angle": round(s["angle"]),
                "base": ox * v[0] + oy * v[1],
                "start": min(along),
                "end": max(along),
                "size": max(s["size"], 0.1),
                "blank": not s["text"],
            }
        )
    items.sort(key=lambda it: (it["angle"], round(it["base"], 1), it["start"]))
    # Lines: the same direction and baseline, no wide gap.
    lines: list[list[dict]] = []
    line_end = 0.0
    for it in items:
        if lines:
            last = lines[-1][-1]
            tol = 0.25 * min(it["size"], last["size"])
            if (
                it["angle"] == last["angle"]
                and abs(it["base"] - last["base"]) <= tol
                and it["start"] - line_end <= 1.5 * max(it["size"], last["size"])
            ):
                lines[-1].append(it)
                line_end = max(line_end, it["end"])
                continue
        lines.append([it])
        line_end = it["end"]
    for n, line in enumerate(lines):
        line.sort(key=lambda it: it["start"])
        for it in line:
            spans[it["i"]]["line"] = n

    # Runs: along each line, one style without a gap wider than a size;
    # a blank span or an ink gap of a fifth of the size is a word space.
    runs: list[dict] = []
    for line in lines:
        cur: dict | None = None
        prev_end = None
        gap_space = False
        for it in line:
            s = spans[it["i"]]
            if it["blank"]:
                gap_space = True
                continue
            space = (
                gap_space
                or s["_lead"]
                or (prev_end is not None and it["start"] - prev_end > 0.2 * it["size"])
            )
            gap_space = s["_trail"]
            if (
                cur is None
                or _style(s) != cur["style"]
                or prev_end is None
                or it["start"] - prev_end > 1.0 * it["size"]
            ):
                cur = {"style": _style(s), "spanIds": [], "text": "", "line": s["line"]}
                runs.append(cur)
                space = False
            s["run"] = len(runs) - 1
            s["spaceBefore"] = space
            cur["spanIds"].append(s["id"])
            cur["text"] += (" " if space else "") + s["text"]
            prev_end = it["end"]

    # Blocks: a line joins the block of a line of the same style just above
    # it (at most two sizes) that it overlaps along the baseline.
    info = []
    for line in lines:
        solid = [it for it in line if not it["blank"]] or line
        info.append(
            {
                "line": line,
                "angle": line[0]["angle"],
                "base": line[0]["base"],
                "start": min(x["start"] for x in solid),
                "end": max(x["end"] for x in solid),
                "size": max(x["size"] for x in solid),
                "style": _style(spans[solid[0]["i"]])[:3],
            }
        )
    order = sorted(range(len(info)), key=lambda n: (info[n]["angle"], info[n]["base"]))
    block_of: dict[int, int] = {}
    blocks = 0
    for pos, n in enumerate(order):
        cur_line = info[n]
        found = None
        for m in reversed(order[max(0, pos - 64) : pos]):
            prev = info[m]
            if prev["angle"] != cur_line["angle"]:
                break
            gap = cur_line["base"] - prev["base"]
            if gap > 2.0 * max(cur_line["size"], prev["size"]):
                break
            if (
                gap > 0
                and prev["style"] == cur_line["style"]
                and cur_line["start"] <= prev["end"]
                and prev["start"] <= cur_line["end"]
            ):
                found = block_of[m]
                break
        if found is None:
            found = blocks
            blocks += 1
        block_of[n] = found
        for it in cur_line["line"]:
            spans[it["i"]]["block"] = found
    for s in spans:
        s.pop("_lead", None)
        s.pop("_trail", None)
    by_id = {s["id"]: s for s in spans}
    for r in runs:
        r["block"] = by_id[r["spanIds"][0]]["block"]
        del r["style"]
    return runs
