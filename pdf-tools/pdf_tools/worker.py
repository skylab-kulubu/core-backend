"""Worker processes: each request's PDF work runs in one of a few child
processes, so a PDF that takes too long is stopped by killing its process.
"""

from __future__ import annotations

import multiprocessing as mp
import queue
import sys
import threading
import time
import traceback
from dataclasses import dataclass

from .errors import Problem

# A worker is replaced after this many jobs (a long-lived PDF library holds
# on to memory).
JOBS_PER_WORKER = 200


def _run(op: str, args: tuple, test_hooks: bool) -> object:
    from . import ops

    if op == "inspect":
        return ops.inspect(*args)
    if op == "strip-text":
        return ops.strip_text(*args)
    if op == "place":
        return ops.place(*args)
    if op == "render":
        return ops.render(*args)
    if test_hooks and op == "sleep":
        time.sleep(float(args[0]))
        return b"slept"
    raise Problem("not_found", f"no operation {op}")


def main(conn, test_hooks: bool) -> None:  # the child process
    # Load the libraries before the first job.
    from . import ops  # noqa: F401

    while True:
        try:
            msg = conn.recv()
        except (EOFError, OSError):
            return
        if msg is None:
            return
        op, args = msg
        try:
            conn.send(("ok", _run(op, args, test_hooks)))
        except Problem as p:
            conn.send(("problem", p.code, p.detail, p.extra))
        except MemoryError:
            conn.send(("problem", "pdf_unreadable", "the PDF needs too much memory", {}))
        except Exception as exc:  # a library error on a hostile input
            traceback.print_exc(file=sys.stderr)
            conn.send(("problem", "pdf_unreadable", f"the PDF could not be processed ({exc.__class__.__name__})", {}))


@dataclass
class _Worker:
    process: mp.Process
    conn: object
    jobs: int = 0


class Pool:
    """A fixed number of workers; a job waits for a free one at most
    queue_wait seconds and runs at most timeout seconds."""

    def __init__(self, size: int = 2, timeout: float = 15.0, queue_wait: float = 10.0, test_hooks: bool = False) -> None:
        self.size = size
        self.timeout = timeout
        self.queue_wait = queue_wait
        self.test_hooks = test_hooks
        self._ctx = mp.get_context("spawn")
        self._idle: queue.Queue[_Worker] = queue.Queue()
        self._lock = threading.Lock()
        self._closed = False
        for _ in range(size):
            self._idle.put(self._start())

    def _start(self) -> _Worker:
        parent, child = self._ctx.Pipe()
        proc = self._ctx.Process(target=main, args=(child, self.test_hooks), daemon=True, name="pdf-tools-worker")
        proc.start()
        child.close()
        return _Worker(process=proc, conn=parent)

    @staticmethod
    def _stop(w: _Worker) -> None:
        try:
            w.process.kill()
        except Exception:
            pass
        w.process.join(timeout=5)
        try:
            w.conn.close()  # type: ignore[attr-defined]
        except Exception:
            pass

    def run(self, op: str, *args: object) -> object:
        try:
            w = self._idle.get(timeout=self.queue_wait)
        except queue.Empty:
            raise Problem("busy", "every worker is busy") from None
        replace = False
        try:
            if not w.process.is_alive():
                self._stop(w)
                w = self._start()
            w.conn.send((op, args))  # type: ignore[attr-defined]
            if not w.conn.poll(self.timeout):  # type: ignore[attr-defined]
                replace = True
                raise Problem("pdf_timeout", f"the PDF took longer than {self.timeout:g} s")
            try:
                reply = w.conn.recv()  # type: ignore[attr-defined]
            except (EOFError, OSError):
                replace = True
                raise Problem("internal", "the worker stopped") from None
            w.jobs += 1
            if w.jobs >= JOBS_PER_WORKER:
                replace = True
        except Problem:
            raise
        except Exception:
            replace = True
            raise
        finally:
            if replace:
                self._stop(w)
                w = self._start() if not self._closed else w
            if not self._closed:
                self._idle.put(w)
        if reply[0] == "ok":
            return reply[1]
        _, code, detail, extra = reply
        raise Problem(code, detail, **extra)

    def close(self) -> None:
        self._closed = True
        while True:
            try:
                w = self._idle.get_nowait()
            except queue.Empty:
                break
            try:
                w.conn.send(None)  # type: ignore[attr-defined]
            except Exception:
                pass
            self._stop(w)
