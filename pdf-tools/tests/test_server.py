from __future__ import annotations

import json
import os
import socket
import statistics
import threading
import time

import pytest

import fixtures
from pdf_tools.server import MAX_BODY_BYTES


def problem(status, headers, body):
    assert headers["content-type"] == "application/problem+json"
    p = json.loads(body)
    assert p["status"] == status and p["type"] == "about:blank" and p["title"]
    return p["code"]


def test_healthz(client):
    status, headers, body = client.request("GET", "/healthz")
    assert status == 200 and json.loads(body) == {"status": "ok"}


def test_inspect_multipart_and_raw(client):
    data = fixtures.turkish()
    status, headers, body = client.post("/v1/inspect", data)
    assert status == 200 and headers["content-type"] == "application/json"
    r = json.loads(body)
    assert r["textCount"] == 3
    status, _, body2 = client.request("POST", "/v1/inspect", data, {"Content-Type": "application/pdf"})
    assert status == 200 and json.loads(body2) == r


def test_strip_place_render(client):
    data = fixtures.certificate()
    spans = json.loads(client.post("/v1/inspect", data)[2])["spans"]
    status, headers, out = client.post("/v1/strip-text", data, {"spanIds": [spans[0]["id"]]})
    assert status == 200 and headers["content-type"] == "application/pdf" and out.startswith(b"%PDF-")
    status, headers, out = client.post(
        "/v1/place", data, {"pageWidthPt": 595, "pageHeightPt": 842, "rect": {"x": 0, "y": 0, "w": 595, "h": 842}}
    )
    assert status == 200 and out.startswith(b"%PDF-")
    status, headers, png = client.post("/v1/render", data, {"maxPx": 800})
    assert status == 200 and headers["content-type"] == "image/png" and png.startswith(b"\x89PNG")
    status, headers, png = client.request("POST", "/v1/render?maxPx=300", data, {"Content-Type": "application/pdf"})
    assert status == 200 and png.startswith(b"\x89PNG")


@pytest.mark.parametrize(
    "path,fixture,params,status,code",
    [
        ("/v1/inspect", "encrypted", None, 422, "pdf_encrypted"),
        ("/v1/render", "two_pages", None, 422, "pdf_pages"),
        ("/v1/place", "two_pages", {"pageWidthPt": 1, "pageHeightPt": 1, "rect": {"x": 0, "y": 0, "w": 1, "h": 1}}, 422, "pdf_pages"),
        ("/v1/strip-text", "turkish", {"spanIds": []}, 400, "bad_request"),
        ("/v1/strip-text", "turkish", {"spanIds": ["42"]}, 400, "bad_request"),
        ("/v1/render", "turkish", {"maxPx": 99999}, 400, "bad_request"),
    ],
)
def test_problems(client, path, fixture, params, status, code):
    st, headers, body = client.post(path, getattr(fixtures, fixture)(), params)
    assert st == status
    assert problem(status, headers, body) == code


def test_unreadable_and_missing_file(client):
    st, headers, body = client.post("/v1/inspect", b"hello")
    assert st == 422 and problem(422, headers, body) == "pdf_unreadable"
    st, headers, body = client.post("/v1/inspect", None, {"a": 1})
    assert st == 400 and problem(400, headers, body) == "bad_request"
    st, headers, body = client.request("POST", "/v1/inspect", b"x", {"Content-Type": "text/plain"})
    assert st == 400
    st, headers, body = client.request("POST", "/v1/inspect", b"--x\r\n", {"Content-Type": "multipart/form-data; boundary=x"})
    assert st == 400 and problem(400, headers, body) == "bad_request"


def test_unknown_path(client):
    st, headers, body = client.request("POST", "/v1/nope", b"x", {"Content-Type": "application/pdf"})
    assert st == 404 and problem(404, headers, body) == "not_found"
    st, headers, body = client.request("GET", "/v1/inspect")
    assert st == 404


