from __future__ import annotations

import os
import sys
import threading

import pytest

HERE = os.path.dirname(__file__)
sys.path.insert(0, os.path.dirname(HERE))
sys.path.insert(0, HERE)

from helpers import Client  # noqa: E402
from pdf_tools.server import make_server  # noqa: E402
from pdf_tools.worker import Pool  # noqa: E402


def _start(pool: Pool):
    server = make_server("127.0.0.1", 0, pool)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, thread


@pytest.fixture(scope="session")
def client():
    """The service with two workers and a short timeout for the tests."""
    pool = Pool(size=2, timeout=4.0, queue_wait=10.0, test_hooks=True)
    server, thread = _start(pool)
    yield Client(server.server_address[1])
    server.shutdown()
    server.server_close()
    pool.close()


@pytest.fixture()
def small_client():
    """One worker that answers busy at once."""
    pool = Pool(size=1, timeout=4.0, queue_wait=0.3, test_hooks=True)
    server, thread = _start(pool)
    yield Client(server.server_address[1])
    server.shutdown()
    server.server_close()
    pool.close()
