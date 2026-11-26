"""The HTTP server: POST /v1/inspect, /v1/strip-text, /v1/place, /v1/render,
/v1/font/sanitize and GET /healthz (README.md)."""

from __future__ import annotations

import base64
import json
import os
import re
import sys
import threading
import time
import traceback
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit

from .errors import Problem
from .fonts import MAX_FONT_BYTES
from .worker import Pool

MAX_PDF_BYTES = 20 << 20
# The multipart envelope and the params part around the PDF.
MAX_BODY_BYTES = MAX_PDF_BYTES + (1 << 20)
MAX_FONT_BODY_BYTES = MAX_FONT_BYTES + (1 << 20)
# A font job's seconds (the pool's own limit is the PDFs'); OTS, which runs
# in its own process, gets FONT_OTS_SHARE of them so that it is done before
# its worker could be killed.
FONT_TIMEOUT_SECONDS = 10.0
FONT_OTS_SHARE = 0.7
# One font job at a time, whatever the workers: OTS writes the sanitized
# font (up to fonts.MAX_SFNT_BYTES) to /tmp, a 64 MiB tmpfs, and two large
# ones at once would fill it. A font request waits for the one before it as
# long as a job waits for a worker, then is busy.
# The content types a font may come as for the whole body.
_FONT_TYPES = {"font/ttf", "font/otf", "font/woff2", "font/sfnt", "application/font-sfnt", "application/octet-stream"}
# Requests being read or waiting for a worker at once; more are turned
# away (each may hold a 20 MiB body).
MAX_REQUESTS = 4

# Only with test hooks on (the tests' pools): a job that sleeps, one whose
# worker dies (and the same as font jobs).
_TEST_OPS = {"/v1/_sleep": "sleep", "/v1/_die": "die", "/v1/_font_sleep": "font-sleep", "/v1/_font_die": "font-die"}

# The client went away or stalled: nothing to answer.
_CLIENT_GONE = (BrokenPipeError, ConnectionResetError, TimeoutError)

_OPS = {
    "/v1/inspect": "inspect",
    "/v1/strip-text": "strip-text",
    "/v1/place": "place",
    "/v1/render": "render",
    "/v1/font/sanitize": "font-sanitize",
}


def parse_multipart(body: bytes, content_type: str) -> dict[str, bytes]:
    """The parts of a multipart/form-data body, by name (the last one of a
    name wins)."""
    m = re.search(r'boundary=(?:"([^"]+)"|([^;\s]+))', content_type)
    if not m:
        raise Problem("bad_request", "multipart body without a boundary")
    boundary = (m.group(1) or m.group(2)).encode("latin-1")
    delim = b"--" + boundary
    parts: dict[str, bytes] = {}
    pieces = body.split(delim)
    if len(pieces) < 3:
        raise Problem("bad_request", "malformed multipart body")
    for piece in pieces[1:-1]:
        if piece.startswith(b"\r\n"):
            piece = piece[2:]
        head, sep, data = piece.partition(b"\r\n\r\n")
        if not sep:
            raise Problem("bad_request", "malformed multipart part")
        if data.endswith(b"\r\n"):
            data = data[:-2]
        name = None
        for line in head.split(b"\r\n"):
            key, _, value = line.decode("latin-1").partition(":")
            if key.strip().lower() == "content-disposition":
                nm = re.search(r'\bname="([^"]*)"', value)
                if nm:
                    name = nm.group(1)
        if name:
            parts[name] = data
    if not pieces[-1].startswith(b"--"):
        raise Problem("bad_request", "multipart body has no closing boundary")
    return parts


def _params(raw: bytes | None) -> dict:
    if not raw:
        return {}
    try:
        value = json.loads(raw)
    except (ValueError, UnicodeDecodeError):
        raise Problem("bad_request", "params is not JSON") from None
    if not isinstance(value, dict):
        raise Problem("bad_request", "params must be a JSON object")
    return value


def _job(op: str, pdf: bytes, params: dict) -> tuple:
    if op == "inspect":
        return (pdf,)
    if op == "strip-text":
        ids = params.get("spanIds")
        if not isinstance(ids, list) or not ids or not all(isinstance(i, str) for i in ids):
            raise Problem("bad_request", "spanIds must be a non-empty list of strings")
        if len(ids) > 5000:
            raise Problem("bad_request", "too many spanIds")
        return (pdf, ids)
    if op == "place":
        return (pdf, params)
    if op == "render":
        max_px = params.get("maxPx", 2000)
        return (pdf, max_px)
    return (pdf, params)


