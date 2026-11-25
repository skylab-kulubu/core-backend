from __future__ import annotations

import io
import json
import os
import socket
import statistics
import sys
import threading
import time
import zlib

import pikepdf
import pytest

import fixtures
from pdf_tools.errors import Problem
from pdf_tools.server import MAX_BODY_BYTES, make_server
from pdf_tools.worker import Pool


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


@pytest.mark.parametrize("method", ["HEAD", "PUT", "DELETE", "PATCH", "OPTIONS", "BREW"])
def test_other_methods_are_405_problems(client, method):
    st, headers, body = client.request(method, "/v1/inspect")
    assert st == 405 and headers["allow"] == "GET, POST"
    assert headers["content-type"] == "application/problem+json"
    if method != "HEAD":
        assert problem(405, headers, body) == "method_not_allowed"


def test_bad_request_line_is_a_problem(client):
    out = _raw(client.port, b"GET /a b HTTP/1.1\r\nHost: x\r\n\r\n")
    head, _, body = out.partition(b"\r\n\r\n")
    assert head.startswith(b"HTTP/1.1 400") and b"application/problem+json" in head
    assert json.loads(body)["code"] == "bad_request"


def test_unexpected_error_is_internal():
    class Broken:
        test_hooks = False

        def run(self, *args):
            raise RuntimeError("boom")

    server = make_server("127.0.0.1", 0, Broken())  # type: ignore[arg-type]
    t = threading.Thread(target=server.serve_forever, daemon=True)
    t.start()
    try:
        from helpers import Client

        st, headers, body = Client(server.server_address[1]).post("/v1/inspect", fixtures.turkish())
        assert st == 500 and problem(500, headers, body) == "internal"
    finally:
        server.shutdown()
        server.server_close()


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


def test_worker_dying_on_a_job_is_the_pdfs_and_the_next_job_runs(client):
    st, headers, body = client.post("/v1/_die", b"x", {})
    assert st == 422 and problem(422, headers, body) == "pdf_unreadable"
    for _ in range(3):
        assert client.post("/v1/inspect", fixtures.turkish())[0] == 200


def _bomb(size: int = 1 << 30) -> bytes:
    """One page whose content stream is ~1 MiB deflated and `size` bytes
    (spaces) inflated."""
    comp = zlib.compressobj(9)
    chunk = b" " * (1 << 20)
    data = b"".join(comp.compress(chunk) for _ in range(size >> 20)) + comp.flush()
    pdf = pikepdf.new()
    contents = pdf.make_stream(b"")
    contents.write(data, filter=pikepdf.Name.FlateDecode)
    pdf.pages.append(
        pikepdf.Page(
            pikepdf.Dictionary(Type=pikepdf.Name.Page, MediaBox=pikepdf.Array([0, 0, 200, 200]), Contents=contents)
        )
    )
    buf = io.BytesIO()
    pdf.save(buf, compress_streams=False)
    return buf.getvalue()


@pytest.mark.skipif(not sys.platform.startswith("linux"), reason="RLIMIT_DATA is enforced on Linux")
def test_inflating_pdf_hits_the_memory_cap(capped_client):
    bomb = _bomb()
    assert len(bomb) < 2 << 20
    st, headers, body = capped_client.post("/v1/inspect", bomb)
    assert st == 422 and problem(422, headers, body) == "pdf_unreadable"
    assert capped_client.post("/v1/inspect", fixtures.turkish())[0] == 200


def test_a_dead_pipe_is_not_the_client():
    pool = Pool(size=1, timeout=10.0, queue_wait=5.0, memory_mib=0)
    try:
        w = pool._idle.get()

        class DeadPipe:
            def send(self, _):
                raise BrokenPipeError

            def close(self):
                pass

        real = w.conn
        w.conn = DeadPipe()
        pool._idle.put(w)
        with pytest.raises(Problem) as e:
            pool.run("inspect", fixtures.turkish())
        assert e.value.code == "internal"
        real.close()
        assert pool._idle.qsize() == 1
        assert pool.run("inspect", fixtures.turkish())["textCount"] == 3
    finally:
        pool.close()


def test_a_worker_that_cannot_start_does_not_shrink_the_pool(monkeypatch):
    pool = Pool(size=1, timeout=10.0, queue_wait=5.0, memory_mib=0)
    try:
        w = pool._idle.get()
        pool._stop(w)
        pool._idle.put(w)
        start = pool._start

        def fail():
            raise OSError("no process")

        monkeypatch.setattr(pool, "_start", fail)
        with pytest.raises(Problem) as e:
            pool.run("inspect", fixtures.turkish())
        assert e.value.code == "internal"
        assert pool._idle.qsize() == 1
        monkeypatch.setattr(pool, "_start", start)
        assert pool.run("inspect", fixtures.turkish())["textCount"] == 3
        assert pool._idle.qsize() == 1
    finally:
        pool.close()


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
    print(f"worker peak memory: {_worker_peak()}")


def _worker_peak() -> str:
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
