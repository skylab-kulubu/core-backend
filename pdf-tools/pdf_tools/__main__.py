"""python -m pdf_tools [serve | health]"""

from __future__ import annotations

import os
import sys
import urllib.request


def health() -> int:
    port = os.environ.get("PORT", "8080")
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/healthz", timeout=4) as resp:
            return 0 if resp.status == 200 else 1
    except Exception:
        return 1


def main(argv: list[str]) -> int:
    cmd = argv[1] if len(argv) > 1 else "serve"
    if cmd == "health":
        return health()
    if cmd == "serve":
        from .server import serve

        serve()
        return 0
    print("usage: python -m pdf_tools [serve | health]", file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv))