def _raw(port: int, head: bytes) -> bytes:
    with socket.create_connection(("127.0.0.1", port), timeout=10) as s:
        s.sendall(head)
        out = b""
        while True:
            chunk = s.recv(65536)
            if not chunk:
                break
            out += chunk
        return out


def test_too_large_is_refused_before_reading(client):
    head = (
        "POST /v1/inspect HTTP/1.1\r\nHost: x\r\nContent-Type: application/pdf\r\n"
        f"Content-Length: {MAX_BODY_BYTES + 1}\r\n\r\n"
    ).encode()
    out = _raw(client.port, head)
    assert out.startswith(b"HTTP/1.1 413") and b'"pdf_too_large"' in out


def test_pdf_part_over_20_mib(client):
    big = b"%PDF-1.7\n" + b"0" * (20 << 20)
    st, headers, body = client.post("/v1/inspect", big)
    assert st == 413 and problem(413, headers, body) == "pdf_too_large"


def test_length_required(client):
    head = b"POST /v1/inspect HTTP/1.1\r\nHost: x\r\nContent-Type: application/pdf\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n"
    out = _raw(client.port, head)
    assert out.startswith(b"HTTP/1.1 411") and b'"length_required"' in out


def test_timeout_kills_the_worker_and_the_next_job_runs(client):
    started = time.monotonic()
    st, headers, body = client.post("/v1/_sleep", b"x", {"seconds": 30})
    assert st == 422 and problem(422, headers, body) == "pdf_timeout"
    assert time.monotonic() - started < 10
    st, _, _ = client.post("/v1/inspect", fixtures.turkish())
    assert st == 200


def test_busy(small_client):
    results = []

    def sleep():
        results.append(small_client.post("/v1/_sleep", b"x", {"seconds": 1.5})[0])

    t = threading.Thread(target=sleep)
    t.start()
    time.sleep(0.4)
    st, headers, body = small_client.post("/v1/inspect", fixtures.turkish())
    t.join()
    assert st == 503 and headers["retry-after"] == "5" and problem(503, headers, body) == "busy"
    assert results == [200]


def test_each_endpoint_under_500_ms(client):
    """Bilet 04's measure: one A4 page with 30 texts."""
    data = fixtures.certificate()
    spans = json.loads(client.post("/v1/inspect", data)[2])["spans"]
    assert len(spans) == 30
    calls = {
        "inspect": ("/v1/inspect", None),
        "strip-text": ("/v1/strip-text", {"spanIds": [s["id"] for s in spans]}),
        "place": ("/v1/place", {"pageWidthPt": 595, "pageHeightPt": 842, "rect": {"x": 0, "y": 210, "w": 595, "h": 420}}),
        "render": ("/v1/render", {"maxPx": 2000}),
    }
    for name, (path, params) in calls.items():
        times = []
        for _ in range(5):
            t = time.perf_counter()
            st, _, _ = client.post(path, data, params)
            times.append((time.perf_counter() - t) * 1000)
            assert st == 200
        median = statistics.median(times)
        print(f"{name}: median {median:.0f} ms, max {max(times):.0f} ms")
        assert median < 500, name
    print(f"worker peak memory: {_worker_peak_kib()} KiB")


def _worker_peak_kib() -> str:
    """The workers' peak resident memory (Linux; the workers are this
    process's children)."""
    if not os.path.isdir("/proc"):
        return "n/a"
    peaks = []
    for pid in os.listdir("/proc"):
        if not pid.isdigit():
            continue
        try:
            with open(f"/proc/{pid}/status") as f:
                status = dict(line.split(":", 1) for line in f if ":" in line)
        except OSError:
            continue
        if int(status.get("PPid", "0").strip()) == os.getpid() and "VmHWM" in status:
            peaks.append(status["VmHWM"].strip())
    return ", ".join(peaks) or "n/a"
