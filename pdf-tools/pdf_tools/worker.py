"""Worker processes: each request's PDF work runs in one of a few child
processes, so a PDF that takes too long is stopped by killing its process.
"""

from __future__ import annotations

import multiprocessing as mp
import os
import queue
import sys
import time
import traceback
from dataclasses import dataclass

from .errors import Problem

# A worker is replaced after this many jobs (a long-lived PDF library holds
# on to memory).
JOBS_PER_WORKER = 200
# Each worker's data cap, MiB (PDF_TOOLS_WORKER_MEMORY_MIB; 0 turns it off).
DEFAULT_WORKER_MEMORY_MIB = 300


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
    if test_hooks and op == "die":
        os._exit(3)
    raise Problem("not_found", f"no operation {op}")


def _limit_memory(mib: int) -> None:
    """Caps the process's data (heap and private writable mappings), so a
    PDF that inflates to gigabytes fails here instead of filling the
    container. Linux enforces it; elsewhere it may be ignored."""
    if mib <= 0:
        return
    try:
        import resource

        limit = mib << 20
        resource.setrlimit(resource.RLIMIT_DATA, (limit, limit))
    except (ImportError, ValueError, OSError):
        pass


def main(conn, test_hooks: bool, memory_mib: int = 0) -> None:  # the child process
    # Load the libraries before the first job, then cap the memory.
    from . import ops  # noqa: F401

    _limit_memory(memory_mib)
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


def _memory_from_env() -> int:
    try:
        return int(os.environ.get("PDF_TOOLS_WORKER_MEMORY_MIB", str(DEFAULT_WORKER_MEMORY_MIB)))
    except ValueError:
        return DEFAULT_WORKER_MEMORY_MIB


class Pool:
    """A fixed number of workers; a job waits for a free one at most
    queue_wait seconds and runs at most timeout seconds. Each worker's data
    is capped at memory_mib (0: no cap; None: PDF_TOOLS_WORKER_MEMORY_MIB,
    else 300)."""

    def __init__(
        self,
        size: int = 2,
        timeout: float = 15.0,
        queue_wait: float = 10.0,
        test_hooks: bool = False,
        memory_mib: int | None = None,
    ) -> None:
        self.size = size
        self.timeout = timeout
        self.queue_wait = queue_wait
        self.test_hooks = test_hooks
        self.memory_mib = _memory_from_env() if memory_mib is None else memory_mib
        self._ctx = mp.get_context("spawn")
        self._idle: queue.Queue[_Worker] = queue.Queue()
        self._closed = False
        for _ in range(size):
            self._idle.put(self._start())

    def _start(self) -> _Worker:
        parent, child = self._ctx.Pipe()
        proc = self._ctx.Process(
            target=main,
            args=(child, self.test_hooks, self.memory_mib),
            daemon=True,
            name="pdf-tools-worker",
        )
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

    def _replace(self, w: _Worker) -> _Worker:
        """A new worker for w. When none starts, w (stopped) stays in its
        place and the next job tries again: the pool never shrinks."""
        self._stop(w)
        if self._closed:
            return w
        try:
            return self._start()
        except Exception:
            traceback.print_exc(file=sys.stderr)
            return w

    def run(self, op: str, *args: object) -> object:
        try:
            w = self._idle.get(timeout=self.queue_wait)
        except queue.Empty:
            raise Problem("busy", "every worker is busy") from None
        try:
            w, outcome = self._job(w, op, args)
        finally:
            if not self._closed:
                self._idle.put(w)
        if isinstance(outcome, Problem):
            raise outcome
        if outcome[0] == "ok":
            return outcome[1]
        _, code, detail, extra = outcome
        raise Problem(code, detail, **extra)

    def _job(self, w: _Worker, op: str, args: tuple) -> tuple[_Worker, object]:
        """Runs one job on w; answers the worker to put back (w or its
        replacement) and the reply or a Problem."""
        if not w.process.is_alive():
            w = self._replace(w)
            if not w.process.is_alive():
                return w, Problem("internal", "no worker could be started")
        try:
            w.conn.send((op, args))  # type: ignore[attr-defined]
        except (OSError, EOFError, ValueError):
            # The worker went away before the job: not the client, not the PDF.
            w = self._replace(w)
            return w, Problem("internal", "the worker could not take the job")
        if not w.conn.poll(self.timeout):  # type: ignore[attr-defined]
            w = self._replace(w)
            return w, Problem("pdf_timeout", f"the PDF took longer than {self.timeout:g} s")
        try:
            reply = w.conn.recv()  # type: ignore[attr-defined]
        except (EOFError, OSError):
            # It died on this PDF (its memory cap, a crash in a library): the
            # same PDF would do it again.
            w = self._replace(w)
            return w, Problem("pdf_unreadable", "reading the PDF stopped its worker (memory or a crash)")
        w.jobs += 1
        if w.jobs >= JOBS_PER_WORKER:
            w = self._replace(w)
        return w, reply

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
