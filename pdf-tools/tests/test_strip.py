from __future__ import annotations

import io

import pikepdf
import pytest

import fixtures
from helpers import changed_pixels, render_rgb
from pdf_tools import ops
from pdf_tools.document import SHOW_OPS
from pdf_tools.errors import Problem

WITH_TEXT = [
    "turkish",
    "rotated",
    "cropbox",
    "rotate90",
    "render_modes",
    "form_text",
    "quote_ops",
    "mixed_line",
    "paragraphs",
    "gradient_transparency",
    "certificate",
    "split_streams",
]


def convertible(data: bytes) -> list[dict]:
    return [s for s in ops.inspect(data)["spans"] if s["convertible"]]


@pytest.mark.parametrize("name", WITH_TEXT)
def test_strip_all_changes_nothing_else(name):
    data = fixtures.ALL[name]()
    chosen = convertible(data)
    out = ops.strip_text(data, [s["id"] for s in chosen])
    outside, inside = changed_pixels(render_rgb(data), render_rgb(out), [s["bbox"] for s in chosen])
    assert outside == 0
    assert inside > 0
    left = ops.inspect(out)["spans"]
    before = ops.inspect(data)["spans"]
    gone = {s["id"] for s in chosen}
    assert sorted(s["text"] for s in left) == sorted(s["text"] for s in before if s["id"] not in gone)


@pytest.mark.parametrize("name", ["quote_ops", "certificate", "mixed_line", "form_text"])
def test_strip_each_alone(name):
    data = fixtures.ALL[name]()
    base = render_rgb(data)
    before = ops.inspect(data)["spans"]
    for s in [x for x in before if x["convertible"]]:
        out = ops.strip_text(data, [s["id"]])
        outside, _ = changed_pixels(base, render_rgb(out), [s["bbox"]])
        assert outside == 0, s["text"]
        after = ops.inspect(out)["spans"]
        # Every other text where it was.
        expect = sorted((x["text"], x["origin"]["x"], x["origin"]["y"]) for x in before if x["id"] != s["id"])
        got = sorted((x["text"], x["origin"]["x"], x["origin"]["y"]) for x in after)
        assert got == expect, s["text"]


def test_text_after_a_removed_one_on_its_line_stays():
    data = fixtures.quote_ops()
    spans = ops.inspect(data)["spans"]
    first = next(s for s in spans if s["text"] == "Birinci")
    second = next(s for s in spans if s["text"] == "ikinci")
    kerned = next(s for s in spans if s["text"] == "Ker nli yazı")
    out = ops.strip_text(data, [first["id"], kerned["id"]])
    after = {s["text"]: s for s in ops.inspect(out)["spans"]}
    assert "Birinci" not in after and "Ker nli yazı" not in after
    assert after["ikinci"]["origin"] == second["origin"]


def _non_text(content: list) -> bytes:
    keep = [ins for ins in content if isinstance(ins, pikepdf.ContentStreamInlineImage) or str(ins.operator) not in SHOW_OPS]
    return pikepdf.unparse_content_stream(keep)


def test_other_operators_unchanged():
    data = fixtures.certificate()
    out = ops.strip_text(data, [s["id"] for s in convertible(data)])
    a = pikepdf.open(io.BytesIO(data))
    b = pikepdf.open(io.BytesIO(out))
    before = list(pikepdf.parse_content_stream(a.pages[0]))
    after = list(pikepdf.parse_content_stream(b.pages[0]))
    assert _non_text(before) == _non_text(after)
    assert not any(str(i.operator) in ("Tj", "'", '"') for i in after)


def test_gradient_and_transparency_kept():
    data = fixtures.gradient_transparency()
    chosen = convertible(data)
    out = ops.strip_text(data, [s["id"] for s in chosen])
    outside, _ = changed_pixels(render_rgb(data, 1.0), render_rgb(out, 1.0), [s["bbox"] for s in chosen], scale=1.0)
    assert outside == 0


def test_unknown_and_blocked_ids():
    data = fixtures.render_modes()
    spans = ops.inspect(data)["spans"]
    with pytest.raises(Problem) as e:
        ops.strip_text(data, ["999"])
    assert e.value.code == "bad_request" and e.value.extra["spanIds"] == ["999"]
    stroke = next(s for s in spans if s["text"] == "Kontur")
    with pytest.raises(Problem) as e:
        ops.strip_text(data, [stroke["id"]])
    assert e.value.code == "bad_request" and e.value.extra["spanIds"] == [stroke["id"]]


def test_shared_form_text_is_refused():
    data = fixtures.form_text()
    shared = [s for s in ops.inspect(data)["spans"] if s.get("reason") == "shared"]
    with pytest.raises(Problem) as e:
        ops.strip_text(data, [shared[0]["id"]])
    assert e.value.code == "bad_request"


def test_incomplete_strip_is_reported(monkeypatch):
    data = fixtures.turkish()
    spans = ops.inspect(data)["spans"]
    # A removal that keeps the operator: the read-back must notice.
    monkeypatch.setattr(ops, "_replacement", lambda ins, kerning: [ins])
    with pytest.raises(Problem) as e:
        ops.strip_text(data, [spans[0]["id"]])
    assert e.value.code == "strip_incomplete"
    assert e.value.status == 422


@pytest.mark.parametrize("fixture,code", [("two_pages", "pdf_pages"), ("encrypted", "pdf_encrypted")])
def test_strip_refuses(fixture, code):
    data = getattr(fixtures, fixture)()
    with pytest.raises(Problem) as e:
        ops.strip_text(data, ["0"])
    assert e.value.code == code
