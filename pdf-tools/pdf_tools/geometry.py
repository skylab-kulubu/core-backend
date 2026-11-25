"""2D affine matrices (PDF's row-vector convention) and the visible page.

A matrix is (a, b, c, d, e, f): a point (x, y) maps to
(a·x + c·y + e, b·x + d·y + f). mul(m, n) applies m first, then n.
"""

from __future__ import annotations

import math
from dataclasses import dataclass

import pikepdf

Matrix = tuple[float, float, float, float, float, float]
IDENTITY: Matrix = (1.0, 0.0, 0.0, 1.0, 0.0, 0.0)

# PDFium's page size when a page has no usable MediaBox (US Letter).
_DEFAULT_MEDIABOX = (0.0, 0.0, 612.0, 792.0)


def mul(m: Matrix, n: Matrix) -> Matrix:
    a, b, c, d, e, f = m
    A, B, C, D, E, F = n
    return (
        a * A + b * C,
        a * B + b * D,
        c * A + d * C,
        c * B + d * D,
        e * A + f * C + E,
        e * B + f * D + F,
    )


def apply(m: Matrix, x: float, y: float) -> tuple[float, float]:
    return (x * m[0] + y * m[2] + m[4], x * m[1] + y * m[3] + m[5])


def linear(m: Matrix, x: float, y: float) -> tuple[float, float]:
    return (x * m[0] + y * m[2], x * m[1] + y * m[3])


def _box(value: object) -> tuple[float, float, float, float] | None:
    try:
        nums = [float(v) for v in value]  # type: ignore[union-attr]
    except (TypeError, ValueError):
        return None
    if len(nums) != 4 or not all(math.isfinite(v) for v in nums):
        return None
    x0, y0, x1, y1 = nums
    x0, x1 = min(x0, x1), max(x0, x1)
    y0, y1 = min(y0, y1), max(y0, y1)
    if x1 - x0 <= 0 or y1 - y0 <= 0:
        return None
    return (x0, y0, x1, y1)


def inherited(page: pikepdf.Dictionary, key: str) -> object | None:
    """A page attribute, looked up through the page tree as PDF inherits it."""
    node: object | None = page
    seen = 0
    while isinstance(node, pikepdf.Dictionary) and seen < 64:
        if key in node:
            return node[key]
        node = node.get("/Parent")
        seen += 1
    return None


@dataclass(frozen=True)
class PageGeometry:
    """The visible part of a page: its CropBox (inside the MediaBox), turned
    by /Rotate, as PDFium shows it."""

    box: tuple[float, float, float, float]
    rotate: int  # 0, 90, 180 or 270, clockwise as shown
    width: float  # pt, as shown
    height: float

    @property
    def to_visible(self) -> Matrix:
        """User space to the visible page, origin bottom left, y up."""
        x0, y0, x1, y1 = self.box
        if self.rotate == 90:
            return (0.0, -1.0, 1.0, 0.0, -y0, x1)
        if self.rotate == 180:
            return (-1.0, 0.0, 0.0, -1.0, x1, y1)
        if self.rotate == 270:
            return (0.0, 1.0, -1.0, 0.0, y1, -x0)
        return (1.0, 0.0, 0.0, 1.0, -x0, -y0)

    @property
    def to_top_left(self) -> Matrix:
        """User space to the visible page, origin top left, y down (the
        coordinates /inspect answers in)."""
        return mul(self.to_visible, (1.0, 0.0, 0.0, -1.0, 0.0, self.height))


def page_geometry(page: pikepdf.Dictionary) -> PageGeometry:
    media = _box(inherited(page, "/MediaBox")) or _DEFAULT_MEDIABOX
    crop = _box(inherited(page, "/CropBox"))
    if crop is None:
        box = media
    else:
        box = (
            max(crop[0], media[0]),
            max(crop[1], media[1]),
            min(crop[2], media[2]),
            min(crop[3], media[3]),
        )
        if box[2] - box[0] <= 0 or box[3] - box[1] <= 0:
            box = media
    rotate_obj = inherited(page, "/Rotate")
    try:
        # C's division, as PDFium: toward zero, then into 0..3.
        quarter = math.trunc(int(rotate_obj) / 90) % 4 if rotate_obj is not None else 0  # type: ignore[call-overload]
    except (TypeError, ValueError):
        quarter = 0
    rotate = quarter * 90
    w, h = box[2] - box[0], box[3] - box[1]
    if rotate in (90, 270):
        w, h = h, w
    return PageGeometry(box=box, rotate=rotate, width=w, height=h)
