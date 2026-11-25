from __future__ import annotations

import pytest

import fixtures
from pdf_tools import ops
from pdf_tools.errors import Problem


def by_text(result: dict) -> dict[str, dict]:
    return {s["text"]: s for s in result["spans"]}


def test_turkish_text_is_nfc_with_ligatures_spelled_out():
    r = ops.inspect(fixtures.turkish())
    spans = by_text(r)
    assert set(spans) == {"Katılım Belgesi ğĞıİşŞçÇöÖüÜ", "Ayşe Yıldız", "OfiS"}
    # The decomposed ş (s + U+0327) became one character.
    assert len("Ayşe") == 4 and "Ayşe Yıldız" in spans
    s = spans["Katılım Belgesi ğĞıİşŞçÇöÖüÜ"]
    assert s["convertible"] is True and "reason" not in s
    assert s["font"]["name"] == "TestSans-Bold"  # subset prefix dropped
    assert s["font"]["weight"] == 700
    assert s["font"]["embedded"] is True
    assert s["size"] == 28.0
    assert s["angle"] == 0.0
    assert s["origin"] == {"x": 60.0, "y": 115.0}  # 595 - 480
    assert s["color"] == "#000000" and s["opacity"] == 1.0
    assert s["renderMode"] == 0
    # The quad runs along the baseline first: bottom-left, bottom-right,
    # top-right, top-left.
    q = s["quad"]
    assert q[0][1] == q[1][1] and q[1][0] > q[0][0] and q[2][1] < q[1][1]
    assert s["bbox"]["x"] >= 60 and s["bbox"]["y"] + s["bbox"]["h"] <= 115.5
    assert r["page"] == {"widthPt": 842.0, "heightPt": 595.0, "rotate": 0}
    assert r["pages"] == 1 and r["encrypted"] is False
    assert r["textCount"] == 3 and r["truncated"] is False


def test_rotated_text_angles_clockwise_as_shown():
    spans = by_text(ops.inspect(fixtures.rotated()))
    up = spans["Dikey yazı 90"]
    assert up["angle"] == -90.0 and up["convertible"]
    assert up["origin"] == {"x": 100.0, "y": 445.0}
    assert up["size"] == 20.0
    slanted = spans["Eğik değil 30"]
    assert slanted["angle"] == -30.0 and slanted["convertible"]
    assert slanted["size"] == 20.0


def test_cropbox_is_the_visible_page():
    r = ops.inspect(fixtures.cropbox())
    assert r["page"] == {"widthPt": 642.0, "heightPt": 495.0, "rotate": 0}
    s = r["spans"][0]
    assert s["origin"] == {"x": 50.0, "y": 245.0}  # (150-100, 545-300)


def test_rotate_90_turns_the_coordinates():
    r = ops.inspect(fixtures.rotate90())
    assert r["page"] == {"widthPt": 595.0, "heightPt": 842.0, "rotate": 90}
    s = r["spans"][0]
    assert s["angle"] == 90.0
    assert s["origin"] == {"x": 300.0, "y": 100.0}
    assert s["convertible"]


def test_render_modes():
    spans = by_text(ops.inspect(fixtures.render_modes()))
    assert spans["Kontur"]["reason"] == "stroke" and spans["Kontur"]["color"] == "#0000ff"
    assert spans["Görünmez OCR"]["reason"] == "invisible"
    assert spans["Kırpma"]["reason"] == "clip"
    assert spans["Dolgu ve kontur"]["convertible"] and spans["Dolgu ve kontur"]["renderMode"] == 2
    assert spans["Düz"]["convertible"]


def test_font_without_tounicode_is_unreadable():
    (s,) = ops.inspect(fixtures.no_tounicode())["spans"]
    assert s["convertible"] is False and s["reason"] == "unreadable"


def test_vertical_writing():
    (s,) = ops.inspect(fixtures.vertical())["spans"]
    assert s["reason"] == "vertical"


def test_form_xobject_text():
    spans = ops.inspect(fixtures.form_text())["spans"]
    texts = [(s["text"], s["convertible"], s.get("reason")) for s in spans]
    assert ("Form içinde", True, None) in texts
    assert ("Sayfada", True, None) in texts
    # Drawn twice from one form: cannot lose its text in one place only.
    assert texts.count(("İki kez", False, "shared")) == 2
    inner = next(s for s in spans if s["text"] == "Form içinde")
    assert inner["origin"] == {"x": 160.0, "y": 235.0}  # cm 100,300 + Matrix 50,50 + Tm 10,10
    assert "." in inner["id"]


def test_quote_operators_and_runs():
    r = ops.inspect(fixtures.quote_ops())
    assert all(s["convertible"] for s in r["spans"])
    runs = [x["text"] for x in r["runs"]]
    assert "Birinci ikinci üçüncü" in runs
    assert "Tırnak satırı" in runs
    assert "Helvetica devam" in runs
    first = r["spans"][0]
    assert first["text"] == "Birinci" and first["spaceBefore"] is False
    third = next(s for s in r["spans"] if s["text"] == "üçüncü")
    assert third["spaceBefore"] is True and third["run"] == first["run"]


def test_mixed_line_one_line_three_runs():
    r = ops.inspect(fixtures.mixed_line())
    spans = r["spans"]
    assert [s["text"] for s in spans] == ["Bu belge", "Ahmet Y", "adina"]
    assert len({s["line"] for s in spans}) == 1
    assert len({s["run"] for s in spans}) == 3
    bold = spans[1]
    assert bold["font"]["name"] == "Helvetica-Bold" and bold["font"]["weight"] == 700
    assert bold["color"] == "#cc1a1a"


def test_paragraph_blocks_by_column():
    spans = ops.inspect(fixtures.paragraphs())["spans"]
    blocks: dict[int, list[str]] = {}
    for s in spans:
        blocks.setdefault(s["block"], []).append(s["text"])
    assert sorted(len(v) for v in blocks.values()) == [3, 3, 3]
    assert len({s["line"] for s in spans}) == 9


def test_gradient_and_transparency():
    spans = by_text(ops.inspect(fixtures.gradient_transparency()))
    assert spans["Yarı saydam"]["opacity"] == 0.502 and spans["Yarı saydam"]["color"] == "#ffffff"
    assert spans["Grup içinde"]["opacity"] == 0.349
    assert all(s["convertible"] for s in spans.values())


def test_certificate_has_thirty_texts():
    r = ops.inspect(fixtures.certificate())
    assert r["textCount"] == 30
    assert sum(1 for s in r["spans"] if s["convertible"]) == 30


def test_text_across_two_content_streams():
    spans = ops.inspect(fixtures.split_streams())["spans"]
    assert [(s["text"], s["convertible"]) for s in spans] == [("Birinci akış", True), ("ikinci akış", True)]


def test_two_pages_inspects_the_first():
    r = ops.inspect(fixtures.two_pages())
    assert r["pages"] == 2 and r["textCount"] == 1


@pytest.mark.parametrize("data,code", [(b"not a pdf", "pdf_unreadable"), (b"%PDF-1.7\n%%EOF", "pdf_unreadable")])
def test_unreadable(data, code):
    with pytest.raises(Problem) as e:
        ops.inspect(data)
    assert e.value.code == code


def test_encrypted():
    with pytest.raises(Problem) as e:
        ops.inspect(fixtures.encrypted())
    assert e.value.code == "pdf_encrypted"
