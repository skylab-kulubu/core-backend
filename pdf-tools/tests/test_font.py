from __future__ import annotations

import base64
import io
import json
import socket
import sys
import threading
import time

import pytest
from fontTools.ttLib import TTFont

import font_fixtures as ff
from helpers import Client
from pdf_tools import fonts
from pdf_tools.errors import Problem
from pdf_tools.server import MAX_FONT_BODY_BYTES, make_server
from pdf_tools.worker import Pool

PATH = "/v1/font/sanitize"


def problem(status, headers, body):
    assert headers["content-type"] == "application/problem+json"
    p = json.loads(body)
    assert p["status"] == status and p["type"] == "about:blank" and p["title"]
    return p


def sanitized(client, data, ctype=None):
    if ctype:
        st, headers, body = client.request("POST", PATH, data, {"Content-Type": ctype})
    else:
        st, headers, body = client.post(PATH, data)
    assert st == 200, body
    assert headers["content-type"] == "application/json"
    r = json.loads(body)
    out = base64.b64decode(r.pop("woff2"))
    return r, out


def reopens(woff2: bytes) -> TTFont:
    """The answer is a WOFF2 that fontTools opens and OTS passes again."""
    assert woff2.startswith(b"wOF2")
    font = TTFont(io.BytesIO(woff2))
    assert font.flavor == "woff2"
    again = fonts._ots(woff2, 5.0)
    assert again[:4] in (b"\x00\x01\x00\x00", b"OTTO")
    return font


def test_ttf(client):
    r, out = sanitized(client, ff.ttf())
    assert r == {
        "family": "Deneme Sans",
        "subfamily": "Regular",
        "weight": 400,
        "italic": False,
        "glyphCount": len(ff.ALL) + 2,
        "missingTurkish": "",
        "fsType": 0,
    }
    font = reopens(out)
    assert ord("ğ") in font.getBestCmap() and "glyf" in font


def test_otf_cff(client):
    r, out = sanitized(client, ff.otf())
    assert (r["family"], r["subfamily"], r["weight"], r["italic"]) == ("Deneme Serif", "Bold", 700, False)
    font = reopens(out)
    assert "CFF " in font


def test_woff2_in_and_raw_body(client):
    data = ff.woff2(ff.ttf(family="Web Yazı", style="Italic", italic=True, weight=300))
    assert data.startswith(b"wOF2")
    r, out = sanitized(client, data, "font/woff2")
    assert (r["family"], r["subfamily"], r["weight"], r["italic"]) == ("Web Yazı", "Italic", 300, True)
    reopens(out)
    r2, _ = sanitized(client, data)
    assert r2 == r


def test_typographic_names_win(client):
    r, _ = sanitized(client, ff.ttf(family="Deneme Sans SemiBold", style="Regular", weight=600, typographic="Deneme Sans"))
    assert r["family"] == "Deneme Sans" and r["weight"] == 600


def test_missing_turkish_letters_are_listed(client):
    chars = "".join(c for c in ff.ALL if c not in "ğıŞ")
    r, out = sanitized(client, ff.ttf(chars=chars))
    assert r["missingTurkish"] == "ğıŞ"
    reopens(out)


def test_missing_latin_and_digits_too(client):
    r, _ = sanitized(client, ff.ttf(chars=ff.TURKISH + "abc"))
    assert r["missingTurkish"].startswith("ABC") and r["missingTurkish"].endswith("0123456789")
    assert "a" not in r["missingTurkish"] and "ğ" not in r["missingTurkish"]


@pytest.mark.parametrize(
    "fixture,status,code",
    [
        (lambda: ff.ttf(fvar=True), 422, "font_variable"),
        (ff.ttc, 422, "font_collection"),
        (lambda: ff.ttf(fs_type=0x0002), 422, "font_embedding_restricted"),
        (lambda: ff.ttf(fs_type=0x0006), 422, "font_embedding_restricted"),
        (lambda: ff.ttf(fs_type=0x0200), 422, "font_embedding_restricted"),
        (lambda: ff.ttf(fs_type=0x0208), 422, "font_embedding_restricted"),
        (ff.bad_head, 422, "font_unreadable"),
        (lambda: ff.ttf()[: 400], 422, "font_unreadable"),
        (lambda: b"hello, not a font", 422, "font_unreadable"),
        (lambda: b"wOFF" + b"\x00" * 60, 422, "font_unreadable"),
        (lambda: b"wOF2ttcf" + b"\x00" * 60, 422, "font_collection"),
    ],
    ids=["fvar", "ttc", "restricted", "restricted-and-print", "bitmap-only", "editable-bitmap-only", "ots-reject", "truncated", "not-a-font", "woff1", "woff2-collection"],
)
def test_refused(client, fixture, status, code):
    st, headers, body = client.post(PATH, fixture())
    assert st == status, body
    assert problem(status, headers, body)["code"] == code


def test_restricted_says_its_fs_type(client):
    st, headers, body = client.post(PATH, ff.ttf(fs_type=0x0002 | 0x0100))
    p = problem(422, headers, body)
    assert p["code"] == "font_embedding_restricted" and p["fsType"] == 0x0102


def test_editable_embedding_passes(client):
    r, _ = sanitized(client, ff.ttf(fs_type=0x0008))
    assert r["fsType"] == 8


