from __future__ import annotations

import http.client
import io
import json
import uuid

import pypdfium2 as pdfium
from PIL import Image, ImageChops, ImageDraw


def render_rgb(data: bytes, scale: float = 2.0) -> Image.Image:
    doc = pdfium.PdfDocument(data)
    page = doc[0]
    try:
        bitmap = page.render(scale=scale, draw_annots=False, may_draw_forms=False, rev_byteorder=True)
        return bitmap.to_pil().convert("RGB")
    finally:
        page.close()
        doc.close()


def changed_pixels(a: Image.Image, b: Image.Image, boxes: list[dict], scale: float = 2.0, pad: int = 3) -> tuple[int, int]:
    """Pixels that differ outside the boxes (pt, top-left origin), grown by
    pad pixels, and inside them."""
    assert a.size == b.size
    diff = ImageChops.difference(a, b).convert("L").point(lambda v: 255 if v else 0)
    mask = Image.new("L", a.size, 255)
    draw = ImageDraw.Draw(mask)
    for bb in boxes:
        draw.rectangle(
            [
                bb["x"] * scale - pad,
                bb["y"] * scale - pad,
                (bb["x"] + bb["w"]) * scale + pad,
                (bb["y"] + bb["h"]) * scale + pad,
            ],
            fill=0,
        )
    outside = ImageChops.multiply(diff, mask).histogram()[255]
    inside = ImageChops.multiply(diff, ImageChops.invert(mask)).histogram()[255]
    return outside, inside


def multipart(pdf: bytes | None, params: dict | None = None) -> tuple[bytes, str]:
    boundary = uuid.uuid4().hex
    out = io.BytesIO()
    if pdf is not None:
        out.write(
            f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="a.pdf"\r\n'
            "Content-Type: application/pdf\r\n\r\n".encode()
        )
        out.write(pdf)
        out.write(b"\r\n")
    if params is not None:
        out.write(f'--{boundary}\r\nContent-Disposition: form-data; name="params"\r\n\r\n'.encode())
        out.write(json.dumps(params).encode())
        out.write(b"\r\n")
    out.write(f"--{boundary}--\r\n".encode())
    return out.getvalue(), f"multipart/form-data; boundary={boundary}"


class Client:
    def __init__(self, port: int) -> None:
        self.port = port

    def request(self, method: str, path: str, body: bytes | None = None, headers: dict | None = None):
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=60)
        try:
            conn.request(method, path, body=body, headers=headers or {})
            resp = conn.getresponse()
            data = resp.read()
            return resp.status, dict((k.lower(), v) for k, v in resp.getheaders()), data
        finally:
            conn.close()

    def post(self, path: str, pdf: bytes | None, params: dict | None = None):
        body, ctype = multipart(pdf, params)
        return self.request("POST", path, body, {"Content-Type": ctype})
