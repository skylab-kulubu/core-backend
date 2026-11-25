from __future__ import annotations

import io

import pytest
from PIL import Image

import fixtures
from pdf_tools import ops
from pdf_tools.errors import Problem


def png_size(data: bytes) -> tuple[int, int]:
    assert data[:8] == b"\x89PNG\r\n\x1a\n"
    return Image.open(io.BytesIO(data)).size


def test_render_long_side_is_max_px():
    w, h = png_size(ops.render(fixtures.certificate(), 2000))
    assert w == 2000 and abs(h - 2000 * 595 / 842) <= 1


def test_render_default_and_rotation():
    w, h = png_size(ops.render(fixtures.rotate90()))
    assert h == 2000 and abs(w - 2000 * 595 / 842) <= 1


def test_render_cropbox():
    w, h = png_size(ops.render(fixtures.cropbox(), 642))
    assert (w, h) == (642, 495)


@pytest.mark.parametrize("max_px", [0, 15, 4001, "2000", 1.5, True])
def test_bad_max_px(max_px):
    with pytest.raises(Problem) as e:
        ops.render(fixtures.turkish(), max_px)
    assert e.value.code == "bad_request"


def test_render_refuses_two_pages():
    with pytest.raises(Problem) as e:
        ops.render(fixtures.two_pages())
    assert e.value.code == "pdf_pages"