def test_woff2_bomb_is_refused_quickly(client):
    """A few KiB that would inflate to 16 MiB: OTS refuses it (its
    compression ratio), nothing is written large."""
    bomb = ff.woff2_bomb()
    assert len(bomb) < 64 << 10
    started = time.monotonic()
    st, headers, body = client.post(PATH, bomb)
    assert st == 422 and problem(422, headers, body)["code"] == "font_unreadable"
    assert time.monotonic() - started < 5


def test_one_font_job_at_a_time():
    """Two workers, but one font job at a time: the second font waits for
    the first (a PDF job does not)."""
    pool = Pool(size=2, timeout=60.0, queue_wait=0.5, test_hooks=True, memory_mib=0)
    server = make_server("127.0.0.1", 0, pool, font_timeout=10.0)
    t = threading.Thread(target=server.serve_forever, daemon=True)
    t.start()
    try:
        c = Client(server.server_address[1])
        results = []
        first = threading.Thread(target=lambda: results.append(c.post("/v1/_font_sleep", b"x", {"seconds": 2})[0]))
        first.start()
        time.sleep(0.3)
        st, headers, body = c.post("/v1/_font_sleep", b"x", {"seconds": 0.1})
        assert st == 503 and problem(503, headers, body)["code"] == "busy" and headers["retry-after"] == "5"
        assert c.post("/v1/_sleep", b"x", {"seconds": 0.1})[0] == 200
        first.join()
        assert results == [200]
        assert c.post("/v1/_font_sleep", b"x", {"seconds": 0.1})[0] == 200
    finally:
        server.shutdown()
        server.server_close()
        pool.close()


def test_ots_reason_is_given(client):
    st, headers, body = client.post(PATH, ff.bad_head())
    p = problem(422, headers, body)
    assert p["reason"] and "/" not in p["reason"]


def test_over_10_mib_part(client):
    big = b"\x00\x01\x00\x00" + b"\x00" * (10 << 20)
    st, headers, body = client.post(PATH, big)
    assert st == 413 and problem(413, headers, body)["code"] == "font_too_large"
    st, headers, body = client.request("POST", PATH, big, {"Content-Type": "font/ttf"})
    assert st == 413 and problem(413, headers, body)["code"] == "font_too_large"


def test_over_the_body_cap_is_refused_before_reading(client):
    head = (
        f"POST {PATH} HTTP/1.1\r\nHost: x\r\nContent-Type: font/ttf\r\n"
        f"Content-Length: {MAX_FONT_BODY_BYTES + 1}\r\n\r\n"
    ).encode()
    with socket.create_connection(("127.0.0.1", client.port), timeout=10) as s:
        s.sendall(head)
        out = b""
        while chunk := s.recv(65536):
            out += chunk
    assert out.startswith(b"HTTP/1.1 413") and b'"font_too_large"' in out


def test_other_content_type_and_missing_file(client):
    st, headers, body = client.request("POST", PATH, ff.ttf(), {"Content-Type": "text/plain"})
    assert st == 400 and problem(400, headers, body)["code"] == "bad_request"
    st, headers, body = client.post(PATH, None, {"a": 1})
    assert st == 400 and problem(400, headers, body)["code"] == "bad_request"


@pytest.fixture()
def font_client():
    """A pool whose own limit is long; font jobs get 1 s."""
    pool = Pool(size=1, timeout=60.0, queue_wait=10.0, test_hooks=True, memory_mib=0)
    server = make_server("127.0.0.1", 0, pool, font_timeout=1.0)
    t = threading.Thread(target=server.serve_forever, daemon=True)
    t.start()
    yield Client(server.server_address[1])
    server.shutdown()
    server.server_close()
    pool.close()


def test_font_jobs_have_their_own_time_limit(font_client):
    started = time.monotonic()
    st, headers, body = font_client.post("/v1/_font_sleep", b"x", {"seconds": 30})
    assert st == 422 and problem(422, headers, body)["code"] == "font_timeout"
    assert time.monotonic() - started < 5
    r, _ = sanitized(font_client, ff.ttf())
    assert r["family"] == "Deneme Sans"


def test_a_worker_dying_on_a_font_is_the_fonts(client):
    st, headers, body = client.post("/v1/_font_die", b"x", {})
    assert st == 422 and problem(422, headers, body)["code"] == "font_unreadable"
    assert client.post(PATH, ff.ttf())[0] == 200


def test_the_sanitizer_is_stopped_on_time(monkeypatch):
    monkeypatch.setattr(fonts, "OTS_COMMAND", [sys.executable, "-c", "import time; time.sleep(30)"])
    started = time.monotonic()
    with pytest.raises(Problem) as e:
        fonts._ots(ff.ttf(), 0.5)
    assert e.value.code == "font_timeout" and time.monotonic() - started < 5


def test_default_font_time_limit_is_10_s():
    from pdf_tools import server

    assert server.FONT_TIMEOUT_SECONDS == 10.0
    assert server.FONT_TIMEOUT_SECONDS * server.FONT_OTS_SHARE < server.FONT_TIMEOUT_SECONDS


def test_large_font_under_the_service_limits(capped_client):
    """A font of almost 10 MiB within the worker's 200 MiB and the 10 s a
    font job has (CI runs this on half a CPU)."""
    data = ff.large()
    assert 9 << 20 < len(data) <= fonts.MAX_FONT_BYTES
    started = time.monotonic()
    r, out = sanitized(capped_client, data)
    took = time.monotonic() - started
    print(f"large font: {len(data) >> 10} KiB -> {len(out) >> 10} KiB WOFF2 in {took:.1f} s")
    assert took < 10
    assert r["glyphCount"] == 19000 and r["missingTurkish"] == ""
    reopens(out)
