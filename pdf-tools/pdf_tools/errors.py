"""The problem codes the service answers (RFC 9457, member ``code``)."""

from __future__ import annotations

# code -> HTTP status. A 4xx says the input is the cause and the same request
# fails again; a 5xx (busy, internal) may pass later.
STATUS = {
    "bad_request": 400,
    "not_found": 404,
    "method_not_allowed": 405,
    "length_required": 411,
    "pdf_too_large": 413,
    "pdf_unreadable": 422,
    "pdf_encrypted": 422,
    "pdf_pages": 422,
    "pdf_timeout": 422,
    "strip_incomplete": 422,
    "busy": 503,
    "internal": 500,
}


class Problem(Exception):
    """An answer other than the result: a code from STATUS and a detail."""

    def __init__(self, code: str, detail: str = "", **extra: object) -> None:
        super().__init__(f"{code}: {detail}" if detail else code)
        self.code = code
        self.status = STATUS[code]
        self.detail = detail
        self.extra = extra

    def body(self) -> dict[str, object]:
        from http import HTTPStatus

        out: dict[str, object] = {
            "type": "about:blank",
            "title": HTTPStatus(self.status).phrase,
            "status": self.status,
            "code": self.code,
        }
        if self.detail:
            out["detail"] = self.detail
        out.update(self.extra)
        return out