class Handler(BaseHTTPRequestHandler):
    server_version = "pdf-tools"
    sys_version = ""
    protocol_version = "HTTP/1.1"
    # Seconds a client may take between bytes of its request.
    timeout = 30

    pool: Pool
    admission: threading.BoundedSemaphore
    font_timeout: float = FONT_TIMEOUT_SECONDS
    font_gate: threading.BoundedSemaphore

    def log_message(self, format: str, *args: object) -> None:  # noqa: A002
        pass

    def _log(self, status: int, started: float, code: str = "", size_in: int = 0, size_out: int = 0) -> None:
        ms = (time.monotonic() - started) * 1000
        line = f'{self.command} {urlsplit(self.path).path} {status} {ms:.0f}ms in={size_in} out={size_out}'
        if code:
            line += f" code={code}"
        print(line, file=sys.stdout, flush=True)

    def _send(self, status: int, body: bytes, content_type: str, extra: dict[str, str] | None = None) -> None:
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def _problem(self, p: Problem) -> int:
        extra = {}
        if p.code == "busy":
            extra["Retry-After"] = "5"
        if p.status in (411, 413):
            self.close_connection = True
            extra["Connection"] = "close"
        self._send(p.status, json.dumps(p.body()).encode(), "application/problem+json", extra)
        return p.status

    def send_error(self, code: int, message: str | None = None, explain: str | None = None) -> None:
        """The base class's errors (a bad request line, an unknown method,
        headers too long) as problem+json too. A method other than GET and
        POST is 405 method_not_allowed."""
        status = int(code)
        if status == 501:
            status = 405
        if status == 405:
            pcode = "method_not_allowed"
        elif 400 <= status < 500:
            pcode = "bad_request"
        else:
            pcode = "internal"
        body: dict[str, object] = {"type": "about:blank", "title": HTTPStatus(status).phrase, "status": status, "code": pcode}
        if status == 405:
            body["detail"] = "only GET and POST"
        elif message:
            body["detail"] = message
        data = json.dumps(body).encode()
        self.close_connection = True
        try:
            self.send_response(status)
            self.send_header("Content-Type", "application/problem+json")
            self.send_header("Content-Length", str(len(data)))
            self.send_header("Cache-Control", "no-store")
            self.send_header("X-Content-Type-Options", "nosniff")
            self.send_header("Connection", "close")
            if status == 405:
                self.send_header("Allow", "GET, POST")
            self.end_headers()
            if self.command != "HEAD":
                self.wfile.write(data)
        except OSError:
            return
        print(f"{self.command or '-'} {status} code={pcode}", file=sys.stdout, flush=True)

    def do_GET(self) -> None:  # noqa: N802
        started = time.monotonic()
        if urlsplit(self.path).path == "/healthz":
            self._send(200, b'{"status":"ok"}', "application/json")
            self._log(200, started)
            return
        status = self._problem(Problem("not_found", "no such path"))
        self._log(status, started, "not_found")

    def do_POST(self) -> None:  # noqa: N802
        started = time.monotonic()
        url = urlsplit(self.path)
        op = _OPS.get(url.path)
        if op is None and self.pool.test_hooks:
            op = _TEST_OPS.get(url.path)
        if op is None:
            self.close_connection = True
            status = self._problem(Problem("not_found", "no such path"))
            self._log(status, started, "not_found")
            return
        if not self.admission.acquire(blocking=False):
            self.close_connection = True
            status = self._problem(Problem("busy", "too many requests at once"))
            self._log(status, started, "busy")
            return
        size_in = 0
        try:
            try:
                size_in, out, ctype = self._handle(op, url.query)
            except Problem as p:
                status = self._problem(p)
                self._log(status, started, p.code, size_in)
                return
            except _CLIENT_GONE:
                raise
            except Exception:
                traceback.print_exc(file=sys.stderr)
                self.close_connection = True
                status = self._problem(Problem("internal", "unexpected error"))
                self._log(status, started, "internal", size_in)
                return
            self._send(200, out, ctype)
            self._log(200, started, "", size_in, len(out))
        except _CLIENT_GONE:
            self.close_connection = True
        finally:
            self.admission.release()

    def _read_body(self, font: bool = False) -> bytes:
        if self.headers.get("Transfer-Encoding"):
            raise Problem("length_required", "send the body with a Content-Length")
        raw = self.headers.get("Content-Length")
        if raw is None:
            raise Problem("length_required", "send the body with a Content-Length")
        try:
            length = int(raw)
        except ValueError:
            raise Problem("bad_request", "bad Content-Length") from None
        if length < 0:
            raise Problem("bad_request", "bad Content-Length")
        if font and length > MAX_FONT_BODY_BYTES:
            raise Problem("font_too_large", f"at most {MAX_FONT_BYTES >> 20} MiB")
        if length > MAX_BODY_BYTES:
            raise Problem("pdf_too_large", f"at most {MAX_PDF_BYTES >> 20} MiB")
        chunks = []
        left = length
        while left > 0:
            chunk = self.rfile.read(min(left, 1 << 20))
            if not chunk:
                raise Problem("bad_request", "the body ended early")
            chunks.append(chunk)
            left -= len(chunk)
        return b"".join(chunks)

    def _handle(self, op: str, query: str) -> tuple[int, bytes, str]:
        if op.startswith("font"):
            return self._handle_font(op)
        body = self._read_body()
        ctype = self.headers.get("Content-Type", "")
        main = ctype.split(";", 1)[0].strip().lower()
        if main == "multipart/form-data":
            parts = parse_multipart(body, ctype)
            pdf = parts.get("file")
            params = _params(parts.get("params"))
        elif main == "application/pdf":
            pdf = body
            params = {}
            q = parse_qs(query)
            if "maxPx" in q:
                try:
                    params["maxPx"] = int(q["maxPx"][0])
                except ValueError:
                    raise Problem("bad_request", "maxPx is not an integer") from None
        else:
            raise Problem("bad_request", "send multipart/form-data (file, params) or application/pdf")
        if op in _TEST_OPS.values():
            return len(body), self.pool.run(op, params.get("seconds", 1)), "text/plain"  # type: ignore[return-value]
        if not pdf:
            raise Problem("bad_request", "the file part is missing")
        if len(pdf) > MAX_PDF_BYTES:
            raise Problem("pdf_too_large", f"at most {MAX_PDF_BYTES >> 20} MiB")
        result = self.pool.run(op, *_job(op, pdf, params))
        if op == "inspect":
            return len(body), json.dumps(result, ensure_ascii=False).encode(), "application/json"
        if op == "render":
            return len(body), result, "image/png"  # type: ignore[return-value]
        return len(body), result, "application/pdf"  # type: ignore[return-value]

    def _handle_font(self, op: str) -> tuple[int, bytes, str]:
        """POST /v1/font/sanitize: the font as the file part or the whole
        body; JSON with the facts and the WOFF2 in base64."""
        body = self._read_body(font=True)
        ctype = self.headers.get("Content-Type", "")
        main = ctype.split(";", 1)[0].strip().lower()
        params: dict = {}
        if main == "multipart/form-data":
            parts = parse_multipart(body, ctype)
            data = parts.get("file")
            params = _params(parts.get("params"))
        elif main in _FONT_TYPES:
            data = body
        else:
            raise Problem("bad_request", "send multipart/form-data (file) or the font as the body (font/ttf, font/otf, font/woff2)")
        if op in _TEST_OPS.values():
            args: tuple = (params.get("seconds", 1),)
        else:
            if not data:
                raise Problem("bad_request", "the file part is missing")
            if len(data) > MAX_FONT_BYTES:
                raise Problem("font_too_large", f"at most {MAX_FONT_BYTES >> 20} MiB")
            args = (data, self.font_timeout * FONT_OTS_SHARE)
        if not self.font_gate.acquire(timeout=self.pool.queue_wait):
            raise Problem("busy", "another font is being checked")
        try:
            result = self.pool.run(op, *args, timeout=self.font_timeout)
        finally:
            self.font_gate.release()
        if op in _TEST_OPS.values():
            return len(body), result, "text/plain"  # type: ignore[return-value]
        assert isinstance(result, dict)
        result["woff2"] = base64.b64encode(result["woff2"]).decode("ascii")
        return len(body), json.dumps(result, ensure_ascii=False).encode(), "application/json"


class Server(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True
    request_queue_size = 16


def make_server(host: str, port: int, pool: Pool, font_timeout: float = FONT_TIMEOUT_SECONDS) -> Server:
    handler = type(
        "BoundHandler",
        (Handler,),
        {
            "pool": pool,
            "admission": threading.BoundedSemaphore(MAX_REQUESTS),
            "font_timeout": font_timeout,
            "font_gate": threading.BoundedSemaphore(1),
        },
    )
    return Server((host, port), handler)


def serve() -> None:
    import signal

    port = int(os.environ.get("PORT", "8080"))
    host = os.environ.get("HOST", "0.0.0.0")
    pool = Pool(
        size=int(os.environ.get("PDF_TOOLS_WORKERS", "2")),
        timeout=float(os.environ.get("PDF_TOOLS_TIMEOUT_SECONDS", "15")),
    )
    server = make_server(host, port, pool)

    def stop(*_: object) -> None:
        threading.Thread(target=server.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    print(f"pdf-tools listening on {host}:{port}", flush=True)
    try:
        server.serve_forever()
    finally:
        server.server_close()
        pool.close()
