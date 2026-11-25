from __future__ import annotations

import io

import pikepdf
import pytest

import fixtures
from helpers import render_rgb
from PIL import ImageChops
from pdf_tools import ops
from pdf_tools.errors import Problem


def content_of(out: bytes) -> tuple[pikepdf.Pdf, bytes]:
    pdf = pikepdf.open(io.BytesIO(out))
    return pdf, pdf.pages[0].obj.Contents.read_bytes()


# The box arithmetic of SPEC §4.2 (core computes the rect; pdf-tools draws
# the visible source page into it): A4 landscape source, A4 portrait page.
SRC_W, SRC_H = 842.0, 595.0
PAGE_W, PAGE_H = 595.0, 842.0


@pytest.mark.parametrize(
    "rect,cm",
    [
        # contain: k = min(595/842, 842/595) = 0.70665; 595 × 420.46, centred.
        (
            {"x": 0, "y": 210.7684, "w": 595.0, "h": 420.4632},
            "0.706651 0 0 0.706651 0 210.7684 cm",
        ),
        # cover: k = 842/595 = 1.41513; 1191.54 × 842, centred, cut by the page.
        (
            {"x": -298.2689, "y": 0, "w": 1191.5378, "h": 842.0},
            "1.415126 0 0 1.415126 -298.2689 0 cm",
        ),
        # fill: each side on its own.
        (
            {"x": 0, "y": 0, "w": 595.0, "h": 842.0},
            "0.706651 0 0 1.415126 0 0 cm",
        ),
    ],
)
def test_place_matrix(rect, cm):
    out = ops.place(fixtures.certificate(), {"pageWidthPt": PAGE_W, "pageHeightPt": PAGE_H, "rect": rect})
    pdf, content = content_of(out)
    assert content == f"q 0 0 595 842 re W n {cm} /Fx0 Do Q".encode()
    page = pdf.pages[0].obj
    assert list(page.MediaBox) == [0, 0, 595, 842]
    assert list(page.CropBox) == [0, 0, 595, 842]


def test_place_matrix_numbers():
    w, h, cm = ops.place_matrix({"pageWidthPt": 600, "pageHeightPt": 400, "rect": {"x": 10, "y": 20, "w": 300, "h": 100}}, 150, 50)
    assert (w, h) == (600, 400)
    # x scale 2, y scale 2; the rect's bottom in PDF space is 400 - 20 - 100.
    assert cm == (2.0, 0.0, 0.0, 2.0, 10.0, 280.0)


@pytest.mark.parametrize("name", ["certificate", "cropbox", "rotate90", "gradient_transparency", "split_streams"])
def test_same_size_draws_the_same_page(name):
    data = fixtures.ALL[name]()
    page = ops.inspect(data)["page"]
    out = ops.place(
        data,
        {"pageWidthPt": page["widthPt"], "pageHeightPt": page["heightPt"], "rect": {"x": 0, "y": 0, "w": page["widthPt"], "h": page["heightPt"]}},
    )
    a, b = render_rgb(data, 1.5), render_rgb(out, 1.5)
    assert a.size == b.size
    assert ImageChops.difference(a, b).getbbox() is None


def test_visible_box_and_rotation_in_the_form():
    pdf, _ = content_of(ops.place(fixtures.cropbox(), {"pageWidthPt": 642, "pageHeightPt": 495, "rect": {"x": 0, "y": 0, "w": 642, "h": 495}}))
    fx = pdf.pages[0].obj.Resources.XObject.Fx0
    assert list(fx.BBox) == [100, 50, 742, 545]
    assert list(fx.Matrix) == [1, 0, 0, 1, -100, -50]
    pdf, _ = content_of(ops.place(fixtures.rotate90(), {"pageWidthPt": 595, "pageHeightPt": 842, "rect": {"x": 0, "y": 0, "w": 595, "h": 842}}))
    fx = pdf.pages[0].obj.Resources.XObject.Fx0
    assert list(fx.Matrix) == [0, -1, 1, 0, 0, 842]


def test_only_page_content_is_carried():
    b = fixtures.Builder()
    f = b.type0()
    b.add("BT", f"/{f} 20 Tf 1 0 0 1 60 300 Tm {b.hex('Açıklamalı')} Tj", "ET")
    data = b.build(annotate=True)
    out = ops.place(data, {"pageWidthPt": 842, "pageHeightPt": 595, "rect": {"x": 0, "y": 0, "w": 842, "h": 595}})
    pdf = pikepdf.open(io.BytesIO(out))
    assert "/Annots" not in pdf.pages[0].obj
    assert "/AcroForm" not in pdf.Root
    assert len(pdf.pages) == 1


def test_overflowing_box_is_cut_to_the_page():
    data = fixtures.certificate()
    out = ops.place(data, {"pageWidthPt": 400, "pageHeightPt": 400, "rect": {"x": -200, "y": -100, "w": 1000, "h": 700}})
    img = render_rgb(out, 1.0)
    assert img.size == (400, 400)


@pytest.mark.parametrize(
    "params",
    [
        {},
        {"pageWidthPt": 0, "pageHeightPt": 100, "rect": {"x": 0, "y": 0, "w": 1, "h": 1}},
        {"pageWidthPt": 100, "pageHeightPt": 100},
        {"pageWidthPt": 100, "pageHeightPt": 100, "rect": {"x": 0, "y": 0, "w": 0, "h": 1}},
        {"pageWidthPt": 100, "pageHeightPt": 100, "rect": {"x": "a", "y": 0, "w": 1, "h": 1}},
        {"pageWidthPt": 100000, "pageHeightPt": 100, "rect": {"x": 0, "y": 0, "w": 1, "h": 1}},
    ],
)
def test_bad_params(params):
    with pytest.raises(Problem) as e:
        ops.place(fixtures.turkish(), params)
    assert e.value.code == "bad_request"


def test_place_refuses_two_pages():
    with pytest.raises(Problem) as e:
        ops.place(fixtures.two_pages(), {"pageWidthPt": 100, "pageHeightPt": 100, "rect": {"x": 0, "y": 0, "w": 1, "h": 1}})
    assert e.value.code == "pdf_pages"
